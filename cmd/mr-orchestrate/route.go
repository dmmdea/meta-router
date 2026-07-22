package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/burnrate"
	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/glmlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/spenddown"
)

// worstPct is a lane's worst (highest) window depletion across its buckets;
// -1 (unknown) is preserved only when EVERY window is unknown, so the router's
// fail-open normalization applies. A single known window dominates.
func worstPct(snap []ledger.Bucket, lane string) float64 {
	worst := -1.0
	for _, b := range snap {
		if b.Lane != lane {
			continue
		}
		if b.UsedPct > worst {
			worst = b.UsedPct
		}
	}
	return worst
}

// laneStates derives each lane's router.LaneState from the EXISTING admission
// package (S2R-3: already Source-aware — estimate-sourced buckets only THROTTLE,
// never reach exhausted; only a real provider signal denies). The mapping:
//
//	claude = admission + billing hard-stop (R10); the fable rule stays in the
//	         run gate (model-specific), NOT here.
//	codex  = admission.
//	glm    = admission + the 1313 latch → hard_stop.
//	local  = always open (free lane, fail-open).
func laneStates(snap []ledger.Bucket, fzs []fuses.Fuse, cfg orchcfg.Config, now time.Time) map[string]router.LaneState {
	out := map[string]router.LaneState{}
	for _, lane := range []string{"claude", "codex", "glm"} {
		d := admission.Decide(snap, lane, now, defaultThresholds)
		st := string(d.State)
		if lane == "claude" && cfg.ClaudeBillingMode != orchcfg.BillingSubscription {
			// R10 billing hard-stop: credits (spend, never) and any garbled mode
			// fail SAFE — the lane must be masked before selection.
			st = "hard_stop"
		}
		if lane == "glm" {
			if _, latched := glmlane.Latched(glmAlertPath()); latched {
				st = "hard_stop"
			}
		}
		out[lane] = router.LaneState{State: st, WorstPct: worstPct(snap, lane), ResumeAt: d.ResumeAt}
	}
	// local: always open. The free lane fails open — its capacity is not ledger
	// -tracked (it goes through the local-offload MCP, S2R-4); the router's ctx
	// prohibition floor is what keeps large tasks off it.
	out["local"] = router.LaneState{State: "open", WorstPct: worstPct(snap, "local")}
	return out
}

// burnDownshiftByLane computes the E1 downshift level per lane: the MAX level
// across the lane's buckets (a 5h window on pace to blow demotes the lane even
// if its 7d window is calm). Empty map when the kill-switch is on; an empty
// trace yields no entries (LevelNone everywhere) — the mechanism is inert until
// quota-trace.jsonl fills (the current live state).
func burnDownshiftByLane(snap []ledger.Bucket, samples []calib.Sample, cfg orchcfg.Config, now time.Time) map[string]int {
	if cfg.BurnDownshiftOff {
		return nil
	}
	opt := burnrate.Defaults()
	// R14 floor: a burn multiple m < 1 is an UNDER-pace lane (m=1 is
	// exhaust-exactly-at-reset), which can never legitimately brake. A sub-1
	// override is operator error (or a mis-scaled edit) and must NOT arm a brake —
	// fall back to burnrate.Defaults() for that threshold.
	if cfg.BurnFastX >= 1 {
		opt.FastX = cfg.BurnFastX
	}
	if cfg.BurnMedX >= 1 {
		opt.MedX = cfg.BurnMedX
	}
	if cfg.BurnSlowX >= 1 {
		opt.SlowX = cfg.BurnSlowX
	}
	down := map[string]int{}
	for _, b := range snap {
		if lv := int(burnrate.Assess(samples, b, now, opt)); lv > down[b.Lane] {
			down[b.Lane] = lv
		}
	}
	for lane, lv := range down {
		if lv == 0 {
			delete(down, lane)
		}
	}
	return down
}

// spendDownOptions maps the orchcfg E2 priors onto spenddown.Options; zero or
// invalid config values fall back to spenddown.Defaults() inside Normalize
// (hand-edit-damage rule, mirroring the E1 threshold floor above).
func spendDownOptions(cfg orchcfg.Config) spenddown.Options {
	return spenddown.Normalize(spenddown.Options{
		FloorUnusedPct: cfg.SpendDownFloorUnusedPct,
		Horizon:        time.Duration(cfg.SpendDownHorizonMin) * time.Minute,
		RaisePct:       cfg.SpendDownRaisePct,
		DropPct:        cfg.SpendDownDropPct,
		Cooldown:       time.Duration(cfg.SpendDownCooldownSec) * time.Second,
		Buffer:         time.Duration(cfg.SpendDownBufferMin) * time.Minute,
		MaxBoost:       cfg.SpendDownMaxBoost,
		AvgWindow:      time.Duration(cfg.SpendDownAvgWindowMin) * time.Minute,
	})
}

