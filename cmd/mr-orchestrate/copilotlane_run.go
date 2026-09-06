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
// (session.usage_checkpoint), so the shadow spend is provider-true, not a
// multiplier estimate. The ledger unit is MILLI-CREDITS. Capacity is the
// configured monthly allowance as an ESTIMATE only until the copilot usage
// poll (poll.go) lands the vendor's entitlement as the measured cap; a
// vendor rate-limit signal observes a limit at the calendar reset (the
// window's true end — credit allowances have no rolling recovery).
func applyCopilotOutcome(l *ledger.Ledger, o copilotlane.Outcome, cfg orchcfg.Config, now time.Time) {
	if b, ok := l.Bucket("copilot", ledger.WinMonth); !ok || b.CapTokens == 0 {
		l.SetCapacityEstimate("copilot", ledger.WinMonth, cfg.CopilotMonthlyCredits*1000) // milli-credits
	}
	l.AnchorIfUnset("copilot", ledger.WinMonth, ledger.NextMonthlyReset(now), now)
	// METERING UNIT — three recalibrations, each measured:
	//   v0.35 (2026-09-01): one dispatch = one request; the vendor figure
	//     (`totalPremiumRequests`) was distrusted (14 for gemini-3.6-flash).
	//   v0.37 (2026-09-05): the vendor figure, floored at one, against a cap
	//     of 300 — the gold probe had hit 402 with the ledger at 60%.
	//   v0.38 (2026-09-06): the vendor figure IS AI CREDITS. `gh api
	//     copilot_internal/user` reports token_based_billing true, entitlement
	//     1500, credits_used 1501, and the official billing report lists the
	//     month as 1,499.37 "Copilot AI Credits" — so 14 was 14 credits, the
	//     116 executed dispatches summed to the month, and the v0.37 cap of
	//     300 was the wrong number in the right unit. Cap is now 1,500 credits
	//     (poll-measured when available), floor one credit per dispatch.
	// Exhaustion is a hard 402 on every model (gpt-5.4-mini included), so the
	// calendar latch stands; the receipt keeps the vendor's key names so the
	// month reconciles against the billing report.
	if o.Class == "ok" || o.Usage.PremiumRequests > 0 {
		l.AddShadow("copilot", ledger.WinMonth, copilotMilliCredits(o.Usage), now)
	}
	if o.Class == "rate_limit" {
		l.ObserveLimit("copilot", "", ledger.WinMonth, ledger.NextMonthlyReset(now), now)
	}
}

// copilotMilliCredits is the metered amount for one dispatch: the vendor's
// per-dispatch figure (AI credits on a token-based plan) in milli-credits,
// never below one credit. Exposed for the meter test; the policy lives here,
// not in the caller.
func copilotMilliCredits(u copilotlane.Usage) int64 {
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
		TimeoutSec: timeoutSec, SkipVersionGate: force, Extra: extra,
		MaxAiCredits: cfg.CopilotMaxAiCredits} // per-dispatch spend bound (config-owned; 0 omits)

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
		Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
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
