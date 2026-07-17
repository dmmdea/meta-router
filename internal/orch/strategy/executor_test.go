package strategy

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// itoa converts a single-digit step id to its ascii char (test step ids are 0-9).
func itoa(i int) string { return string(rune('0' + i)) }

// fakeRunner records call order + returns a scripted class per step id.
type fakeRunner struct {
	mu      sync.Mutex
	script  map[int]NodeResult
	seen    []int
	prompts map[int]string
}

func (f *fakeRunner) run(step Step, prompt string, attempt int) NodeResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, step.ID)
	if f.prompts == nil {
		f.prompts = map[int]string{}
	}
	f.prompts[step.ID] = prompt
	r := f.script[step.ID]
	if r.OutcomeClass == "" {
		r.OutcomeClass = "ok"
	}
	if r.ResultContent == "" {
		r.ResultContent = "out-" + itoa(step.ID)
	}
	return r
}

func setupDispatch(t *testing.T, ir IR) string {
	t.Helper()
	dir := t.TempDir() + "/d1"
	if err := WriteInitial(dir, ir, "d1", t0); err != nil {
		t.Fatal(err)
	}
	return dir
}

// noAlt / noResolve are the null seams for tests that exercise neither.
func noAlt(Step, string) (string, string, string, bool) { return "", "", "", false }

// hintResolve mirrors the executor's default when there is no live router: a node
// resolves to its explicit LaneHint (empty hint => empty lane).
func hintResolve(s Step) string { return s.LaneHint }

func contains(s, sub string) bool { return strings.Contains(s, sub) }

// readJournal reads journal.jsonl into typed entries for assertions.
func readJournal(t *testing.T, dir string) []JournalEntry {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		t.Fatalf("open journal: %v", err)
	}
	defer f.Close()
	var out []JournalEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e JournalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("bad journal line %q: %v", line, err)
		}
		out = append(out, e)
	}
	return out
}

// createLock/removeLock let a test hold the state lock to force a lock-busy
// Mutate (S3R-6). They mirror withLock's O_EXCL create + remove.
func createLock(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
}

func removeLock(path string, f *os.File) {
	if f != nil {
		f.Close()
	}
	os.Remove(path)
}

// ── Task 5: Kahn walk ──────────────────────────────────────────────────────

func TestExecuteRunsChainInTopoOrderAndCompletes(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0), step(2, 1)}}
	dir := setupDispatch(t, ir)
	f := &fakeRunner{script: map[int]NodeResult{}}
	if err := Execute(dir, f.run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 2, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 3 || f.seen[0] != 0 || f.seen[1] != 1 || f.seen[2] != 2 {
		t.Fatalf("run order = %v, want [0 1 2]", f.seen)
	}
	st, _ := Load(dir)
	if st.State != "done" {
		t.Fatalf("state = %q, want done", st.State)
	}
	if st.ResultRef == "" {
		t.Fatal("terminal result_ref must be set on done")
	}
	if !contains(f.prompts[2], "out-1") {
		t.Fatalf("step 2 prompt missing dep context: %q", f.prompts[2])
	}
	if contains(f.prompts[0], "<context") {
		t.Fatalf("root prompt must have no context block: %q", f.prompts[0])
	}
}

func TestExecuteRelegatedNodeDefersNotFails(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	f := &fakeRunner{script: map[int]NodeResult{
		0: {OutcomeClass: "deferred", ResumeAt: t0.Add(time.Hour)},
	}}
	_ = Execute(dir, f.run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	st, _ := Load(dir)
	if st.State != "deferred" {
		t.Fatalf("a relegated root must set dispatch deferred, got %q", st.State)
	}
	if st.StepStatus[1].OutcomeClass != "" {
		t.Fatal("a step whose dep deferred must NOT run")
	}
	if st.StepStatus[0].ResumeAt == nil {
		t.Fatal("a relegated step must record resume_at")
	}
}

func TestExecuteSerializesSameLaneGLM(t *testing.T) {
	// two glm-hinted roots the executor must not co-dispatch (§4.4).
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "codex", Deps: []int{}},
		{ID: 2, Instruction: "j", Deps: []int{0, 1}},
	}}
	// NOTE: 0 and 1 differ so Validate passes; to actually exercise two GLM in a
	// wave we resolve BOTH 0 and 1 to glm via the Resolve seam (S3R-3a) below.
	dir := setupDispatch(t, ir)
	var inFlightGLM, maxGLM int
	var mu sync.Mutex
	f := &fakeRunner{script: map[int]NodeResult{}}
	run := func(s Step, p string, a int) NodeResult {
		if s.LaneHint == "glm" {
			mu.Lock()
			inFlightGLM++
			if inFlightGLM > maxGLM {
				maxGLM = inFlightGLM
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inFlightGLM--
			mu.Unlock()
		}
		return f.run(s, p, a)
	}
	_ = Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 3, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if maxGLM > 1 {
		t.Fatalf("GLM steps must serialize, saw %d in flight", maxGLM)
	}
}

