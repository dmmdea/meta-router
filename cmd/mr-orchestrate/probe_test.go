package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// Task 8: probe --stream's capture is real provider signal — ingestStreamSignal
// feeds it through the cross-process ledger.Update transaction. Driven here
// against the COMMITTED live capture on statepaths-resolved real-path state
// (no live call: the fixture IS the capture, per group-D adjudication).
func TestIngestStreamSignalRealPath(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude", "stream-events-sonnet.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC) // before the fixture's 04:30Z reset
	n, err := ingestStreamSignal(raw, now)
	if err != nil || n != 1 {
		t.Fatalf("fixture carries one fresh rate_limit_event: n=%d err=%v", n, err)
	}
	b, ok := ledger.Open(ledgerPath()).Bucket("claude", ledger.Win5h)
	if !ok || !b.ResetsAt.Equal(time.Unix(1783312200, 0).UTC()) {
		t.Fatalf("the anchor must persist through the Update transaction: %+v", b)
	}
	if b.UsedPct != -1 || b.Source == "provider" {
		t.Fatalf("allowed events anchor only — no fabricated percentage: %+v", b)
	}
	// Idempotent from the ledger's view: a re-ingest of the same capture leaves
	// the same anchor (authoritative overwrite with the identical reset).
	if n, err := ingestStreamSignal(raw, now); err != nil || n != 1 {
		t.Fatalf("re-ingest must stay fail-open: n=%d err=%v", n, err)
	}
	if b, _ := ledger.Open(ledgerPath()).Bucket("claude", ledger.Win5h); !b.ResetsAt.Equal(time.Unix(1783312200, 0).UTC()) {
		t.Fatalf("re-ingest must not move the anchor: %+v", b)
	}
}
