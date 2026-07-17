package strategy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// recoveryAction is the per-step recovery classification. It is INTERNAL — the
// public Resume returns a STRING verdict so no exported enum crosses the cmd
// boundary (Group E wires `strategy-run --resume` against the string).
type recoveryAction int

const (
	// actReconcile — a tagged ok/ok_notional receipt exists for this
	// dispatch_id:step_id:attempt: the step ACTUALLY COMPLETED and the crash only
	// beat the state write. Mark it done from the receipt (S3R-6a) — never strand a
	// step that provably finished, never block it.
	actReconcile recoveryAction = iota
	// actRecord — a non-ok receipt exists: the outcome is KNOWN. Record it; the
	// taxonomy then governs on the re-run (a hard-fail may re-lane via Execute).
	actRecord
	// actRetry — no receipt AND the resolved lane is idempotent (free/read-only
	// local): safe to re-run (S3R-6b). Clear StartedAt so readySet re-picks it.
	actRetry
	// actBlock — no receipt AND the resolved lane is cloud-spend (claude/codex/glm):
	// we cannot prove it didn't spend a window, so NEVER blind re-dispatch (R10).
	// Mark the step + dispatch blocked for operator-gated re-run.
	actBlock
)

// idempotentLane reports whether a step's RESOLVED lane is provably safe to
// re-run on resume. Only the free/read-only local lane qualifies; every
// cloud-spend lane (real quota already possibly burned) does NOT — R10 forbids a
// blind re-dispatch. Keyed on the resolved lane (StepState.Lane), never the empty
// LaneHint (S3R-6b: a router-decided node that ran on local is idempotent).
func idempotentLane(lane string) bool { return lane == "local" }

// resolveAction classifies one started-but-unfinished step from its receipt
// evidence (S3R-6). The idempotency key <dispatch_id>:<step_id>:<attempt> is REAL:
// receiptFound reports whether a receipt for that key was located in dispatch.jsonl
// and receiptClass is its outcome. Reconciliation FIRST (an ok receipt wins over
// any lane policy — the step provably finished); then a known non-ok outcome is
// recorded; only with NO receipt does the resolved-lane idempotency policy apply.
func resolveAction(receiptClass string, receiptFound bool, resolvedLane string) recoveryAction {
	if receiptFound {
		if classifyOutcome(receiptClass) == kindOK { // ok / ok_notional
			return actReconcile
		}
		return actRecord
	}
	if idempotentLane(resolvedLane) {
		return actRetry
	}
	return actBlock
}

// receiptFor scans the global dispatch.jsonl for a tagged strategy receipt
// matching dispatch_id + step_id + attempt (the real idempotency key, S3R-6a). It
// returns the receipt's outcome class and whether one was found. A missing log is
// "no receipt" (found=false), not an error — a crash before any receipt is the
// exact no-receipt case. If several receipts share the key (a re-lane writes one
// per attempt, but a torn write could dup), the LAST wins — the freshest outcome.
func receiptFor(dispatchID string, stepID, attempt int) (class string, found bool, err error) {
	f, oerr := os.Open(statepaths.Dispatch())
	if oerr != nil {
		if os.IsNotExist(oerr) {
			return "", false, nil
		}
		return "", false, oerr
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // tolerate long JSONL lines
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r dispatch.Record
		if json.Unmarshal(line, &r) != nil {
			continue // skip a torn/garbage line, keep scanning (append-log robustness)
		}
		if r.DispatchID == dispatchID && r.StepID == stepID && r.Attempt == attempt {
			class, found = r.OutcomeClass, true
		}
	}
	if serr := sc.Err(); serr != nil {
		return "", false, serr
	}
	return class, found, nil
}

