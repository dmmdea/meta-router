package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/claudelane"
	"github.com/dmmdea/meta-router/internal/orch/codexlane"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/egress"
	"github.com/dmmdea/meta-router/internal/orch/exclusion"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/glmlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/locallane"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/slidewin"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// warnIfExcluded tells the operator when a DIRECT dispatch (--lane / --force,
// which bypass routing) targets a breaker-excluded lane — otherwise the only
// symptom is a generic "unavailable" mask in route consults and the operator
// has no pointer to the self-heal time or to the fact that it is OUR breaker.
func warnIfExcluded(off bool, lane string, now time.Time) {
	if off {
		return
	}
	s, _ := exclusion.Load(exclusionsPath())
	if on, _ := exclusion.Excluded(s, lane, now); on {
		fmt.Fprintln(os.Stderr, "note: "+lane+" is breaker-"+exclusion.Reason(s, lane)+"; this direct dispatch proceeds and its outcome still counts")
	}
}

// upstreamRLO types a receipt's rate-limit origin (W6): keep an origin the
// outcome already carries; otherwise a rate_limit on a lane that classifies
// ONLY vendor output is "upstream" by construction. Receipts must carry the
// distinction on EVERY lane or the local-vs-upstream count is a lie told by
// omission (review 2026-08-12).
func upstreamRLO(class, existing string) string {
	if existing != "" {
		return existing
	}
	if class == "rate_limit" {
		return "upstream"
	}
	return ""
}

// laneGate is the non-claude lane admission: pure ledger admission only —
// billing-mode and the fable fuse are claude-lane rules (the GLM 1313 latch
// joins in Task 6). Same R11 semantics as gate(): --force outranks, loudly.
func laneGate(bs []ledger.Bucket, lane string, now time.Time, th admission.Thresholds, force bool) gateResult {
	d := admission.Decide(bs, lane, now, th)
	if !d.Admit {
		if force {
			return gateResult{Admit: true, State: string(d.State), Forced: true,
				Reason: "FORCED past " + d.Reason + " (operator override, R11)"}
		}
		return gateResult{Admit: false, State: string(d.State), Reason: d.Reason, ResumeAt: d.ResumeAt}
	}
	return gateResult{Admit: true, State: string(d.State), Reason: d.Reason}
}

// applyCodexOutcome is the codex lane's post-run ledger accounting. The ledger
// unit is MILLICREDITS × the Plus-degradation factor (config, default 15 —
// #28879). The 5h cap is a CONFIG GUESS (tier band × degradation), seeded via
// SetCapacityEstimate: S2R-3 — estimate-sourced numbers may THROTTLE admission
// but never EXHAUST it; only a real provider veto (429/turn.failed →
// rate_limit) writes the exhaustion observation (RS5 conservative resume).
// The 7d window anchors at firstUse+7d, UNCAPPED (no weekly band is known):
// shadow accumulates for fitting, percentage stays -1.
//
// Returns the 5h UsedPct the ledger PREDICTED before this run's usage landed —
// the burn-anomaly input (Task 4).
func applyCodexOutcome(l *ledger.Ledger, o codexlane.Outcome, cfg orchcfg.Config, now time.Time) (predictedUsedPct float64) {
	predictedUsedPct = -1
	if b, ok := l.Bucket("codex", ledger.Win5h); ok {
		predictedUsedPct = b.UsedPct
	}
	if b, ok := l.Bucket("codex", ledger.Win5h); !ok || b.CapTokens == 0 {
		l.SetCapacityEstimate("codex", ledger.Win5h, int64(cfg.CodexPlus5hCredits*1000)) // millicredits
	}
	l.AnchorIfUnset("codex", ledger.Win7d, now.Add(7*24*time.Hour), now)
	milli := codexlane.CreditsMilli(o.Usage, cfg.CodexDegradationFactor)
	if milli > 0 {
		l.AddShadow("codex", ledger.Win5h, milli, now)
		l.AddShadow("codex", ledger.Win7d, milli, now)
	}
	if o.Class == "rate_limit" {
		// Real provider signal — the exec stream has NO rate_limits surface
		// (fixture-proven), so the veto itself is the observation. UNGATED on
		// rate-limit origin BY DESIGN: codexlane classifies only vendor output,
		// so no locally-imposed limit can reach here. If a codex-lane limiter
		// is ever added, THIS call must gain the origin gate
		// applyRunOutcomeSubject has, or it will poison quota truth (W6).
		l.ObserveLimit("codex", "", ledger.Win5h, now.Add(5*time.Hour), now)
	}
	return predictedUsedPct
}

