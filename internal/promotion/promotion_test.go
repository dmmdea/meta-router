package promotion

import (
	"fmt"
	"math"
	"testing"
)

// pairs makes n DISTINCT-task pairs (the gate collapses repeats — see the
// pseudo-replication test).
func pairs(n int, template string, tScore, sScore float64, tTok, sTok int64) []PairedRun {
	out := make([]PairedRun, n)
	for i := range out {
		out[i] = PairedRun{Task: fmt.Sprintf("t%02d", i), Template: template,
			TemplateScore: tScore, SoloScore: sScore,
			TemplateTokens: tTok, SoloTokens: sTok}
	}
	return out
}

// A large NOISY win promotes — exercises the real BCa path (non-degenerate CI).
func TestGatePromotesNoisyWin(t *testing.T) {
	ps := pairs(15, "cascade", 1, 0, 1000, 1000)
	ps = append(ps, pairs(15, "cascade", 0.5, 0, 1000, 1000)...)
	for i := range ps {
		ps[i].Task = fmt.Sprintf("t%02d", i) // keep all 30 tasks distinct
	}
	v := Gate("cascade", ps, 4000, 7)
	if !v.Promote || v.CIDegenerate {
		t.Fatalf("noisy 30-task win must promote via the real BCa path: %+v", v)
	}
	if v.UsedN != 30 || v.CILo <= 0 || v.P >= 0.05 {
		t.Fatalf("verdict fields wrong: %+v", v)
	}
}

// Q5's core claim, now literal: at n=5 the sign-flip p is EXACT (2/32=0.0625),
// so refusal holds on EVERY seed — including 372, the seed that defeated the
// Monte-Carlo estimate in review.
func TestGateRefusesSmallNByArithmeticEverySeed(t *testing.T) {
	for _, seed := range []int64{7, 42, 372, 999} {
		v := Gate("cascade", pairs(5, "cascade", 1, 0, 1000, 1000), 4000, seed)
		if v.Promote {
			t.Fatalf("seed %d: n=5 must refuse: %+v", seed, v)
		}
		if math.Abs(v.P-0.0625) > 1e-12 {
			t.Fatalf("seed %d: exact p must be 2/32=0.0625, got %v", seed, v.P)
		}
	}
}

// Pseudo-replication is a gaming vector: 30 reruns of ONE task collapse to a
// single observation and cannot promote.
func TestGateCollapsesDuplicateTasks(t *testing.T) {
	ps := pairs(30, "cascade", 1, 0, 1000, 1000)
	for i := range ps {
		ps[i].Task = "same-task"
	}
	v := Gate("cascade", ps, 4000, 7)
	if v.UsedN != 1 {
		t.Fatalf("30 reruns of one task must collapse to n=1: %+v", v)
	}
	if v.Promote {
		t.Fatalf("n=1 must never promote (exact p=1 at one delta... p=%v): %+v", v.P, v)
	}
}

// A mixed record with no real edge must not promote.
func TestGateRefusesNoEdge(t *testing.T) {
	ps := pairs(15, "cascade", 1, 0, 1000, 1000)
	more := pairs(15, "cascade", 0, 1, 1000, 1000)
	for i := range more {
		more[i].Task = fmt.Sprintf("u%02d", i)
	}
	v := Gate("cascade", append(ps, more...), 4000, 7)
	if v.Promote {
		t.Fatalf("50/50 split must refuse: %+v", v)
	}
}

// Equal-budget exclusion: skewed pairs are excluded and counted; invalid
// token counts land in the SEPARATE invalid counter, never in budget-skew.
func TestGateExcludesBudgetSkewAndCountsInvalid(t *testing.T) {
	ps := pairs(20, "cascade", 1, 0, 1000, 1000)
	skew := pairs(10, "cascade", 1, 0, 5000, 1000)
	for i := range skew {
		skew[i].Task = fmt.Sprintf("s%02d", i)
	}
	bad := pairs(3, "cascade", 1, 0, 0, 1000) // zero tokens = data error
	for i := range bad {
		bad[i].Task = fmt.Sprintf("z%02d", i)
	}
	v := Gate("cascade", append(append(ps, skew...), bad...), 4000, 7)
	if v.ExcludedBudget != 10 || v.Invalid != 3 || v.UsedN != 20 {
		t.Fatalf("counters wrong: %+v", v)
	}
}

// Out-of-range scores are invalid, not judged.
func TestGateRejectsOutOfRangeScores(t *testing.T) {
	ps := pairs(2, "x", 50, 0, 1000, 1000)
	v := Gate("x", ps, 1000, 7)
	if v.Invalid != 2 || v.UsedN != 0 || v.Promote {
		t.Fatalf("out-of-range scores must be invalid: %+v", v)
	}
}

// No pairs → honest refusal with p=1, never a promotion or a panic.
func TestGateEmpty(t *testing.T) {
	v := Gate("cascade", nil, 1000, 7)
	if v.Promote || v.UsedN != 0 || v.Reason == "" || v.P != 1 {
		t.Fatalf("empty input must refuse with p=1 and a reason: %+v", v)
	}
}

// Zero/negative iters must not panic (guards in policyeval).
func TestGateIterZeroSafe(t *testing.T) {
	v := Gate("cascade", pairs(6, "cascade", 1, 0, 1000, 1000), 0, 7)
	_ = v // reaching here without panic is the assertion
}

// BudgetParity normalizes by the SMALLER spend: max/min ≤ 1.25. The 1251–1333
// region is where the lenient (hi-lo)/hi reading disagrees — it must FAIL.
func TestBudgetParity(t *testing.T) {
	cases := []struct {
		a, b int64
		ok   bool
	}{{1000, 1000, true}, {1000, 1249, true}, {1249, 1000, true},
		{1000, 1300, false}, {1300, 1000, false}, // the disagreement region
		{1000, 1400, false}, {0, 1000, false}, {0, 0, false}, {-5, 1000, false}}
	for _, c := range cases {
		if got := BudgetParity(c.a, c.b); got != c.ok {
			t.Errorf("BudgetParity(%d,%d)=%v want %v", c.a, c.b, got, c.ok)
		}
	}
}

// Constant deltas: CI is flagged degenerate and the decision rides the exact p.
func TestDegenerateCIFlagged(t *testing.T) {
	v := Gate("x", pairs(6, "x", 1, 0, 1000, 1000), 4000, 7)
	if !v.CIDegenerate {
		t.Fatalf("uniform deltas must flag ci_degenerate: %+v", v)
	}
	// exact p at n=6 uniform: 2/64 = 0.03125 < .05 → promotes, and honestly so.
	if !v.Promote || math.Abs(v.P-0.03125) > 1e-12 {
		t.Fatalf("n=6 uniform: exact p 0.03125 should promote with the flag: %+v", v)
	}
}

func TestMeanDelta(t *testing.T) {
	v := Gate("x", pairs(30, "x", 0.8, 0.5, 100, 100), 4000, 7)
	if math.Abs(v.MeanDelta-0.3) > 1e-9 {
		t.Fatalf("mean delta: %v", v.MeanDelta)
	}
}
