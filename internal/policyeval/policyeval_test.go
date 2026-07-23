package policyeval

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Micro-oracle: 4 tasks × 3 lanes. Pass rates per cell (trials collapse to a rate).
//
//	t1: local 1, claude 1, codex 0
//	t2: local 0, claude 1, codex 1
//	t3: local 0, claude 0, codex 1
//	t4: local 0, claude 1, codex 0   (claude-only)
func microTable() *Table {
	tb := NewTable()
	add := func(task, lane string, pass bool) { tb.Add(task, lane, pass) }
	add("t1", "local", true)
	add("t1", "claude", true)
	add("t1", "codex", false)
	add("t2", "local", false)
	add("t2", "claude", true)
	add("t2", "codex", true)
	add("t3", "local", false)
	add("t3", "claude", false)
	add("t3", "codex", true)
	add("t4", "local", false)
	add("t4", "claude", true)
	add("t4", "codex", false)
	return tb
}

func TestEvaluateFixedPolicies(t *testing.T) {
	tb := microTable()
	tasks := []string{"t1", "t2", "t3", "t4"}

	ev := Evaluate(tb, tasks, Fixed("claude"))
	if ev.Passes != 3 || !almost(ev.PassRate, 0.75) || ev.Unknown != 0 {
		t.Fatalf("always-claude wrong: %+v", ev)
	}
	if !almost(ev.ClaudeFraction, 1.0) {
		t.Fatalf("always-claude fraction: %v", ev.ClaudeFraction)
	}

	ev = Evaluate(tb, tasks, Fixed("codex"))
	if ev.Passes != 2 || !almost(ev.ClaudeFraction, 0) {
		t.Fatalf("always-codex wrong: %+v", ev)
	}

	// Unknown lane cells count as unknown, never pass.
	ev = Evaluate(tb, tasks, Fixed("glm"))
	if ev.Passes != 0 || ev.Unknown != 4 {
		t.Fatalf("unknown lane wrong: %+v", ev)
	}
}

func TestOracleBestAndRegret(t *testing.T) {
	tb := microTable()
	tasks := []string{"t1", "t2", "t3", "t4"}
	ev := Evaluate(tb, tasks, OracleBest(tb))
	if ev.Passes != 4 {
		t.Fatalf("oracle-best should pass all: %+v", ev)
	}
	// Oracle prefers the cheapest passing lane: t1 local, t2 codex, t3 codex, t4 claude.
	if !almost(ev.ClaudeFraction, 0.25) {
		t.Fatalf("oracle claude fraction: %v", ev.ClaudeFraction)
	}
	base := Evaluate(tb, tasks, Fixed("claude"))
	if r := Regret(ev, base); !almost(r, -0.25) {
		// regret of always-claude vs oracle = 4-3 = 1 task = 0.25; ev vs base is negative
		t.Fatalf("regret: %v", r)
	}
}

// Frontier: claude budget 0 → passes t1(local)+t2,t3(codex)=3; budget ≥1 → 4.
func TestFrontier(t *testing.T) {
	tb := microTable()
	tasks := []string{"t1", "t2", "t3", "t4"}
	pts := Frontier(tb, tasks)
	if len(pts) != len(tasks)+1 {
		t.Fatalf("frontier points: %d", len(pts))
	}
	if pts[0].Passes != 3 || !almost(pts[0].ClaudeFraction, 0) {
		t.Fatalf("frontier b=0: %+v", pts[0])
	}
	if pts[1].Passes != 4 || !almost(pts[1].ClaudeFraction, 0.25) {
		t.Fatalf("frontier b=1: %+v", pts[1])
	}
	if pts[4].Passes != 4 {
		t.Fatalf("frontier b=4: %+v", pts[4])
	}
}

func TestRCI(t *testing.T) {
	assign := map[string]string{"t1": "codex", "t2": "codex", "t3": "codex", "t4": "claude"}
	if r := RCI(assign); !almost(r, 0.75) {
		t.Fatalf("RCI: %v", r)
	}
}

// Sign-flip permutation: identical outcomes → p=1; one-sided big diff → small p.
func TestSignFlipP(t *testing.T) {
	same := []float64{0, 0, 0, 0, 0, 0}
	if p := SignFlipP(same, 2000, 1); p < 0.99 {
		t.Fatalf("all-zero deltas p=%v", p)
	}
	big := []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	if p := SignFlipP(big, 4000, 1); p > 0.01 {
		t.Fatalf("uniform +1 deltas p=%v", p)
	}
}

// BCa bootstrap CI on a mean: covers the true mean, ordered, sane on constant data.
func TestBootstrapCI(t *testing.T) {
	xs := []float64{1, 0, 1, 1, 0, 1, 1, 1, 0, 1, 1, 0, 1, 1, 1, 0}
	lo, hi := BootstrapCI(xs, 0.95, 4000, 7)
	mean := 0.6875
	if !(lo < mean && mean < hi && lo < hi) {
		t.Fatalf("CI [%v,%v] does not bracket %v", lo, hi, mean)
	}
	if lo < 0 || hi > 1 {
		t.Fatalf("CI outside [0,1]: [%v,%v]", lo, hi)
	}
	clo, chi := BootstrapCI([]float64{1, 1, 1, 1}, 0.95, 500, 7)
	if !almost(clo, 1) || !almost(chi, 1) {
		t.Fatalf("constant data CI: [%v,%v]", clo, chi)
	}
}

