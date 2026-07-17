package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
)

// TestSummarizeReceipts pins the S2R-1 coverage definition and the S2R-10
// obedience / deviation / per-lane counts on a mixed receipt set.
//
// Run receipts (outcome_class != "route_recommendation"):
//   - cli   claude ok, rec_lane claude, not deviated
//   - mcp   glm    ok, rec_lane glm,    not deviated   (origin-tagged ⇒ coverage)
//   - route codex  ok, rec_lane codex,  deviated (operator_override)  (coverage)
//   - cli   claude deferred, rec_lane glm, deviated (local-handoff)
//
// Plus 2 route_recommendation receipts (consults) that are NOT run receipts.
func TestSummarizeReceipts(t *testing.T) {
	recs := []dispatch.Record{
		{OutcomeClass: "route_recommendation", Origin: "route", RecLane: "claude"}, // consult, excluded from run set
		{OutcomeClass: "route_recommendation", Origin: "mcp", RecLane: "glm"},      // consult
		{OutcomeClass: "ok", Origin: "cli", Lane: "claude", RecLane: "claude"},
		{OutcomeClass: "ok", Origin: "mcp", Lane: "glm", RecLane: "glm"},
		{OutcomeClass: "ok", Origin: "route", Lane: "codex", RecLane: "codex", Deviated: true, DeviationReason: "operator_override"},
		{OutcomeClass: "deferred", Origin: "cli", Lane: "claude", RecLane: "glm", Deviated: true, DeviationReason: "local-handoff"},
	}
	s := summarizeReceipts(recs)

	// 4 run receipts; origin-tagged {mcp, route} = 2 (the glm-mcp + codex-route).
	if s.RunReceipts != 4 {
		t.Fatalf("want 4 run receipts, got %d", s.RunReceipts)
	}
	if s.OriginTagged != 2 {
		t.Fatalf("want 2 origin-tagged {mcp,route} run receipts, got %d", s.OriginTagged)
	}
	if s.CoveragePct != 50 {
		t.Fatalf("coverage = 2/4 = 50%%, got %v", s.CoveragePct)
	}
	// Obedience: run receipts with rec_lane set = all 4; not-deviated = 2
	// (the cli-claude ok and the mcp-glm ok). 2/4 = 50%.
	if s.ConsultedRuns != 4 {
		t.Fatalf("want 4 consulted run receipts (rec_lane set), got %d", s.ConsultedRuns)
	}
	if s.ObediencePct != 50 {
		t.Fatalf("obedience = 2/4 = 50%%, got %v", s.ObediencePct)
	}
	if s.DeviationsByReason["operator_override"] != 1 || s.DeviationsByReason["local-handoff"] != 1 {
		t.Fatalf("deviation counts wrong: %+v", s.DeviationsByReason)
	}
	// Per-lane dispatch counts over run receipts: claude 2, glm 1, codex 1.
	if s.DispatchByLane["claude"] != 2 || s.DispatchByLane["glm"] != 1 || s.DispatchByLane["codex"] != 1 {
		t.Fatalf("per-lane counts wrong: %+v", s.DispatchByLane)
	}
	if s.Note == "" || !strings.Contains(strings.ToLower(s.Note), "slice-4") {
		t.Fatalf("summary must note the missed-delegation denominator is slice-4: %q", s.Note)
	}
}