// ── S3R-3(a): resolved-lane serialization in pickWave ───────────────────────

// Two empty-hint roots that RESOLVE to the same lane must NOT be co-dispatched.
// This is the router-decided case the plain LaneHint guard misses: without the
// Resolve seam both would fan into a single glmlane.Pace LockWait stall.
func TestPickWaveSerializesOnResolvedLaneNotHint(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{
		step(0), // empty hint
		step(1), // empty hint — but both resolve to "glm"
		step(2, 0, 1),
	}}
	dir := setupDispatch(t, ir)
	var inFlight, maxInFlight int
	var mu sync.Mutex
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 || s.ID == 1 {
			mu.Lock()
			inFlight++
			if inFlight > maxInFlight {
				maxInFlight = inFlight
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(s.ID), Lane: "glm"}
	}
	// Resolve seam: both empty-hint roots resolve to glm (router-decided collision).
	resolve := func(s Step) string {
		if s.ID == 0 || s.ID == 1 {
			return "glm"
		}
		return s.LaneHint
	}
	_ = Execute(dir, run, resolve, noAlt, ExecConfig{MaxConcurrency: 3, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if maxInFlight > 1 {
		t.Fatalf("two nodes RESOLVING to glm must serialize on the resolved lane, saw %d in flight", maxInFlight)
	}
	st, _ := Load(dir)
	if st.State != "done" {
		t.Fatalf("both resolved-glm roots + judge should complete, got %q", st.State)
	}
}

// ── S3R-2: finalize precedence (failed > blocked > deferred > done) ─────────

// (i) A non-terminal branch hard-fails → dispatch is FAILED, not done, even if
// the terminal sink would otherwise be reachable.
func TestFinalizeFailedBeatsDoneWhenNonTerminalHardFails(t *testing.T) {
	// fan-out-judge: 0 and 1 feed terminal judge 2. Leaf 0 hard-fails with no
	// re-lane; the judge can never run (dep 0 not ok) → the DAG is failed.
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "codex", Deps: []int{}},
		{ID: 2, Instruction: "judge", Deps: []int{0, 1}},
	}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 {
			return NodeResult{OutcomeClass: "api_error", ResultContent: "boom", Lane: "glm"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(s.ID), Lane: s.LaneHint}
	}
	_ = Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 2, ReLaneMaxDepth: 0}, func() time.Time { return t0 }, nil)
	st, _ := Load(dir)
	if st.State != "failed" {
		t.Fatalf("a hard-failed non-terminal branch must fail the dispatch, got %q", st.State)
	}
	if st.StepStatus[0].ResultRef == "" {
		t.Fatal("the failed step's partial result_ref must be referenced honestly")
	}
}

// (ii) A mixed DAG where one branch DEFERS and another HARD-FAILS must be
// FAILED, never deferred (failed outranks deferred in the precedence).
func TestFinalizeFailedBeatsDeferredInMixedDAG(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "glm", Deps: []int{}},   // defers
		{ID: 1, Instruction: "b", LaneHint: "codex", Deps: []int{}}, // hard-fails
		{ID: 2, Instruction: "judge", Deps: []int{0, 1}},
	}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		switch s.ID {
		case 0:
			return NodeResult{OutcomeClass: "deferred", ResumeAt: t0.Add(time.Hour), Lane: "glm"}
		case 1:
			return NodeResult{OutcomeClass: "api_error", ResultContent: "boom", Lane: "codex"}
		}
		return NodeResult{OutcomeClass: "ok", Lane: s.LaneHint}
	}
	_ = Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 2, ReLaneMaxDepth: 0}, func() time.Time { return t0 }, nil)
	st, _ := Load(dir)
	if st.State != "failed" {
		t.Fatalf("failed must outrank deferred in a mixed DAG, got %q", st.State)
	}
}

