package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
)

// Fixture timestamps are fixed in 2020 ON PURPOSE: any implementation that
// anchors the window to time.Now() instead of the newest receipt excludes the
// whole fixture and fails these tests loudly.
var repT0 = time.Date(2020, 3, 1, 12, 0, 0, 0, time.UTC)

func repFixture() []dispatch.Record {
	return []dispatch.Record{
		// day 0: an interactive claude run, consulted + obeyed, rated good
		{TS: repT0, Lane: "claude", Model: "opus", AttributedModels: []string{"opus"},
			OutcomeClass: "ok", Origin: "mcp", RecLane: "claude", TokensIn: 100, TokensOut: 50,
			NotionalUSD: 1.5, Quality: "good"},
		// day 0: a consult (route recommendation) carrying spend-down provenance
		{TS: repT0.Add(time.Hour), Lane: "claude", OutcomeClass: "route_recommendation",
			Batch: true, SpendDownBoost: 2},
		// day 1: a codex run that silently fell back, deviated, unrated, retry
		{TS: repT0.Add(24 * time.Hour), Lane: "codex", Model: "gpt-5", AttributedModels: []string{"gpt-5-mini"},
			OutcomeClass: "ok", Origin: "cli", RecLane: "claude", Deviated: true,
			DeviationReason: "quota", TokensIn: 10, TokensOut: 5, CashUSD: 0.02, Attempt: 1},
		// day 1: a refused export — ran nothing
		{TS: repT0.Add(25 * time.Hour), Lane: "glm", OutcomeClass: "egress_denied"},
		// day 2: a strategy step (internal orchestration) on claude
		{TS: repT0.Add(48 * time.Hour), Lane: "claude", Model: "opus", OutcomeClass: "ok",
			Origin: "strategy", DispatchID: "d1", StepID: 1, TokensIn: 30, TokensOut: 20},
		// day 9: a rotated glm run, rated bad
		{TS: repT0.Add(9 * 24 * time.Hour), Lane: "glm", Model: "glm-4", OutcomeClass: "error",
			Origin: "route", RotationFrom: "default", RotationReason: "typed_limit",
			TokensIn: 7, Quality: "bad"},
	}
}

func TestReportAggregates(t *testing.T) {
	r := buildReport(repFixture(), 0, 0, "")
	if r.Receipts != 6 || r.Consults != 1 || r.Runs != 3 || r.StrategySteps != 1 {
		t.Fatalf("counts: %+v", r)
	}
	if r.StrategyRuns != 1 || r.Retries != 1 || r.EgressDenied != 1 {
		t.Fatalf("strategy/retries/egress: %+v", r)
	}
	if r.SilentFallbacks != 1 || r.FallbackPairs["gpt-5→gpt-5-mini"] != 1 {
		t.Fatalf("fallback: %+v", r.FallbackPairs)
	}
	if r.BatchConsults != 1 || r.BoostInfluenced != 1 {
		t.Fatalf("spend-down: %+v", r)
	}
	if r.RotationsByReason["typed_limit"] != 1 {
		t.Fatalf("rotations: %+v", r.RotationsByReason)
	}
	if r.Unrated != 1 { // codex run has no verdict; good+bad are rated
		t.Fatalf("unrated: %d", r.Unrated)
	}
	cl := r.Lanes["claude"]
	if cl.Runs != 1 || cl.StrategySteps != 1 || cl.TokensIn != 130 || cl.TokensOut != 70 {
		t.Fatalf("claude lane (steps must count into tokens): %+v", cl)
	}
	if cl.Good != 1 || r.Lanes["glm"].Bad != 1 || r.Lanes["codex"].Deviated != 1 {
		t.Fatalf("quality/deviation per lane")
	}
	if r.Lanes["glm"].EgressDenied != 1 || r.Lanes["glm"].Runs != 1 {
		t.Fatalf("glm lane: %+v", r.Lanes["glm"])
	}
	if r.From != repT0 || r.To != repT0.Add(9*24*time.Hour) {
		t.Fatalf("span: %v → %v", r.From, r.To)
	}
	// audit taxonomy embedded: 3 run receipts (strategy step excluded), 2 origin-tagged
	if r.Adherence.RunReceipts != 3 || r.Adherence.OriginTagged != 2 {
		t.Fatalf("adherence: %+v", r.Adherence)
	}
}

// The window must anchor to the newest RECEIPT, not the wall clock: with
// -days 2 over a 2020 fixture, day 0 and day 1 fall outside [day7, day9].
func TestReportWindowAnchorsToNewestReceipt(t *testing.T) {
	r := buildReport(repFixture(), 0, 2, "")
	if r.Receipts != 1 || r.Runs != 1 {
		t.Fatalf("want exactly the day-9 receipt, got %d receipts %d runs", r.Receipts, r.Runs)
	}
	if _, ok := r.Lanes["claude"]; ok {
		t.Fatalf("claude receipts are 7+ days older than the anchor and must be excluded")
	}
	// A clock-anchored implementation would have excluded EVERYTHING (fixture
	// is in 2020) and rendered an empty report — caught above.
}

func TestReportLaneFilter(t *testing.T) {
	r := buildReport(repFixture(), 0, 0, "codex")
	if r.Receipts != 1 || r.Runs != 1 || len(r.Lanes) != 1 {
		t.Fatalf("lane filter: %+v", r)
	}
}

func TestReportDeterministicOnFixedInput(t *testing.T) {
	a, b := buildReport(repFixture(), 1, 0, ""), buildReport(repFixture(), 1, 0, "")
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two builds over the same input differ")
	}
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if !bytes.Equal(ja, jb) {
		t.Fatal("JSON render differs across runs")
	}
	var ra, rb bytes.Buffer
	renderReport(&ra, a)
	renderReport(&rb, b)
	if !bytes.Equal(ra.Bytes(), rb.Bytes()) {
		t.Fatal("text render differs across runs")
	}
	if !strings.Contains(ra.String(), "WARN: 1 unparseable line(s) skipped") {
		t.Fatalf("skipped lines must be surfaced in the render:\n%s", ra.String())
	}
}

func TestReportEmpty(t *testing.T) {
	r := buildReport(nil, 0, 0, "")
	if r.Receipts != 0 {
		t.Fatalf("empty: %+v", r)
	}
	var buf bytes.Buffer
	renderReport(&buf, r)
	if !strings.Contains(buf.String(), "no receipts in scope") {
		t.Fatalf("empty render:\n%s", buf.String())
	}
}

func TestLoadReceiptsCountedStates(t *testing.T) {
	dir := t.TempDir()
	// absent file → fs.ErrNotExist, distinguishable from "empty log"
	_, _, err := loadReceiptsCounted(filepath.Join(dir, "absent.jsonl"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("absent file: want fs.ErrNotExist, got %v", err)
	}
	// two good lines + one corrupt line + one blank → 2 records, 1 skipped
	p := filepath.Join(dir, "dispatch.jsonl")
	good, _ := json.Marshal(dispatch.Record{TS: repT0, Lane: "claude", OutcomeClass: "ok"})
	content := string(good) + "\n" + "{corrupt\n" + "\n" + string(good) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	recs, skipped, err := loadReceiptsCounted(p)
	if err != nil || len(recs) != 2 || skipped != 1 {
		t.Fatalf("got %d recs, %d skipped, err=%v", len(recs), skipped, err)
	}
}
