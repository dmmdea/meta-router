package strategy

import (
	"strings"
	"testing"
	"time"
)

// Group G — fault matrix roll-up (strategy package rows). Every row asserts a
// fault is CLASSIFIED or relegated, NEVER a panic. Rows already owned by a
// Group A–F test are NAMED in the evidence doc and not duplicated here; this file
// adds the missing consolidation rows.

// FAULT ROW (IR, consolidation): every malformed IR shape is REJECTED with a
// message and NEVER panics. This is the single "Validate never panics" table
// that rolls up the individual ir_test.go rejection tests (empty / over-cap /
// dup / dep-oob / forward / self / same-lane fan-out / orphan sink). The
// per-shape acceptance/rejection assertions live in ir_test.go; this row pins
// the panic-freedom contract across the whole invalid-shape space at once.
func TestFaultValidateRejectsEveryInvalidShapeNeverPanics(t *testing.T) {
	over := make([]Step, 0, MaxSteps+1)
	for i := 0; i <= MaxSteps; i++ {
		if i == 0 {
			over = append(over, step(i))
		} else {
			over = append(over, step(i, i-1))
		}
	}
	cases := map[string]IR{
		"empty":            {Goal: "g"},
		"over-cap":         {Goal: "g", Steps: over},
		"dup-id":           {Goal: "g", Steps: []Step{step(0), step(0)}},
		"dep-out-of-range": {Goal: "g", Steps: []Step{step(0, 9)}},
		"forward-dep":      {Goal: "g", Steps: []Step{step(0, 1), step(1)}},
		"self-dep":         {Goal: "g", Steps: []Step{step(0, 0)}},
		"same-lane-fanout": {Goal: "g", Steps: []Step{
			{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},
			{ID: 1, Instruction: "b", LaneHint: "glm", Deps: []int{}},
			{ID: 2, Instruction: "j", Deps: []int{0, 1}},
		}},
		"orphan-nonterminal-sink": {Goal: "g", Steps: []Step{step(0), step(1, 0), step(2, 0)}},
	}
	for name, ir := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Validate(%s) PANICKED (%v) — a malformed IR must be classified, never panic", name, r)
				}
			}()
			if err := Validate(ir); err == nil {
				t.Errorf("Validate(%s) must REJECT with a message, got nil", name)
			}
		}()
	}
}

// FAULT ROW (executor cancel): strategy_cancel between waves — a running node's
// wave FINISHES, no NEW wave starts, and the dispatch lands in state "cancelled".
// The mcp-side sentinel write is covered by TestMCPStrategyCancelWritesSentinel;
// this is the missing EXECUTOR-side row proving Execute honors the sentinel at
// the wave boundary (the between-wave cancel floor, S3R cancel). We drop the
// cancel sentinel before Execute so the FIRST wave-boundary check trips: the DAG
// runs no node and finalizes as cancelled — deterministic and panic-free.
func TestFaultCancelBetweenWavesStopsAtBoundary(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	// Request cancel up-front: the first between-wave check (before readySet) trips.
	if err := RequestCancel(dir, t0); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{script: map[int]NodeResult{}}
	if err := Execute(dir, f.run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 2, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatalf("a cancelled dispatch must return cleanly, not error: %v", err)
	}
	st, _ := Load(dir)
	if st.State != "cancelled" {
		t.Fatalf("cancel between waves must land state=cancelled, got %q", st.State)
	}
	if len(f.seen) != 0 {
		t.Fatalf("no new wave may start after a pre-wave cancel, but ran steps %v", f.seen)
	}
	if st.StepStatus[0].OutcomeClass != "" || st.StepStatus[1].OutcomeClass != "" {
		t.Fatal("no step may be recorded terminal when cancelled at the first boundary")
	}
}

// FAULT ROW (executor cancel mid-DAG): a wave that has ALREADY run finishes, then
// a cancel requested during it stops the NEXT wave — the running node's result is
// recorded, downstream never starts, state=cancelled. This exercises the
// "running wave finishes, no new wave starts" half of the cancel contract that
// the pre-wave test above does not: here step 0 runs, the runner itself requests
// cancel, and step 1 must never dispatch.
func TestFaultCancelAfterFirstWaveFinishesThenStops(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	f := &cancelRequestingRunner{dir: dir}
	if err := Execute(dir, f.run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatalf("cancelled dispatch must not error: %v", err)
	}
	st, _ := Load(dir)
	if st.State != "cancelled" {
		t.Fatalf("state must be cancelled after mid-DAG cancel, got %q", st.State)
	}
	// Step 0 (the wave that was already dispatched) completed; step 1 never ran.
	if st.StepStatus[0].OutcomeClass != "ok" {
		t.Fatalf("the in-flight wave's node must finish, step 0 = %q", st.StepStatus[0].OutcomeClass)
	}
	if st.StepStatus[1].OutcomeClass != "" {
		t.Fatal("no new wave may start after cancel — step 1 must not run")
	}
}

// cancelRequestingRunner runs step 0 normally but requests cancel as a side
// effect, so the executor's next wave-boundary check stops the DAG.
type cancelRequestingRunner struct{ dir string }

func (c *cancelRequestingRunner) run(step Step, prompt string, attempt int) NodeResult {
	if step.ID == 0 {
		_ = RequestCancel(c.dir, t0)
	}
	return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(step.ID), Lane: step.LaneHint}
}

// FAULT ROW (executor, honest partial): a hard-failed non-terminal branch with
// NO re-lane left (ReLaneMaxDepth:0, no alternative) must land the dispatch
// FAILED with the failed step's partial result_ref referenced HONESTLY (S3R-2) —
// never a false ok, never a panic. TestFinalizeFailedBeatsDoneWhenNonTerminalHardFails
// pins the state; this row additionally pins that the partial artifact's CONTENT
// is the honest failed-branch output (not silently dropped or relabeled ok).
func TestFaultHardFailNonTerminalReferencesHonestPartial(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "codex", Deps: []int{}},
		{ID: 2, Instruction: "judge", Deps: []int{0, 1}},
	}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 {
			return NodeResult{OutcomeClass: "api_error", ResultContent: "PARTIAL-boom-0", Lane: "glm"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(s.ID), Lane: s.LaneHint}
	}
	if err := Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 2, ReLaneMaxDepth: 0}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatalf("a hard-failed DAG must finalize cleanly, not error: %v", err)
	}
	st, _ := Load(dir)
	if st.State != "failed" {
		t.Fatalf("hard-failed non-terminal branch → failed, got %q", st.State)
	}
	// The failed step's artifact is written and carries its outcome + partial content
	// honestly (never relabeled ok).
	ref := st.StepStatus[0].ResultRef
	if ref == "" {
		t.Fatal("the failed step's partial result_ref must be referenced honestly")
	}
	a, err := ReadArtifact(ref)
	if err != nil {
		t.Fatalf("the honest partial artifact must be readable: %v", err)
	}
	if a.OutcomeClass != "api_error" {
		t.Fatalf("the partial artifact must carry the TRUE outcome (api_error), got %q", a.OutcomeClass)
	}
	if !strings.Contains(a.Content, "PARTIAL-boom-0") {
		t.Fatalf("the partial artifact must carry the honest failed-branch content, got %q", a.Content)
	}
}
