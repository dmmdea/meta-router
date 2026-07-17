package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/jitter"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	"github.com/dmmdea/meta-router/internal/orch/strategy"
)

// jsonMarshalIndent is the status marshaller (indented for human-readable state
// surfaces). Errors are impossible for the map shapes here; the caller ignores them.
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// heartbeatEvery is how often the live supervisor refreshes state.json's
// HeartbeatAt (S3R-7). Mirrors glmlane.Pace's 30s discipline.
const heartbeatEvery = 30 * time.Second

// staleThreshold is when a running/working dispatch with no fresh heartbeat is
// treated as a dead supervisor — 3× the heartbeat interval (mirrors glmlane's
// "3 missed beats" LockStale), so a live but slow supervisor is never reaped.
const staleThreshold = 3 * heartbeatEvery

// nodeDispatch is the doRun seam. Production wires it to doRun; tests inject a
// fake so the supervisor loop is exercised without a live cloud dispatch. It has
// the exact doRun signature.
var nodeDispatch = doRun

// nodeClassFromExit maps a doRun exit code to a strategy NodeResult outcome
// class, honoring S3R-8's honest taxonomy:
//
//	0 → "ok"           the call succeeded, gates downstream deps.
//	3 → "deferred"     relegation (resume_at valid) — parks, no re-lane.
//	4 → "ok_notional"  notional guard tripped: the call SUCCEEDED but was costly.
//	                   ok-with-warning, NOT a hard fail — re-laning would re-spend
//	                   (R10/R14). classifyOutcome treats ok_notional as kindOK.
//	5 → the REAL outcome_class from the run receipt (refusal/api_error/parse_error
//	    /empty_result/rate_limit/incomplete) — NEVER flattened to api_error, so the
//	    executor's re-lane vs relegate-same-lane decision keys on the true class.
//	other (1) → "config_error"  a bad-args config error (kindHardFail).
//
// receiptClass is the outcome_class doRun stamped on the ONE receipt it wrote
// (empty when unavailable); it is only consulted for exit 5.
func nodeClassFromExit(code int, receiptClass string) string {
	switch code {
	case 0:
		return "ok"
	case exitDeferred:
		return "deferred"
	case exitNotional:
		return "ok_notional" // S3R-8: exit-4 = ok-with-warning, NOT a failure
	case exitNotOK:
		if receiptClass != "" {
			return receiptClass // S3R-8: keep the real class, never flatten
		}
		return "api_error" // no receipt to read → conservative hard-fail class
	default:
		return "config_error"
	}
}

// receiptForStep returns the resolved lane + real outcome_class doRun stamped on
// the ONE receipt it wrote for this dispatch:step:attempt (S3R-4 says doRun
// writes exactly one; S3R-3b needs the RESOLVED lane for NodeResult.Lane). The
// LAST matching line wins (freshest). found=false when no receipt exists yet.
func receiptForStep(dispatchID string, stepID, attempt int) (lane, class string, found bool) {
	for _, r := range loadReceipts(dispatchPath()) {
		if r.DispatchID == dispatchID && r.StepID == stepID && r.Attempt == attempt {
			lane, class, found = r.Lane, r.OutcomeClass, true
		}
	}
	return lane, class, found
}

