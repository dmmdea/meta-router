package router

// Task 15 fault matrix — router. Every fault fails OPEN to a safe default,
// NEVER a panic. Rows already covered by the primary suite are referenced in
// the evidence doc:
//   - unknown class → quality-first default (claude-opus-4-8):
//     TestRouteRankAndMask/"unknown class defaults quality-first";
//   - empty/all-open ledger → routing proceeds, all lanes candidate:
//     the openStates() cases throughout TestRouteRankAndMask;
//   - all-masked → relegation with earliest resume (not a panic):
//     TestRouteRankAndMask/"all masked → relegation".
// This file adds the corrupt/missing rank-table Load row the primary suite did
// not exercise: a broken operator override must fail open to the compiled Seed()
// table, never to an empty table (which would route everything to nothing).

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A missing override file → Seed() (the compiled default). No file is the
// normal state; it must not error or empty the table.
func TestFaultLoadMissingFileFailsOpenToSeed(t *testing.T) {
	tbl := Load(filepath.Join(t.TempDir(), "no-such-rank-table.json"))
	if len(tbl) != len(Seed()) {
		t.Fatalf("missing override must fail open to the full Seed() table: got %d classes, want %d", len(tbl), len(Seed()))
	}
	// And it must still route (a real routing decision, not an empty map).
	now := time.Now().UTC()
	d := Route(tbl, HardRepo, map[string]LaneState{"claude": {State: "open"}}, 0, now)
	if d.Lane != "claude" || d.Model != "claude-opus-4-8" {
		t.Fatalf("failed-open table must still route hard-repo to opus: %+v", d)
	}
}

// A corrupt override file (not JSON) → Seed(). The operator's broken file must
// never brick routing; it degrades to the researched default with no panic.
func TestFaultLoadCorruptFileFailsOpenToSeed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rank-table.json")
	if err := os.WriteFile(p, []byte("###not json at all###"), 0o644); err != nil {
		t.Fatal(err)
	}
	tbl := Load(p)
	if len(tbl) != len(Seed()) {
		t.Fatalf("corrupt override must fail open to Seed(): got %d classes", len(tbl))
	}
}

// A syntactically valid but EMPTY table on disk → Seed() (len==0 guard). An
// empty JSON object would otherwise route every class to a relegation.
func TestFaultLoadEmptyTableFailsOpenToSeed(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rank-table.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(Load(p)) != len(Seed()) {
		t.Fatal("an empty {} table must fail open to Seed(), not route everything to nothing")
	}
}
