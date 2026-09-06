package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/egress"
	"github.com/dmmdea/meta-router/internal/orch/freelane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/slidewin"
)

// The free-provider lanes (W4, 2026-09-06). Five lanes, one runner. Each
// run<Lane>Lane below is deliberately a thin, LITERAL wrapper that calls
// egress.Plan itself before handing off: the B14 canary finds a third-party
// lane's dispatcher by name and asserts that its body reaches the gate, and a
// wrapper that merely delegated would make every free lane invisible to it.
// The behavioural guarantees (effective_cwd, exit 6) are asserted on the
// runner's OUTPUT in freelane_run_test.go, the same posture as the GLM tests.

// egressOptions is the operator's third-party egress policy. glm_allow_repos
// keeps its name: the vetting recorded that "the approved free lanes inherit
// it", and one allowlist for every third-party lane is one rule to audit.
func egressOptions(cfg orchcfg.Config) egress.Options {
	return egress.Options{AllowRepos: cfg.GLMAllowRepos, PromptOnlyDenied: cfg.EgressPromptOnlyDenied}
}

// freeReq is one free-lane dispatch after its wrapper ran the egress gate.
type freeReq struct {
	Lane, Prompt, Model, Effort string
	// EffectiveCWD is what egress.Plan resolved; the lane has no child process
	// and reads no files, so it is RECEIPTED (the operator sees what the gate
	// decided) but never used for anything else.
	EffectiveCWD string
	Egress       egress.Decision
	TimeoutSec   int
	Live, Force  bool
	Origin, Desc string
	RF           recFields
	SF           strategyFields
}

func runGroqLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	cfg := orchcfg.Load(configPath())
	planDir, planCleanup, ed := egress.Plan(freelane.LaneGroq, cwd, egressOptions(cfg))
	defer planCleanup()
	return runFreeLane(out, freeReq{Lane: freelane.LaneGroq, Prompt: prompt, Model: model, Effort: effort, EffectiveCWD: planDir, Egress: ed,
		TimeoutSec: timeoutSec, Live: live, Force: force, Origin: origin, Desc: desc, RF: rf, SF: sf}, cfg)
}

func runCloudflareLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	cfg := orchcfg.Load(configPath())
	planDir, planCleanup, ed := egress.Plan(freelane.LaneCloudflare, cwd, egressOptions(cfg))
	defer planCleanup()
	return runFreeLane(out, freeReq{Lane: freelane.LaneCloudflare, Prompt: prompt, Model: model, Effort: effort, EffectiveCWD: planDir, Egress: ed,
		TimeoutSec: timeoutSec, Live: live, Force: force, Origin: origin, Desc: desc, RF: rf, SF: sf}, cfg)
}

func runOpenrouterLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	cfg := orchcfg.Load(configPath())
	planDir, planCleanup, ed := egress.Plan(freelane.LaneOpenRouter, cwd, egressOptions(cfg))
	defer planCleanup()
	return runFreeLane(out, freeReq{Lane: freelane.LaneOpenRouter, Prompt: prompt, Model: model, Effort: effort, EffectiveCWD: planDir, Egress: ed,
		TimeoutSec: timeoutSec, Live: live, Force: force, Origin: origin, Desc: desc, RF: rf, SF: sf}, cfg)
}

func runNimLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	cfg := orchcfg.Load(configPath())
	planDir, planCleanup, ed := egress.Plan(freelane.LaneNIM, cwd, egressOptions(cfg))
	defer planCleanup()
	return runFreeLane(out, freeReq{Lane: freelane.LaneNIM, Prompt: prompt, Model: model, Effort: effort, EffectiveCWD: planDir, Egress: ed,
		TimeoutSec: timeoutSec, Live: live, Force: force, Origin: origin, Desc: desc, RF: rf, SF: sf}, cfg)
}

func runGeminiLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	cfg := orchcfg.Load(configPath())
	planDir, planCleanup, ed := egress.Plan(freelane.LaneGemini, cwd, egressOptions(cfg))
	defer planCleanup()
	return runFreeLane(out, freeReq{Lane: freelane.LaneGemini, Prompt: prompt, Model: model, Effort: effort, EffectiveCWD: planDir, Egress: ed,
		TimeoutSec: timeoutSec, Live: live, Force: force, Origin: origin, Desc: desc, RF: rf, SF: sf}, cfg)
}

// freeWindow maps a provider's window shape onto the ledger.
func freeWindow(spec freelane.Spec) ledger.WindowKind {
	if spec.Window == freelane.WindowTrial {
		return ledger.WinTrial
	}
	return ledger.WinDay
}

// minuteClassLimit: a 429 whose retry horizon is under five minutes is a
// PER-MINUTE ceiling (RPM/TPM), not the day's allowance. Marking the day
// window exhausted for it would be wrong twice: the day is not gone, and the
// short ResetsAt would ROLL the bucket seconds later, zeroing the day's shadow
// count. The receipt still records rate_limit; only the ledger observation
// is withheld.
const minuteClassLimit = 5 * time.Minute

// applyFreeOutcome is the free lanes' post-run ledger accounting.
//
// Unit is MILLI-<unit> (requests / neurons / credits). The cap is the
// registry/config prior seeded as an ESTIMATE (S2R-3: throttle-only) until a
// vendor signal replaces it: Groq's x-ratelimit-* headers land the day window
// as a poll-authority observation and the header limit as the MEASURED cap; a
// day-class 429 or a 402 lands ObserveLimit (the only exhaustion path).
//
// Every outcome that REACHED the vendor is metered — a 429 counts against the
// day on OpenRouter by the vendor's own rule ("failed calls count"), and
// counting it everywhere is the conservative reading. spawn_error (nothing
// left the machine, or nothing came back) and config_error meter nothing.
func applyFreeOutcome(l *ledger.Ledger, spec freelane.Spec, model string, o freelane.Outcome, cfg orchcfg.Config, now time.Time) {
	lane, win := spec.Lane, freeWindow(spec)
	if b, ok := l.Bucket(lane, win); !ok || b.CapTokens == 0 {
		l.SetCapacityEstimate(lane, win, spec.DailyCap*1000)
	}
	if win == ledger.WinDay {
		l.AnchorIfUnset(lane, win, ledger.NextDailyReset(now), now)
	} else {
		// A trial pool never resets. It is anchored FAR out so the bucket is
		// capped-and-anchored (RS4) and derives a percentage from its shadow
		// count; the anchor is not a prediction of anything.
		l.AnchorIfUnset(lane, win, now.AddDate(10, 0, 0), now)
	}
	if o.Class == "spawn_error" || o.Class == "config_error" {
		return
	}
	var units int64 = 1
	if spec.Unit == freelane.UnitNeurons {
		rate, ok := cfg.FreeNeuronRate(model)
		if ok {
			units = freelane.Neurons(o.Usage, rate)
		} else {
			// Unknown model: meter a floor and SAY so. A silent zero would let
			// an unlisted 70B model eat the day unmetered.
			units = 1
			fmt.Fprintf(os.Stderr, "WARN: cloudflare model %q has no neuron rate (free_cloudflare_neuron_rates); metering 1 neuron for this dispatch — add the rate from the pricing page\n", model)
		}
	}
	l.AddShadow(lane, win, units*1000, now)

	if o.Rate.Present && win == ledger.WinDay && o.Rate.LimitRequests > 0 {
		resets := ledger.NextDailyReset(now)
		if o.Rate.ResetRequests > 0 {
			resets = now.Add(o.Rate.ResetRequests)
		}
		// The vendor's own daily figures: MEASURED cap, poll-authority reading.
		l.SetCapacity(lane, win, o.Rate.LimitRequests*1000)
		l.Observe(ledger.Observation{Lane: lane, Window: win, UsedPct: o.Rate.UsedPct(), ResetsAt: resets,
			ObservedAt: now, Source: ledger.ProviderSourcePoll}, now)
	}
	switch o.Class {
	case "rate_limit":
		if o.Rate.RetryAfter > 0 && o.Rate.RetryAfter < minuteClassLimit {
			return // per-minute ceiling: recorded on the receipt, not on the day
		}
		resume := now.Add(24 * time.Hour)
		if win == ledger.WinDay {
			resume = ledger.NextDailyReset(now)
		}
		if o.Rate.RetryAfter > 0 {
			resume = now.Add(o.Rate.RetryAfter)
		}
		l.ObserveLimit(lane, "", win, resume, now)
	case "payment_required":
		// The depletable-pool signal (nim credits gone; an openrouter balance
		// question). Nothing renews it: re-check daily — one request against
		// an empty pool is the price of noticing a top-up.
		l.ObserveLimit(lane, "", win, now.Add(24*time.Hour), now)
	}
}

