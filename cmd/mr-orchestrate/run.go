package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/claudelane"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/jitter"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/profiles"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/router"
)

// Exit codes (machine contract for scheduled wrappers):
//
//	0 ok · 1 config/usage error · 3 deferred (relegation, resume_at valid)
//	4 notional guard tripped · 5 dispatched but outcome not ok
//	(spawn_error/parse_error/refusal/api_error/rate_limit/empty_result)
const (
	exitDeferred = 3
	exitNotional = 4
	exitNotOK    = 5
)

// gateResult is the pre-dispatch admission verdict, pure and unit-testable.
type gateResult struct {
	Admit    bool
	State    string
	Reason   string
	ResumeAt time.Time
	Forced   bool
}

// gate composes the RS7 billing-mode hard-stop, the dated-fuse model rules
// (R10: fable is not a runtime lane after the carve-out expires), and ledger
// admission. Any billing mode other than the exact "subscription" hard-stops:
// "credits" by R10 (spend, never), anything else fail-SAFE (garbled operator
// intent must not silently become the permissive mode). --force outranks
// everything (R11) but is always marked Forced so it warns and audits.
func gate(bs []ledger.Bucket, lane, model string, fzs []fuses.Fuse, now time.Time, cfg orchcfg.Config, force bool, th admission.Thresholds) gateResult {
	return gateSubject(bs, lane, "", model, fzs, now, cfg, force, th)
}

// gateSubject is gate scoped to a credential subject (W2; ""=default). Only
// the admission read is subject-aware — the billing/fable/force rules are
// account-independent.
func gateSubject(bs []ledger.Bucket, lane, subject, model string, fzs []fuses.Fuse, now time.Time, cfg orchcfg.Config, force bool, th admission.Thresholds) gateResult {
	deny := func(state, reason string) gateResult {
		if force {
			return gateResult{Admit: true, State: state, Forced: true,
				Reason: "FORCED past " + reason + " (operator override, R11)"}
		}
		return gateResult{Admit: false, State: state, Reason: reason}
	}
	// A2R-#1: the billing-mode hard-stop is FORCE-PROOF (never consults `force`),
	// exactly like the GLM 1313 latch. R10: --force must NEVER convert quota into
	// real dollar spend — the only place --force yields is that account-loss
	// latch. Any billing mode other than the exact "subscription" (credits by
	// R10; anything else fail-safe against a garbled hand-edit) denies
	// UNCONDITIONALLY, even under --force. Quota/model rules below stay
	// force-overridable via deny().
	if cfg.ClaudeBillingMode != orchcfg.BillingSubscription {
		return gateResult{Admit: false, State: "hard_stop",
			Reason: fmt.Sprintf("claude_billing_mode=%q: R10 hard-stop, FORCE-PROOF (only \"subscription\" dispatches; credits are real spend — --force does NOT bypass)", cfg.ClaudeBillingMode)}
	}
	if strings.Contains(strings.ToLower(model), "fable") && !fuseActive(fzs, "fable-carveout", now) {
		return deny("model_retired", "fable-carveout fuse expired 2026-07-07: fable is usage-credits-only, NOT a runtime lane (R10)")
	}
	d := admission.DecideSubject(bs, lane, subject, now, th)
	if !d.Admit {
		g := deny(string(d.State), d.Reason)
		g.ResumeAt = d.ResumeAt
		return g
	}
	return gateResult{Admit: true, State: string(d.State), Reason: d.Reason}
}

func fuseActive(fzs []fuses.Fuse, name string, now time.Time) bool {
	for _, f := range fuses.Active(fzs, now) {
		if f.Name == name {
			return true
		}
	}
	return false
}

type deferral struct {
	Deferred bool       `json:"deferred"`
	ResumeAt *time.Time `json:"resume_at"`
	// RetryAt is the E4 jittered scheduling hint (resume_at + uniform[0,90s)) so
	// concurrent deferred callers don't re-hammer the reset boundary in lockstep.
	// resume_at stays the truthful admission estimate.
	RetryAt *time.Time `json:"retry_at,omitempty"`
	Reason  string     `json:"reason"`
}