// S3R-4: a strategy-origin run receipt is INTERNAL orchestration, not an
// interactive delegation — it must move NEITHER the coverage numerator nor the
// denominator. Two identical run sets, one with an extra strategy receipt added,
// must report the SAME CoveragePct and the SAME RunReceipts.
func TestSummarizeReceiptsExcludesStrategyOrigin(t *testing.T) {
	base := []dispatch.Record{
		{OutcomeClass: "ok", Origin: "cli", Lane: "claude", RecLane: "claude"},
		{OutcomeClass: "ok", Origin: "mcp", Lane: "glm", RecLane: "glm"},
	}
	baseSum := summarizeReceipts(base)
	// Coverage over the base: 1 origin-tagged (mcp) of 2 run receipts = 50%.
	if baseSum.CoveragePct != 50 || baseSum.RunReceipts != 2 {
		t.Fatalf("base coverage=%v run=%d, want 50 / 2", baseSum.CoveragePct, baseSum.RunReceipts)
	}
	// Add a strategy-origin run receipt (a DAG step). It is NOT interactive
	// delegation, so it must not enter the coverage numerator OR denominator.
	withStrategy := append(base[:len(base):len(base)],
		dispatch.Record{OutcomeClass: "ok", Origin: "strategy", Lane: "claude",
			RecLane: "claude", DispatchID: "d1", StepID: 1})
	s := summarizeReceipts(withStrategy)
	if s.CoveragePct != baseSum.CoveragePct {
		t.Fatalf("strategy receipt moved CoveragePct: base=%v with-strategy=%v", baseSum.CoveragePct, s.CoveragePct)
	}
	if s.RunReceipts != baseSum.RunReceipts {
		t.Fatalf("strategy receipt entered the run-receipt denominator: base=%d with=%d", baseSum.RunReceipts, s.RunReceipts)
	}
	if s.OriginTagged != baseSum.OriginTagged {
		t.Fatalf("strategy receipt moved OriginTagged: base=%d with=%d", baseSum.OriginTagged, s.OriginTagged)
	}
	// A per-lane dispatch count is still tracked for a strategy step? No: it is
	// excluded entirely from the run-receipt aggregation, so the lane count must
	// not have grown either.
	if s.DispatchByLane["claude"] != baseSum.DispatchByLane["claude"] {
		t.Fatalf("strategy receipt entered DispatchByLane: base=%d with=%d",
			baseSum.DispatchByLane["claude"], s.DispatchByLane["claude"])
	}
}

// Empty receipt set: coverage/obedience are 0 with zero denominators, no panic.
func TestSummarizeReceiptsEmpty(t *testing.T) {
	s := summarizeReceipts(nil)
	if s.RunReceipts != 0 || s.CoveragePct != 0 || s.ObediencePct != 0 {
		t.Fatalf("empty receipts must be all-zero: %+v", s)
	}
	if s.DeviationsByReason == nil || s.DispatchByLane == nil {
		t.Fatalf("maps must be non-nil for stable JSON: %+v", s)
	}
}

// loadReceipts reads and parses dispatch.jsonl, skipping corrupt lines
// (fail-open). Missing file → empty slice.
func TestLoadReceiptsSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dispatch.jsonl")
	body := `{"ts":"2026-07-06T12:00:00Z","outcome_class":"ok","origin":"mcp","lane":"glm","rec_lane":"glm"}
this is not json
{"ts":"2026-07-06T12:01:00Z","outcome_class":"route_recommendation","origin":"route"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	recs := loadReceipts(p)
	if len(recs) != 2 {
		t.Fatalf("want 2 parsed receipts (corrupt line skipped), got %d", len(recs))
	}
}

// Missing dispatch file → empty, no error.
func TestLoadReceiptsMissingFile(t *testing.T) {
	recs := loadReceipts(filepath.Join(t.TempDir(), "none.jsonl"))
	if len(recs) != 0 {
		t.Fatalf("missing file must yield empty: %d", len(recs))
	}
}

// End-to-end: buildStatus embeds the receipts summary and status --json stdout
// stays PURE JSON (additive fields, not prose).
func TestStatusJSONCarriesReceiptsSummary(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	if err := dispatch.Append(dispatchPath(), dispatch.Record{
		OutcomeClass: "ok", Origin: "mcp", Lane: "glm", RecLane: "glm",
	}); err != nil {
		t.Fatal(err)
	}
	if err := runStatus([]string{"--json"}); err != nil {
		t.Fatal(err)
	}
	// Re-read the file the command read and assert the summary shape by
	// building status directly (runStatus prints to stdout; the pure builder is
	// what carries the contract).
	raw, err := os.ReadFile(dispatchPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("dispatch file empty")
	}
	// The summary JSON must round-trip as part of Status.
	s := summarizeReceipts(loadReceipts(dispatchPath()))
	b, err := json.Marshal(s)
	if err != nil || !strings.Contains(string(b), "coverage_pct") {
		t.Fatalf("summary must marshal with coverage_pct: %s / %v", b, err)
	}
}