// B'2: ClassBest derives a per-CLASS lane assignment from a TUNING subset —
// aggregated pass over the class's tuning cells, cheapest lane on ties, and a
// class with no data in any lane is absent from the map (unknown, never
// imputed).
func TestClassBestDerivation(t *testing.T) {
	tb := NewTable()
	classOf := map[string]string{"t1": "coding", "t2": "coding", "t3": "review", "t4": "empty"}
	// coding: codex 3/4 beats claude 2/4 across the two tuning tasks.
	tb.Add("t1", "codex", true)
	tb.Add("t1", "codex", true)
	tb.Add("t1", "claude", true)
	tb.Add("t1", "claude", false)
	tb.Add("t2", "codex", true)
	tb.Add("t2", "codex", false)
	tb.Add("t2", "claude", true)
	tb.Add("t2", "claude", false)
	// review: claude and glm tie at 1/2 → cheaper glm wins the tie.
	tb.Add("t3", "claude", true)
	tb.Add("t3", "claude", false)
	tb.Add("t3", "glm", true)
	tb.Add("t3", "glm", false)
	got, cov := ClassBest(tb, []string{"t1", "t2", "t3", "t4"}, classOf)
	if cov["coding"]["codex"] != 2 || cov["coding"]["claude"] != 2 {
		t.Fatalf("coverage must count observed tasks per class-lane: %v", cov)
	}
	if got["coding"] != "codex" {
		t.Fatalf("coding must pick codex (3/4 > 2/4): %v", got)
	}
	if got["review"] != "glm" {
		t.Fatalf("review tie must break to the cheaper lane: %v", got)
	}
	if _, ok := got["empty"]; ok {
		t.Fatalf("a data-less class must be absent, never imputed: %v", got)
	}
	// ByClass routes heldout tasks through the class map; unknown class → "".
	p := ByClass(got, map[string]string{"h1": "coding", "h2": "mystery"})
	if p("h1") != "codex" || p("h2") != "" {
		t.Fatalf("ByClass routing wrong: h1=%s h2=%s", p("h1"), p("h2"))
	}
}

// ClassBest must only ever see the tasks it is given (the tuning subset):
// heldout cells in the table must not influence the derivation.
func TestClassBestIgnoresTasksOutsideSubset(t *testing.T) {
	tb := NewTable()
	classOf := map[string]string{"tune": "coding", "held": "coding"}
	tb.Add("tune", "claude", true) // tuning: claude 1/1
	tb.Add("tune", "codex", false)
	// heldout says codex is perfect — must NOT leak into the derivation.
	tb.Add("held", "codex", true)
	tb.Add("held", "codex", true)
	got, _ := ClassBest(tb, []string{"tune"}, classOf)
	if got["coding"] != "claude" {
		t.Fatalf("heldout data leaked into ClassBest: %v", got)
	}
}

// ClassBest scores lanes by the MEAN of per-task rates (the eval objective),
// never pooled pass/n: a lane with many trials on one easy task must not
// outweigh its per-task performance.
func TestClassBestUsesTaskMeanNotPooled(t *testing.T) {
	tb := NewTable()
	classOf := map[string]string{"a": "c1", "b": "c1"}
	// codex: task a rate 1.0 (4 trials), task b rate 0.0 (1 trial):
	// task-mean 0.5, pooled 4/5 = 0.8.
	for i := 0; i < 4; i++ {
		tb.Add("a", "codex", true)
	}
	tb.Add("b", "codex", false)
	// claude: both tasks rate 0.6 (3/5): task-mean 0.6 beats codex 0.5
	// (pooled would wrongly say codex 0.8 > 0.6).
	for _, task := range []string{"a", "b"} {
		tb.Add(task, "claude", true)
		tb.Add(task, "claude", true)
		tb.Add(task, "claude", true)
		tb.Add(task, "claude", false)
		tb.Add(task, "claude", false)
	}
	got, _ := ClassBest(tb, []string{"a", "b"}, classOf)
	if got["c1"] != "claude" {
		t.Fatalf("task-mean objective must pick claude (0.6 > 0.5); pooled would pick codex: %v", got)
	}
}

// The exact sign-flip enumeration must cover the B'2 heldout size (n=23) so a
// Monte-Carlo p never re-enters the seed-luck regime on split verdicts.
func TestSignFlipExactAtHeldoutN(t *testing.T) {
	deltas := make([]float64, 23)
	for i := range deltas {
		deltas[i] = float64(i%3) - 1
	}
	p1 := SignFlipP(deltas, 100, 1)
	p2 := SignFlipP(deltas, 100, 999)
	if p1 != p2 {
		t.Fatalf("n=23 must be exact (seed-independent): %v vs %v", p1, p2)
	}
}

// Unknown lanes never win ties by zero-cost accident, and equal-cost ties
// break lexically - the pick order is TOTAL, never map-iteration-dependent.
func TestBetterPickTotalOrder(t *testing.T) {
	if betterPick(0.5, "mystery", 0.5, "local") {
		t.Fatal("an unknown lane must not beat local at equal rate (max-cost sentinel)")
	}
	if !betterPick(0.5, "aaa", 0.5, "zzz") {
		t.Fatal("equal rate + equal (unknown) cost must break lexically")
	}
}
