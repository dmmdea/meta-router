package strategy

import (
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// markStarted simulates a crash mid-flight: the step got StartedAt + resolved
// Lane written (mark-started) but never an outcome — the exact recovery window.
func markStarted(t *testing.T, dir string, id int, lane string) {
	t.Helper()
	started := t0
	if err := Mutate(dir, func(s *State) {
		s.State = "running"
		s.StepStatus[id].StartedAt = &started
		s.StepStatus[id].Lane = lane
	}, t0); err != nil {
		t.Fatalf("markStarted step %d: %v", id, err)
	}
}

// writeReceipt appends a tagged strategy receipt to the global dispatch.jsonl so
// recovery's reconciliation can find it by dispatch_id + step_id + attempt.
func writeReceipt(t *testing.T, id string, step, attempt int, class string) {
	t.Helper()
	if err := dispatch.Append(statepaths.Dispatch(), dispatch.Record{
		Origin:       "strategy",
		DispatchID:   id,
		StepID:       step,
		Attempt:      attempt,
		OutcomeClass: class,
		TS:           t0,
	}); err != nil {
		t.Fatalf("writeReceipt: %v", err)
	}
}

// (i) started + ok-receipt in dispatch.jsonl → RECONCILED to done, NOT blocked.
// The crash beat the state write; the receipt proves completion (S3R-6a).
func TestResumeReconcilesOkReceiptToDone(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	ir := IR{Goal: "g", Steps: []Step{{ID: 0, Instruction: "spend", LaneHint: "glm", Deps: []int{}}}}
	dir := statepaths.StrategyDir("dOK")
	if err := WriteInitial(dir, ir, "dOK", t0); err != nil {
		t.Fatal(err)
	}
	markStarted(t, dir, 0, "glm")      // cloud lane started, no outcome...
	writeReceipt(t, "dOK", 0, 0, "ok") // ...but a receipt proves it finished ok

	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	if verdict != "done" {
		t.Fatalf("verdict = %q, want done (receipt reconciled)", verdict)
	}
	st, _ := Load(dir)
	if st.StepStatus[0].OutcomeClass != "ok" {
		t.Fatalf("step 0 outcome = %q, want ok (reconciled, not stranded)", st.StepStatus[0].OutcomeClass)
	}
	if st.State == "blocked" {
		t.Fatal("cloud step with an ok receipt must NOT be blocked")
	}
}

// F2c (S3R-1 / R10): the empty-hint local-class case end-to-end. An empty-hint
// node whose class routes to `local` now (F2a) dispatches on `local` and the
// executor records the RESOLVED lane "local" at mark-started (F2b keeps ss.Lane
// truthful). On a crash before the receipt, recovery must key idempotency on that
// TRUE resolved lane: idempotentLane("local")==true → actRetry (safe re-run of a
// FREE local step), NEVER actBlock. A wrong keying (on the empty LaneHint) would
// have blocked a free local step — or worse, if F2a were missing, the node would
// have really run on cloud and re-dispatching would double-spend.
func TestResumeEmptyHintLocalStepRetriesNotBlocked(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	// Empty LaneHint, class the router sends to local — mirrors the real node.
	ir := IR{Goal: "g", Steps: []Step{{ID: 0, Instruction: "normalize", Class: "mechanical-text", Deps: []int{}}}}
	dir := statepaths.StrategyDir("dLoc")
	if err := WriteInitial(dir, ir, "dLoc", t0); err != nil {
		t.Fatal(err)
	}
	// Mark-started with the RESOLVED lane "local" (what the executor now records) —
	// NO receipt (crash before it was written).
	markStarted(t, dir, 0, "local")

	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	if verdict != "resumable" {
		t.Fatalf("an idempotent free-local step with no receipt must be resumable, got %q (blocked would waste the free lane / mis-key idempotency)", verdict)
	}
	st, _ := Load(dir)
	if st.State == "blocked" {
		t.Fatal("a free local step must NEVER be blocked on resume (R10 is about cloud spend)")
	}
	// The step is reset for re-run (StartedAt cleared so readySet re-picks it).
	if st.StepStatus[0].StartedAt != nil || st.StepStatus[0].OutcomeClass != "" {
		t.Fatalf("the local step must be reset for re-run (actRetry), got started=%v outcome=%q",
			st.StepStatus[0].StartedAt, st.StepStatus[0].OutcomeClass)
	}
}

// ok_notional also counts as a completed cloud step (S3R-8: exit-4 succeeded).
func TestResumeReconcilesOkNotionalReceiptToDone(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	ir := IR{Goal: "g", Steps: []Step{{ID: 0, Instruction: "spend", LaneHint: "claude", Deps: []int{}}}}
	dir := statepaths.StrategyDir("dOKN")
	if err := WriteInitial(dir, ir, "dOKN", t0); err != nil {
		t.Fatal(err)
	}
	markStarted(t, dir, 0, "claude")
	writeReceipt(t, "dOKN", 0, 0, "ok_notional")

	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	if verdict != "done" {
		t.Fatalf("verdict = %q, want done", verdict)
	}
	if st, _ := Load(dir); st.StepStatus[0].OutcomeClass != "ok_notional" {
		t.Fatalf("outcome = %q, want ok_notional", st.StepStatus[0].OutcomeClass)
	}
}

// (ii) started + no-receipt + Lane=="local" → AUTO-RETRY: StartedAt cleared so
// readySet re-picks it (re-running a read-only local call is safe, S3R-6b).
func TestResumeLocalNoReceiptAutoRetries(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	ir := IR{Goal: "g", Steps: []Step{{ID: 0, Instruction: "read", LaneHint: "local", Deps: []int{}}}}
	dir := statepaths.StrategyDir("dLoc")
	if err := WriteInitial(dir, ir, "dLoc", t0); err != nil {
		t.Fatal(err)
	}
	markStarted(t, dir, 0, "local") // resolved lane = local; no receipt written

	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	if verdict != "resumable" {
		t.Fatalf("verdict = %q, want resumable (local auto-retry)", verdict)
	}
	st, _ := Load(dir)
	if st.StepStatus[0].StartedAt != nil || st.StepStatus[0].OutcomeClass != "" {
		t.Fatal("idempotent local step must be reset (StartedAt cleared) for re-run")
	}
	// reset step is now ready again
	if len(readySet(st)) != 1 {
		t.Fatal("reset local step must re-enter the ready set")
	}
	if st.State == "blocked" {
		t.Fatal("a free local step must never block")
	}
}

// (iii) started + no-receipt + cloud lane → BLOCKED (dispatch state blocked): we
// cannot prove it didn't spend a window, so never blind re-dispatch (R10 / S3R-6).
func TestResumeCloudNoReceiptBlocks(t *testing.T) {
	for _, lane := range []string{"claude", "glm", "codex"} {
		t.Run(lane, func(t *testing.T) {
			t.Setenv("MR_ORCH_STATE", t.TempDir())
			ir := IR{Goal: "g", Steps: []Step{
				{ID: 0, Instruction: "spend", LaneHint: lane, Deps: []int{}},
				{ID: 1, Instruction: "next", Deps: []int{0}},
			}}
			dir := statepaths.StrategyDir("dC")
			if err := WriteInitial(dir, ir, "dC", t0); err != nil {
				t.Fatal(err)
			}
			markStarted(t, dir, 0, lane) // cloud lane started, NO receipt

			verdict, err := Resume(dir)
			if err != nil {
				t.Fatalf("Resume err: %v", err)
			}
			if verdict != "blocked" {
				t.Fatalf("%s: verdict = %q, want blocked", lane, verdict)
			}
			st, _ := Load(dir)
			if st.State != "blocked" {
				t.Fatalf("%s: dispatch state = %q, want blocked (operator-gated)", lane, st.State)
			}
			// the blocked step must record blocked so it is not re-dispatched.
			if st.StepStatus[0].OutcomeClass != "blocked" {
				t.Fatalf("%s: step 0 outcome = %q, want blocked", lane, st.StepStatus[0].OutcomeClass)
			}
		})
	}
}

// non-ok receipt (e.g. api_error) → the outcome is KNOWN; record it. The taxonomy
// then governs on the re-run (a hard-fail is re-lane-eligible via Execute).
func TestResumeNonOkReceiptRecordsOutcome(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "spend", LaneHint: "glm", Deps: []int{}},
		{ID: 1, Instruction: "next", Deps: []int{0}},
	}}
	dir := statepaths.StrategyDir("dNonOk")
	if err := WriteInitial(dir, ir, "dNonOk", t0); err != nil {
		t.Fatal(err)
	}
	markStarted(t, dir, 0, "glm")
	writeReceipt(t, "dNonOk", 0, 0, "api_error") // known non-ok outcome

	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	st, _ := Load(dir)
	if st.StepStatus[0].OutcomeClass != "api_error" {
		t.Fatalf("step 0 outcome = %q, want api_error (recorded from receipt)", st.StepStatus[0].OutcomeClass)
	}
	// step 0 hard-failed with no re-lane budget consumed here and step 1 depends on
	// it, so the DAG cannot proceed → failed (not blocked: the outcome IS known).
	if verdict != "failed" {
		t.Fatalf("verdict = %q, want failed (known hard-fail, dep unsatisfiable)", verdict)
	}
}

