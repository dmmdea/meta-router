package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	"github.com/dmmdea/meta-router/internal/orch/strategy"
)

// S3R-8: the doRun exit code maps to a NodeResult outcome class. Exit-4
// (notional guard) is ok-with-warning ("ok_notional"), NOT a hard fail — it must
// NOT trigger a re-lane. Exit-5 keeps the REAL outcome class from the run (read
// from the receipt), never flattened to api_error.
func TestNodeClassFromExit(t *testing.T) {
	cases := []struct {
		code         int
		receiptClass string
		want         string
	}{
		{0, "ok", "ok"},
		{exitDeferred, "deferred", "deferred"},
		{exitNotional, "ok", "ok_notional"}, // S3R-8: exit-4 is ok-with-warning
		{exitNotOK, "refusal", "refusal"},   // S3R-8: keep the real class
		{exitNotOK, "api_error", "api_error"},
		{exitNotOK, "rate_limit", "rate_limit"}, // relegate class survives
		{1, "", "config_error"},
	}
	for _, c := range cases {
		if got := nodeClassFromExit(c.code, c.receiptClass); got != c.want {
			t.Errorf("exit %d receipt=%q → %q, want %q", c.code, c.receiptClass, got, c.want)
		}
	}
}

// prodNodeRunner writes exactly ONE receipt per node (S3R-4: doRun's own tagged
// receipt), never a second. NodeResult.Lane is the RESOLVED lane read back from
// that receipt (S3R-3b). The class comes from the exit code + the receipt's real
// outcome_class.
func TestProdNodeRunnerOneReceiptResolvedLane(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "abc123"
	// Fake doRun: writes the ONE tagged receipt (as real doRun does) and returns
	// exit 0. The receipt carries the resolved lane "glm".
	oldDispatch := nodeDispatch
	defer func() { nodeDispatch = oldDispatch }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: "glm", Model: "glm-4.6", OutcomeClass: "ok",
			Origin: "strategy", DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		io.WriteString(out, `{"result":"hi"}`)
		return 0, nil
	}
	runner := prodNodeRunner(id, nil)
	step := strategy.Step{ID: 0, Instruction: "do x", LaneHint: "glm"}
	res := runner(step, "do x", 0)
	if res.OutcomeClass != "ok" {
		t.Fatalf("class = %q, want ok", res.OutcomeClass)
	}
	if res.Lane != "glm" {
		t.Fatalf("NodeResult.Lane must be the resolved lane from the receipt, got %q", res.Lane)
	}
	if res.ResultContent != `{"result":"hi"}` {
		t.Fatalf("result content not captured: %q", res.ResultContent)
	}
	// Exactly one strategy receipt for this dispatch:step:attempt.
	n := 0
	for _, r := range loadReceipts(dispatchPath()) {
		if r.DispatchID == id && r.StepID == 0 && r.Attempt == 0 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("prodNodeRunner must yield exactly ONE receipt per node (S3R-4), got %d", n)
	}
}

// S3R-8: exit-4 maps to ok_notional (which classifies as kindOK → satisfies a
// dep, no re-lane).
func TestProdNodeRunnerExit4IsOkNotional(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "ex4"
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: "claude", OutcomeClass: "ok", Origin: "strategy",
			DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		return exitNotional, nil
	}
	res := prodNodeRunner(id, nil)(strategy.Step{ID: 0, LaneHint: "claude"}, "p", 0)
	if res.OutcomeClass != "ok_notional" {
		t.Fatalf("exit-4 must be ok_notional, got %q", res.OutcomeClass)
	}
}

