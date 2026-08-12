package main

import (
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/policyeval"
)

// seedConfigs resolves a lane to a config by rank, and router.Seed() is a MAP.
// Without a tie-break on the config key, Go's randomized map iteration decides
// which of several equally-ranked entries wins — observed live as the claude
// reference resolving to |xhigh on one run and |high on the next, which
// changes the scorecard's reference (and therefore every quality ratio) run to
// run.
//
// One call cannot catch it: the randomization has to be given room to differ.
// Repeat until either it diverges (fail) or the repetitions make agreement
// meaningful.
func TestSeedConfigsIsDeterministic(t *testing.T) {
	first, firstAll := seedConfigs()
	for i := 0; i < 200; i++ {
		byLane, all := seedConfigs()
		if len(byLane) != len(first) {
			t.Fatalf("iteration %d: lane count changed %d -> %d", i, len(first), len(byLane))
		}
		for lane, want := range first {
			if got := byLane[lane]; got != want {
				t.Fatalf("iteration %d: lane %q resolved to %s, first run said %s — map iteration is deciding the pick",
					i, lane, got.Key(), want.Key())
			}
		}
		if len(all) != len(firstAll) {
			t.Fatalf("iteration %d: ranked-config count changed %d -> %d", i, len(firstAll), len(all))
		}
		for j := range all {
			if all[j] != firstAll[j] {
				t.Fatalf("iteration %d: ranked config %d changed %s -> %s", i, j, firstAll[j].Key(), all[j].Key())
			}
		}
	}
}

// The resolved config must be the lane's best-ranked entry, with ties broken
// lexically — not merely stable, but stable on the RIGHT value.
func TestSeedConfigsPicksBestRankThenLexicalKey(t *testing.T) {
	byLane, _ := seedConfigs()
	// Recompute the expectation straight from the seed, independently of the
	// function under test.
	type best struct {
		rank int
		key  string
	}
	want := map[string]best{}
	for _, entries := range router.Seed() {
		for _, e := range entries {
			k := policyeval.Config{Lane: e.Lane, Model: e.Model,
				Effort: policyeval.NormalizeEffort(e.Effort)}.Key()
			b, ok := want[e.Lane]
			if !ok || e.Rank < b.rank || (e.Rank == b.rank && k < b.key) {
				want[e.Lane] = best{rank: e.Rank, key: k}
			}
		}
	}
	if len(byLane) != len(want) {
		t.Fatalf("lane count: got %d want %d", len(byLane), len(want))
	}
	for lane, w := range want {
		if got := byLane[lane]; got.Key() != w.key {
			t.Fatalf("lane %q resolved to %s; best-rank-then-lexical is %s", lane, got.Key(), w.key)
		}
	}
}
