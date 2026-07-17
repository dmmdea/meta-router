package strategy

import (
	"fmt"
	"sync"
	"time"
)

// ── Seams (fake-injectable here; prod-wired in Group E) ─────────────────────

// NodeResult is the classified return of one node run, mapped from doRun in
// prod. Lane is the RESOLVED lane the node ACTUALLY ran on (S3R-3): the re-lane
// excludes it so an escalation never retries the just-failed lane, even for a
// router-decided (empty-hint) node.
type NodeResult struct {
	OutcomeClass  string
	ResultContent string
	NotionalUSD   float64
	ResumeAt      time.Time
	Attempt       int
	Lane          string // S3R-3: the resolved lane this node ran on
	// VerifyVerdict / VerifyReason carry a verifier node's tier-2 JUDGMENT
	// ("yes"/"no"/"unsure" + reason), extracted from the result by the prod runner
	// for verifier-role nodes. Advisory: the executor records them but NEVER lets
	// them change OutcomeClass or the re-lane decision (tier-1 governs). finalize
	// flags the dispatch when the terminal verdict is non-affirmative. Empty for
	// non-verifier nodes.
	VerifyVerdict string
	VerifyReason  string
}

// NodeRunner runs ONE node and returns its classified result. prompt already
// carries the dep-context block. In prod it wraps doRun (Origin:"strategy") — it
// receives the Step (which carries Class) so the local two-door and the ONE
// receipt/node key on the DAG class, never a re-derived heuristic. In tests it
// is a fake. The runner writes the step receipt (via runOpts) — the executor
// NEVER appends receipts itself (Group B→C contract).
type NodeRunner func(step Step, prompt string, attempt int) NodeResult

// Resolve returns the lane a node WILL resolve to BEFORE dispatch (S3R-3a) so
// pickWave can serialize on the resolved lane, not just the explicit LaneHint.
// In prod it wraps computeRunRec/buildRouteDecision; in tests it is a fake. A
// node with an explicit LaneHint typically resolves to that hint.
type Resolve func(step Step) (lane string)

// Alternatives resolves the next Pareto runner-up lane for a re-lane, EXCLUDING
// excludeLane (S3R-3b: the resolved lane that actually ran and failed — never
// the empty hint). ok=false means no alternative → the failure stands. In prod
// it wraps computeRunRec with the failed lane masked; in tests it is a fake.
type Alternatives func(step Step, excludeLane string) (lane, model, effort string, ok bool)

type ExecConfig struct {
	MaxConcurrency int // default 2
	ReLaneMaxDepth int // default 1
}

// ── S3R-8: honest failure taxonomy ─────────────────────────────────────────

type outcomeKind int

const (
	// kindOK — the call succeeded; satisfies deps. Covers "ok" AND "ok_notional"
	// (exit-4 / notional-guard-tripped: the call SUCCEEDED but was costly →
	// ok-with-warning, NOT a hard fail; re-laning would re-spend, R10/R14).
	kindOK outcomeKind = iota
	// kindRelegate — deferred/rate_limit/incomplete: an out-of-turns /
	// rate-limited / relegated node is NOT a lane-quality failure. Relegate the
	// dispatch (deferred) or retry the SAME lane — never a DIFFERENT-lane re-lane.
	kindRelegate
	// kindHardFail — a genuine failure (refusal/api_error/parse_error/
	// empty_result/spawn_error): re-lane-eligible (the one max_depth=1 escalation
	// to a DIFFERENT lane). If no re-lane is left, it hard-fails the dispatch.
	kindHardFail
)

// classifyOutcome maps an outcome_class string to its taxonomy kind. Keeping the
// distinct classes UNFLATTENED is the S3R-8 contract: the re-lane decision
// depends on the distinction (exit-4 no-re-lane; incomplete/rate_limit
// no-different-lane-re-lane; api_error re-lanes).
func classifyOutcome(class string) outcomeKind {
	switch class {
	case "ok", "ok_notional":
		return kindOK
	case "deferred", "rate_limit", "incomplete":
		return kindRelegate
	default:
		// refusal, api_error, parse_error, empty_result, spawn_error, and any
		// unknown non-ok class → a genuine hard-fail, re-lane-eligible.
		return kindHardFail
	}
}

