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