// spendDownReq carries a consult's E2 inputs. Batch marks an explicitly-tagged,
// already-queued batch task (Q2: never interactive); Est is its expected
// duration for the completion-fit gate (0 = unknown → no boost); Persist
// authorizes latch-transition writes — REAL consults persist, dry-run and
// introspection surfaces (run --dry-run, route --no-receipt) preview the boost
// without state side effects.
type spendDownReq struct {
	Batch   bool
	Est     time.Duration
	Persist bool
}

// spendDownBoostByLane computes the E2 boost per lane for a BATCH-TAGGED
// consult: every non-local bucket's latch is assessed (transitions persisted to
// spend-down.json when persist — the hysteresis/cooldown anchors), and a lane's
// boost is the MAX level among its armed buckets whose reset the task's
// expected duration actually fits before (Q2 completion-fit). Hard exclusions:
//   - local never boosts (the free lane has no window to strand);
//   - a lane carrying an active E1 downshift, or whose admission state is not
//     open (throttled/exhausted on ANY window), never boosts — an over-pace or
//     over-threshold signal and an under-utilization boost are contradictory,
//     and the brake wins (a 7d-idle bucket must not steer batch work onto a
//     lane whose 5h window is about to exhaust).
//
// While a lane is boost-excluded its latch HOLDS (it never ramps in the
// background — otherwise the exclusion lifting would fire an un-paced
// accumulated level, the same blast refinement 5 forbids).
//
// Only called on batch-tagged consults, so an interactive route never advances
// the latch nor gains a boost. Orphan latch keys (lane/window no longer in the
// snapshot) are pruned on persisting consults.
func spendDownBoostByLane(snap []ledger.Bucket, samples []calib.Sample, cfg orchcfg.Config, down map[string]int, states map[string]router.LaneState, est time.Duration, now time.Time, persist bool) map[string]int {
	if cfg.SpendDownOff {
		return nil
	}
	opt := spendDownOptions(cfg)
	st := spenddown.LoadState(spendDownPath())
	boost := map[string]int{}
	live := map[string]bool{}
	changed := false
	for _, b := range snap {
		if b.Lane == "local" {
			continue
		}
		k := spenddown.Key(b)
		live[k] = true
		// The freeze below must compare against the EPOCH-GUARDED prev — the
		// raw persisted entry can carry a stale level from an earlier window,
		// which would let an excluded lane "arm" to 1 right after an epoch
		// flip (delta-review NEW-1).
		prev := spenddown.EpochGuard(st[k], b)
		e := spenddown.Assess(samples, b, prev, now, opt)
		eligible := down[b.Lane] == 0 && states[b.Lane].State == "open"
		if !eligible && e.Level > prev.Level {
			e = prev // hold, never ramp, while the lane is boost-excluded
		}
		if e != st[k] {
			st[k] = e
			changed = true
		}
		if eligible && e.Level > boost[b.Lane] && spenddown.Fits(b, est, now, opt) {
			boost[b.Lane] = e.Level
		}
	}
	// Prune orphan keys — but only when the snapshot actually carried buckets:
	// a failed ledger read yields an EMPTY snapshot, and wiping the whole latch
	// on a transient lock would be state loss, not hygiene.
	if len(live) > 0 {
		for k := range st {
			if !live[k] {
				delete(st, k)
				changed = true
			}
		}
	}
	if persist && changed {
		warnIf(spenddown.SaveState(spendDownPath(), st), "spend-down state save")
	}
	return boost
}

// spendDownArmedByLane is the STATUS view of the E2 latch: the would-be armed
// level per lane (max across its buckets), assessed READ-ONLY — status must
// never advance the latch (only batch consults do) — and without the per-task
// completion-fit / E1-conflict gates, which are consult-time and task-specific.
// Nil when the kill-switch is on; zero levels are elided (omitempty parity
// with burnDownshiftByLane).
func spendDownArmedByLane(snap []ledger.Bucket, samples []calib.Sample, cfg orchcfg.Config, now time.Time) map[string]int {
	if cfg.SpendDownOff {
		return nil
	}
	opt := spendDownOptions(cfg)
	st := spenddown.LoadState(spendDownPath())
	armed := map[string]int{}
	for _, b := range snap {
		if b.Lane == "local" {
			continue
		}
		if e := spenddown.Assess(samples, b, st[spenddown.Key(b)], now, opt); e.Level > armed[b.Lane] {
			armed[b.Lane] = e.Level
		}
	}
	for lane, lv := range armed {
		if lv == 0 {
			delete(armed, lane)
		}
	}
	return armed
}

