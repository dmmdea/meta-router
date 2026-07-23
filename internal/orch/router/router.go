// Package router is the deterministic rank-table router v0 (design brief §1):
// rank-based (NOT scalar-score — the routing-collapse lesson), quota mask
// applied BEFORE selection, Pareto-pruned alternatives, a v0 shadow price that
// demotes the throttled lane by exactly one rank. NO LLM on the hot path — the
// table is data (Seed() compiled defaults, fuses-pattern Load override) and
// Route() is a pure function of (class, lane states, ctx tokens, now).
//
// The rank table's priors come from docs/specs/2026-07-06-v3-model-capability
// -baseline.md — every Entry cites its evidence (R14a: researched priors, not
// cost instinct; the evidence-citation test is the contract).
package router

import (
	"fmt"
	"sort"
	"time"
)

type Class string

const ( // one per capability-baseline §1 row — dropping rows is a defect
	HardRepo        Class = "hard-repo"
	TerminalBounded Class = "terminal-bounded"
	Workhorse       Class = "workhorse-coding"
	ManyTool        Class = "many-tool-orchestration"
	MCPStructured   Class = "mcp-structured"
	DeepReasoning   Class = "deep-reasoning"
	FormalMath      Class = "formal-math"
	CompetitionMath Class = "competition-math"
	LongContext     Class = "long-context"
	LatencyIter     Class = "latency-iteration"
	CheapToolLoops  Class = "cheap-tool-loops"
	MechanicalText  Class = "mechanical-text"
	DocSummarize    Class = "doc-summarize"
	VerifyGate      Class = "verify-gate"
	HardCaseReclaim Class = "hard-case-reclaim"
)

type Entry struct {
	Lane, Model, Effort, Evidence string
	Rank                          int
}

type Table map[Class][]Entry

// LaneState is a lane's routing-time status, derived by the caller from
// admission + the lane gates (see cmd laneStates). WorstPct is the worst
// (highest) window depletion; -1 means unknown → treated as 0 (fail-open,
// most-available) in the tie-break.
type LaneState struct {
	State    string
	WorstPct float64
	ResumeAt time.Time
	// Downshift is the E1 burn-rate level (0-3, burnrate.Level as int), set by
	// the caller from the quota-trace trajectory. >= 2 (medium) demotes +1 rank
	// below; 1 (slow) is advisory-only. Kept as int so this package stays free
	// of a burnrate import (the router consumes a signal, not the mechanism).
	Downshift int
	// Boost is the E2 spend-down rank boost (0..bounded, spenddown.Entry.Level),
	// subtracted from the lane's rank below. The caller sets it ONLY for
	// explicitly batch-tagged consults whose task passes the completion-fit
	// gate, on lanes whose window is measured under-utilized near reset — an
	// untagged (interactive) consult always routes with Boost 0. Kept as int
	// for the same signal-not-mechanism reason as Downshift.
	Boost int
	// PaceSlack is the lane's binding pace slack (W1: elapsed-fraction −
	// used-ratio, min across known windows), set by the caller from pace.
	// nil = unknown. Consumed ONLY under Opts.PaceRank (default off, B8);
	// always surfaced for receipts either way. Pointer, not float, so an
	// unknown can never win a tie by accident.
	PaceSlack *float64
}

// Opts are optional routing knobs (variadic on Route for source
// compatibility; first value wins).
type Opts struct {
	// PaceRank enables the slack tie-break: among lanes tied on effective
	// rank AND depletion, higher binding slack wins before the static lane
	// priority. Ships OFF — a routing-visible change that promotes only
	// through a budget-state eval (Bible B8).
	PaceRank bool
}

type Masked struct{ Lane, Model, Reason string }

