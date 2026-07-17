package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestMain lets this test binary impersonate the local-offload CLIs when
// re-exec'd with the MR_TEST_LOCAL_STUB sentinel set (the standard Go pattern
// for faking an external binary — cf. codexlane's binaryName injection). A
// normal `go test` run leaves the env unset and runs the suite unchanged.
func TestMain(m *testing.M) {
	switch os.Getenv("MR_TEST_LOCAL_STUB") {
	case "defer":
		// Stand in for `offload-harness <verb> - --json` → structured DEFER.
		os.Stdout.WriteString(`{"ok":false,"deferred":true,"reason":"input exceeds local window","meta":{}}` + "\n")
		os.Exit(0)
	case "ok":
		os.Stdout.WriteString(`{"ok":true,"deferred":false,"result":{"summary":"a gist"},"meta":{}}` + "\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestRunLocalLaneDryRunDoorSelection: a verify-gate node dry-runs through the
// cascade door with the triage verb; a hard-case-reclaim node dry-runs through
// the agent door. Pins the S3R-1 routing rule end-to-end at the CMD seam.
func TestRunLocalLaneDryRunDoorSelection(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())

	var out bytes.Buffer
	// verify-gate → cascade door, triage verb.
	code, err := runLocalLane(&out, "check this", "verify-gate", "qwythos", "", 30, false, "cli", "d", recFields{}, strategyFields{})
	if err != nil || code != 0 {
		t.Fatalf("dry-run must succeed: code=%d err=%v", code, err)
	}
	s := out.String()
	if !strings.Contains(s, `"door": "cascade"`) || !strings.Contains(s, `"verb": "triage"`) {
		t.Fatalf("verify-gate must dry-run the cascade door with triage: %s", s)
	}

	out.Reset()
	// hard-case-reclaim → agent door.
	code, err = runLocalLane(&out, "reclaim", "hard-case-reclaim", "qwythos-think", "", 30, false, "cli", "d", recFields{}, strategyFields{})
	if err != nil || code != 0 {
		t.Fatalf("dry-run must succeed: code=%d err=%v", code, err)
	}
	if !strings.Contains(out.String(), `"door": "agent"`) {
		t.Fatalf("hard-case-reclaim must dry-run the agent door: %s", out.String())
	}
}

// TestRunLocalLaneFailOpenAppendsOneReceiptNoLedger is the S3R-1 fail-open +
// S3R-4 one-receipt + S3R-10 no-ledger contract, all in one: with the local
// binaries stripped from PATH, a live local dispatch fails OPEN to a classified
// spawn_error (never a Go error), appends EXACTLY ONE receipt, and writes NO
// ledger file (the free lane takes no ledger lock and meters nothing but the
// receipt).
func TestRunLocalLaneFailOpenAppendsOneReceiptNoLedger(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	t.Setenv("PATH", "") // strip offload-harness/local-agent → spawn_error

	var out bytes.Buffer
	// mechanical-text → cascade door; the missing binary must fail open.
	code, err := runLocalLane(&out, "summarize me", "mechanical-text", "gemma4-cascade", "", 30, true,
		"strategy", "node0", recFields{TaskClass: "mechanical-text"},
		strategyFields{DispatchID: "abc123", StepID: 0, Attempt: 1})
	if err != nil {
		t.Fatalf("a missing binary must fail open (classified outcome), not error: %v", err)
	}
	if code != exitNotOK {
		t.Fatalf("a spawn_error must exit %d, got %d", exitNotOK, code)
	}

	recs := loadReceipts(dispatchPath())
	if len(recs) != 1 {
		t.Fatalf("S3R-4: exactly ONE receipt per local node, got %d: %+v", len(recs), recs)
	}
	r := recs[0]
	if r.OutcomeClass != "spawn_error" || r.Lane != "local" {
		t.Fatalf("receipt must be a local spawn_error: %+v", r)
	}
	if r.Origin != "strategy" || r.DispatchID != "abc123" || r.StepID != 0 || r.Attempt != 1 {
		t.Fatalf("the ONE receipt must carry the strategy DAG identity (sf.stamp): %+v", r)
	}
	// S3R-10: the free local lane must NOT create/touch the ledger.
	if _, statErr := os.Stat(ledgerPath()); statErr == nil {
		t.Fatalf("S3R-10: the free local lane must take no ledger lock — no ledger file should exist at %s", ledgerPath())
	}
}

// TestRunLocalLaneDeferredRelegates pins that a cascade DEFER surfaces as exit 3
// (relegation), so the DAG/scheduled-wrapper escalates to a cloud alternative
// rather than re-laning. The fake offload-harness is this test binary re-exec'd
// (TestMain, MR_TEST_LOCAL_STUB=defer) — portable and race-safe.
func TestRunLocalLaneDeferredRelegates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	t.Setenv("MR_TEST_LOCAL_STUB", "defer")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfgBytes, _ := json.Marshal(map[string]string{"local_offload_bin": self})
	if err := os.WriteFile(configPath(), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := runLocalLane(&out, "summarize me", "doc-summarize", "qwythos", "", 30, true,
		"strategy", "node0", recFields{TaskClass: "doc-summarize"}, strategyFields{DispatchID: "d", StepID: 1})
	if err != nil {
		t.Fatalf("defer must not error: %v", err)
	}
	if code != exitDeferred {
		t.Fatalf("a structured DEFER must exit %d (relegation), got %d; out=%s", exitDeferred, code, out.String())
	}
	recs := loadReceipts(dispatchPath())
	if len(recs) != 1 || recs[0].OutcomeClass != "deferred" {
		t.Fatalf("a defer must append exactly one 'deferred' receipt: %+v", recs)
	}
	// S3R-10: still no ledger for a deferred free-lane node.
	if _, statErr := os.Stat(ledgerPath()); statErr == nil {
		t.Fatalf("S3R-10: a deferred local node must not create a ledger file")
	}
}

// TestRunLocalLaneOKThroughStub: a cascade OK result exits 0 with one 'ok'
// receipt — the happy path through the same re-exec stub.
func TestRunLocalLaneOKThroughStub(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	t.Setenv("MR_TEST_LOCAL_STUB", "ok")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfgBytes, _ := json.Marshal(map[string]string{"local_offload_bin": self})
	if err := os.WriteFile(configPath(), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := runLocalLane(&out, "summarize me", "mechanical-text", "gemma4-cascade", "", 30, true,
		"cli", "node0", recFields{TaskClass: "mechanical-text"}, strategyFields{})
	if err != nil {
		t.Fatalf("ok must not error: %v", err)
	}
	if code != 0 {
		t.Fatalf("a cascade ok must exit 0, got %d; out=%s", code, out.String())
	}
	recs := loadReceipts(dispatchPath())
	if len(recs) != 1 || recs[0].OutcomeClass != "ok" {
		t.Fatalf("an ok cascade must append exactly one 'ok' receipt: %+v", recs)
	}
}