// buildRouteDecision is the core of the route command (status.go pattern):
// laneStates → E1 burn downshift → E2 spend-down boost (batch-tagged consults
// only) → operator rank-table override (fail-open to Seed) → router.Route. It
// is NOT pure: it loads the quota trace from disk (calib.Load(quotaTracePath()))
// for the E1/E2 trajectories, the rank table (router.Load(rankTablePath())),
// and on batch consults reads+persists the spend-down latch, so callers/tests
// must isolate MR_ORCH_STATE. No dispatch, no cloud call — deterministic given
// the state files. sd carries the consult's E2 inputs (see spendDownReq).
func buildRouteDecision(cfg orchcfg.Config, fzs []fuses.Fuse, snap []ledger.Bucket, class router.Class, ctxTokens int64, now time.Time, sd spendDownReq) router.Decision {
	states := laneStates(snap, fzs, cfg, now)
	samples := calib.Load(quotaTracePath())
	down := burnDownshiftByLane(snap, samples, cfg, now)
	for lane, lv := range down {
		st := states[lane]
		st.Downshift = lv
		states[lane] = st
	}
	if sd.Batch {
		for lane, bv := range spendDownBoostByLane(snap, samples, cfg, down, states, sd.Est, now, sd.Persist) {
			st := states[lane]
			st.Boost = bv
			states[lane] = st
		}
	}
	tbl := router.Load(rankTablePath())
	return router.Route(tbl, class, states, ctxTokens, now)
}

// dispatchVia is the S2R-4(a) execution-front-door field: the local lane's
// front door is the local-offload MCP (until slice 3), every other lane goes
// through mr-orchestrate.
func dispatchVia(lane string) string {
	if lane == "local" {
		return "local-offload-mcp"
	}
	return "mr-orchestrate"
}

// routeOut is the emitted route JSON. The §6c contract is the SIX named keys
// (lane/model/effort/strategy/quota_state/reason); class, rule, dispatch_via,
// masked, alternatives are ADDITIVE.
type routeOut struct {
	Lane         string            `json:"lane"`
	Model        string            `json:"model"`
	Effort       string            `json:"effort"`
	Strategy     string            `json:"strategy"`
	QuotaState   map[string]string `json:"quota_state"`
	Reason       string            `json:"reason"`
	Class        string            `json:"class"`
	Rule         string            `json:"rule,omitempty"`
	DispatchVia  string            `json:"dispatch_via"`
	Masked       []router.Masked   `json:"masked,omitempty"`
	Alternatives []router.Entry    `json:"alternatives,omitempty"`
	// SpendDownBoost is the E2 boost the winning lane carried (batch-tagged
	// consults only; omitted when 0). Additive to the §6c six-key contract.
	SpendDownBoost int `json:"spend_down_boost,omitempty"`
}

// writeRouteReceipt appends the consult receipt for a route recommendation —
// the delegation-coverage numerator (RS9 / S2R-1). Origin is faithfully plumbed
// so coverage counts Origin-tagged receipts ({mcp, route}), NOT rec_lane
// presence (S2R-1 kills that circular metric). A relegation (empty Lane) writes
// an all_masked receipt; a real recommendation writes the lane/model/rule. Both
// the CLI `route` command and the MCP `route` tool call this so the receipt
// shape is identical across surfaces.
func writeRouteReceipt(d router.Decision, class router.Class, desc, origin string, batch bool, now time.Time) {
	if d.Lane == "" {
		warnIf(dispatch.Append(dispatchPath(), dispatch.Record{
			TS: now, OutcomeClass: "route_recommendation", Origin: origin,
			TaskClass: string(class), RecRule: d.Rule, Admit: false,
			AdmitState: "all_masked", AdmitReason: d.Reason, Desc: desc, Batch: batch,
		}), "dispatch append (route deferral)")
		return
	}
	warnIf(dispatch.Append(dispatchPath(), dispatch.Record{
		TS: now, OutcomeClass: "route_recommendation", Origin: origin,
		TaskClass: string(class), RecLane: d.Lane, RecModel: d.Model, RecRule: d.Rule,
		Admit: true, AdmitState: d.QuotaState[d.Lane], Desc: desc,
		Batch: batch, SpendDownBoost: d.SpendDownBoost,
	}), "dispatch append (route receipt)")
}