// S3R-3b: at attempt>0 the runner pins the lane from the alternatives seam,
// EXCLUDING the lane that failed the prior attempt (read from the prior receipt).
func TestProdNodeRunnerPinsAltLaneOnRetry(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "retry1"
	// Prior attempt (0) failed on lane "claude" — seed that receipt.
	_ = dispatch.Append(dispatchPath(), dispatch.Record{
		TS: time.Now().UTC(), Lane: "claude", OutcomeClass: "api_error", Origin: "strategy",
		DispatchID: id, StepID: 0, Attempt: 0,
	})
	var gotLane string
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		gotLane = opts.Lane
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: opts.Lane, OutcomeClass: "ok", Origin: "strategy",
			DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		return 0, nil
	}
	// alt seam returns glm, excluding whatever lane is passed.
	alt := func(s strategy.Step, exclude string) (string, string, string, bool) {
		if exclude != "claude" {
			t.Errorf("alt must be asked to EXCLUDE the failed lane claude, got exclude=%q", exclude)
		}
		return "glm", "glm-4.6", "", true
	}
	res := prodNodeRunner(id, alt)(strategy.Step{ID: 0, LaneHint: "claude"}, "p", 1)
	if gotLane != "glm" {
		t.Fatalf("retry must dispatch on the pinned alt lane glm, got %q", gotLane)
	}
	if res.Lane != "glm" {
		t.Fatalf("NodeResult.Lane on retry must be the alt lane glm, got %q", res.Lane)
	}
}

// F2a (S3R-1 + R10 double-spend): an empty-hint node whose class the router
// resolves to `local` (mechanical-text → local/gemma4-cascade, rank-1) must be
// dispatched with the EXPLICIT resolved lane "local" — so doRun's local switch
// hits runLocalLane (the two-door). It must NOT pass "auto", which sends
// resolveLane's auto→local handoff to a CLOUD alternative (glm), defeating S3R-1
// AND making the dispatched lane diverge from the mark-started `local` guess
// (which then double-spends cloud on recovery, since idempotentLane(local)==true).
func TestProdNodeRunnerLocalClassDispatchesLocalNotAuto(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "loc1"
	var gotLane string
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		gotLane = opts.Lane
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: "local", OutcomeClass: "ok", Origin: "strategy",
			DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		io.WriteString(out, `{"result":"cleaned"}`)
		return 0, nil
	}
	// Empty LaneHint + a class the router sends to local (rank-1).
	step := strategy.Step{ID: 0, Class: "mechanical-text", Instruction: "normalize this text"}
	res := prodNodeRunner(id, nil)(step, "normalize this text", 0)
	if gotLane != "local" {
		t.Fatalf("an empty-hint local-class node must dispatch with the EXPLICIT resolved lane \"local\" (hits runLocalLane / the two-door), got %q — \"auto\" would auto-fall to cloud (S3R-1/R10)", gotLane)
	}
	if res.Lane != "local" {
		t.Fatalf("NodeResult.Lane must be the resolved lane local, got %q", res.Lane)
	}
}

// F2 regression (found by the S3R-9 live gate): an empty-hint CLOUD-class node
// must dispatch with a RESOLVED --model, not just a lane. F2a fixed the LANE but
// left model=step.ModelHint (empty for a template node), so claude/codex/glm —
// which REQUIRE a pinned --model — hard-failed with config_error in ~5ms before
// reaching the binary. The fake-based tests never caught it (they don't require a
// model); the live gate did. prodNodeRunner now fills model+effort from the router
// decision for the chosen lane.
func TestProdNodeRunnerResolvesModelForEmptyHintCloudNode(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "mdl1"
	var gotLane, gotModel string
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		gotLane, gotModel = opts.Lane, opts.Model
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: opts.Lane, Model: opts.Model, OutcomeClass: "ok",
			Origin: "strategy", DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		return 0, nil
	}
	// Empty LaneHint + a hard cloud class the router sends to a cloud lane (rank-1).
	step := strategy.Step{ID: 0, Class: "hard-repo", Instruction: "write a function"}
	prodNodeRunner(id, nil)(step, "write a function", 0)
	if gotLane == "" || gotLane == "auto" {
		t.Fatalf("empty-hint hard-repo node must resolve to an explicit cloud lane, got %q", gotLane)
	}
	if gotModel == "" {
		t.Fatalf("empty-hint cloud node must dispatch with a RESOLVED --model (F2 dropped it → config_error); got empty model on lane %q", gotLane)
	}
}