// prodNodeRunner drives one strategy node through the shared doRun core with
// Origin:"strategy". It writes NO extra receipt — the DAG identity threads INTO
// runOpts so doRun writes exactly ONE tagged receipt (S3R-4; a second line would
// drag system-wide CoveragePct down). NodeResult.Lane is the RESOLVED lane read
// back from that receipt (S3R-3b) so the executor's re-lane excludes the lane
// that actually ran. At attempt>0 the runner pins the alternative lane from alt,
// EXCLUDING the lane the prior attempt actually failed on (read from the prior
// receipt) — the max_depth=1 "escalate to a DIFFERENT lane" guarantee.
func prodNodeRunner(dispatchID string, alt strategy.Alternatives) strategy.NodeRunner {
	return func(step strategy.Step, prompt string, attempt int) strategy.NodeResult {
		lane := step.LaneHint
		model := step.ModelHint
		effort := step.EffortHint
		// Resolve the FULL router decision (lane + model + effort), not just the lane.
		// F2a fixed the LANE — a local-class node must hit runLocalLane's two-door, not
		// resolveLane's auto→local→CLOUD handoff (S3R-1 / R10) — by passing the explicit
		// resolved lane instead of "auto". But an explicit lane with NO model breaks
		// claude/codex/glm, which REQUIRE a pinned --model (config_error). "auto" used to
		// fill the model via resolveLane's adoption; an explicit lane does not. So fill
		// any hint a template node did not pin, keying the model to the chosen lane. A
		// broken/all-masked oracle → lane "auto" (doRun's relegation path). (Caught by the
		// S3R-9 live gate; the fake-based unit tests never require a model.)
		if lane == "" || model == "" {
			dec := computeRunRec(step.Class, step.Instruction, 0, false, time.Now().UTC())
			if lane == "" {
				if dec.Lane != "" {
					lane = dec.Lane
				} else {
					lane = "auto"
				}
			}
			// model/effort for the CHOSEN lane, from the decision (primary pick, else a
			// Pareto alternative).
			if dec.Lane == lane {
				if model == "" {
					model = dec.Model
				}
				if effort == "" {
					effort = dec.Effort
				}
			}
			if model == "" {
				for _, a := range dec.Alternatives {
					if a.Lane == lane {
						model = a.Model
						if effort == "" {
							effort = a.Effort
						}
						break
					}
				}
			}
			// An explicit cloud LaneHint the CLASS decision does not rank still has no
			// model here (e.g. cascade's glm verify-gate verifier — verify-gate has NO
			// glm row; fan-out-judge's codex worker on a codex-less class). Resolve the
			// lane's model straight from the rank table so the node dispatches WITH a
			// --model instead of config_error→re-lane, which would collapse the template's
			// deliberate different-lane design onto the wrong lane. Local needs no model.
			if model == "" && lane != "" && lane != "auto" && lane != "local" {
				if m, e := laneModelFromTable(step.Class, lane); m != "" {
					model = m
					if effort == "" {
						effort = e
					}
				}
			}
		}
		if attempt > 0 && alt != nil {
			// S3R-3b: exclude the lane the PRIOR attempt actually ran and failed on
			// (from its receipt), never the empty hint.
			failedLane, _, _ := receiptForStep(dispatchID, step.ID, attempt-1)
			if l, m, e, ok := alt(step, failedLane); ok {
				lane, model, effort = l, m, e
			}
		}

		var buf bytes.Buffer
		code, err := nodeDispatch(runOpts{
			Prompt:     prompt,
			Class:      step.Class,
			Lane:       lane,
			Model:      model,
			Effort:     effort,
			Origin:     "strategy",
			Desc:       step.Instruction,
			DispatchID: dispatchID,
			StepID:     step.ID,
			Deps:       step.Deps,
			Attempt:    attempt,
			Live:       true,
		}, &buf)

		// Read back the ONE receipt doRun wrote: the RESOLVED lane (S3R-3b) and the
		// real outcome_class (S3R-8, for exit 5).
		resolvedLane, receiptClass, _ := receiptForStep(dispatchID, step.ID, attempt)
		if resolvedLane == "" {
			resolvedLane = lane // fall back to the requested lane if no receipt surfaced
		}

		class := nodeClassFromExit(code, receiptClass)
		if err != nil {
			// A config_error (bad args) never reached the binary — exit 1 semantics.
			class = "config_error"
		}
		res := strategy.NodeResult{
			OutcomeClass:  class,
			ResultContent: buf.String(),
			Attempt:       attempt,
			Lane:          resolvedLane,
		}
		// Make-verify-count: extract the tier-2 judgment from a verifier node's
		// result so finalize can FLAG a non-affirmative verdict (advisory only —
		// class is untouched; the tier-1 gate still governs). Best-effort + fail-empty.
		if step.Role == "verifier" {
			res.VerifyVerdict, res.VerifyReason = parseVerifyVerdict(buf.String())
		}
		return res
	}
}

