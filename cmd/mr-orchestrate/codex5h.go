package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

// codex5hGate is the decision to suppress the codex 5h capacity ESTIMATE for
// one dispatch (B1, v0.40.6). Zero value = today's behaviour (seed the
// estimate).
type codex5hGate struct {
	Suppress bool
	Reason   string // the receipt text when Suppress; else why not (status)
}

// codex5hEstimateGate decides from CONFIG and the recorded plan facts. All
// four legs must hold, and each is a distinct guard:
//   - the operator armed codex_5h_estimate_off (B8: default OFF; arming is a
//     config act with a receipt, reversible);
//   - the facts are FRESH (recorded within quota_stale_hours) — a machine
//     without a recent wham poll falls through to today's behaviour, which
//     is why arming the same config on both fleet nodes does not guarantee
//     identical routing;
//   - has_5h_window is recorded FALSE;
//   - a 5h window was seen in a sibling block of the SAME response
//     (saw_5h_elsewhere) — corroboration. A bare absence never suppresses:
//     that is the literal Plus case Q10 protects.
//
// A returning 5h window on the next poll flips has_5h_window and the gate
// closes by itself; the seed then re-arms on the next dispatch.
func codex5hEstimateGate(cfg orchcfg.Config, ps pollState, now time.Time) codex5hGate {
	if !cfg.Codex5hEstimateOff {
		return codex5hGate{Reason: "codex_5h_estimate_off not armed"}
	}
	if ps.CodexFactsAt == nil || ps.CodexHas5h == nil {
		return codex5hGate{Reason: "armed but no recorded codex plan facts (poll first)"}
	}
	if ps.CodexSaw5hElsewhere == nil {
		// Distinct from "never polled": a v0.40.5 poll-state has the plan and
		// has_5h_window but not the corroboration field, and telling the
		// operator to "poll first" next to a fresh observed_at reads as a
		// contradiction.
		return codex5hGate{Reason: "armed but the recorded facts predate v0.40.6 (no saw_5h_elsewhere); the next successful codex poll records it"}
	}
	stale := time.Duration(cfg.QuotaStaleHours) * time.Hour
	if age := now.Sub(*ps.CodexFactsAt); age > stale || age < 0 {
		return codex5hGate{Reason: fmt.Sprintf("armed but codex plan facts are stale (%s old, limit %s)", age.Round(time.Minute), stale)}
	}
	// THE PLAN IS A LEG. has_5h_window=false means "this response yielded no
	// 5h snapshot", which is NOT "this plan has no 5h window" — and the
	// corroboration leg cannot tell them apart, because a sibling model
	// block reporting its own 5h says nothing about the main allowance. On
	// Plus (the literal Q10 case) wham omits a window the plan really has
	// while the Spark block still reports one, so every other leg holds.
	// Only a plan MEASURED to have no 5h window may suppress.
	if !planAllowsCodex5hSuppression(cfg, ps.CodexPlan) {
		return codex5hGate{Reason: fmt.Sprintf("armed but plan %q is not in codex_5h_estimate_off_plans %v (a plan is only eligible once it has been MEASURED to have no 5h window; Q10 says an omitted window on Plus is a reporting failure, not a plan fact)",
			ps.CodexPlan, cfg.Codex5hEstimateOffPlans)}
	}
	if *ps.CodexHas5h {
		return codex5hGate{Reason: "armed but the vendor reports a 5h window (plan " + ps.CodexPlan + ")"}
	}
	// A short-span window that was PRESENT but unreadable (null used_percent,
	// zero reset_at, a re-spanned window) reaches has_5h_window=false by the
	// same path as a genuine absence. Refuse: a decoding failure must never
	// become a capacity decision.
	if ps.CodexUndecodable5h != nil && *ps.CodexUndecodable5h {
		return codex5hGate{Reason: "armed but the response carried a short-window rate-limit block this build could not decode (null used_percent, zero reset_at, or a re-spanned window): unreadable is not absent"}
	}
	if !*ps.CodexSaw5hElsewhere {
		return codex5hGate{Reason: "armed but the 5h absence is uncorroborated (no sibling 5h window in the same response; Q10 bare-absence rule)"}
	}
	return codex5hGate{Suppress: true, Reason: fmt.Sprintf("codex 5h estimate suppressed: plan %s (eligible) reports no 5h window while a sibling limit block does (facts at %s)",
		ps.CodexPlan, ps.CodexFactsAt.UTC().Format(time.RFC3339))}
}

// planAllowsCodex5hSuppression: the plan type must be on the operator's
// measured allowlist. An empty list is the same as the knob being off.
func planAllowsCodex5hSuppression(cfg orchcfg.Config, plan string) bool {
	for _, p := range cfg.Codex5hEstimateOffPlans {
		if strings.EqualFold(strings.TrimSpace(p), strings.TrimSpace(plan)) && strings.TrimSpace(plan) != "" {
			return true
		}
	}
	return false
}

// codexAdmitReason composes the dispatch receipt's admit_reason from the
// admission decision and the gate. Non-empty parts only: admission returns an
// EMPTY reason whenever every window sits below the throttle band (a fresh
// state dir, or the fleet's second node on its first codex dispatch), and
// concatenating blindly emitted a receipt starting with "; " that any
// consumer splitting on "; " reads as a blank first reason.
func codexAdmitReason(admitReason string, gate codex5hGate) string {
	parts := []string{}
	for _, p := range []string{admitReason, gateSuppressionNote(gate)} {
		if p = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), ";")); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, "; ")
}

// gateSuppressionNote is the gate's contribution to the receipt: its reason
// when it actually suppressed, nothing otherwise. A non-suppressing gate has
// a reason too (why it did not), and putting that on every codex receipt
// would be noise, not disclosure.
func gateSuppressionNote(gate codex5hGate) string {
	if !gate.Suppress {
		return ""
	}
	return gate.Reason
}