func deferralJSON(g gateResult) []byte {
	d := deferral{Deferred: true, Reason: g.Reason}
	if !g.ResumeAt.IsZero() {
		t := g.ResumeAt
		d.ResumeAt = &t
		rt := jitter.RetryAt(g.ResumeAt, jitter.DefaultWindow, nil)
		d.RetryAt = &rt
	}
	b, _ := json.MarshalIndent(d, "", "  ")
	return b
}

func warnIf(err error, what string) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "warn:", what+":", err)
	}
}

// applyRunOutcome is the post-run ledger accounting, extracted pure so the
// rate_limit→exhaustion wiring is testable end-to-end (F13). Shadow tokens
// land in both windows; a live 429 is a provider signal, not just a receipt:
// the 5h window is marked exhausted with the RS5 conservative resume so the
// very next invocation defers instead of hammering the closed window.
func applyRunOutcome(l *ledger.Ledger, o claudelane.Outcome, now time.Time) {
	applyRunOutcomeSubject(l, "", o, now)
}

// applyRunOutcomeSubject accounts a run's usage to the credential subject that
// carried it (W2; ""=default). The subscription's usage belongs to the account
// that spent it, so a rotated dispatch's shadow + any 429 exhaustion land on
// that subject's windows, never the default's.
func applyRunOutcomeSubject(l *ledger.Ledger, subject string, o claudelane.Outcome, now time.Time) {
	_, _, _ = quotasig.IngestTraced(l, dropPath(), "", "claude", now) // default-subject tee (the primary session)
	tok := o.TotalTokens()
	l.AddShadowSubject("claude", subject, ledger.Win5h, tok, now)
	l.AddShadowSubject("claude", subject, ledger.Win7d, tok, now)
	if o.Class == "rate_limit" {
		l.ObserveProviderSubject("claude", subject, ledger.Win5h, 100, now.Add(5*time.Hour), now)
	}
}

// recFields are the RS9/§6c adherence fields stamped onto every run receipt so
// delegation obedience is COUNTABLE from receipts alone. RecLane/RecModel/
// RecRule mirror what the oracle said; Deviated + DeviationReason record when
// the action diverged (R11: recorded, never blocked). Origin is set by the
// caller from the --origin flag (S2R-1: coverage counts Origin-tagged receipts).
type recFields struct {
	TaskClass       string
	RecLane         string
	RecModel        string
	RecRule         string
	Deviated        bool
	DeviationReason string
	// E2 spend-down provenance: the batch tag and the boost the recommendation
	// carried, so boost-influenced dispatches are countable from receipts alone
	// (the calibration substrate the spend_down_* priors depend on).
	Batch          bool
	SpendDownBoost int
}

// strategyFields (S3R-4) ties the ONE receipt a lane path writes to a strategy
// DAG node. Zero on the interactive path (all omitempty on the wire). Threaded
// from runOpts through doRun into runCodexLane/runGLMLane so exactly one
// receipt per node carries the DAG identity — the executor appends NO extra line.
type strategyFields struct {
	DispatchID string
	StepID     int
	Deps       []int
	Attempt    int
}

// stamp copies the strategy identity onto a receipt in place. Cheap, so every
// Append site can call it uniformly (deferral, relegation, success).
func (sf strategyFields) stamp(r *dispatch.Record) {
	r.DispatchID = sf.DispatchID
	r.StepID = sf.StepID
	r.Deps = sf.Deps
	r.Attempt = sf.Attempt
}

// computeRunRec builds the internal route recommendation for a run. FAIL-OPEN:
// any panic/error in the oracle path is swallowed to an empty Decision so a
// broken table/ledger never blocks dispatch (the plan's fail-open law). The
// class is the explicit --class or the fallback heuristic on --desc.
func computeRunRec(classFlag, desc string, ctxTokens int64, latency bool, sd spendDownReq, now time.Time) (dec router.Decision) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintln(os.Stderr, "warn: route recommendation failed, proceeding without rec fields (fail-open):", r)
			dec = router.Decision{}
		}
	}()
	var class router.Class
	if classFlag != "" {
		class = router.Class(classFlag)
	} else {
		class, _ = router.Classify(desc, ctxTokens, latency)
	}
	cfg := orchcfg.Load(configPath())
	fzs, _ := fuses.Load(fusesPath())
	l, _ := ledger.OpenChecked(ledgerPath())
	return buildRouteDecision(cfg, fzs, l.Snapshot(), class, ctxTokens, now, sd)
}