// Resume is the crash-recovery pass that `strategy-run --resume <id>` (Group E)
// invokes before re-running Execute. It reconciles state.json against the global
// dispatch.jsonl for every step in the crash window (StartedAt set, OutcomeClass
// empty) and applies the S3R-6 idempotent-only policy, then returns a STRING
// verdict — no exported enum crosses the cmd boundary:
//
//	"resumable" — runnable steps remain; Group E's caller re-runs Execute.
//	"blocked"   — a cloud-spend step started with no ok receipt (R10): operator-gated.
//	"done" / "failed" / "deferred" — the DAG is already terminal (finalize precedence).
//
// Every Mutate return is surfaced (S3R-6c: no silent state drops). Resume is
// idempotent: a second pass over a reconciled/blocked/retried state re-derives the
// same verdict without double-processing (a reconciled step now has an outcome, so
// it is no longer in the crash window).
func Resume(dir string) (string, error) {
	st, err := Load(dir)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()

	blocked := false
	for _, s := range st.IR.Steps {
		ss := st.StepStatus[s.ID]
		// Only steps in the crash window: mark-started but no recorded outcome.
		// A not-started step (StartedAt==nil) is left for Execute's ready-set.
		if ss == nil || ss.StartedAt == nil || ss.OutcomeClass != "" {
			continue
		}
		class, found, rerr := receiptFor(st.DispatchID, s.ID, ss.Attempt)
		if rerr != nil {
			return "", fmt.Errorf("reconcile receipts step %d: %w", s.ID, rerr)
		}
		switch resolveAction(class, found, ss.Lane) {
		case actReconcile:
			// The receipt proves completion — mark the step done from it (S3R-6a).
			// KNOWN BOUNDED LIMITATION (advisory-only): a verifier that crashed in the
			// narrow window between its receipt and persistOutcome's state write loses
			// its tier-2 VerifyVerdict here — the receipt carries OutcomeClass but not the
			// verdict (which lived only in the lost in-memory result), so finalize surfaces
			// no verify flag on this resumed dispatch. This is a lost SURFACING signal
			// only: the tier-1 state stays correct (done/failed via OutcomeClass), and R10 /
			// idempotency / the re-lane decision are unaffected (the verdict never gates).
			// Fully closing it would require carrying the verdict on dispatch.Record; not
			// worth the receipt-schema churn for a millisecond-wide advisory edge — re-run
			// the (cheap, local) verify to recover the signal if it is ever needed.
			if err := JournalDetail(dir, "resume_reconciled", s.ID,
				fmt.Sprintf("ok receipt found (%s) — step completed before crash", class), now); err != nil {
				return "", fmt.Errorf("journal resume_reconciled step %d: %w", s.ID, err)
			}
			if err := Mutate(dir, func(state *State) {
				state.StepStatus[s.ID].OutcomeClass = class
				finTS := now
				state.StepStatus[s.ID].TS = &finTS
			}, now); err != nil {
				return "", fmt.Errorf("reconcile step %d: %w", s.ID, err)
			}
		case actRecord:
			// A known non-ok outcome — record it; the taxonomy governs the re-run.
			if err := JournalDetail(dir, "resume_recorded", s.ID,
				fmt.Sprintf("non-ok receipt found (%s) — outcome known", class), now); err != nil {
				return "", fmt.Errorf("journal resume_recorded step %d: %w", s.ID, err)
			}
			if err := Mutate(dir, func(state *State) {
				state.StepStatus[s.ID].OutcomeClass = class
				finTS := now
				state.StepStatus[s.ID].TS = &finTS
			}, now); err != nil {
				return "", fmt.Errorf("record step %d: %w", s.ID, err)
			}
		case actRetry:
			// Idempotent free-local step, no receipt — safe to re-run. Clear the
			// started signal so readySet re-derives it (S3R-6b).
			if err := Journal(dir, "resume_retry", s.ID, now); err != nil {
				return "", fmt.Errorf("journal resume_retry step %d: %w", s.ID, err)
			}
			if err := Mutate(dir, func(state *State) {
				state.StepStatus[s.ID].StartedAt = nil
				state.StepStatus[s.ID].OutcomeClass = ""
			}, now); err != nil {
				return "", fmt.Errorf("reset local step %d: %w", s.ID, err)
			}
		case actBlock:
			// Cloud-spend step, no receipt — cannot prove a window wasn't spent.
			// Mark the step + dispatch blocked; wait for operator-gated re-run (R10).
			blocked = true
			if err := Journal(dir, "resume_blocked", s.ID, now); err != nil {
				return "", fmt.Errorf("journal resume_blocked step %d: %w", s.ID, err)
			}
			if err := Mutate(dir, func(state *State) {
				state.StepStatus[s.ID].OutcomeClass = "blocked"
				finTS := now
				state.StepStatus[s.ID].TS = &finTS
			}, now); err != nil {
				return "", fmt.Errorf("mark step blocked %d: %w", s.ID, err)
			}
		}
	}

	// Verdict. blocked outranks: a cloud step we could not prove clean stops the
	// whole dispatch for operator-gated re-run.
	if blocked {
		if err := Mutate(dir, func(state *State) { state.State = "blocked" }, now); err != nil {
			return "", fmt.Errorf("mark dispatch blocked: %w", err)
		}
		return "blocked", nil
	}

	// Re-derive readiness from the reconciled state: if any step is still runnable,
	// Group E re-runs Execute (which owns its own finalize). Otherwise the DAG is
	// already terminal — PERSIST the terminal state so a subsequent strategy_status
	// reports the truth (never a stale "running"), then return the verdict.
	st2, err := Load(dir)
	if err != nil {
		return "", err
	}
	if len(readySet(st2)) > 0 {
		return "resumable", nil
	}
	if err := finalize(dir, func() time.Time { return now }); err != nil {
		return "", fmt.Errorf("finalize on resume: %w", err)
	}
	final, err := Load(dir)
	if err != nil {
		return "", err
	}
	// finalize sets state via S3R-2 precedence; a step blocked during recovery
	// carries "blocked" (not a classifyOutcome kind), so surface blocked here.
	if v := blockedOrState(final); v != "" {
		return v, nil
	}
	return final.State, nil
}

// blockedOrState reports "blocked" when any step was blocked during recovery
// (finalize's precedence does not know the recovery-only "blocked" step class), so
// the dispatch verdict honestly reflects an operator-gated step even when finalize
// otherwise labeled the DAG. Empty string means defer to finalize's own state.
func blockedOrState(st State) string {
	for _, s := range st.IR.Steps {
		if ss := st.StepStatus[s.ID]; ss != nil && ss.OutcomeClass == "blocked" {
			return "blocked"
		}
	}
	return ""
}