// Finding-1 fix (adversarial re-review): an EXPLICIT cloud LaneHint the class does
// NOT rank must still resolve a --model from the rank table, not config_error →
// re-lane. The shipped templates emit exactly these: cascade's glm verify-gate
// verifier (verify-gate has no glm row) and fan-out-judge's codex worker on a
// codex-less class.
func TestProdNodeRunnerResolvesModelForExplicitCloudLane(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "expl1"
	var gotLane, gotModel string
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		gotLane, gotModel = opts.Lane, opts.Model
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: opts.Lane, Model: opts.Model, OutcomeClass: "ok",
			Origin: "strategy", DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		return 0, nil
	}
	// cascade verifier: glm pinned on verify-gate (no glm row for that class).
	prodNodeRunner(id, nil)(strategy.Step{ID: 1, Role: "verifier", Class: "verify-gate", LaneHint: "glm"}, "p", 0)
	if gotLane != "glm" || gotModel == "" {
		t.Fatalf("explicit glm verify-gate node must dispatch glm WITH a resolved model (not config_error), got lane=%q model=%q", gotLane, gotModel)
	}
	// fan-out-judge worker: codex pinned on a codex-less class (mechanical-text).
	prodNodeRunner(id, nil)(strategy.Step{ID: 0, Role: "worker", Class: "mechanical-text", LaneHint: "codex"}, "p", 0)
	if gotLane != "codex" || gotModel == "" {
		t.Fatalf("explicit codex worker must dispatch codex WITH a resolved model, got lane=%q model=%q", gotLane, gotModel)
	}
}

// Coverage (re-review NIT): a pinned lane that IS a Pareto alternative for its class
// (not the router's primary pick) resolves its model from dec.Alternatives — the
// middle fill branch, distinct from laneModelFromTable. Shipped trigger:
// fan-out-judge's codex worker on many-tool-orchestration (codex is a rank-1
// alternative there, claude the primary), so the alternatives loop, not the rank-
// table fallback, does the fill.
func TestProdNodeRunnerResolvesModelFromParetoAlternative(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "alt2"
	var gotLane, gotModel string
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		gotLane, gotModel = opts.Lane, opts.Model
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: opts.Lane, Model: opts.Model, OutcomeClass: "ok",
			Origin: "strategy", DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		return 0, nil
	}
	prodNodeRunner(id, nil)(strategy.Step{ID: 1, Role: "worker", Class: "many-tool-orchestration", LaneHint: "codex"}, "p", 0)
	if gotLane != "codex" || gotModel == "" {
		t.Fatalf("a pinned lane that is a Pareto alternative must resolve its model from the decision, got lane=%q model=%q", gotLane, gotModel)
	}
}

// The supervisor heartbeats state.json while it runs (S3R-7). A trivial one-step
// DAG driven by a fake doRun must reach a terminal state AND have stamped a
// HeartbeatAt.
func TestRunStrategyRunHeartbeatsAndCompletes(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "hb1"
	dir := statepaths.StrategyDir(id)
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", LaneHint: "local", Deps: []int{}}}}, id, time.Now().UTC())
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: "local", OutcomeClass: "ok", Origin: "strategy",
			DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		io.WriteString(out, `{"result":"done"}`)
		return 0, nil
	}
	if err := runStrategyRun([]string{id}); err != nil {
		t.Fatalf("runStrategyRun: %v", err)
	}
	st, _ := strategy.Load(dir)
	if st.State != "done" {
		t.Fatalf("dispatch must be done, got %q", st.State)
	}
	if st.HeartbeatAt == nil {
		t.Fatal("supervisor must have stamped HeartbeatAt (S3R-7)")
	}
}