// A pure relegation (a dep defers, nothing hard-fails) is still deferred, not
// failed — the precedence only escalates to failed on a genuine hard-fail.
func TestFinalizePureRelegationIsDeferred(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 {
			return NodeResult{OutcomeClass: "deferred", ResumeAt: t0.Add(time.Hour)}
		}
		return NodeResult{OutcomeClass: "ok"}
	}
	_ = Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	st, _ := Load(dir)
	if st.State != "deferred" {
		t.Fatalf("a pure relegation must be deferred (resumable), got %q", st.State)
	}
}

// F2b (S3R-1 / R10): persistOutcome must RECONCILE ss.Lane to the lane the node
// ACTUALLY ran on (NodeResult.Lane, read from the receipt) — not leave the
// mark-started resolve() guess. If the actual lane ever diverges from the guess
// (e.g. a router re-pick between mark-started and dispatch), recovery's
// idempotency must key on the TRUTH. Here resolve() guesses "glm" at mark-started
// but the node actually ran on "local"; ss.Lane must end up "local".
func TestPersistOutcomeReconcilesLaneToActual(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0)}}
	dir := setupDispatch(t, ir)
	// mark-started guess = "glm"; the node actually ran on "local".
	guessResolve := func(s Step) string { return "glm" }
	run := func(s Step, p string, a int) NodeResult {
		return NodeResult{OutcomeClass: "ok", ResultContent: "done", Lane: "local"}
	}
	if err := Execute(dir, run, guessResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(dir)
	if st.StepStatus[0].Lane != "local" {
		t.Fatalf("ss.Lane must be reconciled to the lane the node ACTUALLY ran on (r.Lane=local), got %q — recovery would key idempotency on the stale mark-started guess", st.StepStatus[0].Lane)
	}
}

// F3 (S3R-2 blocked precedence): finalize must PRESERVE a recovery-only "blocked"
// step class — persisting state.State="blocked" (blocked > failed > deferred >
// done). Previously classifyOutcome("blocked") fell through to kindHardFail and
// finalize mislabeled the dispatch "failed", losing the R10 block signal on disk
// while blocked_step stayed surfaced (an inconsistent status lie). Guards the
// finalize path directly, independent of Resume.
func TestFinalizeBlockedOutranksFailed(t *testing.T) {
	// Step 0 is blocked (recovery marked it so); step 1 hard-failed. blocked must
	// win the precedence — the dispatch is blocked, not failed.
	ir := IR{Goal: "g", Steps: []Step{
		{ID: 0, Instruction: "a", LaneHint: "claude", Deps: []int{}},
		{ID: 1, Instruction: "b", LaneHint: "codex", Deps: []int{}},
	}}
	dir := setupDispatch(t, ir)
	fin := t0
	if err := Mutate(dir, func(s *State) {
		s.State = "running"
		s.StepStatus[0].OutcomeClass = "blocked"
		s.StepStatus[0].TS = &fin
		s.StepStatus[1].OutcomeClass = "api_error"
		s.StepStatus[1].ResultRef = "ref1"
		s.StepStatus[1].TS = &fin
	}, t0); err != nil {
		t.Fatal(err)
	}
	if err := finalize(dir, func() time.Time { return t0 }); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	st, _ := Load(dir)
	if st.State != "blocked" {
		t.Fatalf("finalize must persist blocked (blocked > failed), got %q", st.State)
	}
}

// ── Task 6 / S3R-3(b) / S3R-8: bounded re-lane ─────────────────────────────