// depSatisfied reports whether a dep's recorded outcome satisfies a dependent's
// readiness — the tier-1 gate (S3R-8): only a SUCCEEDED call (ok / ok_notional)
// satisfies a dep. A deferred/relegated/hard-failed dep never satisfies, so its
// dependents never become ready (the DAG pauses, it does not spin).
func depSatisfied(class string) bool { return classifyOutcome(class) == kindOK }

// ── Execute: the Kahn walk ─────────────────────────────────────────────────

// Execute runs the Kahn walk over the DAG. It loads state fresh, marks running,
// then repeatedly: find the READY set (every dep satisfied per the tier-1 gate),
// compose a wave (concurrency cap + resolved-lane serialization, S3R-3a),
// dispatch it concurrently, persist each outcome — hard-failed steps with a
// re-lane budget and a resolvable alternative are RESET for one retry (Task 6),
// everything else recorded terminal — and loop until no node is runnable. It
// then classifies the dispatch terminal state (S3R-2 precedence). Critical state
// writes surface their error (S3R-6) rather than corrupting recovery silently.
func Execute(dir string, run NodeRunner, resolve Resolve, alt Alternatives, cfg ExecConfig, now func() time.Time, guard func(*State) bool) error {
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if resolve == nil {
		resolve = func(s Step) string { return s.LaneHint }
	}
	if alt == nil {
		alt = func(Step, string) (string, string, string, bool) { return "", "", "", false }
	}
	if _, err := Load(dir); err != nil {
		return err
	}
	// mark running — a critical write; surface its error (S3R-6).
	if err := Mutate(dir, func(s *State) { s.State = "running" }, now()); err != nil {
		return fmt.Errorf("mark running: %w", err)
	}

	for {
		// S3R cancel: a between-wave sentinel. A running node always finishes (no
		// hard mid-node kill in slice-3); we simply refuse to START a new wave and
		// mark the dispatch cancelled. Checked here, before readySet, so a cancel
		// requested during a wave is honored at the next wave boundary.
		if CancelRequested(dir) {
			if err := Mutate(dir, func(s *State) { s.State = "cancelled" }, now()); err != nil {
				return fmt.Errorf("mark cancelled: %w", err)
			}
			return nil
		}
		st, err := Load(dir)
		if err != nil {
			return err
		}
		ready := readySet(st)
		if len(ready) == 0 {
			break
		}
		wave := pickWave(ready, resolve, cfg.MaxConcurrency)

		// F1b (R10 double-spend guard): re-verify THIS supervisor still holds the
		// exclusion lease BEFORE dispatching (or even mark-starting) any node in the
		// wave. The lease is acquired once at drive start and never re-checked inside
		// the loop; a laptop-sleep >staleThreshold lets a --sweep/--resume reaper STEAL
		// the stale lease and dispatch the next wave. When this frozen supervisor wakes
		// and continues its Kahn walk, it would re-dispatch the SAME step the reaper
		// already dispatched → two cloud windows for one step. Re-checking ownership
		// here and stepping aside (clean nil, NOT an error, NOT a finalize — the
		// dispatch belongs to the new holder now) closes that race. A nil guard keeps
		// the exact prior behavior (tests / non-supervisor callers pass nil).
		// guard is a state PREDICATE (ownership check on the locked state), so the
		// wave-boundary check reuses the st already loaded this iteration (no second
		// Load to glitch). The truly atomic re-check happens INSIDE each node's
		// mark-started Mutate below, closing the guard→mark-started TOCTOU.
		if guard != nil && !guard(&st) {
			return nil
		}

		// Dispatch the wave concurrently. mark-started is a critical write.
		var wg sync.WaitGroup
		results := make(map[int]NodeResult, len(wave))
		var rmu sync.Mutex
		for _, s := range wave {
			s := s
			attempt := st.StepStatus[s.ID].Attempt
			ts := now()
			// S3R-6: record the RESOLVED lane at mark-started — the crash-recovery
			// signal for what lane this started step was going to spend on. Recovery
			// classifies idempotency on this resolved lane, never the empty LaneHint.
			lane := resolve(s)
			// R10 ATOMIC ownership check: fold the lease-ownership predicate INTO the
			// mark-started Mutate so decide-and-mark-started is ONE locked operation.
			// If this supervisor froze after the wave-boundary guard passed but before
			// this write, and a reaper stole the lease in that gap, guard(state) here —
			// under the state lock, on the freshest state — catches it and we do NOT
			// mark started (never dispatch a node a competing holder now owns). No
			// TOCTOU: there is no unlocked window between the check and the write.
			stolen := false
			if err := Mutate(dir, func(state *State) {
				if guard != nil && !guard(state) {
					stolen = true
					return
				}
				state.StepStatus[s.ID].StartedAt = &ts
				state.StepStatus[s.ID].Attempt = attempt
				state.StepStatus[s.ID].Lane = lane
			}, ts); err != nil {
				return fmt.Errorf("mark started step %d: %w", s.ID, err)
			}
			if stolen {
				return nil // lease lost atomically at mark-started — step aside for the new holder
			}
			if err := Journal(dir, "step_started", s.ID, ts); err != nil {
				return fmt.Errorf("journal step_started %d: %w", s.ID, err)
			}
			ctx, cerr := ResolveContext(dir, s.Deps, st.StepStatus)
			if cerr != nil {
				ctx = "" // fail-open: a missing dep artifact degrades to no context
			}
			// S3R-10a: a root/solo node with NO dep context gets the BARE instruction
			// — never a dangling "\n" separator — so the 1-node solo DAG is
			// byte-identical to the sync fast path (the committed solo==sync pin). A
			// node WITH context still gets instruction + "\n" + the fenced context block.
			prompt := s.Instruction
			if ctx != "" {
				prompt = s.Instruction + "\n" + ctx
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := run(s, prompt, attempt)
				rmu.Lock()
				results[s.ID] = r
				rmu.Unlock()
			}()
		}
		wg.Wait()

		// Persist this wave's outcomes (S3R-2 relegation, Task 6 re-lane).
		for _, s := range wave {
			r := results[s.ID]
			if err := persistOutcome(dir, s, r, cfg, alt, now); err != nil {
				return err
			}
		}
	}

	return finalize(dir, now)
}

