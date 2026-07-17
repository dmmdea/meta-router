package strategy

import "testing"

func step(id int, deps ...int) Step { return Step{ID: id, Instruction: "x", Deps: deps} }

func TestValidateAcceptsSoloAndChain(t *testing.T) {
	if err := Validate(IR{Goal: "g", Steps: []Step{step(0)}}); err != nil {
		t.Fatalf("solo must validate: %v", err)
	}
	chain := IR{Goal: "g", Steps: []Step{step(0), step(1, 0), step(2, 1)}}
	if err := Validate(chain); err != nil {
		t.Fatalf("plan-work-verify chain must validate: %v", err)
	}
}

func TestValidateRejectsOverCap(t *testing.T) {
	var s []Step
	for i := 0; i < MaxSteps+1; i++ {
		if i == 0 {
			s = append(s, step(i))
		} else {
			s = append(s, step(i, i-1))
		}
	}
	err := Validate(IR{Goal: "g", Steps: s})
	if err == nil {
		t.Fatal("over-cap IR must be REJECTED, never truncated")
	}
}

func TestValidateRejectsEmptyDupForwardCycle(t *testing.T) {
	cases := map[string]IR{
		"empty":       {Goal: "g"},
		"dup-id":      {Goal: "g", Steps: []Step{step(0), step(0)}},
		"dep-oob":     {Goal: "g", Steps: []Step{step(0, 9)}},
		"forward-dep": {Goal: "g", Steps: []Step{step(0, 1), step(1)}}, // 0 depends on later 1
		"self-dep":    {Goal: "g", Steps: []Step{step(0, 0)}},
	}
	for name, ir := range cases {
		if err := Validate(ir); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

func TestValidateRejectsSameLaneFanOut(t *testing.T) {
	// two root steps (both deps=[]) pinned to the SAME explicit lane = illegal fan-out (§4.4)
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "glm", Deps: []int{}},
		{ID: 2, Instruction: "j", Deps: []int{0, 1}},
	}}
	if err := Validate(ir); err == nil {
		t.Fatal("same-lane parallel fan-out must be rejected at validation")
	}
	// different lanes on the same fan-out is fine
	ir.Steps[1].LaneHint = "codex"
	if err := Validate(ir); err != nil {
		t.Fatalf("cross-lane fan-out must validate: %v", err)
	}
}

// S3R-2 (terminal-sink honesty): the last-listed step must be the UNIQUE SINK
// (the sole step with no dependents). An orphan branch — a non-last step that
// nothing depends on — would have its result silently dropped by finalize, so
// it must be REJECTED at validation. Concretely, the fan-out-judge shape (two
// leaves both feeding a terminal judge) is legal because the judge is the sole
// sink; the same two leaves with a terminal that only depends on ONE of them
// leaves the other leaf a dangling sink and must be rejected.
func TestValidateRejectsOrphanBranchNonTerminalSink(t *testing.T) {
	// step 1 is a dangling sink: it is not last AND nothing depends on it.
	// finalize would answer with step 2 while step 1's result vanishes.
	orphan := IR{Goal: "g", Steps: []Step{
		step(0),    // root
		step(1, 0), // branch off 0 — NOTHING depends on it (orphan sink)
		step(2, 0), // terminal, depends only on 0
	}}
	if err := Validate(orphan); err == nil {
		t.Fatal("an orphan non-terminal sink (dangling branch) must be rejected — finalize would silently drop it")
	}

	// fan-out-judge: two leaves both feed the terminal judge → judge is the
	// unique sink → legal.
	judge := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "codex", Deps: []int{}},
		{ID: 2, Instruction: "judge", Deps: []int{0, 1}},
	}}
	if err := Validate(judge); err != nil {
		t.Fatalf("fan-out-judge (terminal is unique sink) must validate: %v", err)
	}
}

// S3R-2 corollary: the last step is a sink by construction (forward-dep rule
// forbids anything depending on it), but a MID-LIST step that is also a sink is
// the failure mode. A chain (each step feeds the next) has exactly one sink (the
// last) and validates; verified here so the sink rule does not over-reject.
func TestValidateChainHasUniqueTerminalSink(t *testing.T) {
	chain := IR{Goal: "g", Steps: []Step{step(0), step(1, 0), step(2, 1), step(3, 2)}}
	if err := Validate(chain); err != nil {
		t.Fatalf("a linear chain has exactly one terminal sink and must validate: %v", err)
	}
}