func TestReLaneRetriesOnceThenSucceeds(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	attempts := map[int]int{}
	var amu sync.Mutex
	run := func(s Step, p string, a int) NodeResult {
		amu.Lock()
		attempts[s.ID]++
		amu.Unlock()
		if s.ID == 0 && a == 0 {
			return NodeResult{OutcomeClass: "api_error", ResultContent: "boom", Lane: "claude"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "ok-" + itoa(s.ID), Lane: "codex"}
	}
	alt := func(s Step, excludeLane string) (string, string, string, bool) { return "codex", "gpt-x", "", true }
	if err := Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(dir)
	if st.State != "done" {
		t.Fatalf("re-laned chain must complete, got %q", st.State)
	}
	if attempts[0] != 2 {
		t.Fatalf("step 0 must be attempted twice (original + one re-lane), got %d", attempts[0])
	}
	if st.StepStatus[0].Attempt != 1 {
		t.Fatalf("re-laned step must record Attempt=1, got %d", st.StepStatus[0].Attempt)
	}
}

func TestReLaneRespectsMaxDepthOne(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	attempts := 0
	var amu sync.Mutex
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 {
			amu.Lock()
			attempts++
			amu.Unlock()
			return NodeResult{OutcomeClass: "api_error", Lane: "claude"}
		}
		return NodeResult{OutcomeClass: "ok"}
	}
	alt := func(s Step, excludeLane string) (string, string, string, bool) { return "codex", "gpt-x", "", true }
	_ = Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if attempts != 2 {
		t.Fatalf("max_depth=1 → 2 attempts total, got %d", attempts)
	}
	st, _ := Load(dir)
	if st.State != "failed" {
		t.Fatalf("exhausted re-lane must fail honestly, got %q", st.State)
	}
}

// S3R-3(b): the re-lane must EXCLUDE the resolved lane that ACTUALLY RAN AND
// FAILED (NodeResult.Lane), not the empty hint — so a router-decided node never
// retries the just-failed lane. We assert alt() is called with the failed lane.
func TestReLaneExcludesTheResolvedFailedLane(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	var gotExclude string
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 && a == 0 {
			// empty LaneHint, but the router RESOLVED it to glm and it failed there.
			return NodeResult{OutcomeClass: "api_error", Lane: "glm"}
		}
		return NodeResult{OutcomeClass: "ok"}
	}
	alt := func(s Step, excludeLane string) (string, string, string, bool) {
		gotExclude = excludeLane
		return "claude", "opus", "", true
	}
	_ = Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if gotExclude != "glm" {
		t.Fatalf("re-lane must exclude the RESOLVED failed lane (glm), got exclude=%q", gotExclude)
	}
}

// S3R-3(c): the replan deviation reason must NAME the quality tradeoff, never a
// bare "re-laned". We read the journal for the replan event's detail.
func TestReLaneDeviationReasonNamesTradeoff(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 && a == 0 {
			return NodeResult{OutcomeClass: "api_error", Lane: "claude"}
		}
		return NodeResult{OutcomeClass: "ok"}
	}
	alt := func(s Step, excludeLane string) (string, string, string, bool) { return "glm", "glm-4", "", true }
	_ = Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	entries := readJournal(t, dir)
	var replanDetail string
	var sawReplan bool
	for _, e := range entries {
		if e.Event == "replan" {
			sawReplan = true
			replanDetail = e.Detail
		}
	}
	if !sawReplan {
		t.Fatal("a re-lane must journal a replan event")
	}
	// names from→to AND the quality tradeoff, not a bare "re-laned".
	if replanDetail == "re-laned" || replanDetail == "" {
		t.Fatalf("replan detail must name the quality tradeoff, got %q", replanDetail)
	}
	for _, want := range []string{"claude", "glm", "quality"} {
		if !contains(replanDetail, want) {
			t.Fatalf("replan detail %q must mention %q (from→to + tradeoff)", replanDetail, want)
		}
	}
}

// ── S3R-8: honest failure taxonomy ─────────────────────────────────────────