// freeLaneStates derives the router state of every free lane:
//
//	off          — free_lanes_off or free_providers.<lane>.off (policy, force-proof)
//	unconfigured — no credential file at <state>/free/<lane>.token
//	gated        — gemini with an EMPTY class allowlist (never seated); a
//	               non-empty allowlist is refined per class in buildRouteDecision
//	else         — ledger admission over the lane's day/trial buckets
func freeLaneStates(snap []ledger.Bucket, cfg orchcfg.Config, stateDir string, now time.Time) map[string]router.LaneState {
	out := map[string]router.LaneState{}
	for _, lane := range freelane.Lanes {
		spec, _ := freelane.Provider(lane)
		ls := router.LaneState{WorstPct: worstPct(snap, lane, now)}
		switch {
		case cfg.FreeLanesOff || cfg.FreeLimits(lane).Off:
			ls.State = "off"
		case !freeConfigured(stateDir, lane):
			ls.State = "unconfigured"
		case spec.ClassGated && len(cfg.FreeGeminiAllowClasses) == 0:
			ls.State = "gated"
		default:
			d := admission.Decide(snap, lane, now, defaultThresholds)
			ls.State = string(d.State)
			ls.ResumeAt = d.ResumeAt
		}
		out[lane] = ls
	}
	return out
}

// freeConfigured reports whether a usable credential file exists. Any read
// problem (absent, empty, unreadable) is "not configured" for routing —
// masking is the safe direction, and `run` reports the specific error.
func freeConfigured(stateDir, lane string) bool {
	_, err := freelane.LoadToken(stateDir, lane)
	return err == nil
}