// parseVerifyVerdict extracts a verifier node's tier-2 judgment from its result
// content. The local triage door returns {ok, result:{decision, reason}, meta}
// (decision ∈ yes/no/unsure); a payload-only shape {decision, reason} is also
// accepted. A cloud re-lane fallback returns free-text with no structured decision
// → ("", ""), which finalize treats as "no verdict" (NO false flag). Best-effort +
// fail-empty: a parse miss NEVER fabricates a flag.
func parseVerifyVerdict(content string) (verdict, reason string) {
	// Full cascade result: {ok, result:{decision, reason}, meta}.
	var full struct {
		Result struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(content), &full) == nil && full.Result.Decision != "" {
		return strings.ToLower(strings.TrimSpace(full.Result.Decision)), full.Result.Reason
	}
	// Payload-only: {decision, reason}.
	var p struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if json.Unmarshal([]byte(content), &p) == nil && p.Decision != "" {
		return strings.ToLower(strings.TrimSpace(p.Decision)), p.Reason
	}
	return "", ""
}

// laneModelFromTable resolves the model+effort for an explicitly-pinned lane the
// class decision does NOT rank, straight from the router rank table. Prefers the
// SAME class's best-rank entry for the lane, else the lane's best-rank entry in any
// class (deterministic: classes sorted, lowest Rank wins, first-seen on tie). Returns
// ("","") when the lane appears nowhere → the caller leaves the model empty (doRun
// relegates rather than mis-dispatch). Load fails open to Seed().
func laneModelFromTable(class, lane string) (model, effort string) {
	tbl := router.Load(rankTablePath())
	best := func(entries []router.Entry) (m, e string, rank int) {
		rank = -1
		for _, en := range entries {
			if en.Lane == lane && (rank < 0 || en.Rank < rank) {
				m, e, rank = en.Model, en.Effort, en.Rank
			}
		}
		return m, e, rank
	}
	// 1) same class, best rank.
	if m, e, r := best(tbl[router.Class(class)]); r >= 0 {
		return m, e
	}
	// 2) cross-class fallback, deterministic over sorted class keys.
	classes := make([]string, 0, len(tbl))
	for c := range tbl {
		classes = append(classes, string(c))
	}
	sort.Strings(classes)
	bestRank := -1
	for _, c := range classes {
		if m, e, r := best(tbl[router.Class(c)]); r >= 0 && (bestRank < 0 || r < bestRank) {
			model, effort, bestRank = m, e, r
		}
	}
	return model, effort
}

// prodResolve returns the lane the router WOULD pick for a step BEFORE dispatch
// (S3R-3a) so pickWave serializes on the resolved lane, not just the explicit
// LaneHint. Cheap, deterministic, no LLM — wraps computeRunRec/buildRouteDecision.
// An explicit LaneHint short-circuits (the operator pinned it). A broken oracle
// fails open to "" (the executor gives an empty-lane node its own slot).
func prodResolve(step strategy.Step) string {
	if step.LaneHint != "" {
		return step.LaneHint
	}
	dec := computeRunRec(step.Class, step.Instruction, 0, false, time.Now().UTC())
	return dec.Lane
}

// prodAlternatives returns the next Pareto runner-up lane for a re-lane,
// EXCLUDING excludeLane — the resolved lane that actually ran and failed (S3R-3b).
// ok=false when no different lane is available → the failure stands. Wraps
// computeRunRec/buildRouteDecision with the failed lane masked.
func prodAlternatives(step strategy.Step, excludeLane string) (lane, model, effort string, ok bool) {
	dec := computeRunRec(step.Class, step.Instruction, 0, false, time.Now().UTC())
	// The primary recommendation is a candidate too (unless it IS the excluded
	// lane), then each Pareto alternative — pick the first that is not the failed
	// lane and not the operator's explicit hint that just failed.
	type cand struct{ lane, model, effort string }
	cands := []cand{{dec.Lane, dec.Model, dec.Effort}}
	for _, a := range dec.Alternatives {
		cands = append(cands, cand{a.Lane, a.Model, a.Effort})
	}
	for _, c := range cands {
		if c.lane == "" || c.lane == excludeLane {
			continue
		}
		return c.lane, c.model, c.effort, true
	}
	return "", "", "", false
}

