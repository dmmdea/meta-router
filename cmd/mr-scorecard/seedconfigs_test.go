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
	none := map[string]int{}

	got, measured := resolveLaneConfig(byLane, "glm", map[string]int{b.Key(): 159}, map[string]int{b.Key(): 159})
	if got != b || !measured {
		t.Fatalf("must take the best-ranked MEASURED config: got %s measured=%v", got.Key(), measured)
	}
	// With evidence for both, rank wins — evidence breaks ties, it does not
	// outrank the table.
	both := map[string]int{a.Key(): 1, b.Key(): 159}
	got, _ = resolveLaneConfig(byLane, "glm", both, both)
	if got != a {
		t.Fatalf("with both measured, rank order must win: got %s", got.Key())
	}
	// No evidence anywhere: fall back to rank-1 and SAY it is unmeasured.
	got, measured = resolveLaneConfig(byLane, "glm", none, none)
	if got != a || measured {
		t.Fatalf("no evidence must fall back to rank-1 and report unmeasured: got %s measured=%v", got.Key(), measured)
	}
	// An unknown lane resolves to the zero Config, never an invented one.
	if got, _ := resolveLaneConfig(byLane, "nosuch", none, none); got != (policyeval.Config{}) {
		t.Fatalf("unknown lane must abstain, got %s", got.Key())
	}
}

// Under -split, "observed" must mean observed on the EVALUATION set: a ranked
// config whose only rows sit on the tuning split resolved the reference to a
// cell unmeasured on every task actually scored (review 2026-08-12, round 3).
func TestResolveLaneConfigPrefersEvalSetEvidence(t *testing.T) {
	a := policyeval.Config{Lane: "claude", Model: "claude-opus-4-8", Effort: "high"} // rank 1: tuning-only evidence
	b := policyeval.Config{Lane: "claude", Model: "claude-sonnet-5", Effort: "high"} // rank 2: heldout evidence
	byLane := map[string][]policyeval.Config{"claude": {a, b}}
	obsEval := map[string]int{b.Key(): 200}            // what the heldout tasks saw
	obsAll := map[string]int{a.Key(): 1, b.Key(): 200} // the whole oracle

	got, measured := resolveLaneConfig(byLane, "claude", obsEval, obsAll)
	if got != b || !measured {
		t.Fatalf("a config observed on the eval set must beat a higher-ranked config observed only outside it: got %s", got.Key())
	}
	// With NO eval-set evidence at all, any-oracle evidence still beats an
	// unmeasured rank-1: honest fallback, not an invented cell.
	got, _ = resolveLaneConfig(byLane, "claude", map[string]int{}, obsAll)
	if got != a {
		t.Fatalf("fallback tier must use whole-oracle evidence in rank order: got %s", got.Key())
	}
}

// rankedConfigs trims lane and model exactly as the oracle side trims on
// ingest: a padded field in the operator's rank-table override otherwise
// builds a key no oracle row can match, silently dropping the lane to
// pass_rate 0 while oracle-best scores the same evidence fine.
func TestRankedConfigsTrimsLaneAndModel(t *testing.T) {
	tbl := router.Table{"workhorse-coding": {
		{Lane: " glm ", Model: " glm-5.2 ", Effort: "high", Rank: 1},
	}}
	byLane, all := rankedConfigs(tbl)
	want := policyeval.Config{Lane: "glm", Model: "glm-5.2", Effort: "high"}
	if len(all) != 1 || all[0] != want {
		t.Fatalf("padded table entry must produce the trimmed config %s, got %v", want.Key(), all)
	}
	if len(byLane["glm"]) != 1 {
		t.Fatalf("the trimmed lane must key byLane, got %v", byLane)
	}
	if len(byLane[" glm "]) != 0 {
		t.Fatal("the padded lane must not appear as its own lane")
	}
}
