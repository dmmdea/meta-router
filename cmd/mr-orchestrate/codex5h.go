package main

import (
	"fmt"
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
	if ps.CodexFactsAt == nil || ps.CodexHas5h == nil || ps.CodexSaw5hElsewhere == nil {
		return codex5hGate{Reason: "armed but no recorded codex plan facts (poll first)"}
	}
	stale := time.Duration(cfg.QuotaStaleHours) * time.Hour
	if age := now.Sub(*ps.CodexFactsAt); age > stale || age < 0 {
		return codex5hGate{Reason: fmt.Sprintf("armed but codex plan facts are stale (%s old, limit %s)", age.Round(time.Minute), stale)}
	}
	if *ps.CodexHas5h {
		return codex5hGate{Reason: "armed but the vendor reports a 5h window (plan " + ps.CodexPlan + ")"}
	}
	if !*ps.CodexSaw5hElsewhere {
		return codex5hGate{Reason: "armed but the 5h absence is uncorroborated (no sibling 5h window in the same response; Q10 bare-absence rule)"}
	}
	return codex5hGate{Suppress: true, Reason: fmt.Sprintf("codex 5h estimate suppressed: plan %s reports no 5h window while a sibling limit block does (facts at %s)",
		ps.CodexPlan, ps.CodexFactsAt.UTC().Format(time.RFC3339))}
}