// persistOutcome records one node's result. A genuine hard-fail (S3R-8
// kindHardFail) with a re-lane budget remaining AND a resolvable alternative
// (excluding the resolved lane that just failed, S3R-3b) is RESET for one retry:
// Attempt+1, outcome cleared so readySet re-picks it — the prod runner then
// consults alt at Attempt>0 to pin the new lane. Everything else (ok / relegate /
// exhausted hard-fail) is recorded terminal.
func persistOutcome(dir string, s Step, r NodeResult, cfg ExecConfig, alt Alternatives, now func() time.Time) error {
	fin := now()
	cur, err := Load(dir)
	if err != nil {
		return err
	}
	curAttempt := cur.StepStatus[s.ID].Attempt

	if classifyOutcome(r.OutcomeClass) == kindHardFail && curAttempt < cfg.ReLaneMaxDepth {
		// S3R-3b: exclude the resolved lane that ACTUALLY RAN AND FAILED, never
		// the empty hint. r.Lane is that lane.
		toLane, _, _, ok := alt(s, r.Lane)
		if ok {
			// S3R-3c / S3R-10b: the replan detail NAMES the from→to re-lane AND the
			// quality tradeoff — never a bare "re-laned".
			detail := fmt.Sprintf(
				"re-lane %s→%s: quality tradeoff (R14a); account-2 Claude preferred once modeled (R15)",
				r.Lane, toLane)
			if err := JournalDetail(dir, "replan", s.ID, detail, fin); err != nil {
				return fmt.Errorf("journal replan step %d: %w", s.ID, err)
			}
			// bounded reactive re-lane: bump Attempt, clear the started/outcome so
			// readySet re-picks it next wave on the alternative lane. The failed
			// attempt's receipt was already written by the runner; the retry writes a
			// SECOND receipt sharing dispatch_id+step_id with Attempt+1 (prod runner).
			if err := Mutate(dir, func(state *State) {
				ss := state.StepStatus[s.ID]
				ss.Attempt = curAttempt + 1
				ss.OutcomeClass = ""
				ss.StartedAt = nil
			}, fin); err != nil {
				return fmt.Errorf("reset for re-lane step %d: %w", s.ID, err)
			}
			return nil
		}
	}

	// Terminal for this step. Write the artifact (partial results are referenced
	// honestly even on a failure). WriteArtifact errors are surfaced — a lost
	// artifact corrupts the idempotency/recovery evidence.
	ref, aerr := WriteArtifact(dir, Artifact{StepID: s.ID, OutcomeClass: r.OutcomeClass, Content: r.ResultContent})
	if aerr != nil {
		return fmt.Errorf("write artifact step %d: %w", s.ID, aerr)
	}
	if err := Mutate(dir, func(state *State) {
		ss := state.StepStatus[s.ID]
		ss.OutcomeClass = r.OutcomeClass
		ss.ResultRef = ref
		ss.TS = &fin
		// F2b (S3R-1 / R10): reconcile ss.Lane to the lane the node ACTUALLY ran on
		// (r.Lane, read from the receipt) so recovery's idempotency keys on the TRUTH
		// even if the actual lane diverged from the mark-started resolve() guess. Only
		// overwrite when the runner reported a real lane — never wipe a good
		// mark-started lane with an empty one.
		if r.Lane != "" {
			ss.Lane = r.Lane
		}
		if !r.ResumeAt.IsZero() {
			ra := r.ResumeAt
			ss.ResumeAt = &ra
		}
		// Tier-2 (advisory): record the verifier's judgment on the step. NEVER touches
		// OutcomeClass — finalize reads this to flag the dispatch, but the tier-1 gate
		// above already decided ok/relegate/hard-fail.
		if r.VerifyVerdict != "" {
			ss.VerifyVerdict = r.VerifyVerdict
			ss.VerifyReason = r.VerifyReason
		}
	}, fin); err != nil {
		return fmt.Errorf("record outcome step %d: %w", s.ID, err)
	}
	if err := Journal(dir, "step_finished", s.ID, fin); err != nil {
		return fmt.Errorf("journal step_finished %d: %w", s.ID, err)
	}
	return nil
}