// exit-4 / notional-guard-tripped = ok-with-warning: the call SUCCEEDED, so it
// satisfies deps and must NEVER trigger a re-lane (re-laning re-spends).
func TestTaxonomyNotionalOKDoesNotReLaneAndSatisfiesDeps(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	attempts := map[int]int{}
	run := func(s Step, p string, a int) NodeResult {
		attempts[s.ID]++
		if s.ID == 0 {
			return NodeResult{OutcomeClass: "ok_notional", ResultContent: "expensive-but-done", Lane: "claude"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "downstream", Lane: "codex"}
	}
	alt := func(s Step, excludeLane string) (string, string, string, bool) { return "codex", "x", "", true }
	_ = Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if attempts[0] != 1 {
		t.Fatalf("exit-4 ok_notional must NOT re-lane, got %d attempts on step 0", attempts[0])
	}
	st, _ := Load(dir)
	if st.State != "done" {
		t.Fatalf("ok_notional satisfies deps → downstream runs → done, got %q", st.State)
	}
	if attempts[1] != 1 {
		t.Fatal("downstream dep of an ok_notional step must run (dep satisfied)")
	}
}

// incomplete / rate_limit = relegate or retry-SAME-lane, NOT a different-lane
// re-lane. We assert alt() is NEVER consulted for these classes.
func TestTaxonomyIncompleteAndRateLimitDoNotDifferentLaneReLane(t *testing.T) {
	for _, cls := range []string{"incomplete", "rate_limit"} {
		ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
		dir := setupDispatch(t, ir)
		altCalled := false
		run := func(s Step, p string, a int) NodeResult {
			if s.ID == 0 {
				return NodeResult{OutcomeClass: cls, Lane: "claude"}
			}
			return NodeResult{OutcomeClass: "ok"}
		}
		alt := func(s Step, excludeLane string) (string, string, string, bool) {
			altCalled = true
			return "codex", "x", "", true
		}
		_ = Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
		if altCalled {
			t.Fatalf("%s must NOT trigger a different-lane re-lane (relegate/retry-same-lane only)", cls)
		}
	}
}

// api_error (a genuine hard-fail) DOES trigger the one different-lane re-lane.
func TestTaxonomyApiErrorDoesReLane(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	altCalled := false
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 && a == 0 {
			return NodeResult{OutcomeClass: "api_error", Lane: "claude"}
		}
		return NodeResult{OutcomeClass: "ok"}
	}
	alt := func(s Step, excludeLane string) (string, string, string, bool) {
		altCalled = true
		return "codex", "x", "", true
	}
	_ = Execute(dir, run, hintResolve, alt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if !altCalled {
		t.Fatal("api_error is a genuine hard-fail and MUST be re-lane-eligible")
	}
}

// ── S3R-6: a critical Mutate error must NOT be swallowed ────────────────────

// If a critical state write fails (lock busy the whole bounded window), Execute
// must surface an error rather than silently completing with corrupt state.
func TestExecuteSurfacesCriticalMutateError(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0)}}
	dir := setupDispatch(t, ir)

	// Shrink the lock wait so a held lock surfaces fast.
	savedWait, savedStale := lockWait, lockStale
	lockWait, lockStale = 100*time.Millisecond, 30*time.Second
	defer func() { lockWait, lockStale = savedWait, savedStale }()

	// Hold the lock with a fresh mtime so every Mutate inside Execute is lock-busy.
	lock := statePath(dir) + ".lock"
	lf, err := createLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	defer removeLock(lock, lf)

	f := &fakeRunner{script: map[int]NodeResult{}}
	err = Execute(dir, f.run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)
	if err == nil {
		t.Fatal("a critical Mutate failure (lock busy) must be surfaced, never swallowed")
	}
}

// ── F1b (R10): per-wave supervisor-lease guard ─────────────────────────────