// resolveLane reconciles the internal route recommendation with the operator's
// explicit flags (R11: the operator outranks the oracle, every override
// recorded). Pure and unit-tested.
//
//   - laneFlag == "auto": adopt the recommendation's lane/model/effort wholesale;
//     an explicit --model/--effort still overrides the adopted part. A clean
//     adoption is Deviated:false. S2R-4(b): if the rec resolves to lane "local",
//     run CANNOT dispatch it (local's front door is the local-offload MCP until
//     slice 3) — auto-fall to the first DISPATCHABLE alternative (next non-local
//     candidate) and stamp Deviated:true, DeviationReason "local-handoff". With
//     no dispatchable alternative the lane comes back "" so the caller relegates.
//   - explicit lane: use it; if it differs from the rec lane, Deviated:true with
//     DeviationReason = the --deviation arg or "operator_override".
//
// A nil/empty Decision (broken oracle) fails open: empty rec fields, the
// operator's explicit flags untouched, dispatch proceeds.
func resolveLane(rec router.Decision, laneFlag, modelFlag, effortFlag, deviation string) (lane, model, effort string, rf recFields) {
	rf = recFields{TaskClass: string(rec.Class), RecLane: rec.Lane, RecModel: rec.Model, RecRule: rec.Rule}

	pick := func(adopted, explicit string) string {
		if explicit != "" {
			return explicit
		}
		return adopted
	}

	if laneFlag == "auto" {
		// S2R-4(b): a local recommendation cannot be dispatched by run.
		if rec.Lane == "local" {
			rf.Deviated = true
			rf.DeviationReason = "local-handoff"
			for _, alt := range rec.Alternatives {
				if alt.Lane == "local" {
					continue
				}
				return pick(alt.Lane, ""), pick(alt.Model, modelFlag), pick(alt.Effort, effortFlag), rf
			}
			// No dispatchable alternative: relegate (empty lane).
			return "", modelFlag, effortFlag, rf
		}
		// Clean adoption (or an empty rec → empty lane, fail-open relegation).
		return rec.Lane, pick(rec.Model, modelFlag), pick(rec.Effort, effortFlag), rf
	}

	// Explicit lane. It differs from the rec ⇒ recorded deviation (R11).
	if rec.Lane != "" && laneFlag != rec.Lane {
		rf.Deviated = true
		if deviation != "" {
			rf.DeviationReason = deviation
		} else {
			rf.DeviationReason = "operator_override"
		}
	}
	return laneFlag, modelFlag, effortFlag, rf
}

// runOpts is the fully-parsed run request, shared by the CLI (runRun) and the
// MCP `run` tool (doRun). Extracting it lets both surfaces share ONE dispatch
// core with ONE exit-code contract — the CLI still owns flag parsing +
// os.Exit; the MCP owns marshalling doRun's return into a tool result.
type runOpts struct {
	Prompt      string
	Lane        string
	Model       string
	Effort      string
	Extra       string
	Live        bool
	Force       bool
	CWD         string
	TimeoutSec  int
	MaxNotional float64
	KeepHome    bool
	Class       string
	Desc        string
	CtxTokens   int64
	Latency     bool
	Origin      string
	Deviation   string
	// E2 spend-down (Q2): Batch tags an already-queued batch task (never set
	// on interactive dispatches); EstMinutes is its expected duration for the
	// completion-fit gate (0 = unknown → no boost). Float: fractional minutes
	// are natural on the MCP surface.
	Batch      bool
	EstMinutes float64

	// Strategy seam (S3R-4): when the strategy executor drives a DAG node
	// through doRun (Origin:"strategy"), these tie the ONE receipt doRun writes
	// to its node — so there is exactly one receipt per node, never a second one
	// appended by a prodNodeRunner on top. Zero-valued on the interactive path
	// (all omitempty on the wire). The executor sets them; it must NOT append a
	// second receipt.
	DispatchID string
	StepID     int
	Deps       []int
	Attempt    int
}