// runStrategyRun is the detached supervisor entry (S3R-7). It runs HEADLESS —
// spawned by strategy_dispatch as a detached child, or inline from the CLI. It
// builds the prod seams and drives strategy.Execute; on --resume it runs the
// idempotent-only recovery first; on --sweep it reaps every stale dispatch. A
// heartbeat goroutine keeps state.json fresh so a crash is detectable. All
// diagnostics go to stderr (headless; not the MCP transport).
func runStrategyRun(args []string) error {
	resume := false
	sweep := false
	var id string
	for _, a := range args {
		switch a {
		case "--resume":
			resume = true
		case "--sweep":
			sweep = true
		default:
			if id == "" {
				id = a
			}
		}
	}

	if sweep {
		return sweepStale()
	}
	if id == "" {
		return fmt.Errorf("strategy-run: dispatch_id required (or --sweep)")
	}
	return driveDispatch(id, resume)
}

// driveDispatch resumes (if asked) then Executes a single dispatch under a live
// heartbeat. A "blocked" resume STOPS (never feeds a blocked step into Execute —
// Group D contract); a terminal resume verdict returns without re-running.
//
// F1 (R10 double-spend guard): it FIRST acquires the cross-process
// supervisor-exclusion lease (AcquireSupervisorLease). If a LIVE supervisor
// already drives this dispatch (fresh heartbeat — e.g. the original supervisor
// frozen under laptop sleep but still alive), it REFUSES with a clean no-op —
// NEVER a second Execute over the same DAG (which would burn a second cloud
// window on a not-yet-started step). A genuinely dead holder's stale lease is
// stolen. The lease is held for the WHOLE drive (Resume + Execute) and
// heartbeated so the live holder is never stolen mid-flight. ALL three entrypoints
// (plain drive, --resume, and --sweep's per-dispatch reap) funnel through here,
// so no two supervisors ever drive one dispatch.
func driveDispatch(id string, resume bool) error {
	dir := statepaths.StrategyDir(id)
	if _, err := strategy.Load(dir); err != nil {
		return fmt.Errorf("strategy-run: no such dispatch %q: %w", id, err)
	}

	// F1: acquire the supervisor lease before driving. A live holder → refuse.
	pid := os.Getpid()
	acquired, aerr := strategy.AcquireSupervisorLease(dir, pid, time.Now().UTC(), staleThreshold)
	if aerr != nil {
		return fmt.Errorf("strategy-run %s: acquire supervisor lease: %w", id, aerr)
	}
	if !acquired {
		fmt.Fprintf(os.Stderr, "strategy-run %s: already supervised by a live process — refusing to start a second Execute (R10)\n", id)
		return nil // clean no-op, never a second Execute
	}
	// Release the lease when this supervisor is done driving (terminal / return).
	defer func() { _ = strategy.ReleaseSupervisorLease(dir, pid, time.Now().UTC()) }()

	if resume {
		verdict, rerr := strategy.Resume(dir)
		if rerr != nil {
			return fmt.Errorf("strategy-run resume %s: %w", id, rerr)
		}
		switch verdict {
		case "blocked":
			fmt.Fprintf(os.Stderr, "strategy-run %s: BLOCKED — a cloud-spend step started with no ok receipt; operator-gated re-run required (R10)\n", id)
			return nil
		case "done", "failed", "deferred", "cancelled":
			fmt.Fprintf(os.Stderr, "strategy-run %s: already terminal (%s)\n", id, verdict)
			return nil
		case "resumable":
			// fall through to Execute
		}
	}

	stop := startHeartbeat(dir, pid)
	defer stop()

	cfg := orchcfg.Load(configPath())
	// F1b (R10): the per-wave guard re-checks that THIS pid still holds the
	// supervisor lease. If a --sweep/--resume reaper stole a stale lease while this
	// supervisor was frozen under laptop sleep, Execute steps aside instead of
	// re-dispatching a wave the reaper already dispatched (double-spend on wake).
	err := strategy.Execute(dir, prodNodeRunner(id, prodAlternatives), prodResolve, prodAlternatives,
		strategy.ExecConfig{MaxConcurrency: cfg.StrategyMaxConcurrency, ReLaneMaxDepth: 1},
		func() time.Time { return time.Now().UTC() },
		// State predicate: still ours iff the (locked/loaded) state records THIS pid.
		// No Load inside — the executor supplies the state, so a transient Windows
		// read glitch can never make a healthy owner spuriously abandon its drive
		// (fail-open on a read hiccup; the on-disk lease is untouched).
		func(s *strategy.State) bool { return s.SupervisorPID == pid })
	if err != nil {
		return fmt.Errorf("strategy-run execute %s: %w", id, err)
	}
	return nil
}