// (iv) a fully-ok DAG (nothing mid-flight) resumes cleanly to done.
func TestResumeFullyOkDagIsDone(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "local", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "glm", Deps: []int{0}},
	}}
	dir := statepaths.StrategyDir("dDone")
	if err := WriteInitial(dir, ir, "dDone", t0); err != nil {
		t.Fatal(err)
	}
	fin := t0
	if err := Mutate(dir, func(s *State) {
		s.State = "running"
		for _, id := range []int{0, 1} {
			s.StepStatus[id].OutcomeClass = "ok"
			s.StepStatus[id].ResultRef = "ref"
			s.StepStatus[id].TS = &fin
		}
	}, t0); err != nil {
		t.Fatal(err)
	}
	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	if verdict != "done" {
		t.Fatalf("verdict = %q, want done", verdict)
	}
	if st, _ := Load(dir); st.State != "done" {
		t.Fatalf("state = %q, want done", st.State)
	}
}

// a DAG with a not-yet-started step (StartedAt==nil) still has runnable work →
// resumable; Resume must leave the un-started step alone for Execute to pick up.
func TestResumeNotStartedStepIsResumable(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "local", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "glm", Deps: []int{0}},
	}}
	dir := statepaths.StrategyDir("dResume")
	if err := WriteInitial(dir, ir, "dResume", t0); err != nil {
		t.Fatal(err)
	}
	// step 0 done, step 1 never started
	fin := t0
	if err := Mutate(dir, func(s *State) {
		s.State = "running"
		s.StepStatus[0].OutcomeClass = "ok"
		s.StepStatus[0].ResultRef = "ref"
		s.StepStatus[0].TS = &fin
	}, t0); err != nil {
		t.Fatal(err)
	}
	verdict, err := Resume(dir)
	if err != nil {
		t.Fatalf("Resume err: %v", err)
	}
	if verdict != "resumable" {
		t.Fatalf("verdict = %q, want resumable (step 1 still runnable)", verdict)
	}
	st, _ := Load(dir)
	if st.StepStatus[1].StartedAt != nil {
		t.Fatal("a not-started step must be left untouched for Execute")
	}
}