// doRun is the shared dispatch core (Task 12 refactor). It writes the result
// JSON to out and RETURNS the exit code (0/1/3/4/5) instead of calling
// os.Exit — so the MCP surface can map the code into isError without the
// process dying. runRun wraps it with os.Exit; the MCP run tool reads the
// returned code + buffered stdout. A non-nil error is a config_error (exit 1
// at the CLI). Every existing pure helper (computeRunRec/resolveLane/gate/
// applyRunOutcome + the lane funcs) is preserved unchanged.
func doRun(opts runOpts, out io.Writer) (exitCode int, err error) {
	if opts.Prompt == "" {
		return 1, fmt.Errorf("run: a prompt is required")
	}
	if opts.TimeoutSec == 0 {
		opts.TimeoutSec = 600
	}
	if opts.MaxNotional == 0 {
		opts.MaxNotional = 2.0
	}
	if opts.Origin == "" {
		opts.Origin = "cli"
	}

	nowRec := time.Now().UTC()

	// S3R-4: the strategy identity threads from runOpts into the ONE receipt
	// each lane path writes — never a second receipt appended by the executor.
	sf := strategyFields{DispatchID: opts.DispatchID, StepID: opts.StepID, Deps: opts.Deps, Attempt: opts.Attempt}

	// Every run computes the route recommendation INTERNALLY (cheap,
	// deterministic — the same buildRouteDecision the oracle uses) so
	// rec-vs-action is COUNTABLE from receipts alone. FAIL-OPEN: a broken
	// table/ledger read leaves rec empty + WARNs; a dead oracle must never
	// block dispatch.
	// Latch persistence is gated on Live: a dry-run consult previews the boost
	// but must not advance persistent spend-down state (review: no state side
	// effects from a "print the decision" surface).
	rec := computeRunRec(opts.Class, opts.Desc, opts.CtxTokens, opts.Latency, spendDownReq{
		Batch: opts.Batch, Est: time.Duration(opts.EstMinutes * float64(time.Minute)),
		Persist: opts.Batch && opts.Live,
	}, nowRec)

	// Reconcile with the operator's explicit flags (R11). --lane auto adopts the
	// rec; S2R-4(b) auto→local falls to the first dispatchable alternative.
	resolvedLane, resolvedModel, resolvedEffort, rf := resolveLane(rec, opts.Lane, opts.Model, opts.Effort, opts.Deviation)
	rf.Batch, rf.SpendDownBoost = opts.Batch, rec.SpendDownBoost
	if opts.Lane == "auto" && resolvedLane == "" {
		// auto resolved to nothing dispatchable (local with no alternative, or a
		// broken oracle) — relegate rather than dispatch a phantom.
		g := gateResult{Admit: false, State: "relegated",
			Reason: "route --lane auto resolved to a non-dispatchable recommendation (local-handoff or empty oracle); pass an explicit --lane"}
		if !rec.ResumeAt.IsZero() {
			g.ResumeAt = rec.ResumeAt
		}
		rec := dispatch.Record{
			TS: nowRec, OutcomeClass: "deferred", Origin: opts.Origin, TaskClass: rf.TaskClass,
			RecLane: rf.RecLane, RecModel: rf.RecModel, RecRule: rf.RecRule,
			Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost,
			Admit: false, AdmitState: g.State, AdmitReason: g.Reason, Desc: opts.Desc,
		}
		sf.stamp(&rec)
		warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (auto relegation)")
		fmt.Fprintln(out, string(deferralJSON(g)))
		return exitDeferred, nil
	}

	var extraArgs []string
	if opts.Extra != "" {
		extraArgs = strings.Fields(opts.Extra)
	}

	// Lane switch (slice 2): the claude path is the slice-1 contract; other
	// lanes dispatch from lanes.go. The resolved lane/model/effort + adherence
	// fields (rf) + origin thread into each lane's receipt.
	switch resolvedLane {
	case "claude":
		// falls through to the claude path below
	case "codex":
		return runCodexLane(out, opts.Prompt, resolvedModel, resolvedEffort, opts.CWD, opts.TimeoutSec, extraArgs, opts.Live, opts.Force, opts.KeepHome, opts.Origin, opts.Desc, rf, sf)
	case "glm":
		return runGLMLane(out, opts.Prompt, resolvedModel, resolvedEffort, opts.CWD, opts.TimeoutSec, extraArgs, opts.Live, opts.Force, opts.Origin, opts.Desc, rf, sf)
	case "local":
		// S3R-1: an explicit --lane local now dispatches through the two-door
		// local-offload adapter (cascade door for grunt/verify classes + cascade
		// models; agent door for the agentic class/model). The door is keyed on the
		// resolved class (rf.TaskClass) + model. resolveLane's auto→local handoff is
		// unchanged (it fires only for laneFlag=="auto", before this switch).
		return runLocalLane(out, opts.Prompt, rf.TaskClass, resolvedModel, opts.CWD, opts.TimeoutSec, opts.Live, opts.Origin, opts.Desc, rf, sf)
	default:
		return 1, fmt.Errorf("run: unknown lane %q (available: claude, codex, glm, auto)", resolvedLane)
	}

	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	fzs, _ := fuses.Load(fusesPath())
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "warn:", warn)
	}
	if _, note, ierr := quotasig.IngestTraced(l, dropPath(), quotaTracePath(), "claude", now); ierr != nil {
		fmt.Fprintln(os.Stderr, "warn: statusline drop unreadable:", ierr)
	} else if note != "" {
		fmt.Fprintln(os.Stderr, "warn:", note)
	}

	// W2: pick the credential subject (dual-account rotation). Single-profile
	// machines get the default subject + empty home — the gate/env/receipt
	// below are then byte-identical to pre-W2.
	reg, rerr0 := profiles.Load(profilesPath())
	if rerr0 != nil {
		fmt.Fprintln(os.Stderr, "warn: profiles registry invalid, default subject only:", rerr0)
	}
	sel := pickSubject(reg, l, "claude", now)
	g := gateSubject(l.Snapshot(), "claude", sel.Subject, resolvedModel, fzs, now, cfg, opts.Force, defaultThresholds)
	req := claudelane.RunReq{Prompt: opts.Prompt, Model: resolvedModel, Effort: resolvedEffort, CWD: opts.CWD, TimeoutSec: opts.TimeoutSec}
	if sel.Home != "" { // non-default subject: relocate the credential read (probe-verified 2026-07-23)
		req.Env = append(req.Env, "CLAUDE_CONFIG_DIR="+sel.Home)
	}
	if opts.Extra != "" {
		req.Extra = extraArgs
	}

	if !g.Admit {
		// Relegation, never rejection: the deferral itself is a countable
		// dispatch decision (RS9) — log it, hand back a valid resume.
		rec := dispatch.Record{
			TS: now, Lane: "claude", Model: resolvedModel, OutcomeClass: "deferred",
			Origin: opts.Origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
			RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost,
			Admit: false, AdmitState: g.State, AdmitReason: g.Reason, Desc: opts.Desc,
			Subject: sel.Subject, RotationFrom: sel.RotationFrom, RotationReason: sel.RotationReason,
		}
		sf.stamp(&rec)
		warnIf(dispatch.Append(dispatchPath(), rec), "dispatch append (deferral)")
		warnIf(ledger.Update(ledgerPath(), func(fresh *ledger.Ledger) {
			_, _, _ = quotasig.IngestTraced(fresh, dropPath(), "", "claude", now)
		}), "ledger save (deferral)")
		fmt.Fprintln(out, string(deferralJSON(g)))
		return exitDeferred, nil
	}
	if g.Forced {
		fmt.Fprintln(os.Stderr, "WARN:", g.Reason)
	}

	if !opts.Live {
		argv, berr := claudelane.BuildArgs(req)
		if berr != nil {
			return 1, berr
		}
		b, _ := json.MarshalIndent(map[string]any{
			"dry_run": true, "admit": true, "admit_state": g.State, "admit_reason": g.Reason, "forced": g.Forced, "args": argv,
		}, "", "  ")
		fmt.Fprintln(out, string(b))
		return 0, nil
	}

	o, raw, rerr := claudelane.Run(context.Background(), req)
	if rerr != nil {
		return 1, rerr // config_error: bad args never reached the binary
	}
	attributed := make([]string, 0, len(o.ModelUsage))
	var in, outTok int64
	for m, u := range o.ModelUsage {
		attributed = append(attributed, m)
		in += u.InputTokens
		outTok += u.OutputTokens
	}
	sort.Strings(attributed)
	// Post-run accounting is a cross-process transaction: fresh state under
	// the lock, so concurrent run/status/probe invocations never lose writes.
	warnIf(ledger.Update(ledgerPath(), func(fresh *ledger.Ledger) {
		applyRunOutcomeSubject(fresh, sel.Subject, o, now)
	}), "ledger update (post-run)")
	drec := dispatch.Record{
		TS: now, Lane: "claude", Model: resolvedModel, AttributedModels: attributed, OutcomeClass: o.Class,
		Admit: true, AdmitState: g.State, AdmitReason: g.Reason,
		TokensIn: in, TokensOut: outTok, NumTurns: o.NumTurns, NotionalUSD: o.NotionalUSD,
		Origin: opts.Origin, TaskClass: rf.TaskClass, RecLane: rf.RecLane, RecModel: rf.RecModel,
		RecRule: rf.RecRule, Deviated: rf.Deviated, DeviationReason: rf.DeviationReason, Batch: rf.Batch, SpendDownBoost: rf.SpendDownBoost, Desc: opts.Desc,
		Subject: sel.Subject, RotationFrom: sel.RotationFrom, RotationReason: sel.RotationReason,
	}
	sf.stamp(&drec)
	warnIf(dispatch.Append(dispatchPath(), drec), "dispatch append")
	if len(raw) > 0 {
		fmt.Fprintln(out, string(raw))
	} else {
		b, _ := json.Marshal(map[string]string{"outcome_class": o.Class, "detail": o.Result})
		fmt.Fprintln(out, string(b))
	}
	// F4 (S3R-8 fail-safe): the OUTCOME FAILURE is checked FIRST. A failed call
	// (o.Class != "ok") returns not_ok (exit-5) REGARDLESS of cost — exit-4
	// (ok_notional) is kindOK downstream and must never launder a hard failure
	// into a success. The notional guard (exit-4) fires ONLY for a SUCCESSFUL call
	// that was costly.
	code := claudeExitCode(o.Class, o.NotionalUSD, opts.MaxNotional)
	switch code {
	case exitNotOK:
		fmt.Fprintf(os.Stderr, "outcome %q is not ok (exit %d)\n", o.Class, exitNotOK)
	case exitNotional:
		fmt.Fprintf(os.Stderr, "NOTIONAL GUARD: total_cost_usd %.4f exceeds --max-notional-usd %.2f (notional under subscription auth, still a size alarm)\n", o.NotionalUSD, opts.MaxNotional)
	}
	return code, nil
}