// F1b-1: the supervisor wake-up race. Supervisor A dispatches wave-0 (step0),
// then the laptop sleeps mid-dispatch; on wake a competitor B STEALS the stale
// lease and dispatches step1 (cloud). A also wakes and continues its Execute
// loop — it must NOT dispatch step1 AGAIN (that is the R10 double-spend: two
// cloud windows for one step). The per-wave guard makes A re-check lease
// ownership BEFORE dispatching wave-1; seeing the foreign pid, it steps aside
// with a clean nil (an abort, not an error, not a finalize).
//
// The steal is on-disk and faithful to the prod mechanism: the fake runner, the
// instant it records step0, writes a FOREIGN SupervisorPID via Mutate. The guard
// reads on-disk SupervisorPID and compares to A's pid — exactly what the prod
// guard closure in driveDispatch does. So by the time Execute computes wave-1 and
// calls guard(), guard() reads the foreign pid and returns false.
func TestExecute_AbortsWaveWhenLeaseStolen(t *testing.T) {
	const aPID = 4242
	const bPID = 9999 // the thief

	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	// A holds the lease when the drive starts.
	if err := Mutate(dir, func(s *State) { s.SupervisorPID = aPID }, t0); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var dispatched []int
	run := func(s Step, p string, a int) NodeResult {
		mu.Lock()
		dispatched = append(dispatched, s.ID)
		mu.Unlock()
		if s.ID == 0 {
			// laptop-sleep window: B steals the stale lease mid-dispatch.
			if err := Mutate(dir, func(st *State) { st.SupervisorPID = bPID }, t0); err != nil {
				t.Errorf("steal mutate: %v", err)
			}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(s.ID), Lane: s.LaneHint}
	}
	// The prod-shaped guard: a state predicate, still ours iff the state records A.
	guard := func(s *State) bool { return s.SupervisorPID == aPID }

	if err := Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, guard); err != nil {
		t.Fatalf("stepping aside for a competing supervisor is a clean abort, not an error: %v", err)
	}

	mu.Lock()
	got := append([]int(nil), dispatched...)
	mu.Unlock()
	for _, id := range got {
		if id == 1 {
			t.Fatalf("R10 double-spend: step1 was dispatched by A after the lease was stolen; dispatched=%v", got)
		}
	}
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("A should have dispatched ONLY step0 before stepping aside, dispatched=%v", got)
	}
	// The abort is NOT a finalize: the dispatch stays running (B owns it now), never
	// mislabeled done.
	st, _ := Load(dir)
	if st.State == "done" {
		t.Fatalf("a stepped-aside supervisor must not finalize the dispatch to done, got %q", st.State)
	}
}

// F1b-1 non-vacuity: the SAME steal scenario with guard=nil (current behavior /
// the bug) MUST dispatch step1 — proving the guard is load-bearing. Without the
// per-wave re-check, A blindly continues its Kahn walk and burns the second cloud
// window on step1. If this test ever fails (step1 NOT dispatched with a nil
// guard) the guard is not the thing protecting us and the passing test above is
// vacuous.
func TestExecute_WithoutGuardDoubleDispatchesStolenWave(t *testing.T) {
	const aPID = 4242
	const bPID = 9999

	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	if err := Mutate(dir, func(s *State) { s.SupervisorPID = aPID }, t0); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var dispatched []int
	run := func(s Step, p string, a int) NodeResult {
		mu.Lock()
		dispatched = append(dispatched, s.ID)
		mu.Unlock()
		if s.ID == 0 {
			if err := Mutate(dir, func(st *State) { st.SupervisorPID = bPID }, t0); err != nil {
				t.Errorf("steal mutate: %v", err)
			}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(s.ID), Lane: s.LaneHint}
	}

	// nil guard == current behavior: no per-wave re-check.
	_ = Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil)

	mu.Lock()
	got := append([]int(nil), dispatched...)
	mu.Unlock()
	sawStep1 := false
	for _, id := range got {
		if id == 1 {
			sawStep1 = true
		}
	}
	if !sawStep1 {
		t.Fatalf("non-vacuity: with a nil guard the walk MUST dispatch step1 (the bug); dispatched=%v — the guard test would be vacuous", got)
	}
}

// F1b-1 companion: a supervisor that RETAINS ownership (guard always true)
// dispatches the whole DAG normally and finalizes done — the guard must not
// spuriously abort the common no-steal path.
func TestExecute_RetainsOwnershipDispatchesNormally(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)

	var mu sync.Mutex
	var dispatched []int
	run := func(s Step, p string, a int) NodeResult {
		mu.Lock()
		dispatched = append(dispatched, s.ID)
		mu.Unlock()
		return NodeResult{OutcomeClass: "ok", ResultContent: "out-" + itoa(s.ID), Lane: s.LaneHint}
	}
	guard := func(*State) bool { return true } // owner retained the whole drive

	if err := Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, guard); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	got := append([]int(nil), dispatched...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 0 || got[1] != 1 {
		t.Fatalf("an owner that keeps the lease must dispatch both steps in order, dispatched=%v", got)
	}
	st, _ := Load(dir)
	if st.State != "done" {
		t.Fatalf("a retained-ownership drive must finalize done, got %q", st.State)
	}
}