// S3R-7 --resume: a blocked dispatch (a cloud-spend step started with no ok
// receipt) must STOP, not feed a blocked step into Execute.
func TestRunStrategyRunResumeBlockedStops(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "blk1"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", LaneHint: "claude", Deps: []int{}}}}, id, now)
	// Mark the step started on a cloud lane with NO receipt → Resume blocks it.
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "running"
		started := now
		s.StepStatus[0].StartedAt = &started
		s.StepStatus[0].Lane = "claude"
	}, now)
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	called := false
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) { called = true; return 0, nil }
	if err := runStrategyRun([]string{"--resume", id}); err != nil {
		t.Fatalf("runStrategyRun --resume: %v", err)
	}
	if called {
		t.Fatal("a blocked step must NOT be dispatched into Execute (Group D contract)")
	}
	st, _ := strategy.Load(dir)
	if st.State != "blocked" {
		t.Fatalf("resume of an unprovable cloud step must leave state blocked, got %q", st.State)
	}
}

// S3R-7 --sweep: a stale running dispatch (dead supervisor) is reaped — Resume
// runs on it. Here the stale dispatch has an idempotent local step with no
// receipt, so Resume retries it and --sweep drives it to done.
func TestRunStrategyRunSweepReapsStale(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "stale1"
	dir := statepaths.StrategyDir(id)
	old := time.Now().UTC().Add(-time.Hour) // ancient
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", LaneHint: "local", Deps: []int{}}}}, id, old)
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "running"
		st := old
		s.StepStatus[0].StartedAt = &st
		s.StepStatus[0].Lane = "local"
	}, old)
	_ = strategy.Heartbeat(dir, old) // last beat an hour ago → stale

	oldD := nodeDispatch
	defer func() { nodeDispatch = oldD }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: "local", OutcomeClass: "ok", Origin: "strategy",
			DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		io.WriteString(out, `{"r":"ok"}`)
		return 0, nil
	}
	if err := runStrategyRun([]string{"--sweep"}); err != nil {
		t.Fatalf("runStrategyRun --sweep: %v", err)
	}
	st, _ := strategy.Load(dir)
	if st.State != "done" {
		t.Fatalf("sweep must reap the stale dispatch to a terminal state, got %q", st.State)
	}
}

// F1 (R10 double-spend): driveDispatch (all three entrypoints) must REFUSE to
// start a second Execute when a LIVE supervisor already holds the lease — the
// laptop-sleep false-positive guard. Here a live lease is held by a DIFFERENT pid
// with a fresh heartbeat; runStrategyRun must return a clean no-op WITHOUT
// dispatching any node (no second cloud window burned).
func TestDriveDispatchRefusesWhenLiveLeaseHeld(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "live1"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "spend", LaneHint: "claude", Deps: []int{}}}}, id, now)
	// A LIVE other supervisor holds the lease: pid 4242, fresh heartbeat NOW.
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "running"
		s.SupervisorPID = 4242
		hb := now
		s.HeartbeatAt = &hb
	}, now)

	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	called := false
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) { called = true; return 0, nil }

	if err := runStrategyRun([]string{id}); err != nil {
		t.Fatalf("runStrategyRun must be a clean no-op when a live lease is held: %v", err)
	}
	if called {
		t.Fatal("a second supervisor must NOT dispatch when a LIVE lease is held (R10 double-spend)")
	}
	// The live holder's lease is untouched.
	st, _ := strategy.Load(dir)
	if st.SupervisorPID != 4242 {
		t.Fatalf("a refused drive must not disturb the live lease, pid=%d want 4242", st.SupervisorPID)
	}
}

// F1: --sweep must only reap a GENUINELY DEAD supervisor (stale lease/heartbeat),
// never a live one. A dispatch with a fresh heartbeat is not stale → sweep skips
// it and never dispatches its node. (The lease in driveDispatch is the
// authoritative belt; Stale() here is the cheap pre-filter — both must agree.)
func TestSweepSkipsLiveLeaseDispatch(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "livesweep"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "spend", LaneHint: "claude", Deps: []int{}}}}, id, now)
	// A LIVE supervisor: running, pid set, heartbeat NOW (well within staleThreshold).
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "running"
		s.SupervisorPID = 7777
		started := now
		s.StepStatus[0].StartedAt = &started
		s.StepStatus[0].Lane = "claude"
	}, now)
	_ = strategy.Heartbeat(dir, now)

	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	called := false
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) { called = true; return 0, nil }

	if err := runStrategyRun([]string{"--sweep"}); err != nil {
		t.Fatalf("runStrategyRun --sweep: %v", err)
	}
	if called {
		t.Fatal("--sweep must NOT reap a LIVE dispatch (fresh heartbeat) — never a second Execute on a live cloud step (R10)")
	}
	// The live dispatch is untouched: still running, still held, step not blocked.
	st, _ := strategy.Load(dir)
	if st.State != "running" || st.SupervisorPID != 7777 {
		t.Fatalf("sweep disturbed a live dispatch: state=%q pid=%d", st.State, st.SupervisorPID)
	}
}