// claudeExitCode is the post-run exit-code decision for the claude lane, extracted
// pure so the S3R-8 ordering is directly testable (F4). A FAILED call (class !=
// "ok") is not_ok (exit-5) REGARDLESS of notional cost — never laundered into
// exit-4 (ok_notional, kindOK downstream). The notional guard (exit-4) fires ONLY
// for a SUCCESSFUL call over the cap. A successful call under the cap is clean (0).
func claudeExitCode(class string, notionalUSD, maxNotional float64) int {
	if class != "ok" {
		return exitNotOK // outcome failure outranks the cost warning (fail-safe)
	}
	if notionalUSD > maxNotional {
		return exitNotional // succeeded but costly = ok-with-warning
	}
	return 0
}

func runRun(args []string) error {
	prompt := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		prompt, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	lane := fs.String("lane", "claude", "dispatch lane: claude|codex|glm (local delegates via the local-offload MCP)")
	model := fs.String("model", "", "model to pin (REQUIRED — unpinned claude -p defaults to Sonnet 5)")
	effort := fs.String("effort", "", "effort passthrough (R11)")
	extra := fs.String("extra", "", "space-separated extra lane-binary flags, validated against the forbidden list (R11)")
	live := fs.Bool("live", false, "actually dispatch (default: dry-run printing the admission decision + args)")
	force := fs.Bool("force", false, "operator override: admit even when exhausted/hard-stopped (WARN + audited, R11)")
	cwd := fs.String("cwd", "", "working directory for the lane-binary call")
	timeoutSec := fs.Int("timeout-sec", 600, "hard timeout for the lane-binary call (tree-killed on expiry)")
	maxNotional := fs.Float64("max-notional-usd", 2.0, "post-hoc notional guard: exit 4 above this (claude lane)")
	keepHome := fs.Bool("keep-codex-home", false, "codex lane: keep the per-run CODEX_HOME for debugging")
	class := fs.String("class", "", "task class for the internal route recommendation (absent → heuristic on --desc)")
	desc := fs.String("desc", "", "task description (classifier input; also the receipt Desc, S2R-9)")
	ctxTokens := fs.Int64("ctx-tokens", 0, "estimated input context tokens (ctx-cap masks)")
	latency := fs.Bool("latency-sensitive", false, "classifier hint: prefer the low-latency lane")
	origin := fs.String("origin", "cli", "receipt origin tag (S2R-1: cli|mcp|route|nightshift)")
	deviation := fs.String("deviation", "", "reason recorded when the chosen lane differs from the recommendation (R11)")
	batch := fs.Bool("batch", false, "E2 spend-down tag: this is an already-queued BATCH task (never set for interactive work); enables the under-utilized-window rank boost")
	estMinutes := fs.Float64("est-minutes", 0, "expected task duration in minutes (E2 completion-fit gate; 0 = unknown → no boost)")
	strategyName := fs.String("strategy", "", "run a named strategy template as an async DAG dispatch (R11 seam): solo|plan-work-verify|cascade|fan-out-judge|single-critique. Expands from the prompt (goal) + --class, then spawns a detached supervisor and prints {dispatch_id}. Poll via the strategy_status MCP tool.")
	_ = fs.Parse(args)

	if prompt == "" && fs.NArg() > 0 {
		prompt = fs.Arg(0)
	}

	// --strategy seam (R11): expand the named template from the prompt (goal) +
	// --class into the IR, Validate, then dispatch it on the SAME detached-
	// supervisor path as the strategy_dispatch MCP tool. An unknown template name
	// is a clear error listing the valid names — never a crash.
	if *strategyName != "" {
		if prompt == "" {
			return fmt.Errorf("run --strategy %q: a prompt (the strategy goal) is required", *strategyName)
		}
		id, derr := dispatchNamedStrategy(prompt, *strategyName, *class)
		if derr != nil {
			return fmt.Errorf("run --strategy %q: %w", *strategyName, derr)
		}
		b, _ := json.Marshal(map[string]any{"dispatch_id": id, "state": "working", "strategy": *strategyName})
		fmt.Println(string(b))
		return nil
	}
	if prompt == "" {
		return fmt.Errorf("run: a prompt is required")
	}

	// The shared core writes the result JSON to a buffer and RETURNS the exit
	// code; runRun owns os.Stdout + os.Exit. A config_error comes back as a
	// non-nil err (exit 1 via main); the exit-code contract (0/3/4/5) is a
	// value both surfaces read.
	var buf bytes.Buffer
	code, err := doRun(runOpts{
		Prompt: prompt, Lane: *lane, Model: *model, Effort: *effort, Extra: *extra,
		Live: *live, Force: *force, CWD: *cwd, TimeoutSec: *timeoutSec,
		MaxNotional: *maxNotional, KeepHome: *keepHome, Class: *class, Desc: *desc,
		CtxTokens: *ctxTokens, Latency: *latency, Origin: *origin, Deviation: *deviation,
		Batch: *batch, EstMinutes: *estMinutes,
	}, &buf)
	if buf.Len() > 0 {
		fmt.Print(buf.String())
	}
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}
