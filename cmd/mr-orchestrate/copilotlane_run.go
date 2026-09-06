package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/copilotlane"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

// applyCopilotOutcome is the copilot lane's post-run ledger accounting on the
// MONTHLY calendar window. The vendor SELF-REPORTS consumption per dispatch
// (session.usage_checkpoint: premium requests + nano-AIU), so the shadow spend
// is provider-true, not a multiplier estimate. Capacity is the configured
// monthly allowance in milli-requests; a vendor rate-limit signal observes a
// limit at the calendar reset (the window's true end — premium allowances
// have no rolling recovery).
func applyCopilotOutcome(l *ledger.Ledger, o copilotlane.Outcome, cfg orchcfg.Config, now time.Time) {
	if b, ok := l.Bucket("copilot", ledger.WinMonth); !ok || b.CapTokens == 0 {
		l.SetCapacityEstimate("copilot", ledger.WinMonth, cfg.CopilotMonthlyRequests*1000) // milli-requests
	}
	l.AnchorIfUnset("copilot", ledger.WinMonth, ledger.NextMonthlyReset(now), now)
	// METERING UNIT (recalibrated 2026-09-05): the vendor's own per-dispatch
	// `session.usage_checkpoint.totalPremiumRequests`, floored at 1 for any
	// dispatch that ran. Until then the lane metered ONE DISPATCH = ONE REQUEST
	// on the argument that the vendor figure was unverified (gemini-3.6-flash
	// reported 14 while burning the fewest AI units). The 2026-09-05 gold probe
	// settled which error is worse: under that unit the ledger believed 60% of
	// the month remained when GitHub returned 402 "exceeded your monthly quota"
	// on about the 110th dispatch (78% served by gpt-5.6-luna), so the router
	// never paced and the lane latched for the rest of the month. Undercounting
	// spends the month blind; overcounting a rare model merely throttles early.
	// The "exceeding degrades to included models" premise was also false for
	// CLI dispatch: gpt-5.4-mini answered 402 on the exhausted account. The
	// figure lands on the receipt (PremiumRequests/NanoAiu) so the billing page
	// can be reconciled against it — that page remains the only ground truth.
	if o.Class == "ok" || o.Usage.PremiumRequests > 0 {
		l.AddShadow("copilot", ledger.WinMonth, copilotMilliRequests(o.Usage), now)
	}
	if o.Class == "rate_limit" {
		l.ObserveLimit("copilot", "", ledger.WinMonth, ledger.NextMonthlyReset(now), now)
	}
}

// copilotMilliRequests is the metered amount for one dispatch: the vendor's
// per-dispatch premium-request figure in milli-requests, never below one
// request. Exposed for the meter test; the policy lives here, not in the
// caller.
func copilotMilliRequests(u copilotlane.Usage) int64 {
	if u.PremiumRequests > 1 {
		return u.PremiumRequests * 1000
	}
	return 1000
}

// runCopilotLane mirrors the codex dispatch path for `run --lane copilot`:
// gate → dry-run or EnsureHome+MintToken+Run → ledger accounting → receipt.
// The dispatch prints the raw JSONL (like codex) so callers keep full
// fidelity; the minted token lives only in the child's env and appears in no
// output (R10 boundary).
func runCopilotLane(out io.Writer, prompt, model, cwd string, timeoutSec int, extra []string, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	if model == "" {
		model = cfg.CopilotModel // "auto" is vendor-supported; the JSONL reports the model actually served
	}
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	g := laneGate(l.Snapshot(), "copilot", now, defaultThresholds, force)
	req := copilotlane.RunReq{Prompt: prompt, Model: model, CWD: cwd,
		TimeoutSec: timeoutSec, SkipVersionGate: force, Extra: extra}

	if !g.Admit {
		rec := dispatch.Record{
			TS: now, Lane: "copilot", Model: model, OutcomeClass: "deferred",
			Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
			RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost,
			Admit: false, AdmitState: g.State, AdmitReason: g.Reason, Desc: desc,
		}
		sf.stamp(&rec)
		warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (deferral)")
		fmt.Fprintln(out, string(deferralJSON(g)))
		return exitDeferred, nil
	}
	if g.Forced {
		fmt.Fprintln(os.Stderr, "WARN:", g.Reason)
	}

	if !live {
		// Dry-run must not require the token (it spawns nothing), but the
		// argv contract is still validated with a placeholder so a config
		// error surfaces before any --live attempt.
		dry := req
		dry.Home, dry.Token = "(dry-run)", "(dry-run)"
		argv, err := copilotlane.BuildArgs(dry)
		if err != nil {
			return 1, err
		}
		b, _ := json.MarshalIndent(map[string]any{
			"dry_run": true, "admit": true, "admit_state": g.State, "admit_reason": g.Reason, "forced": g.Forced, "args": argv,
		}, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0, nil
	}

	tok, err := copilotlane.MintToken(cfg.CopilotTokenUser)
	if err != nil {
		return 1, fmt.Errorf("copilot lane: %w", err)
	}
	req.Token = tok
	home, cleanup, err := copilotlane.EnsureHome(stateDir())
	if err != nil {
		return 1, err
	}
	defer cleanup()
	req.Home = home
	if force {
		if v, ok := copilotlane.VersionGate(); !ok {
			fmt.Fprintf(os.Stderr, "WARN: version gate SKIPPED by --force (copilot %q <1.0.82, unverified flag surface)\n", v)
		}
	}
	o, raw, err := copilotlane.Run(context.Background(), req)
	if err != nil {
		return 1, err // config_error: bad args / version gate — never reached the binary
	}
	warnIf(updateLedger(func(fresh *ledger.Ledger) {
		applyCopilotOutcome(fresh, o, cfg, now)
	}), "ledger update (post-run)")
	servedModel := o.Model
	if servedModel == "" {
		servedModel = model
	}
	rec := dispatch.Record{
		TS: now, Lane: "copilot", Model: servedModel, OutcomeClass: o.Class, RateLimitOrigin: upstreamRLO(o.Class, ""),
		Admit: true, AdmitState: g.State, AdmitReason: g.Reason,
		NumTurns: o.Turns, PremiumRequests: o.Usage.PremiumRequests, NanoAiu: o.Usage.NanoAiu,
		Origin:   origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
		RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost, Desc: desc,
	}
	sf.stamp(&rec)
	warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append")
	noteLaneHealth(cfg.ExclusionOff, "copilot", o.Class, now) // W6 breaker
	if len(raw) > 0 {
		fmt.Fprintln(out, string(raw))
	} else {
		b, _ := json.Marshal(map[string]string{"outcome_class": o.Class, "detail": o.Result})
		fmt.Fprintln(out, string(b))
	}
	if o.Class != "ok" {
		fmt.Fprintf(os.Stderr, "outcome %q is not ok (exit %d)\n", o.Class, exitNotOK)
		return exitNotOK, nil
	}
	return 0, nil
}