// strategyStatusJSON returns the published 3-field core; an unknown id is a clean
// error object, not a crash.
func TestStrategyStatusReadsStateAndReceipts(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "deadbeef"
	dir := statepaths.StrategyDir(id)
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", Deps: []int{}}}}, id, time.Now().UTC())
	_ = strategy.Mutate(dir, func(s *strategy.State) { s.State = "done"; s.ResultRef = "r0" }, time.Now().UTC())
	js := strategyStatusJSON(id)
	if !strings.Contains(js, `"state": "done"`) {
		t.Fatalf("status missing state: %s", js)
	}
	if !strings.Contains(js, "result_ref") {
		t.Fatalf("status missing result_ref: %s", js)
	}
	// Unknown id → clean error object.
	un := strategyStatusJSON("nope")
	if !strings.Contains(un, "no such dispatch") {
		t.Fatalf("unknown id must return a clean error object: %s", un)
	}
}

// S3R-9 blocked visibility: a blocked step's identity + lane + spend must be
// FRONT-AND-CENTER (a top-level blocked_step field), not buried in the array.
func TestStrategyStatusBlockedStepFrontAndCenter(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "blkstat"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 2, Instruction: "spend", LaneHint: "claude", Deps: []int{}}}}, id, now)
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "blocked"
		s.StepStatus[2].OutcomeClass = "blocked"
		s.StepStatus[2].Lane = "claude"
	}, now)
	// Seed the step's spend receipt so status can surface what it cost.
	_ = dispatch.Append(dispatchPath(), dispatch.Record{
		TS: now, Lane: "claude", OutcomeClass: "blocked", Origin: "strategy",
		NotionalUSD: 0.42, DispatchID: id, StepID: 2, Attempt: 0,
	})
	js := strategyStatusJSON(id)
	if !strings.Contains(js, "blocked_step") {
		t.Fatalf("a blocked dispatch must surface a top-level blocked_step: %s", js)
	}
	// step_id 2 + lane claude must be present in the blocked_step block.
	if !strings.Contains(js, `"step_id": 2`) || !strings.Contains(js, "claude") {
		t.Fatalf("blocked_step must carry step_id + lane: %s", js)
	}
}

// S3R-7 stale detection in status: a running dispatch whose heartbeat is stale is
// reported needs_resume with a resume command, not lying "running".
func TestStrategyStatusStaleReportsNeedsResume(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "stalestat"
	dir := statepaths.StrategyDir(id)
	old := time.Now().UTC().Add(-time.Hour)
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", Deps: []int{}}}}, id, old)
	_ = strategy.Mutate(dir, func(s *strategy.State) { s.State = "running" }, old)
	_ = strategy.Heartbeat(dir, old)
	js := strategyStatusJSON(id)
	if !strings.Contains(js, "needs_resume") {
		t.Fatalf("a stale running dispatch must report needs_resume: %s", js)
	}
	if !strings.Contains(js, "strategy-run") {
		t.Fatalf("needs_resume must carry the resume command: %s", js)
	}
}