// runFreeLane is the shared dispatcher. Every ruling is receipted in order:
// off → egress → class gate → credential → admission → (dry-run) → local RPM
// limiter → HTTP → ledger → receipt → breaker → output.
func runFreeLane(out io.Writer, r freeReq, cfg orchcfg.Config) (int, error) {
	now := time.Now().UTC()
	spec, ok := freelane.Resolve(r.Lane, cfg.FreeLimits(r.Lane))
	if !ok {
		return 1, fmt.Errorf("run: %q is not a free-provider lane", r.Lane)
	}
	baseRec := func(class, state, reason string) dispatch.Record {
		rec := dispatch.Record{
			TS: now, Lane: r.Lane, Model: r.Model, OutcomeClass: class,
			Origin: r.Origin, TaskClass: r.RF.TaskClass, RecLane: r.RF.RecLane, RecModel: r.RF.RecModel,
			RecRule: r.RF.RecRule, Deviated: r.RF.Deviated, DeviationReason: r.RF.DeviationReason,
			Batch: r.RF.Batch, SpendDownBoost: r.RF.SpendDownBoost,
			Admit: false, AdmitState: state, AdmitReason: reason, Desc: r.Desc,
			EgressGate: r.Egress.Reason,
		}
		r.SF.stamp(&rec)
		return rec
	}
	refuse := func(kind, reason string, exit int) (int, error) {
		warnIf(dispatch.Append(dispatchPath(), baseRec(kind, kind, reason)), "dispatch append ("+kind+")")
		fmt.Fprintln(os.Stderr, "BLOCKED:", reason)
		b, _ := json.MarshalIndent(map[string]any{kind: true, "lane": r.Lane, "reason": reason}, "", "  ")
		fmt.Fprintln(out, string(b))
		return exit, nil
	}

	// 1. Policy off-switch. Force-proof: a switched-off lane is operator
	//    policy, not a quota judgement --force may override (the retirement
	//    class).
	if cfg.FreeLanesOff {
		return refuse("lane_off", fmt.Sprintf("free lane %s is switched off (free_lanes_off: true in config.json); set it false to re-enable — --force does not apply", r.Lane), exitDeferred)
	}
	if cfg.FreeLimits(r.Lane).Off {
		return refuse("lane_off", fmt.Sprintf("free lane %s is switched off (free_providers.%s.off: true in config.json); set it false to re-enable — --force does not apply", r.Lane, r.Lane), exitDeferred)
	}
	// 2. Data boundary (decided by the wrapper via egress.Plan, before quota,
	//    force-proof). The lane sends only the prompt, but a REQUESTED cwd is
	//    the caller asserting repository context, and that assertion is judged
	//    by the one rule every third-party lane shares.
	if !r.Egress.Allowed {
		rec := baseRec("egress_denied", "egress_denied", r.Egress.Reason)
		warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (egress denial)")
		fmt.Fprintln(os.Stderr, "BLOCKED:", r.Egress.Reason)
		b, _ := json.MarshalIndent(map[string]any{"egress_denied": true, "lane": r.Lane, "reason": r.Egress.Reason}, "", "  ")
		fmt.Fprintln(out, string(b))
		return exitEgressDenied, nil
	}
	// 3. Non-sensitive class gate (gemini). Force-proof: the operator's
	//    2026-07-23 ruling is that this lane is NOT SEATED without it.
	if spec.ClassGated && !cfg.FreeGeminiClassAllowed(r.RF.TaskClass) {
		return refuse("gated", fmt.Sprintf("free lane %s is gated: task class %q is not in free_gemini_allow_classes (%v). Free-tier prompts train Google models, so only operator-allowlisted NON-SENSITIVE classes may dispatch here (operator ruling 2026-07-23; multi-brand isolation). Pass --class with an allowlisted class or extend the allowlist — --force does not apply",
			r.Lane, r.RF.TaskClass, cfg.FreeGeminiAllowClasses), exitDeferred)
	}
	// 4. Credential file. Absent = a typed deferral naming the path (the lane
	//    routes as "unconfigured" too); unreadable/empty = config error.
	token, err := freelane.LoadToken(stateDir(), r.Lane)
	if err != nil {
		if errors.Is(err, freelane.ErrUnconfigured) {
			g := gateResult{Admit: false, State: "unconfigured", Reason: err.Error()}
			warnIf(dispatch.Append(dispatchPath(), baseRec("deferred", g.State, g.Reason)), "dispatch append (unconfigured)")
			b, _ := json.MarshalIndent(map[string]any{"deferred": true, "unconfigured": true, "lane": r.Lane, "reason": g.Reason}, "", "  ")
			fmt.Fprintln(out, string(b))
			return exitDeferred, nil
		}
		return 1, fmt.Errorf("run --lane %s: %w", r.Lane, err)
	}
	// 5. Quota admission over the lane's day/trial buckets (R11: --force
	//    outranks, loudly).
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	g := laneGate(l.Snapshot(), r.Lane, now, defaultThresholds, r.Force)
	if !g.Admit {
		warnIf(dispatch.Append(dispatchPath(), baseRec("deferred", g.State, g.Reason)), "dispatch append (deferral)")
		fmt.Fprintln(out, string(deferralJSON(g)))
		return exitDeferred, nil
	}
	if g.Forced {
		fmt.Fprintln(os.Stderr, "WARN:", g.Reason)
	}
	// Argv-equivalent contract, validated before any --live attempt: the
	// endpoint (cloudflare account id) and the body (model pin, :free rule).
	endpoint, err := freelane.Endpoint(spec, cfg.FreeCloudflareAccountID)
	if err != nil {
		return 1, err
	}
	req := freelane.RunReq{Lane: r.Lane, Model: r.Model, Prompt: r.Prompt, Effort: r.Effort, Token: token,
		AccountID: cfg.FreeCloudflareAccountID, MaxTokens: cfg.FreeMaxTokens, TimeoutSec: r.TimeoutSec}
	if req.TimeoutSec <= 0 || req.TimeoutSec > cfg.FreeTimeoutSec {
		req.TimeoutSec = cfg.FreeTimeoutSec
	}
	body, err := freelane.BuildBody(req)
	if err != nil {
		return 1, err
	}
	if !r.Live {
		// Dry-run never prints the token; the endpoint, model, gate decision
		// and the directory the gate resolved are the complete story of what
		// WOULD leave the machine.
		b, _ := json.MarshalIndent(map[string]any{
			"dry_run": true, "admit": true, "admit_state": g.State, "admit_reason": g.Reason, "forced": g.Forced,
			"lane": r.Lane, "model": r.Model, "endpoint": endpoint, "body_bytes": len(body),
			"rpm": spec.RPM, "daily_cap": spec.DailyCap, "unit": spec.Unit, "window": spec.Window,
			"egress_gate": r.Egress.Reason, "effective_cwd": r.EffectiveCWD,
		}, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0, nil
	}
	// 6. Local per-minute limiter (W6 shape: a LOCAL rate limit, typed so it
	//    can never be recorded as the vendor's 429). Fail OPEN on a wedged
	//    lock, visibly.
	if spec.RPM > 0 {
		allow, retryAt, lwarn, derr := slidewin.Decide(freeLimiterPath(r.Lane), now, spec.RPM, time.Minute)
		if lwarn != "" {
			fmt.Fprintln(os.Stderr, "warn:", lwarn)
		}
		if derr != nil {
			resilAlert(r.Lane+" limiter", derr)
			allow = true
		} else {
			resilAlertClear()
		}
		if !allow {
			detail := fmt.Sprintf("%s lane sliding-window limit (%d/60s) reached; earliest retry %s", r.Lane, spec.RPM, retryAt.UTC().Format(time.RFC3339))
			rec := baseRec("rate_limit", "open", detail)
			rec.RateLimitOrigin = "local"
			warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (local limiter)")
			b, _ := json.Marshal(map[string]string{"outcome_class": "rate_limit", "rate_limit_origin": "local", "detail": detail, "retry_at": retryAt.UTC().Format(time.RFC3339)})
			fmt.Fprintln(out, string(b))
			fmt.Fprintf(os.Stderr, "outcome %q is not ok (exit %d)\n", "rate_limit", exitNotOK)
			return exitNotOK, nil
		}
	}
	// 7. The dispatch.
	o, raw, err := freelane.Run(context.Background(), req)
	if err != nil {
		return 1, err // config_error: never reached the network
	}
	warnIf(updateLedger(func(fresh *ledger.Ledger) {
		applyFreeOutcome(fresh, spec, r.Model, o, cfg, now)
	}), "ledger update (post-run)")
	served := o.Model
	if served == "" {
		served = r.Model
	}
	rec := dispatch.Record{
		TS: now, Lane: r.Lane, Model: served, OutcomeClass: o.Class, RateLimitOrigin: upstreamRLO(o.Class, ""),
		Admit: true, AdmitState: g.State, AdmitReason: g.Reason,
		TokensIn: o.Usage.Prompt, TokensOut: o.Usage.Completion, NumTurns: 1,
		Origin: r.Origin, TaskClass: r.RF.TaskClass, RecLane: r.RF.RecLane, RecModel: r.RF.RecModel,
		RecRule: r.RF.RecRule, Deviated: r.RF.Deviated, DeviationReason: r.RF.DeviationReason,
		Batch: r.RF.Batch, SpendDownBoost: r.RF.SpendDownBoost, Desc: r.Desc, EgressGate: r.Egress.Reason,
	}
	if served != r.Model {
		rec.AttributedModels = []string{served}
	}
	r.SF.stamp(&rec)
	warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append")
	noteLaneHealth(cfg.ExclusionOff, r.Lane, o.Class, now) // W6 breaker (spawn_error/parse_error arm it; vendor errors do not)
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