// startHeartbeat launches the S3R-7 liveness ticker (mirrors glmlane.Pace: a
// goroutine + stop channel + WaitGroup join, race-clean). It stamps HeartbeatAt
// immediately, then every heartbeatEvery, and stops (joined) when the returned
// func is called. Heartbeat failures are logged, never fatal.
func startHeartbeat(dir string, pid int) func() {
	// F1b-2 (R10): beat ONLY while this pid still holds the lease. If a reaper
	// stole the lease while this supervisor was frozen (laptop sleep), a plain
	// Heartbeat would keep refreshing HeartbeatAt and make the STOLEN lease look
	// live — "covering for" the thief. HeartbeatOwned no-ops once ownership is lost.
	_, _ = strategy.HeartbeatOwned(dir, pid, time.Now().UTC()) // first beat before any wave
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				owned, err := strategy.HeartbeatOwned(dir, pid, time.Now().UTC())
				if err != nil {
					fmt.Fprintln(os.Stderr, "warn: strategy heartbeat:", err)
				}
				if !owned {
					// Lease stolen — stop beating so we no longer refresh the new
					// holder's liveness clock. The Execute per-wave guard is the primary
					// double-dispatch protection; this just keeps the clock honest.
					return
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(stop)
			wg.Wait()
		})
	}
}

// sweepStale scans every dispatch under StateDir()/strategy/*/state.json and
// reaps the ones in running/working whose heartbeat is stale (dead supervisor,
// S3R-7) by resuming + re-driving each. A scheduled/nightshift task can call
// `strategy-run --sweep`. Per-dispatch errors are logged and the sweep
// continues — one wedged dispatch must not stop the reaper.
func sweepStale() error {
	base := filepath.Join(statepaths.StateDir(), "strategy")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing dispatched yet
		}
		return err
	}
	now := time.Now().UTC()
	reaped := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		dir := statepaths.StrategyDir(id)
		st, lerr := strategy.Load(dir)
		if lerr != nil {
			continue // no readable state.json → not a dispatch dir
		}
		if !strategy.Stale(st, now, staleThreshold) {
			continue
		}
		fmt.Fprintf(os.Stderr, "sweep: reaping stale dispatch %s (state=%s)\n", id, st.State)
		if derr := driveDispatch(id, true); derr != nil {
			fmt.Fprintf(os.Stderr, "sweep: reap %s failed: %v\n", id, derr)
			continue
		}
		reaped++
	}
	fmt.Fprintf(os.Stderr, "sweep: reaped %d stale dispatch(es)\n", reaped)
	return nil
}