// Tier-2 verify flag (make-verify-count): a non-affirmative terminal verifier
// verdict FLAGS the dispatch (State.VerifyFlag) and surfaces the verdict+reason —
// WITHOUT changing outcome_class or the terminal state (the tier-1 receipt gate
// still governs; a weak local judge must never hard-reject a completed answer).
// This is the signal solo cannot give: "completed, but the independent check
// disagreed."
func TestExecute_FlagsNonAffirmativeVerifyVerdict(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}} // worker(0) → verifier(1)
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 1 {
			return NodeResult{OutcomeClass: "ok", ResultContent: "v", Lane: s.LaneHint,
				VerifyVerdict: "no", VerifyReason: "midpoint is not overflow-safe"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "w", Lane: s.LaneHint}
	}
	if err := Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(dir)
	if st.State != "done" {
		t.Fatalf("advisory verify must NOT change the terminal state (tier-1 governs); got %q, want done", st.State)
	}
	if !st.VerifyFlag {
		t.Fatalf("a non-affirmative verify verdict must FLAG the dispatch (VerifyFlag=true)")
	}
	if st.VerifyVerdict != "no" || st.VerifyReason == "" {
		t.Fatalf("dispatch must carry the verdict+reason, got %q / %q", st.VerifyVerdict, st.VerifyReason)
	}
}

// An affirmative verdict surfaces the verdict but does NOT flag; a solo DAG with
// no verifier (no verdict) never flags.
func TestExecute_AffirmativeAndAbsentVerifyDoNotFlag(t *testing.T) {
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 1 {
			return NodeResult{OutcomeClass: "ok", ResultContent: "v", Lane: s.LaneHint, VerifyVerdict: "yes"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "w", Lane: s.LaneHint}
	}
	if err := Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(dir)
	if st.VerifyFlag {
		t.Fatalf("an affirmative verdict must NOT flag")
	}
	if st.VerifyVerdict != "yes" {
		t.Fatalf("verdict should still be surfaced as yes, got %q", st.VerifyVerdict)
	}

	// solo: single node, no verifier, no verdict → never flags.
	ir2 := IR{Goal: "g", Steps: []Step{step(0)}}
	dir2 := setupDispatch(t, ir2)
	runSolo := func(s Step, p string, a int) NodeResult {
		return NodeResult{OutcomeClass: "ok", ResultContent: "w", Lane: s.LaneHint}
	}
	if err := Execute(dir2, runSolo, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	st2, _ := Load(dir2)
	if st2.VerifyFlag || st2.VerifyVerdict != "" {
		t.Fatalf("a solo DAG with no verifier must never flag, got flag=%v verdict=%q", st2.VerifyFlag, st2.VerifyVerdict)
	}
}

// Finding-2 fix (re-review): a verifier that is NOT the terminal sink (legal in an
// operator-authored custom IR) must still surface its verdict — finalize scans ALL
// steps in IR order, not only the sink.
func TestExecute_SurfacesMidChainVerifierVerdict(t *testing.T) {
	// verifier(0) → sink worker(1); the SINK carries no verdict.
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	dir := setupDispatch(t, ir)
	run := func(s Step, p string, a int) NodeResult {
		if s.ID == 0 {
			return NodeResult{OutcomeClass: "ok", ResultContent: "v", Lane: s.LaneHint,
				VerifyVerdict: "no", VerifyReason: "mid-chain reject"}
		}
		return NodeResult{OutcomeClass: "ok", ResultContent: "w", Lane: s.LaneHint}
	}
	if err := Execute(dir, run, hintResolve, noAlt, ExecConfig{MaxConcurrency: 1, ReLaneMaxDepth: 1}, func() time.Time { return t0 }, nil); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(dir)
	if !st.VerifyFlag || st.VerifyVerdict != "no" || st.VerifyReason == "" {
		t.Fatalf("a mid-chain verifier's verdict must still flag+surface, got flag=%v verdict=%q reason=%q", st.VerifyFlag, st.VerifyVerdict, st.VerifyReason)
	}
}