// (v) idempotent replay: running Resume twice is STABLE — no double-processing.
// After the first pass reconciles/blocks/retries, a second pass is a no-op that
// returns the same verdict.
func TestResumeIsIdempotentReplay(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())

	// case A: ok-receipt reconcile — twice → done, twice.
	irOk := IR{Goal: "g", Steps: []Step{{ID: 0, Instruction: "s", LaneHint: "glm", Deps: []int{}}}}
	dirOk := statepaths.StrategyDir("rOK")
	if err := WriteInitial(dirOk, irOk, "rOK", t0); err != nil {
		t.Fatal(err)
	}
	markStarted(t, dirOk, 0, "glm")
	writeReceipt(t, "rOK", 0, 0, "ok")
	v1, _ := Resume(dirOk)
	v2, _ := Resume(dirOk)
	if v1 != "done" || v2 != "done" {
		t.Fatalf("ok-reconcile replay: v1=%q v2=%q, want done/done", v1, v2)
	}

	// case B: cloud-no-receipt block — twice → blocked, twice (still blocked).
	irB := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "s", LaneHint: "claude", Deps: []int{}},
		{ID: 1, Instruction: "t", Deps: []int{0}},
	}}
	dirB := statepaths.StrategyDir("rBlk")
	if err := WriteInitial(dirB, irB, "rBlk", t0); err != nil {
		t.Fatal(err)
	}
	markStarted(t, dirB, 0, "claude")
	b1, _ := Resume(dirB)
	// F3 (S3R-2 blocked precedence): assert st.State is PERSISTED as "blocked" after
	// BOTH passes. Resume#1 blocks the step (no receipt on a cloud lane, R10);
	// Resume#2 reaches finalize, which must PRESERVE "blocked" on disk — it must NOT
	// classify the recovery-only "blocked" step as kindHardFail and corrupt
	// state.State to "failed" (which would make strategy_status report "failed" while
	// blocked_step is still surfaced — an inconsistent lie).
	if st, _ := Load(dirB); st.State != "blocked" {
		t.Fatalf("Resume#1 must persist state.State=blocked, got %q", st.State)
	}
	b2, _ := Resume(dirB)
	if b1 != "blocked" || b2 != "blocked" {
		t.Fatalf("block replay: b1=%q b2=%q, want blocked/blocked", b1, b2)
	}
	if st, _ := Load(dirB); st.State != "blocked" {
		t.Fatalf("Resume#2 must LEAVE state.State=blocked (not corrupt it to failed), got %q", st.State)
	}
	if st, _ := Load(dirB); st.StepStatus[0].OutcomeClass != "blocked" {
		t.Fatal("blocked step must stay blocked across replay")
	}
}

// unit: resolveAction classifies a single mid-flight step given (receiptClass,
// receiptFound, resolvedLane). This is the plan draft's resumeAction, keyed on the
// RESOLVED lane and reconciliation-aware (S3R-6).
func TestResolveActionTable(t *testing.T) {
	cases := []struct {
		name         string
		receiptClass string
		found        bool
		lane         string
		want         recoveryAction
	}{
		{"ok-receipt reconciles", "ok", true, "glm", actReconcile},
		{"ok_notional reconciles", "ok_notional", true, "claude", actReconcile},
		{"non-ok receipt records", "api_error", true, "glm", actRecord},
		{"local no-receipt retries", "", false, "local", actRetry},
		{"claude no-receipt blocks", "", false, "claude", actBlock},
		{"glm no-receipt blocks", "", false, "glm", actBlock},
		{"codex no-receipt blocks", "", false, "codex", actBlock},
	}
	for _, c := range cases {
		if got := resolveAction(c.receiptClass, c.found, c.lane); got != c.want {
			t.Errorf("%s: resolveAction(%q,%v,%q) = %v, want %v", c.name, c.receiptClass, c.found, c.lane, got, c.want)
		}
	}
}