// runCodexLane mirrors the claude dispatch path for `run --lane codex`:
// gate → dry-run or EnsureHome+Run → ledger.Update accounting → receipt. The
// origin/desc/recFields (Task 11) thread through so the receipt carries the
// rec-vs-action adherence data (RS9/§6c) for every dispatch, not just claude.
func runCodexLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, extra []string, live, force, keepHome bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	if model == "" {
		return 1, fmt.Errorf("run: --model is required on the codex lane (pin -m; unpinned models drift with vendor defaults)")
	}
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	g := laneGate(l.Snapshot(), "codex", now, defaultThresholds, force)
	req := codexlane.RunReq{Prompt: prompt, Model: model, Effort: effort, CWD: cwd,
		TimeoutSec: timeoutSec, SkipVersionGate: force, Extra: extra}

	if !g.Admit {
		rec := dispatch.Record{
			TS: now, Lane: "codex", Model: model, OutcomeClass: "deferred",
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
		argv, err := codexlane.BuildArgs(req)
		if err != nil {
			return 1, err
		}
		b, _ := json.MarshalIndent(map[string]any{
			"dry_run": true, "admit": true, "admit_state": g.State, "admit_reason": g.Reason, "forced": g.Forced, "args": argv,
		}, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0, nil
	}

	home, cleanup, err := codexlane.EnsureHome(stateDir())
	if err != nil {
		return 1, err
	}
	if keepHome {
		fmt.Fprintln(os.Stderr, "warn: --keep-codex-home:", home, "left for debugging — delete it yourself")
	} else {
		defer cleanup()
	}
	req.Home = home
	if force {
		if v, ok := codexlane.VersionGate(); !ok {
			fmt.Fprintf(os.Stderr, "WARN: version gate SKIPPED by --force (codex %q <0.142.5 leaks prompts to trace logs)\n", v)
		}
	}
	o, raw, err := codexlane.Run(context.Background(), req)
	if err != nil {
		return 1, err // config_error: bad args / version gate — never reached the binary
	}
	predicted := -1.0
	warnIf(updateLedger(func(fresh *ledger.Ledger) {
		predicted = applyCodexOutcome(fresh, o, cfg, now)
	}), "ledger update (post-run)")
	// Burn-anomaly latch (fact-refresh gap #6): a real veto at predicted
	// headroom is a vendor-misfire signal — latch it visibly, never mutate
	// capacity from it.
	if note, anomalous := codexlane.BurnAnomaly(predicted, o.Class == "rate_limit"); anomalous {
		fmt.Fprintln(os.Stderr, "WARN:", note)
		warnIf(writeCodexAlert(codexAlertPath(), note, now), "codex alert latch")
	}
	rec := dispatch.Record{
		TS: now, Lane: "codex", Model: model, OutcomeClass: o.Class, RateLimitOrigin: upstreamRLO(o.Class, ""),
		Admit: true, AdmitState: g.State, AdmitReason: g.Reason,
		TokensIn: o.Usage.Input, TokensOut: o.Usage.Output + o.Usage.ReasoningOutput,
		NumTurns: o.Turns,
		Origin:   origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
		RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost, Desc: desc,
	}
	sf.stamp(&rec)
	warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append")
	noteLaneHealth(cfg.ExclusionOff, "codex", o.Class, now) // W6 breaker
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

// applyLocalOutcome meters the FREE local lane. S3R-10: local is free and
// always-open (route.go laneStates keeps local uncapped) — so this must NOT take
// the ledger Update lock to do nothing. There is no spend window, no shadow
// tokens, no provider observation for a local node; the ONE receipt runLocalLane
// writes IS the countable dispatch. Kept as a named no-op seam so the "free lane
// meters nothing, locklessly" contract is explicit and testable. It never
// touches *ledger.Ledger, so runLocalLane never calls ledger.Update for a local
// node — zero lock contention for zero work on every local dispatch.
func applyLocalOutcome(o claudelane.Outcome, now time.Time) {
	// Intentionally no ledger, no AddShadow / ObserveProvider: local is free
	// (S2R-4, R14) and metered by the receipt alone. No lock acquired.
	_ = o
	_ = now
}

// resilAlert latches a W6 persistence failure into a state file the operator
// can actually see: stderr is null-wired on the detached strategy path, so a
// breaker/limiter whose state stops persisting would otherwise degrade with
// zero observable trace. Best-effort by construction — if even this write
// fails, stderr is all that is left. Cleared by resilAlertClear on the next
// successful persist (self-healing signal). Surfaced in `status`.
func resilAlert(what string, err error) {
	fmt.Fprintln(os.Stderr, "warn:", what+":", err)
	b, _ := json.Marshal(map[string]string{
		"ts": time.Now().UTC().Format(time.RFC3339), "what": what, "err": err.Error()})
	_ = os.WriteFile(statepaths.ResilAlert(), b, 0o644)
}

func resilAlertClear() { _ = os.Remove(statepaths.ResilAlert()) }

// noteLaneHealth feeds the W6 self-healing breaker from a dispatch outcome,
// through the LOCKED read-modify-write (parallel dispatches would otherwise
// lose updates silently — e.g. a concurrent heal overwritten by a stale
// armed set). Qualifying failures are ADAPTER classes only — spawn_error and
// parse_error on every lane (our binary/contract broke), plus api_error on
// the free local lane (a harness tier fault; the cloud lanes' vendor-error
// classes — claude/glm "api_error", codex "error" — are the VENDOR's
// incident, which quota/admission owns, and never arm this breaker). ok and
// deferred are HEALTHY (an honest defer is the local lane working as
// designed) and clear the breaker. Everything else (rate_limit, refusal,
// empty_result, incomplete) is neither: quota and policy signals never open
// an infrastructure breaker. (config_error structurally never reaches here —
// every lane runner returns its error before the receipt.) A dispatch to a
// currently-excluded lane (--force / direct --lane) still records honestly;
// the Reason surface in `status` shows the operator the sliding self-heal.
func noteLaneHealth(off bool, lane, class string, now time.Time) {
	if off { // the caller's ALREADY-LOADED cfg decides — a config edit landing
		return // mid-dispatch must not make recorder and router disagree
	}
	warn, err := exclusion.Update(exclusionsPath(), func(s exclusion.Set) (exclusion.Set, bool) {
		switch class {
		case "spawn_error", "parse_error":
			return exclusion.RecordFailure(s, lane, class, now, exclusion.Defaults()), true
		case "api_error":
			if lane != "local" {
				return s, false
			}
			return exclusion.RecordFailure(s, lane, class, now, exclusion.Defaults()), true
		case "ok", "deferred":
			if _, tracked := s[lane]; !tracked {
				return s, false // nothing to clear — skip the write
			}
			return exclusion.RecordSuccess(s, lane), true
		default:
			return s, false
		}
	})
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	if err != nil {
		resilAlert("exclusion update", err)
		return
	}
	resilAlertClear()
}

// runLocalLane dispatches `run --lane local` through the S3R-1 TWO-DOOR
// local-offload black-box adapter, keyed on the resolved class/model:
//
//   - cascade door (offload-harness) for the grunt/verify classes + cascade
//     models; a structured DEFER comes back as `deferred` → exitDeferred
//     (relegation, so the DAG escalates to a cloud alternative).
//   - agent door (local-agent) for the agentic class/model.
//
// No gate (the free lane is always open, fail-open); a cold endpoint / missing
// binary comes back CLASSIFIED as spawn_error (relegation, never a crash). The
// ONE receipt threads origin/desc/rf + the strategy fields (sf.stamp) exactly
// like every other lane — the executor appends NO second line (S3R-4). S3R-10:
// NO ledger.Update is taken for the free local node.
func runLocalLane(out io.Writer, prompt, class, model, cwd string, timeoutSec int, live bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	root := cwd
	if root == "" {
		root = "."
	}
	d, verb := locallane.ResolveDoor(class, model)
	bin := cfg.LocalOffloadBin
	if d == locallane.DoorAgent {
		bin = cfg.LocalAgentBin
	}

	if !live {
		dry := map[string]any{"dry_run": true, "lane": "local", "root": root, "bin": bin}
		if d == locallane.DoorAgent {
			dry["door"] = "agent"
		} else {
			dry["door"] = "cascade"
			dry["verb"] = verb
		}
		b, _ := json.MarshalIndent(dry, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0, nil
	}

	// W6 sliding-window limiter: the local box serves interactive work too; a
	// headless burst must not wedge it. A denial is a LOCAL rate limit — typed
	// so it can never read as vendor exhaustion — and relegates (exit 3) like
	// an honest defer, so the DAG escalates to a cloud alternative. Negative
	// local_max_per_min disables; state failures fail OPEN (warned).
	warnIfExcluded(cfg.ExclusionOff, "local", now)
	if lim := cfg.LocalMaxPerMin; lim > 0 {
		allow, retryAt, warn, derr := slidewin.Decide(localLimiterPath(), now, lim, time.Minute)
		if warn != "" {
			fmt.Fprintln(os.Stderr, "warn:", warn)
		}
		if derr != nil {
			// Fail OPEN: a wedged lock or unpersistable state must not brick
			// the free lane — but it must be VISIBLE (latched; stderr is
			// null-wired on the strategy path).
			resilAlert("local limiter", derr)
			allow = true
		} else {
			resilAlertClear()
		}
		if !allow {
			o := claudelane.Outcome{Class: "rate_limit", RateLimitOrigin: "local",
				Result: fmt.Sprintf("local lane sliding-window limit (%d/60s) reached; earliest retry %s", lim, retryAt.UTC().Format(time.RFC3339))}
			rec := dispatch.Record{
				TS: now, Lane: "local", Model: model, OutcomeClass: o.Class, RateLimitOrigin: o.RateLimitOrigin,
				Admit: false, AdmitState: "open", AdmitReason: o.Result,
				Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
				RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost, Desc: desc,
			}
			sf.stamp(&rec)
			warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (local limiter)")
			b, _ := json.Marshal(map[string]string{"outcome_class": o.Class, "rate_limit_origin": o.RateLimitOrigin, "detail": o.Result})
			fmt.Fprintln(out, string(b))
			fmt.Fprintf(os.Stderr, "local limiter: %s (exit %d, relegation)\n", o.Result, exitDeferred)
			return exitDeferred, nil
		}
	}

	var o claudelane.Outcome
	var raw []byte
	var err error
	if d == locallane.DoorAgent {
		o, raw, err = locallane.Run(context.Background(), bin, prompt, root, timeoutSec)
	} else {
		o, raw, err = locallane.RunCascade(context.Background(), bin, verb, prompt, desc, timeoutSec)
	}
	if err != nil {
		return 1, err // reserved for nothing: the adapter is fail-open (classified Outcome)
	}
	// W6: feed the self-healing breaker (ok/deferred heal; adapter classes arm).
	noteLaneHealth(cfg.ExclusionOff, "local", o.Class, now)
	// S3R-10: free lane — meter without any ledger lock (no-op seam).
	applyLocalOutcome(o, now)
	rec := dispatch.Record{
		TS: now, Lane: "local", Model: model, OutcomeClass: o.Class, RateLimitOrigin: upstreamRLO(o.Class, o.RateLimitOrigin),
		Admit: true, AdmitState: "open", NumTurns: o.NumTurns,
		Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
		RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost, Desc: desc,
	}
	sf.stamp(&rec)
	warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (local)")
	if len(raw) > 0 {
		fmt.Fprintln(out, string(raw))
	} else {
		b, _ := json.Marshal(map[string]string{"outcome_class": o.Class, "detail": o.Result})
		fmt.Fprintln(out, string(b))
	}
	// A structured DEFER is a RELEGATION, not a failure: exit 3 so the DAG (and a
	// scheduled wrapper) escalates to a cloud alternative rather than re-laning.
	if o.Class == "deferred" {
		fmt.Fprintf(os.Stderr, "local cascade deferred: %s (exit %d, relegation)\n", o.Result, exitDeferred)
		return exitDeferred, nil
	}
	if o.Class != "ok" {
		fmt.Fprintf(os.Stderr, "outcome %q is not ok (exit %d)\n", o.Class, exitNotOK)
		return exitNotOK, nil
	}
	return 0, nil
}

// glmGate is the GLM admission: the 1313 latch PRECEDES ledger admission, and
// it is the ONE place --force yields — >3 Fair-Usage violations = ban, an
// account-loss event; R11 override still exists via ack-then-run, a
// deliberate two-step (`probe --ack-glm`).
func glmGate(bs []ledger.Bucket, alertPath string, now time.Time, th admission.Thresholds, force bool) gateResult {
	if note, latched := glmlane.Latched(alertPath); latched {
		return gateResult{Admit: false, State: "hard_stop",
			Reason: "GLM 1313 latched: " + note + " — --force yields here; clear with `mr-orchestrate probe --ack-glm`, then re-run"}
	}
	return laneGate(bs, "glm", now, th, force)
}

// applyGLMOutcome is the GLM lane's post-run ledger accounting. S2R-2: the
// ledger unit is PROMPT-UNITS from the very first dispatch — one -p invocation
// meters Multiplier(model, now) units; the outcome's token counts NEVER
// token-scale into glm buckets (a token-scaled shadow would blow past the
// 80-prompt cap on run one and self-brick the lane). A spawn_error never
// reached the provider, so it meters nothing.
//
// Error side-effects (Task 6, fact refresh §3): the raw body is scanned for
// GLM 13xx codes — cooldown (1308/1316) exhausts the 5h window until the
// embedded next_flush_time (past/absent → RS5 now+5h); the offline class
// (1310/1317–1321) exhausts the weekly window (flush or RS5 now+24h, daily
// re-check); retry/config/unknown write nothing (fail-open). 1313 surfaces to
// the caller for the latch. S2R-8: a rate_limit with NO parseable 13xx code
// gets the generic claude-style 5h→100% + RS5 resume so the next invocation
// defers instead of re-hammering.
func applyGLMOutcome(l *ledger.Ledger, o claudelane.Outcome, raw []byte, model string,
	cfg orchcfg.Config, fzs []fuses.Fuse, now time.Time) (glmlane.GLMErr, bool) {
	if o.Class == "spawn_error" {
		return glmlane.GLMErr{}, false
	}
	// S2R-2 state migration, exactly-once by construction: it can only fire
	// while the cap is UNSEEDED (pre-Task-7 state), and shadow beyond even the
	// weekly prompt budget is unambiguously token-scale, not prompts.
	for _, w := range []ledger.WindowKind{ledger.Win5h, ledger.Win7d} {
		if b, ok := l.Bucket("glm", w); ok && b.CapTokens == 0 && b.ShadowTokens > 5*cfg.GLM5hPrompts {
			l.ClearShadow("glm", w, now)
		}
	}
	// Seed caps on first touch: 5h from config (Lite default 80); weekly is
	// 5× the 5h cap, NEVER 10× (fact refresh §1). SetCapacity, not estimate:
	// these are the provider's DOCUMENTED plan quotas, and the 429 code
	// classes provide the real denial signal regardless.
	if b, ok := l.Bucket("glm", ledger.Win5h); !ok || b.CapTokens == 0 {
		l.SetCapacity("glm", ledger.Win5h, cfg.GLM5hPrompts)
	}
	if b, ok := l.Bucket("glm", ledger.Win7d); !ok || b.CapTokens == 0 {
		l.SetCapacity("glm", ledger.Win7d, 5*cfg.GLM5hPrompts)
	}
	l.AnchorIfUnset("glm", ledger.Win7d, now.Add(7*24*time.Hour), now)
	units := glmlane.Multiplier(model, now, fzs) // ONE prompt = one -p invocation, priced by time-of-day
	l.AddShadow("glm", ledger.Win5h, units, now)
	l.AddShadow("glm", ledger.Win7d, units, now)

	e, classified := glmlane.ClassifyError(raw, now)
	if classified {
		switch e.Action {
		case glmlane.ActCooldown:
			resume := e.NextFlush
			if !resume.After(now) { // absent OR stale flush: RS5 conservative
				resume = now.Add(5 * time.Hour)
			}
			l.ObserveLimit("glm", "", ledger.Win5h, resume, now)
		case glmlane.ActOffline:
			resume := e.NextFlush
			if !resume.After(now) {
				resume = now.Add(24 * time.Hour) // RS5: re-checked daily
			}
			l.ObserveLimit("glm", "", ledger.Win7d, resume, now)
		}
		return e, true
	}
	if o.Class == "rate_limit" { // S2R-8 generic fallback
		l.ObserveLimit("glm", "", ledger.Win5h, now.Add(5*time.Hour), now)
	}
	return glmlane.GLMErr{}, false
}

// latchGLMHardStop is the lock-INDEPENDENT 1313 latch write (A2R-#3). It is a
// pure function of the raw result body: it classifies `raw` for a GLM 13xx
// code and, when the code is the 1313 Fair-Usage hard-stop, writes the latch
// (atomic create-once, first violation stands) and WARNs. Critically it does
// NOT run inside the ledger.Update closure, so a lock-busy Update never drops a
// Fair-Usage strike. Returns whether the latch fired (for test/observation).
func latchGLMHardStop(raw []byte, alertPath string, now time.Time) bool {
	e, ok := glmlane.ClassifyError(raw, now)
	if !ok || e.Action != glmlane.ActHardStop {
		return false
	}
	warnIf(glmlane.LatchAlert(alertPath, e, now), "glm 1313 latch")
	fmt.Fprintf(os.Stderr, "WARN: GLM 1313 Fair-Usage violation — lane LATCHED hard-stop (>3 violations = ban); clear with `mr-orchestrate probe --ack-glm`\n")
	return true
}

// runGLMLane mirrors the claude dispatch path for `run --lane glm`: gate →
// dry-run or token+env dispatch through the claude binary → ledger.Update
// prompt-unit accounting → receipt. R10: the token is resolved only on the
// --live path, lives only in the child's env, and appears in no output.
func runGLMLane(out io.Writer, prompt, model, effort, cwd string, timeoutSec int, extra []string, live, force bool, origin, desc string, rf recFields, sf strategyFields) (int, error) {
	if model == "" {
		model = glmlane.DefaultModel // R14a: the lane defaults to its strongest model
	}
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	fzs, _ := fuses.Load(fusesPath())
	egressReason := ""
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	// DATA BOUNDARY, resolved before anything is sent and BEFORE the quota gate.
	// egress.Plan resolves the EFFECTIVE working directory — an empty CWD means
	// the child inherits the orchestrator's cwd, not "no context" — and either
	// allowlists it, enforces genuine prompt-only in a neutral directory, or
	// refuses. --force must not bypass it: a data export is not a quota
	// judgement (same force-proof class as the R10 billing hard-stop).
	// --extra can widen the child's reach past cwd (--add-dir), so every such
	// directory is gated too.
	egressOpt := egress.Options{AllowRepos: cfg.GLMAllowRepos, PromptOnlyDenied: cfg.EgressPromptOnlyDenied}
	denyEgress := func(reason string) (int, error) {
		rec := dispatch.Record{
			TS: now, Lane: "glm", Model: model, OutcomeClass: "egress_denied",
			Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
			RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason,
			Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost,
			Admit: false, AdmitState: "egress_denied", AdmitReason: reason, Desc: desc,
			EgressGate: reason,
		}
		sf.stamp(&rec)
		warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (egress denial)")
		fmt.Fprintln(os.Stderr, "BLOCKED:", reason)
		b, _ := json.MarshalIndent(map[string]any{
			"egress_denied": true, "lane": "glm", "reason": reason,
		}, "", "  ")
		fmt.Fprintln(out, string(b))
		return exitEgressDenied, nil
	}
	planDir, planCleanup, ed := egress.Plan("glm", cwd, egressOpt)
	defer planCleanup()
	if !ed.Allowed {
		return denyEgress(ed.Reason)
	}
	// --extra is forwarded VERBATIM to the child, so it is a second export
	// channel with the same reach as cwd. Gate every path it carries, and refuse
	// anything the gate cannot account for: --add-dir is variadic in the shipped
	// binary, so a token the extractor does not model is a token that reaches the
	// provider ungated (review 2026-07-25).
	if bad := egress.RefuseExtras(extra); len(bad) > 0 {
		return denyEgress("third-party lane glm denied: --extra contains " +
			strconv.Itoa(len(bad)) + " token(s) that may name a filesystem path: " +
			strings.Join(bad, " ") +
			" — they are forwarded VERBATIM to the provider and resolved by the child against ITS working directory, so this gate cannot judge what they point at. Repository context reaches a third-party lane through -cwd plus glm_allow_repos, which is gated in one place with one rule.")
	}
	cwd = planDir // ALWAYS explicit: never let the child inherit our cwd
	egressReason = ed.Reason

	g := glmGate(l.Snapshot(), glmAlertPath(), now, defaultThresholds, force)
	req := claudelane.RunReq{Prompt: prompt, Model: model, Effort: effort, CWD: cwd,
		TimeoutSec: timeoutSec, Extra: extra}

	if !g.Admit {
		rec := dispatch.Record{
			TS: now, Lane: "glm", Model: model, OutcomeClass: "deferred",
			Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
			RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost,
			Admit: false, AdmitState: g.State, AdmitReason: g.Reason, Desc: desc,
			// The gate already ruled before quota was consulted; omitting it here
			// made "every decision lands on the receipt" false for every deferral.
			EgressGate: egressReason,
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
		// Dry-run never reads the token: argv is token-free by construction
		// (env-only auth, R10), so the printed args are the complete story.
		argv, err := claudelane.BuildArgs(req)
		if err != nil {
			return 1, err
		}
		b, _ := json.MarshalIndent(map[string]any{
			"dry_run": true, "admit": true, "admit_state": g.State, "admit_reason": g.Reason,
			"forced": g.Forced, "args": argv, "base_url": glmlane.BaseURL,
			// The data-boundary decision and the directory the child would
			// ACTUALLY run in — without these a dry-run cannot show what would
			// leave the machine, which is the whole question for this lane.
			"egress_gate": egressReason, "effective_cwd": cwd,
		}, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0, nil
	}

	// S2R-6 cadence hygiene (config-gated, ships ON): max concurrency 1 + a
	// jittered inter-dispatch interval — the GLM ban class fires on PATTERN,
	// not volume. Only a genuinely held lock relegates; fs anomalies fail open.
	if cfg.GLMPacing {
		unlock, waited, perr := glmlane.Pace(stateDir(),
			glmlane.DefaultPace(cfg.GLMPaceMinSec, cfg.GLMPaceJitterSec), time.Now, time.Sleep)
		if perr != nil {
			g := gateResult{Admit: false, State: "paced", Reason: perr.Error(),
				ResumeAt: time.Now().UTC().Add(time.Duration(cfg.GLMPaceMinSec) * time.Second)}
			rec := dispatch.Record{
				TS: now, Lane: "glm", Model: model, OutcomeClass: "deferred",
				Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
				RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost,
				Admit: false, AdmitState: g.State, AdmitReason: g.Reason, Desc: desc,
				EgressGate: egressReason,
			}
			sf.stamp(&rec)
			warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (deferral)")
			fmt.Fprintln(out, string(deferralJSON(g)))
			return exitDeferred, nil
		}
		defer unlock()
		if waited > 0 {
			fmt.Fprintf(os.Stderr, "glm pacing: waited %s before dispatch (S2R-6 cadence hygiene)\n", waited.Round(time.Second))
		}
	}

	o, raw, err := glmlane.Run(context.Background(), req, glmTokenPath())
	if err != nil {
		return 1, err // config_error: missing token / bad args — never reached the binary
	}
	// A2R-#3: the 1313 latch decision is a pure function of `raw` and its write
	// target (glmAlertPath) is state SEPARATE from the ledger — so it must NOT
	// depend on acquiring the ledger lock. Classify + latch BEFORE ledger.Update:
	// a lock-busy Update (concurrent status/probe/run) must never silently drop a
	// Fair-Usage strike and re-admit into a banned-risk account.
	latchGLMHardStop(raw, glmAlertPath(), now)
	classified := false
	warnIf(updateLedger(func(fresh *ledger.Ledger) {
		_, classified = applyGLMOutcome(fresh, o, raw, model, cfg, fzs, now)
	}), "ledger update (post-run)")
	if !classified && o.Class == "rate_limit" {
		fmt.Fprintln(os.Stderr, "WARN: glm 429 carried no parseable 13xx code — generic 5h cooldown applied (S2R-8); the raw body below is fixture-promotion material")
	}
	attributed := make([]string, 0, len(o.ModelUsage))
	var in, outTok int64
	for m, u := range o.ModelUsage {
		attributed = append(attributed, m)
		in += u.InputTokens
		outTok += u.OutputTokens
	}
	sort.Strings(attributed)
	rec := dispatch.Record{
		TS: now, Lane: "glm", Model: model, AttributedModels: attributed, OutcomeClass: o.Class, RateLimitOrigin: upstreamRLO(o.Class, o.RateLimitOrigin),
		Admit: true, AdmitState: g.State, AdmitReason: g.Reason,
		TokensIn: in, TokensOut: outTok, NumTurns: o.NumTurns, NotionalUSD: o.NotionalUSD,
		Origin: origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
		RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost, Desc: desc, EgressGate: egressReason,
	}
	sf.stamp(&rec)
	warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append")
	noteLaneHealth(cfg.ExclusionOff, "glm", o.Class, now) // W6 breaker
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