type Decision struct {
	Lane, Model, Effort, Strategy, Rule, Reason string
	Class                                       Class
	QuotaState                                  map[string]string
	Masked                                      []Masked
	Alternatives                                []Entry   // Pareto-pruned runners-up (receipt/replay substrate)
	ResumeAt                                    time.Time // earliest resume when EVERYTHING is masked (relegation, never rejection)
	SpendDownBoost                              int       // E2 boost the WINNING lane carried (0 = none); transparency for receipts/JSON
	PaceSlack                                   *float64  // W1: the WINNING lane's binding pace slack at decision time (nil = unknown); advisory surface
}

const CtxCapCodex = 258_000 // CLI hard cap 272K-in incl. reserve, ~258K effective (baseline §1, independent)
const CtxCapLocal = 100_000 // local prohibition floor (baseline §2: >~100–210K)

// lanePriority is the S2R-12 residual tiebreak: a TOTAL, stable order that
// prefers claude over codex at parity (codex is the surgical resource — don't
// spend it on run one). claude < codex < glm < local.
func lanePriority(lane string) int {
	switch lane {
	case "claude":
		return 0
	case "codex":
		return 1
	case "glm":
		return 2
	case "local":
		return 3
	default:
		return 4
	}
}

// normPct maps unknown depletion (-1) to 0: fail-open, most-available.
func normPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	return p
}

// maskStates are the lane states that evict a candidate from selection entirely
// (relegation, never rejection: a dead lane never wins).
func masked(state string) bool {
	switch state {
	case "exhausted", "hard_stop", "model_retired", "unavailable":
		return true
	}
	return false
}

// scored is a candidate after the shadow-price adjustment, ready for the total
// deterministic sort.
type scored struct {
	e        Entry
	effRank  int
	usedPct  float64 // normalized (-1 → 0)
	priority int
	slack    *float64 // W1 binding pace slack; nil = unknown (never wins a tie)
}

// less is the TOTAL deterministic order (S2R-12): (1) effRank asc; (2)
// normalized worst-window UsedPct asc; (3) stable lane priority (claude first).
func lessScored(a, b scored, paceRank bool) bool {
	if a.effRank != b.effRank {
		return a.effRank < b.effRank
	}
	if a.usedPct != b.usedPct {
		return a.usedPct < b.usedPct
	}
	if paceRank {
		// Higher binding slack wins; a known slack beats nil; nil-vs-nil
		// falls through to the static priority (unknown never decides).
		switch {
		case a.slack != nil && b.slack != nil && *a.slack != *b.slack:
			return *a.slack > *b.slack
		case a.slack != nil && b.slack == nil:
			return true
		case a.slack == nil && b.slack != nil:
			return false
		}
	}
	return a.priority < b.priority
}