// strategyStatusJSON builds the published 3-field core {state, step_receipts[],
// result_ref} plus the S3R-7 stale-detection and S3R-9 blocked-visibility
// surfaces. step_receipts = dispatch.jsonl filtered on DispatchID (reuse
// loadReceipts). An unknown id is a clean error object, never a crash.
func strategyStatusJSON(id string) string {
	st, err := strategy.Load(statepaths.StrategyDir(id))
	if err != nil {
		b, _ := jsonMarshalIndent(map[string]any{"error": "no such dispatch", "dispatch_id": id})
		return string(b)
	}
	var receipts []dispatch.Record
	for _, r := range loadReceipts(dispatchPath()) {
		if r.DispatchID == id {
			receipts = append(receipts, r)
		}
	}

	state := st.State
	out := map[string]any{
		"state":         state,
		"step_receipts": receipts,
		"result_ref":    st.ResultRef,
	}

	// E4: a deferred dispatch surfaces the earliest step ResumeAt (truthful) +
	// a jittered retry_at so N deferred dispatches sharing a reset don't
	// re-dispatch in lockstep.
	if st.State == "deferred" {
		var earliest time.Time
		for _, ss := range st.StepStatus {
			if ss != nil && ss.ResumeAt != nil && (earliest.IsZero() || ss.ResumeAt.Before(earliest)) {
				earliest = *ss.ResumeAt
			}
		}
		if !earliest.IsZero() {
			out["resume_at"] = earliest
			// Stable per-dispatch: strategy_status is POLLED, so the retry hint must
			// not re-roll on every read (a scheduler could never settle on a wake
			// time). Seed on the dispatch id — distinct dispatches sharing this reset
			// still spread (E4 anti-herd), this one just stops flickering.
			out["retry_at"] = jitter.RetryAtStable(earliest, jitter.DefaultWindow, id)
		}
	}

	// S3R-7 stale detection: a running/working dispatch whose heartbeat is stale
	// is a dead supervisor — report needs_resume with the resume command, never
	// lie "running".
	if strategy.Stale(st, time.Now().UTC(), staleThreshold) {
		out["state"] = "needs_resume"
		out["stale"] = true
		out["resume_cmd"] = fmt.Sprintf("mr-orchestrate strategy-run --resume %s", id)
	}

	// S3R-9 blocked visibility: surface the blocked step's identity + lane + spend
	// FRONT-AND-CENTER (a top-level blocked_step), not buried in the array, so the
	// operator's "your call" is a 5-second decision.
	if bs := blockedStep(st, receipts); bs != nil {
		out["blocked_step"] = bs
	}

	// Make-verify-count: surface the tier-2 verify judgment FRONT-AND-CENTER. When
	// flagged (non-affirmative), the operator sees "completed, but the independent
	// check disagreed" — the one thing a solo run cannot tell them. Advisory: the
	// dispatch state is unchanged (tier-1 governs); this is a signal, not a gate.
	if st.VerifyVerdict != "" {
		out["verify"] = map[string]any{
			"verdict": st.VerifyVerdict,
			"reason":  st.VerifyReason,
			"flagged": st.VerifyFlag,
		}
	}

	b, _ := jsonMarshalIndent(out)
	return string(b)
}

// blockedStep finds the blocked step (dispatch or any step blocked) and returns
// its {step_id, lane, outcome_class, notional_usd} — what it is and what it
// already spent (S3R-9). Nil when nothing is blocked.
func blockedStep(st strategy.State, receipts []dispatch.Record) map[string]any {
	for _, s := range st.IR.Steps {
		ss := st.StepStatus[s.ID]
		if ss == nil || ss.OutcomeClass != "blocked" {
			continue
		}
		spend := 0.0
		for _, r := range receipts {
			if r.StepID == s.ID {
				spend += r.NotionalUSD
			}
		}
		lane := ss.Lane
		if lane == "" {
			lane = s.LaneHint
		}
		return map[string]any{
			"step_id":       s.ID,
			"lane":          lane,
			"outcome_class": ss.OutcomeClass,
			"notional_usd":  spend,
			"instruction":   s.Instruction,
		}
	}
	return nil
}