// Make-verify-count: parseVerifyVerdict extracts the triage decision from the full
// cascade shape AND the payload-only shape, lowercases it, and fails EMPTY on
// free-text / garbage (never a false flag).
func TestParseVerifyVerdict(t *testing.T) {
	cases := []struct{ name, in, wantV, wantR string }{
		{"full-cascade-no", `{"ok":true,"result":{"decision":"NO","reason":"midpoint overflows"},"meta":{}}`, "no", "midpoint overflows"},
		{"full-cascade-yes", `{"ok":true,"result":{"decision":"yes","reason":"correct"}}`, "yes", "correct"},
		{"payload-only", `{"decision":"unsure","reason":"cannot tell"}`, "unsure", "cannot tell"},
		{"free-text-cloud", `{"type":"result","result":"looks fine to me"}`, "", ""},
		{"garbage", `not json`, "", ""},
	}
	for _, c := range cases {
		v, r := parseVerifyVerdict(c.in)
		if v != c.wantV || r != c.wantR {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, v, r, c.wantV, c.wantR)
		}
	}
}

// A verifier-role node's tier-2 judgment is extracted into NodeResult; a
// non-verifier node carries NO verdict even with identical content (no false flag).
func TestProdNodeRunnerExtractsVerifyVerdict(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "vext"
	old := nodeDispatch
	defer func() { nodeDispatch = old }()
	nodeDispatch = func(opts runOpts, out io.Writer) (int, error) {
		_ = dispatch.Append(dispatchPath(), dispatch.Record{
			TS: time.Now().UTC(), Lane: "local", OutcomeClass: "ok", Origin: "strategy",
			DispatchID: opts.DispatchID, StepID: opts.StepID, Attempt: opts.Attempt,
		})
		io.WriteString(out, `{"ok":true,"result":{"decision":"no","reason":"bug present"}}`)
		return 0, nil
	}
	vres := prodNodeRunner(id, nil)(strategy.Step{ID: 1, Role: "verifier", Class: "verify-gate", LaneHint: "local"}, "p", 0)
	if vres.VerifyVerdict != "no" || vres.VerifyReason == "" {
		t.Fatalf("verifier node must extract verdict+reason, got %q/%q", vres.VerifyVerdict, vres.VerifyReason)
	}
	wres := prodNodeRunner(id, nil)(strategy.Step{ID: 0, Role: "worker", Class: "hard-repo", LaneHint: "local"}, "p", 0)
	if wres.VerifyVerdict != "" {
		t.Fatalf("a non-verifier node must NOT carry a verdict, got %q", wres.VerifyVerdict)
	}
}

// strategy_status surfaces a flagged verify verdict front-and-center WITHOUT
// changing the (tier-1) state.
func TestStrategyStatusSurfacesVerifyFlag(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "vstat"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", Deps: []int{}}}}, id, now)
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "done"
		s.VerifyFlag = true
		s.VerifyVerdict = "no"
		s.VerifyReason = "midpoint not overflow-safe"
	}, now)
	js := strategyStatusJSON(id)
	if !strings.Contains(js, `"verify"`) || !strings.Contains(js, `"flagged": true`) {
		t.Fatalf("a flagged verify must surface a top-level verify block with flagged=true: %s", js)
	}
	if !strings.Contains(js, "midpoint not overflow-safe") {
		t.Fatalf("verify block must carry the reason: %s", js)
	}
	if !strings.Contains(js, `"state": "done"`) {
		t.Fatalf("advisory verify must NOT change the state: %s", js)
	}
}

func TestStrategyDirHelper(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", filepath.Join(t.TempDir(), "s"))
	got := statepaths.StrategyDir("xyz")
	if !strings.Contains(got, "xyz") {
		t.Fatalf("StrategyDir must include the id: %s", got)
	}
}

