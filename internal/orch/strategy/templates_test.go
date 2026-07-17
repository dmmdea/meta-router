package strategy

import "testing"

func TestSoloIsOneNodeNoDeps(t *testing.T) {
	ir := Solo("do X", "workhorse-coding")
	if len(ir.Steps) != 1 || len(ir.Steps[0].Deps) != 0 {
		t.Fatalf("solo must be 1 node, 0 deps: %+v", ir)
	}
	if ir.Steps[0].Instruction != "do X" || ir.Steps[0].Class != "workhorse-coding" {
		t.Fatalf("solo must carry goal+class: %+v", ir.Steps[0])
	}
	if err := Validate(ir); err != nil {
		t.Fatalf("solo must validate: %v", err)
	}
}

func TestPlanWorkVerifyShapeAndGating(t *testing.T) {
	// hard class → 3 nodes with the thinker present
	hard := PlanWorkVerify("build feature", "hard-repo")
	if len(hard.Steps) != 3 {
		t.Fatalf("hard class → 3 nodes, got %d", len(hard.Steps))
	}
	// deps must be [[],[0],[1]]
	wantDeps := [][]int{{}, {0}, {1}}
	for i, s := range hard.Steps {
		if !sameDeps(s.Deps, wantDeps[i]) {
			t.Fatalf("node %d deps = %v, want %v", i, s.Deps, wantDeps[i])
		}
	}
	if hard.Steps[0].Role != "thinker" || hard.Steps[1].Role != "worker" || hard.Steps[2].Role != "verifier" {
		t.Fatalf("roles must be thinker/worker/verifier: %+v", hard.Steps)
	}
	// the verifier node routes to the offload-harness triage door (S3R-1):
	// LaneHint=local, Class=verify-gate.
	if hard.Steps[2].LaneHint != "local" || hard.Steps[2].Class != "verify-gate" {
		t.Fatalf("verifier must be local/verify-gate (triage door): %+v", hard.Steps[2])
	}
	// verifier must be on a DIFFERENT lane hint than the worker (when both pinned)
	if hard.Steps[2].LaneHint != "" && hard.Steps[2].LaneHint == hard.Steps[1].LaneHint {
		t.Fatal("verifier must not share the worker's lane")
	}
	if err := Validate(hard); err != nil {
		t.Fatalf("plan-work-verify must validate: %v", err)
	}
	// easy class → thinker GATED OUT (removing it improves quality on easy tasks): 2 nodes
	easy := PlanWorkVerify("tiny fix", "latency-iteration")
	if len(easy.Steps) != 2 {
		t.Fatalf("easy class must drop the thinker → 2 nodes, got %d", len(easy.Steps))
	}
	if easy.Steps[0].Role != "worker" || easy.Steps[1].Role != "verifier" {
		t.Fatalf("gated shape must be worker→verifier: %+v", easy.Steps)
	}
	if easy.Steps[1].LaneHint != "local" || easy.Steps[1].Class != "verify-gate" {
		t.Fatalf("gated verifier must still be local/verify-gate: %+v", easy.Steps[1])
	}
	if err := Validate(easy); err != nil {
		t.Fatalf("gated plan-work-verify must validate: %v", err)
	}
}

// Every hard class must yield the 3-node (thinker present) shape; every non-hard
// class the 2-node gated shape.
func TestPlanWorkVerifyGatingRuleCoversAllHardClasses(t *testing.T) {
	for class := range hardClasses {
		if got := len(PlanWorkVerify("g", class).Steps); got != 3 {
			t.Errorf("hard class %q must yield 3 nodes, got %d", class, got)
		}
	}
	for _, easy := range []string{"latency-iteration", "workhorse-coding", "doc-summarize", "mechanical-text", "", "unknown-class"} {
		if got := len(PlanWorkVerify("g", easy).Steps); got != 2 {
			t.Errorf("easy class %q must yield 2 nodes, got %d", easy, got)
		}
	}
}

func TestSeamTemplatesValidate(t *testing.T) {
	for name, ir := range map[string]IR{
		"cascade":         Cascade("g", "hard-repo"),
		"fan-out-judge":   FanOutJudge("g", "deep-reasoning"),
		"single-critique": SingleCritique("g", "workhorse-coding"),
	} {
		if err := Validate(ir); err != nil {
			t.Errorf("seam template %s must validate: %v", name, err)
		}
	}
}

// FanOutJudge's two workers must be on DISTINCT explicit lanes so they are not a
// same-lane fan-out (Validate rule §4.4 / S3R-3a), and the judge is the terminal
// sink depending on both.
func TestFanOutJudgeDistinctWorkerLanes(t *testing.T) {
	ir := FanOutJudge("g", "deep-reasoning")
	if len(ir.Steps) != 3 {
		t.Fatalf("fan-out-judge is 2 workers + 1 judge = 3 nodes, got %d", len(ir.Steps))
	}
	if ir.Steps[0].LaneHint == "" || ir.Steps[1].LaneHint == "" || ir.Steps[0].LaneHint == ir.Steps[1].LaneHint {
		t.Fatalf("the two workers need distinct explicit lanes: %q vs %q", ir.Steps[0].LaneHint, ir.Steps[1].LaneHint)
	}
	if !sameDeps(ir.Steps[2].Deps, []int{0, 1}) {
		t.Fatalf("judge must depend on both workers: %v", ir.Steps[2].Deps)
	}
	if err := Validate(ir); err != nil {
		t.Fatalf("fan-out-judge must validate: %v", err)
	}
}

// The registry exposes all five templates by name, and unknown names error.
func TestExpandByNameAndUnknown(t *testing.T) {
	for _, name := range []string{"solo", "plan-work-verify", "cascade", "fan-out-judge", "single-critique"} {
		ir, err := Expand(name, "g", "workhorse-coding")
		if err != nil {
			t.Fatalf("%s expand: %v", name, err)
		}
		if err := Validate(ir); err != nil {
			t.Fatalf("%s expanded IR must validate: %v", name, err)
		}
		if ir.Name != name {
			t.Errorf("expanded IR should carry its name %q, got %q", name, ir.Name)
		}
	}
	if _, err := Expand("no-such-template", "g", "x"); err == nil {
		t.Fatal("unknown template must error")
	}
}

// TemplateNames lists exactly the five invokable templates (no auto-default).
func TestTemplateNamesRegistry(t *testing.T) {
	names := TemplateNames()
	want := map[string]bool{"solo": true, "plan-work-verify": true, "cascade": true, "fan-out-judge": true, "single-critique": true}
	if len(names) != len(want) {
		t.Fatalf("want %d templates, got %d: %v", len(want), len(names), names)
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected template name %q", n)
		}
	}
}
