package main

import (
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/policyeval"
)

// rankedConfigs walks a MAP (router.Table), so without a total order Go's
// randomized iteration decides which equally-ranked config represents a lane —
// observed live as the scorecard's reference flipping between |xhigh and |high
// across two runs over identical input, which silently rebases every quality
// ratio. One call cannot catch it; repeat until agreement is meaningful.
func TestRankedConfigsIsDeterministic(t *testing.T) {
	firstByLane, firstAll := rankedConfigs(router.Seed())
	for i := 0; i < 200; i++ {
		byLane, all := rankedConfigs(router.Seed())
		if len(all) != len(firstAll) {
			t.Fatalf("iteration %d: config count changed %d -> %d", i, len(firstAll), len(all))
		}
		for j := range all {
			if all[j] != firstAll[j] {
				t.Fatalf("iteration %d: ranked config %d changed %s -> %s", i, j, firstAll[j].Key(), all[j].Key())
			}
		}
		for lane, want := range firstByLane {
			got := byLane[lane]
			if len(got) != len(want) {
				t.Fatalf("iteration %d: lane %q order length changed", i, lane)
			}
			for j := range got {
				if got[j] != want[j] {
					t.Fatalf("iteration %d: lane %q position %d changed %s -> %s",
						i, lane, j, want[j].Key(), got[j].Key())
				}
			}
		}
	}
}

// Each lane's order is rank-ascending, ties broken lexically on the full key —
// a TOTAL order, so nothing is left for map iteration to decide.
func TestRankedConfigsOrderIsTotal(t *testing.T) {
	byLane, _ := rankedConfigs(router.Seed())
	rankOf := map[string]int{}
	for _, entries := range router.Seed() {
		for _, e := range entries {
			k := policyeval.Config{Lane: e.Lane, Model: e.Model,
				Effort: policyeval.NormalizeEffort(e.Effort)}.Key()
			if r, seen := rankOf[k]; !seen || e.Rank < r {
				rankOf[k] = e.Rank
			}
		}
	}
	for lane, cfgs := range byLane {
		for i := 1; i < len(cfgs); i++ {
			pr, cr := rankOf[cfgs[i-1].Key()], rankOf[cfgs[i].Key()]
			if pr > cr || (pr == cr && cfgs[i-1].Key() > cfgs[i].Key()) {
				t.Fatalf("lane %q not in (rank, key) order at %d: %s(rank %d) before %s(rank %d)",
					lane, i, cfgs[i-1].Key(), pr, cfgs[i].Key(), cr)
			}
		}
	}
}

// resolveLaneConfig takes the best-ranked config the oracle actually OBSERVED.
// Rank-1-only resolution pointed three of four lanes at cells with zero rows on
// the live oracle, so every lane row read pass_rate 0 while 580 observations
// sat unreachable in the table.
func TestResolveLaneConfigPrefersEvidence(t *testing.T) {
	a := policyeval.Config{Lane: "glm", Model: "glm-4.7", Effort: "high"} // rank 1, unmeasured
	b := policyeval.Config{Lane: "glm", Model: "glm-5.2", Effort: "high"} // rank 2, measured
	byLane := map[string][]policyeval.Config{"glm": {a, b}}

	got, measured := resolveLaneConfig(byLane, "glm", map[string]int{b.Key(): 159})
	if got != b || !measured {
		t.Fatalf("must take the best-ranked MEASURED config: got %s measured=%v", got.Key(), measured)
	}
	// With evidence for both, rank wins — evidence breaks ties, it does not
	// outrank the table.
	got, _ = resolveLaneConfig(byLane, "glm", map[string]int{a.Key(): 1, b.Key(): 159})
	if got != a {
		t.Fatalf("with both measured, rank order must win: got %s", got.Key())
	}
	// No evidence anywhere: fall back to rank-1 and SAY it is unmeasured.
	got, measured = resolveLaneConfig(byLane, "glm", map[string]int{})
	if got != a || measured {
		t.Fatalf("no evidence must fall back to rank-1 and report unmeasured: got %s measured=%v", got.Key(), measured)
	}
	// An unknown lane resolves to the zero Config, never an invented one.
	if got, _ := resolveLaneConfig(byLane, "nosuch", map[string]int{}); got != (policyeval.Config{}) {
		t.Fatalf("unknown lane must abstain, got %s", got.Key())
	}
}
