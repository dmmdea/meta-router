package policyeval

import (
	"math"
	"testing"
)

func almost(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// lc is the legacy-fixture bridge: one canonical config per lane, so the
// pre-Config tests keep their exact meaning now that evidence is keyed by
// (lane, model, effort). Tests that care about two models on one lane build
// their Configs explicitly.
func lc(lane string) Config {
	return Config{Lane: lane, Model: lane + "-m", Effort: EffortUnrecorded}
}

// cfg spells a full evidence cell for tests that care about the distinction
// between two models or two efforts on one lane.
func cfg(lane, model, effort string) Config {
	return Config{Lane: lane, Model: model, Effort: NormalizeEffort(effort)}
}

// Micro-oracle: 4 tasks × 3 lanes. Pass rates per cell (trials collapse to a rate).
//
//	t1: local 1, claude 1, codex 0
//	t2: local 0, claude 1, codex 1
//	t3: local 0, claude 0, codex 1
//	t4: local 0, claude 1, codex 0   (claude-only)
func microTable() *Table {
	tb := NewTable()
	add := func(task, lane string, pass bool) { tb.Add(task, lc(lane), pass) }
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

	ev := Evaluate(tb, tasks, Fixed(lc("claude")))
	if ev.Passes != 3 || !almost(ev.PassRate, 0.75) || ev.Unknown != 0 {
		t.Fatalf("always-claude wrong: %+v", ev)
	}
	if !almost(ev.ClaudeFraction, 1.0) {
		t.Fatalf("always-claude fraction: %v", ev.ClaudeFraction)
	}

	ev = Evaluate(tb, tasks, Fixed(lc("codex")))
	if ev.Passes != 2 || !almost(ev.ClaudeFraction, 0) {
		t.Fatalf("always-codex wrong: %+v", ev)
	}

	// Unknown lane cells count as unknown, never pass.
	ev = Evaluate(tb, tasks, Fixed(lc("glm")))
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
	base := Evaluate(tb, tasks, Fixed(lc("claude")))
	r, n := Regret(ev, base)
	if !almost(r, -0.25) || n != 4 {
		// regret of always-claude vs oracle = 4-3 = 1 task = 0.25 over the 4
		// shared-evidence tasks; ev vs base is negative
		t.Fatalf("regret: %v over n=%d", r, n)
	}
}

// Regret is a paired delta, so it must gate on Measured — differencing the
// raw PassRates converts "not measured" into "failed": an all-holes candidate
// and a measured-failed-everything candidate scored byte-identical regret.
func TestRegretGatesOnMeasured(t *testing.T) {
	tb := microTable()
	tasks := []string{"t1", "t2", "t3", "t4"}
	base := Evaluate(tb, tasks, Fixed(lc("claude")))

	// An all-holes candidate shares NO measured task with the reference: the
	// gap is undefined (n=0), not a measured -0.75.
	holes := Evaluate(tb, tasks, Fixed(lc("glm")))
	if r, n := Regret(holes, base); n != 0 || r != 0 {
		t.Fatalf("all-holes vs measured must be undefined (0, 0), got (%v, %d)", r, n)
	}

	// A measured candidate pairs only on the tasks BOTH saw.
	local := Evaluate(tb, tasks, Fixed(lc("local")))
	if r, n := Regret(local, base); n != 4 || !almost(r, 0.5) {
		// claude beats local on t2,t4 (+1 each) over 4 shared tasks = +0.5
		t.Fatalf("paired regret wrong: (%v, %d)", r, n)
	}

	// The gate must bind on BOTH sides: b unmeasured on a task a measured.
	// Without the b-side check, b's imputed 0 on t5 would enter the delta as
	// a fake failure — deleting `if !b.Measured[task]` passed the suite
	// before this case existed (round 4, mutation-verified).
	tb5 := microTable()
	tb5.Add("t5", lc("local"), true) // local measured on t5; claude is NOT
	tasks5 := []string{"t1", "t2", "t3", "t4", "t5"}
	a5 := Evaluate(tb5, tasks5, Fixed(lc("local")))
	b5 := Evaluate(tb5, tasks5, Fixed(lc("claude")))
	if r, n := Regret(a5, b5); n != 4 || !almost(r, 0.5) {
		t.Fatalf("t5 (a measured, b not) must be excluded: (%v, %d), want (0.5, 4)", r, n)
	}
}

// Identical inputs produce bit-identical output: the deltas are summed in
// sorted task order, because float addition is not associative and map-order
// accumulation drifts in the last ULP across runs (round 4).
func TestRegretIsDeterministic(t *testing.T) {
	tb := NewTable()
	tasks := []string{"a", "b", "c", "d", "e", "f", "g"}
	for i, task := range tasks {
		for j := 0; j < 7; j++ {
			tb.Add(task, lc("local"), j <= i)
			tb.Add(task, lc("claude"), j >= i)
		}
	}
	a := Evaluate(tb, tasks, Fixed(lc("local")))
	b := Evaluate(tb, tasks, Fixed(lc("claude")))
	first, _ := Regret(a, b)
	for i := 0; i < 500; i++ {
		if r, _ := Regret(a, b); r != first {
			t.Fatalf("run %d: %x != %x — map order reached the output", i, r, first)
		}
	}
}

// Frontier: claude budget 0 → passes t1(local)+t2,t3(codex)=3; budget ≥1 → 4.
func TestFrontier(t *testing.T) {
	tb := microTable()
	tasks := []string{"t1", "t2", "t3", "t4"}
	pts, unmeasured, freeUnmeasured := Frontier(tb, tasks)
	if unmeasured != 0 || freeUnmeasured != 0 {
		t.Fatalf("fully-measured table must exclude nothing: %d, %d", unmeasured, freeUnmeasured)
	}
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

// The curve's rates share the caller's denominator with every policy row.
// Rebasing onto the measured subset claimed pass_rate 1.0 at zero budget over
// 2 measured tasks while policy rows maxed at 0.333 over 6.
func TestFrontierSharesThePolicyDenominator(t *testing.T) {
	tb := NewTable()
	tb.Add("t1", lc("local"), true)
	tb.Add("t2", lc("local"), true)
	tasks := []string{"t1", "t2", "t3", "t4", "t5", "t6"} // 4 unmeasured
	pts, unmeasured, _ := Frontier(tb, tasks)
	if unmeasured != 4 {
		t.Fatalf("unmeasured = %d, want 4", unmeasured)
	}
	if !almost(pts[0].PassRate, 2.0/6.0) {
		t.Fatalf("pass_rate must be over the FULL evaluation set (2/6), got %v", pts[0].PassRate)
	}
	// Same table, same tasks, scored as a policy: identical denominator.
	ev := Evaluate(tb, tasks, Fixed(lc("local")))
	if !almost(pts[0].PassRate, ev.PassRate) {
		t.Fatalf("frontier (%v) and policy (%v) pass rates must be comparable", pts[0].PassRate, ev.PassRate)
	}
}

// A task measured ONLY on the claude side has an unknown free-lane value. It
// STAYS in the sweep (excluding it discarded its measured claude evidence and
// let oracle-best exceed the curve's maximum — the envelope violation of
// review round 4); the claude-only-vs-measured-free-failure distinction lives
// in the COUNTER, which is how the artifact tells the two tables apart.
func TestFrontierCountsFreeUnmeasuredTasks(t *testing.T) {
	claudeOnly := NewTable()
	claudeOnly.Add("t1", lc("claude"), false)
	claudeOnly.Add("t2", lc("claude"), false)

	freeFailed := NewTable()
	freeFailed.Add("t1", lc("claude"), false)
	freeFailed.Add("t2", lc("claude"), false)
	freeFailed.Add("t1", lc("glm"), false)
	freeFailed.Add("t2", lc("glm"), false)

	tasks := []string{"t1", "t2"}
	ptsA, _, freeUnA := Frontier(claudeOnly, tasks)
	_, _, freeUnB := Frontier(freeFailed, tasks)
	if freeUnA != 2 || freeUnB != 0 {
		t.Fatalf("free-unmeasured counts: %d (want 2), %d (want 0)", freeUnA, freeUnB)
	}
	if len(ptsA) != 3 {
		t.Fatalf("claude-only tasks must stay in the sweep: %d points, want 3", len(ptsA))
	}
}

// The ENVELOPE property: no policy scored on the same tasks may exceed the
// frontier's maximum point. Excluding a claude-only task from the sweep broke
// this — oracle-best routed the task to its measured claude PASS while the
// curve had discarded that evidence and divided by the full set anyway.
func TestFrontierIsAnEnvelope(t *testing.T) {
	tb := NewTable()
	tb.Add("t1", lc("claude"), true) // claude-only, PASSES
	tb.Add("t2", lc("local"), true)  // free-only, passes
	tasks := []string{"t1", "t2"}

	pts, _, freeUn := Frontier(tb, tasks)
	if freeUn != 1 {
		t.Fatalf("freeUnmeasured = %d, want 1", freeUn)
	}
	best := Evaluate(tb, tasks, OracleBest(tb))
	maxRate := 0.0
	for _, p := range pts {
		if p.PassRate > maxRate {
			maxRate = p.PassRate
		}
	}
	if best.PassRate > maxRate+1e-12 {
		t.Fatalf("oracle-best (%.3f) exceeds the frontier maximum (%.3f): the envelope is broken", best.PassRate, maxRate)
	}
	if !almost(maxRate, 1.0) {
		t.Fatalf("full-budget point must reach the measured ceiling, got %v", maxRate)
	}
	// And the budget-0 point imputes the claude-only task's unknown free base
	// as 0 — visible via the counter, so it reads "at least 0.5", never a
	// measurement of t1's free side.
	if !almost(pts[0].PassRate, 0.5) {
		t.Fatalf("budget-0 point: %v, want 0.5", pts[0].PassRate)
	}
}

// ClaudeFraction divides by the caller's full task set, exactly like PassRate
// — reverting it to the sweepable count passed the whole suite (round 4).
func TestFrontierClaudeFractionSharesTheDenominator(t *testing.T) {
	tb := NewTable()
	tb.Add("t1", lc("claude"), true)
	tb.Add("t1", lc("local"), false)
	tasks := []string{"t1", "t2", "t3"} // one sweepable of three
	pts, _, _ := Frontier(tb, tasks)
	if len(pts) != 2 {
		t.Fatalf("points: %d, want 2 (budget 0..1)", len(pts))
	}
	if !almost(pts[1].ClaudeFraction, 1.0/3.0) {
		t.Fatalf("claude_fraction at budget 1 = %v, want 1/3 (full-set denominator)", pts[1].ClaudeFraction)
	}
}

func TestRCI(t *testing.T) {
	assign := map[string]Config{"t1": lc("codex"), "t2": lc("codex"), "t3": lc("codex"), "t4": lc("claude")}
	if r := RCI(assign); !almost(r, 0.75) {
		t.Fatalf("RCI: %v", r)
	}
}

// An abstain (the zero Config) is not a lane: a task routed NOWHERE must not
// count toward the modal lane, or an all-abstain policy reports rci 1.0 — the
// maximum collapse value for a policy that collapsed onto nothing.
func TestRCIAbstainIsNotALane(t *testing.T) {
	allAbstain := map[string]Config{"t1": {}, "t2": {}, "t3": {}}
	if r := RCI(allAbstain); r != 0 {
		t.Fatalf("all-abstain rci = %v, want 0", r)
	}
	mixed := map[string]Config{"t1": lc("codex"), "t2": lc("codex"), "t3": {}, "t4": {}}
	if r := RCI(mixed); !almost(r, 0.5) {
		t.Fatalf("abstains stay in the denominator: rci = %v, want 0.5", r)
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
	tb.Add("t1", lc("codex"), true)
	tb.Add("t1", lc("codex"), true)
	tb.Add("t1", lc("claude"), true)
	tb.Add("t1", lc("claude"), false)
	tb.Add("t2", lc("codex"), true)
	tb.Add("t2", lc("codex"), false)
	tb.Add("t2", lc("claude"), true)
	tb.Add("t2", lc("claude"), false)
	// review: claude and glm tie at 1/2 → cheaper glm wins the tie.
	tb.Add("t3", lc("claude"), true)
	tb.Add("t3", lc("claude"), false)
	tb.Add("t3", lc("glm"), true)
	tb.Add("t3", lc("glm"), false)
	got, cov := ClassBest(tb, []string{"t1", "t2", "t3", "t4"}, classOf)
	if cov["coding"][lc("codex").Key()] != 2 || cov["coding"][lc("claude").Key()] != 2 {
		t.Fatalf("coverage must count observed tasks per class-lane: %v", cov)
	}
	if got["coding"] != lc("codex") {
		t.Fatalf("coding must pick codex (3/4 > 2/4): %v", got)
	}
	if got["review"] != lc("glm") {
		t.Fatalf("review tie must break to the cheaper lane: %v", got)
	}
	if _, ok := got["empty"]; ok {
		t.Fatalf("a data-less class must be absent, never imputed: %v", got)
	}
	// ByClass routes heldout tasks through the class map; unknown class → "".
	p := ByClass(got, map[string]string{"h1": "coding", "h2": "mystery"})
	if p("h1") != lc("codex") || (p("h2") != Config{}) {
		t.Fatalf("ByClass routing wrong: h1=%v h2=%v", p("h1"), p("h2"))
	}
}

// ClassBest must only ever see the tasks it is given (the tuning subset):
// heldout cells in the table must not influence the derivation.
func TestClassBestIgnoresTasksOutsideSubset(t *testing.T) {
	tb := NewTable()
	classOf := map[string]string{"tune": "coding", "held": "coding"}
	tb.Add("tune", lc("claude"), true) // tuning: claude 1/1
	tb.Add("tune", lc("codex"), false)
	// heldout says codex is perfect — must NOT leak into the derivation.
	tb.Add("held", lc("codex"), true)
	tb.Add("held", lc("codex"), true)
	got, _ := ClassBest(tb, []string{"tune"}, classOf)
	if got["coding"] != lc("claude") {
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
		tb.Add("a", lc("codex"), true)
	}
	tb.Add("b", lc("codex"), false)
	// claude: both tasks rate 0.6 (3/5): task-mean 0.6 beats codex 0.5
	// (pooled would wrongly say codex 0.8 > 0.6).
	for _, task := range []string{"a", "b"} {
		tb.Add(task, lc("claude"), true)
		tb.Add(task, lc("claude"), true)
		tb.Add(task, lc("claude"), true)
		tb.Add(task, lc("claude"), false)
		tb.Add(task, lc("claude"), false)
	}
	got, _ := ClassBest(tb, []string{"a", "b"}, classOf)
	if got["c1"] != lc("claude") {
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
	if betterPick(0.5, cfg("mystery", "m", "high"), 0.5, cfg("local", "m", "high")) {
		t.Fatal("an unknown lane must not beat local at equal rate (max-cost sentinel)")
	}
	if !betterPick(0.5, cfg("aaa", "m", "high"), 0.5, cfg("zzz", "m", "high")) {
		t.Fatal("equal rate + equal (unknown) cost must break lexically")
	}
	// Same lane, two configs: cost ties, so the CONFIG key breaks it — the order
	// stays total once a lane holds more than one cell.
	if !betterPick(0.5, cfg("claude", "a-model", "high"), 0.5, cfg("claude", "b-model", "high")) {
		t.Fatal("two configs on one lane must break lexically on the full key")
	}
}

// THE point of the change: two models on one lane are two cells, not one
// average. Pooling them is how 204 claude rows recorded Sonnet's results while
// the rank table dispatched Opus.
func TestTwoModelsOnOneLaneAreTwoCells(t *testing.T) {
	tb := NewTable()
	sonnet := cfg("claude", "claude-sonnet-5", "high")
	opus := cfg("claude", "claude-opus-4-8", "high")
	tb.Add("t1", sonnet, true)
	tb.Add("t1", opus, false)

	rs, ok := tb.Rate("t1", sonnet)
	if !ok || !almost(rs, 1) {
		t.Fatalf("sonnet cell = %v,%v; want 1,true", rs, ok)
	}
	ro, ok := tb.Rate("t1", opus)
	if !ok || !almost(ro, 0) {
		t.Fatalf("opus cell = %v,%v; want 0,true — a lane-keyed table would average these to 0.5", ro, ok)
	}
}

// B6: an unobserved config is a HOLE. It never inherits a sibling's rate, and
// the unrecorded-effort marker is a real value that satisfies nothing else.
func TestUnobservedConfigNeverInheritsASibling(t *testing.T) {
	tb := NewTable()
	legacy := cfg("claude", "claude-sonnet-5", EffortUnrecorded)
	tb.Add("t1", legacy, true)

	for _, sib := range []Config{
		cfg("claude", "claude-sonnet-5", "high"), // same lane+model, real effort
		cfg("claude", "claude-opus-4-8", EffortUnrecorded),
		cfg("codex", "claude-sonnet-5", EffortUnrecorded),
	} {
		if r, ok := tb.Rate("t1", sib); ok {
			t.Fatalf("config %s inherited evidence it has none of (rate %v)", sib.Key(), r)
		}
	}
	ev := Evaluate(tb, []string{"t1"}, Fixed(cfg("claude", "claude-sonnet-5", "high")))
	if ev.Unknown != 1 || ev.PassRate != 0 {
		t.Fatalf("an effort-naming policy must score UNKNOWN against legacy evidence: %+v", ev)
	}
}

// THE regression guard for the deepest blocker in this change. `Policy` returns
// a Config, and ClaudeFraction was `lane == "claude"`. Both obvious repairs
// compile and are silently wrong:
//   - Config{Lane: lane} makes every lookup miss ("claude||"), so
//     ev.Unknown == len(tasks) and PassRate == 0;
//   - a key-string Policy breaks the "claude" comparison, so ClaudeFraction is 0
//     for every policy and NonInferior = RatioCILo >= 1-margin &&
//     ClaudeFraction < 1 becomes UNCONDITIONALLY TRUE — the non-inferiority
//     verdict destroyed, silently.
func TestClaudeFractionSurvivesConfigPolicies(t *testing.T) {
	tb := NewTable()
	cc := Config{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh"}
	cx := Config{Lane: "codex", Model: "gpt-5.6-terra", Effort: "high"}
	tb.Add("t1", cc, true)
	tb.Add("t2", cx, true)
	ev := Evaluate(tb, []string{"t1", "t2"}, func(task string) Config {
		if task == "t1" {
			return cc
		}
		return cx
	})
	if ev.ClaudeFraction != 0.5 {
		t.Fatalf("ClaudeFraction = %v, want 0.5 — a key-prefix comparison silently yields 0", ev.ClaudeFraction)
	}
	// The other half of the same blocker: the lookups must actually HIT.
	if ev.Unknown != 0 || !almost(ev.PassRate, 1) {
		t.Fatalf("every cell was observed; Unknown=%d PassRate=%v — a Config{Lane:lane} policy misses every lookup", ev.Unknown, ev.PassRate)
	}
}