// readySet is every step not yet run whose deps ALL satisfy the tier-1 gate
// (depSatisfied). A step is skipped once it has a recorded outcome (terminal for
// this attempt). A dep that is deferred/failed never satisfies → its dependents
// never become ready and the DAG PAUSES (it does not spin).
func readySet(st State) []Step {
	var out []Step
	for _, s := range st.IR.Steps {
		ss := st.StepStatus[s.ID]
		if ss != nil && ss.OutcomeClass != "" {
			continue
		}
		ok := true
		for _, d := range s.Deps {
			ds := st.StepStatus[d]
			if ds == nil || !depSatisfied(ds.OutcomeClass) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// pickWave caps the ready set at MaxConcurrency and serializes RESOLVED-lane
// collisions (S3R-3a): a resolved lane may appear at most once per wave, so two
// router-decided nodes that resolve to the same lane (e.g. two glm nodes) are
// NOT co-dispatched into a single glmlane.Pace LockWait stall inside wg.Wait().
// The rest wait for the next wave. Nodes with an empty resolved lane (unknown /
// free local) never collide — each gets its own slot. Deterministic: ready is
// already in IR order.
func pickWave(ready []Step, resolve Resolve, cap int) []Step {
	var wave []Step
	usedLane := map[string]bool{}
	for _, s := range ready {
		if len(wave) >= cap {
			break
		}
		lane := resolve(s)
		if lane != "" {
			if usedLane[lane] {
				continue // defer this resolved-lane collision to a later wave
			}
			usedLane[lane] = true
		}
		wave = append(wave, s)
	}
	return wave
}

// finalize sets the dispatch terminal state with the S3R-2 + F3 precedence:
//
//	blocked > failed > deferred > done
//
// (blocked outranks failed: an operator-gated cloud-spend block is the R10 signal
// that must survive on disk, so a DAG with both a blocked and a hard-failed step
// finalizes to "blocked", not "failed".)
//
// A dispatch is FAILED if ANY step hard-failed (a terminal kindHardFail with no
// retry left) — NOT judged purely by the last step; a hard-failed non-terminal
// branch fails the whole dispatch, and failed outranks a concurrent deferral. It
// is DEFERRED if no step hard-failed but a step relegated (resumable). It is
// DONE only when the terminal sink step is ok AND no step hard-failed; result_ref
// is the terminal artifact. A failed dispatch references the partial result_ref
// honestly (the terminal step's artifact if it produced one).
func finalize(dir string, now func() time.Time) error {
	st, err := Load(dir)
	if err != nil {
		return err
	}
	last := st.IR.Steps[len(st.IR.Steps)-1]
	lastSS := st.StepStatus[last.ID]

	anyBlocked := false
	anyHardFailed := false
	anyDeferred := false
	for _, s := range st.IR.Steps {
		ss := st.StepStatus[s.ID]
		if ss == nil || ss.OutcomeClass == "" {
			continue // never terminally recorded — not a hard-fail signal here
		}
		// F3 (S3R-2): "blocked" is a recovery-only TERMINAL state, not a
		// classifyOutcome kind. It must be PRESERVED (never treated as a
		// kindHardFail default) so a resume of a blocked dispatch persists
		// state.State="blocked" on disk — never corrupts it to "failed".
		if ss.OutcomeClass == "blocked" {
			anyBlocked = true
			continue
		}
		switch classifyOutcome(ss.OutcomeClass) {
		case kindHardFail:
			anyHardFailed = true
		case kindRelegate:
			anyDeferred = true
		}
	}

	state := "failed"
	resultRef := ""
	switch {
	case anyBlocked:
		// F3 (S3R-2): blocked outranks failed/deferred/done — an operator-gated
		// cloud-spend step that could not be proven clean (R10) stops the dispatch;
		// persist "blocked" so strategy_status reports the truth, consistently with
		// the surfaced blocked_step.
		state = "blocked"
		if lastSS != nil {
			resultRef = lastSS.ResultRef // honest partial
		}
	case anyHardFailed:
		// failed outranks everything: any terminal hard-fail fails the dispatch.
		state = "failed"
		if lastSS != nil {
			resultRef = lastSS.ResultRef // honest partial
		}
	case lastSS != nil && depSatisfied(lastSS.OutcomeClass):
		// done ONLY when the terminal sink is ok AND nothing hard-failed.
		state, resultRef = "done", lastSS.ResultRef
	case anyDeferred:
		state = "deferred"
	default:
		// The terminal never reached ok and nothing hard-failed or deferred — a
		// pause with an unsatisfiable dep. Report failed with the honest partial.
		state = "failed"
		if lastSS != nil {
			resultRef = lastSS.ResultRef
		}
	}

	// Tier-2 verify surface (advisory — NEVER a state change; the state above is
	// final): the terminal-most verifier's judgment, FLAGGED when non-affirmative.
	// Scan ALL steps in IR order (not just the sink) so a verifier that is NOT the
	// terminal sink — legal in an operator-authored custom IR — still surfaces; the
	// last (highest-IR-order) verdict wins as the authoritative final judgment. Empty
	// for a DAG with no verifier (e.g. solo) → no flag. A flagged dispatch still
	// reports its tier-1 state (done/failed); the flag is the independent-check signal
	// solo cannot give — "completed, but the check disagreed."
	verifyFlag := false
	var vVerdict, vReason string
	for _, s := range st.IR.Steps {
		ss := st.StepStatus[s.ID]
		if ss == nil || ss.VerifyVerdict == "" {
			continue
		}
		vVerdict, vReason = ss.VerifyVerdict, ss.VerifyReason
		verifyFlag = vVerdict != "yes"
	}

	if err := Mutate(dir, func(s *State) {
		s.State = state
		s.ResultRef = resultRef
		s.VerifyFlag = verifyFlag
		s.VerifyVerdict = vVerdict
		s.VerifyReason = vReason
	}, now()); err != nil {
		return fmt.Errorf("finalize dispatch: %w", err)
	}
	return nil
}