// E4: a deferred strategy dispatch surfaces resume_at (truthful, earliest step)
// + retry_at (jittered) in strategy_status. Two steps with distinct ResumeAts,
// where the SECOND-inserted step (ID 1) carries the EARLIER reset, pin the
// min-scan over StepStatus (a map — iteration order is nondeterministic) rather
// than a naive first/insertion-order pick: resume_at MUST equal the earlier of
// the two exactly, and retry_at MUST fall in [resume_at, resume_at+90s).
func TestStrategyStatusDeferredCarriesRetryAt(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "jitstat"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	later := now.Add(45 * time.Minute)   // step 0 — the LATER reset
	earlier := now.Add(30 * time.Minute) // step 1 (inserted second) — the EARLIER reset
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{
		{ID: 0, Instruction: "x", Deps: []int{}},
		{ID: 1, Instruction: "y", Deps: []int{}},
	}}, id, now)
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "deferred"
		s.StepStatus[0].OutcomeClass = "deferred"
		s.StepStatus[0].ResumeAt = &later
		s.StepStatus[1].OutcomeClass = "deferred"
		s.StepStatus[1].ResumeAt = &earlier
	}, now)
	js := strategyStatusJSON(id)

	var out map[string]any
	if err := json.Unmarshal([]byte(js), &out); err != nil {
		t.Fatalf("status JSON must parse: %v\n%s", err, js)
	}
	resumeStr, _ := out["resume_at"].(string)
	retryStr, _ := out["retry_at"].(string)
	if resumeStr == "" || retryStr == "" {
		t.Fatalf("deferred status must carry resume_at + retry_at: %s", js)
	}
	gotResume, err := time.Parse(time.RFC3339Nano, resumeStr)
	if err != nil {
		t.Fatalf("resume_at must be RFC3339: %v", err)
	}
	// Truthful: resume_at is EXACTLY the earlier step's reset (min-scan, not the
	// insertion-order-first step 0).
	if !gotResume.Equal(earlier) {
		t.Fatalf("resume_at must be the EARLIER reset %v (min-scan), got %v", earlier, gotResume)
	}
	gotRetry, err := time.Parse(time.RFC3339Nano, retryStr)
	if err != nil {
		t.Fatalf("retry_at must be RFC3339: %v", err)
	}
	// Bounds: retry_at ∈ [resume_at, resume_at+90s) — jittered, never before the
	// truthful resume, never beyond the smoothing window.
	if gotRetry.Before(gotResume) || !gotRetry.Before(gotResume.Add(90*time.Second)) {
		t.Fatalf("retry_at %v must be in [resume_at, resume_at+90s) from %v", gotRetry, gotResume)
	}
}

// strategy_status is POLLED — a scheduler reads it repeatedly to decide when to
// wake a deferred dispatch. retry_at MUST be stable across reads; the old code
// re-rolled the jitter on every call, so a poller saw a moving target and could
// never settle. Two reads of the same deferred dispatch must return the SAME
// retry_at.
func TestStrategyStatusRetryAtStableAcrossReads(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "jitstable"
	dir := statepaths.StrategyDir(id)
	now := time.Now().UTC()
	// MULTIPLE steps with distinct ResumeAts (+ a tie) so this also pins the
	// min-scan's order-invariance: StepStatus is a map (randomized iteration), and
	// the whole fix rests on the earliest-reset seed being identical every read.
	later := now.Add(45 * time.Minute)
	earliest := now.Add(30 * time.Minute)
	earliestTie := now.Add(30 * time.Minute) // equal instant, different step
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{
		{ID: 0, Instruction: "x", Deps: []int{}},
		{ID: 1, Instruction: "y", Deps: []int{}},
		{ID: 2, Instruction: "z", Deps: []int{}},
	}}, id, now)
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		s.State = "deferred"
		s.StepStatus[0].OutcomeClass = "deferred"
		s.StepStatus[0].ResumeAt = &later
		s.StepStatus[1].OutcomeClass = "deferred"
		s.StepStatus[1].ResumeAt = &earliest
		s.StepStatus[2].OutcomeClass = "deferred"
		s.StepStatus[2].ResumeAt = &earliestTie
	}, now)

	retryOf := func() string {
		var out map[string]any
		if err := json.Unmarshal([]byte(strategyStatusJSON(id)), &out); err != nil {
			t.Fatalf("status JSON must parse: %v", err)
		}
		s, _ := out["retry_at"].(string)
		if s == "" {
			t.Fatal("deferred status must carry retry_at")
		}
		return s
	}
	first := retryOf()
	for i := 0; i < 20; i++ {
		if got := retryOf(); got != first {
			t.Fatalf("read %d re-rolled retry_at: %s != %s (must be stable across polls)", i, got, first)
		}
	}
}