func routeJSON(d router.Decision) []byte {
	o := routeOut{
		Lane: d.Lane, Model: d.Model, Effort: d.Effort, Strategy: d.Strategy,
		QuotaState: d.QuotaState, Reason: d.Reason, Class: string(d.Class),
		Rule: d.Rule, DispatchVia: dispatchVia(d.Lane),
		Masked: d.Masked, Alternatives: d.Alternatives,
		SpendDownBoost: d.SpendDownBoost,
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	return b
}

func runRoute(args []string) error {
	fs := flag.NewFlagSet("route", flag.ExitOnError)
	classFlag := fs.String("class", "", "task class (the §6c brain normally knows it); absent → heuristic Classify on --desc")
	desc := fs.String("desc", "", "task description (classifier input; also the receipt Desc, S2R-9)")
	ctxTokens := fs.Int64("ctx-tokens", 0, "estimated input context tokens (ctx-cap masks)")
	latency := fs.Bool("latency-sensitive", false, "classifier hint: prefer the low-latency lane")
	origin := fs.String("origin", "cli", "receipt origin tag (S2R-1: cli|mcp|route|nightshift)")
	noReceipt := fs.Bool("no-receipt", false, "skip the consult receipt (tests/introspection loops)")
	batch := fs.Bool("batch", false, "E2 spend-down tag: this is an already-queued BATCH task (never set for interactive work); enables the under-utilized-window rank boost")
	estMinutes := fs.Float64("est-minutes", 0, "expected task duration in minutes (E2 completion-fit gate; 0 = unknown → no boost)")
	_ = fs.Parse(args)

	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	fzs, _ := fuses.Load(fusesPath())

	// RS1: ingest the statusline drop so interactive Claude usage participates
	// in the mask. Fail-open — a bad drop must never break the oracle.
	var snap []ledger.Bucket
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		if _, note, ierr := quotasig.IngestTraced(l, dropPath(), quotaTracePath(), "claude", now); ierr != nil {
			fmt.Fprintln(os.Stderr, "warn: statusline drop unreadable:", ierr)
		} else if note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
		snap = l.Snapshot()
	}); err != nil {
		fmt.Fprintln(os.Stderr, "warn: ledger update failed, reading a read-only snapshot:", err)
		l, warn := ledger.OpenChecked(ledgerPath())
		if warn != "" {
			fmt.Fprintln(os.Stderr, "warn:", warn)
		}
		snap = l.Snapshot()
	}

	// Class: explicit --class wins; otherwise the fallback heuristic classifier.
	var class router.Class
	if *classFlag != "" {
		class = router.Class(*classFlag)
	} else {
		var tag string
		class, tag = router.Classify(*desc, *ctxTokens, *latency)
		fmt.Fprintf(os.Stderr, "note: no --class; heuristic classified as %q (rule=%s) — the brain should pass --class for precision\n", class, tag)
	}

	d := buildRouteDecision(cfg, fzs, snap, class, *ctxTokens, now, spendDownReq{
		Batch: *batch, Est: time.Duration(*estMinutes * float64(time.Minute)),
		// --no-receipt marks a test/introspection consult — it must not
		// advance persistent spend-down state either.
		Persist: *batch && !*noReceipt,
	})

	// All-masked relegation: emit the standard deferral JSON with resume_at and
	// exit 3 (RS5). The consult is still a countable decision — receipt it.
	if d.Lane == "" {
		if !*noReceipt {
			writeRouteReceipt(d, class, *desc, *origin, *batch, now)
		}
		dj := deferral{Deferred: true, Reason: d.Reason}
		if !d.ResumeAt.IsZero() {
			t := d.ResumeAt
			dj.ResumeAt = &t
		}
		b, _ := json.MarshalIndent(dj, "", "  ")
		fmt.Println(string(b))
		os.Exit(exitDeferred)
	}

	fmt.Println(string(routeJSON(d)))

	// Consult receipt: the delegation-coverage numerator (RS9 / S2R-1). Origin
	// is faithfully plumbed so coverage counts Origin-tagged receipts, NOT
	// rec_lane presence (S2R-1 kills that circular metric).
	if !*noReceipt {
		writeRouteReceipt(d, class, *desc, *origin, *batch, now)
	}
	return nil
}