func Route(t Table, c Class, states map[string]LaneState, ctxTokens int64, now time.Time, opts ...Opts) Decision {
	var opt Opts
	if len(opts) > 0 {
		opt = opts[0]
	}
	d := Decision{Class: c, Strategy: "solo", QuotaState: map[string]string{}}
	for lane, st := range states {
		d.QuotaState[lane] = st.State
	}

	// 1. candidates = t[c]; unknown class → HardRepo ordering (quality-first
	//    default, R14a — the reason marks it, S2R-11 keeps rank-1 = Opus).
	candidates, ok := t[c]
	unknownClass := !ok
	if unknownClass {
		candidates = t[HardRepo]
	}

	// 2. Quota mask BEFORE selection + shadow price (step 3 folded in). Every
	//    drop is recorded in Masked with its reason.
	var pool []scored
	for _, e := range candidates {
		st := states[e.Lane]
		if masked(st.State) {
			d.Masked = append(d.Masked, Masked{Lane: e.Lane, Model: e.Model,
				Reason: fmt.Sprintf("%s lane %s", e.Lane, st.State)})
			continue
		}
		if e.Lane == "codex" && ctxTokens > CtxCapCodex {
			d.Masked = append(d.Masked, Masked{Lane: e.Lane, Model: e.Model,
				Reason: fmt.Sprintf("codex masked: ctx %d > 258K effective cap (CLI 272K-in incl. reserve, baseline §1)", ctxTokens)})
			continue
		}
		if e.Lane == "local" && ctxTokens > CtxCapLocal {
			d.Masked = append(d.Masked, Masked{Lane: e.Lane, Model: e.Model,
				Reason: fmt.Sprintf("local masked: ctx %d > 100K prohibition floor (baseline §2)", ctxTokens)})
			continue
		}
		// 3. Shadow price (R14-compliant): +1 rank when the lane is throttled —
		//    the ONLY demotion trigger is the existing 80% real-consumption
		//    threshold (an account fact, not a reserve).
		eff := e.Rank
		if st.State == "throttled" {
			eff = e.Rank + 1
		}
		// 3b. E1 burn-rate downshift (slice-4): +1 rank when the lane's measured
		//     burn trajectory is on pace to exhaust the window before reset
		//     (Downshift >= burnrate.LevelMedium). Same R14 justification: a
		//     measured over-pace is an account fact; with a nominal burn or an
		//     empty trace this is a no-op. Stacks with the throttle price — they
		//     are independent facts.
		if st.Downshift >= 2 {
			eff++
		}
		// 3c. E2 spend-down boost (slice-4): a bounded rank RAISE for tagged
		//     batch work toward a window measured on pace to strand budget at
		//     reset (Q2: rank-delta, never scalar; the caller owns the batch
		//     tag, forecast, hysteresis, and completion-fit gates — and never
		//     sets Boost on a throttled, downshifted, or masked lane, so the
		//     demotions above and this raise cannot fight over one lane).
		eff -= st.Boost
		pool = append(pool, scored{e: e, effRank: eff,
			usedPct: normPct(st.WorstPct), priority: lanePriority(e.Lane), slack: st.PaceSlack})
	}

	// 5. All masked → relegation carrying the earliest masked resume (RS5); the
	//    caller emits the standard deferral (exit 3).
	if len(pool) == 0 {
		var earliest time.Time
		for _, m := range candidates {
			st := states[m.Lane]
			if !masked(st.State) {
				continue
			}
			if st.ResumeAt.IsZero() {
				continue
			}
			if earliest.IsZero() || st.ResumeAt.Before(earliest) {
				earliest = st.ResumeAt
			}
		}
		d.Reason = "all lanes masked"
		d.ResumeAt = earliest
		return d
	}

	// 4. Winner = min under the TOTAL order (deterministic).
	sort.SliceStable(pool, func(i, j int) bool { return lessScored(pool[i], pool[j], opt.PaceRank) })
	win := pool[0]
	d.Lane, d.Model, d.Effort = win.e.Lane, win.e.Model, win.e.Effort
	d.Rule = fmt.Sprintf("%s#%d:%s", c, win.e.Rank, win.e.Lane)
	if unknownClass {
		d.Reason = "unknown class → quality-first default (HardRepo ordering, R14a/S2R-11); brain should pass --class for precision"
	} else {
		d.Reason = fmt.Sprintf("rank %d %s/%s admitted (state=%s)", win.e.Rank, win.e.Lane, win.e.Model, states[win.e.Lane].State)
	}
	d.PaceSlack = states[win.e.Lane].PaceSlack
	if b := states[win.e.Lane].Boost; b > 0 {
		d.SpendDownBoost = b
		d.Reason += fmt.Sprintf(" (spend-down boost -%d: window under-utilized near reset, batch-tagged)", b)
	}

	// Alternatives = remaining candidates minus Pareto-dominated ones. Dominated
	// ⇔ another survivor has ≤ effRank AND ≤ usedPct with at least one strict.
	// The pool is already in the total-order sort, so iteration is deterministic.
	rest := pool[1:]
	for i, a := range rest {
		dominated := false
		for j, b := range rest {
			if i == j {
				continue
			}
			if b.effRank <= a.effRank && b.usedPct <= a.usedPct &&
				(b.effRank < a.effRank || b.usedPct < a.usedPct) {
				dominated = true
				break
			}
		}
		if !dominated {
			d.Alternatives = append(d.Alternatives, a.e)
		}
	}
	return d
}
