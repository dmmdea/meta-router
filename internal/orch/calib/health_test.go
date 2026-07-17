package calib

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// E6: the trace-health assessor is the loud alarm for the "capacity learning
// built but UNFED" state — quota-trace.jsonl missing (the current live state),
// empty, or not growing means calib.Fit silently never learns. AssessTrace makes
// that visible instead of silent.
func TestAssessTraceMissingEmptyFreshStale(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()

	missing := AssessTrace(filepath.Join(dir, "nope.jsonl"), now, 48*time.Hour)
	if missing.Exists || !missing.Stale || missing.Rows != 0 {
		t.Fatalf("missing trace must be {Exists:false Stale:true Rows:0}, got %+v", missing)
	}

	empty := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(empty, nil, 0o644)
	e := AssessTrace(empty, now, 48*time.Hour)
	if !e.Exists || !e.Stale || e.Rows != 0 {
		t.Fatalf("empty trace must be {Exists:true Stale:true Rows:0}, got %+v", e)
	}

	rows := filepath.Join(dir, "rows.jsonl")
	os.WriteFile(rows, []byte(
		`{"ts":"2026-07-08T10:00:00Z","lane":"claude","window":"5h","used_pct":10,"shadow_tokens":100}`+"\n"+
			`{"ts":"2026-07-08T11:00:00Z","lane":"claude","window":"5h","used_pct":20,"shadow_tokens":200}`+"\n"), 0o644)
	fresh := AssessTrace(rows, now, 48*time.Hour)
	if !fresh.Exists || fresh.Stale || fresh.Rows != 2 || !fresh.LastTS.Equal(time.Date(2026, 7, 8, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("fresh 2-row trace misread: %+v", fresh)
	}

	stale := AssessTrace(rows, now.Add(72*time.Hour), 48*time.Hour)
	if !stale.Stale {
		t.Fatalf("a trace whose last row is 73h old at staleAfter=48h must be stale, got %+v", stale)
	}
}
