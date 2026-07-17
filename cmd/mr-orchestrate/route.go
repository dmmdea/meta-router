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

// buildRouteDecision is the core of the route command (status.go pattern):
// laneStates → E1 burn downshift → operator rank-table override (fail-open to
// Seed) → router.Route. It is NOT pure: it loads the quota trace from disk
// (calib.Load(quotaTracePath())) for the burn-downshift trajectory and the rank
// table (router.Load(rankTablePath())), so callers/tests must isolate
// MR_ORCH_STATE. No dispatch, no cloud call — deterministic and read-only.
func buildRouteDecision(cfg orchcfg.Config, fzs []fuses.Fuse, snap []ledger.Bucket, class router.Class, ctxTokens int64, now time.Time) router.Decision {
	states := laneStates(snap, fzs, cfg, now)
	for lane, lv := range burnDownshiftByLane(snap, calib.Load(quotaTracePath()), cfg, now) {
		st := states[lane]
		st.Downshift = lv
		states[lane] = st
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
}

// writeRouteReceipt appends the consult receipt for a route recommendation —
// the delegation-coverage numerator (RS9 / S2R-1). Origin is faithfully plumbed
// so coverage counts Origin-tagged receipts ({mcp, route}), NOT rec_lane
// presence (S2R-1 kills that circular metric). A relegation (empty Lane) writes
// an all_masked receipt; a real recommendation writes the lane/model/rule. Both
// the CLI `route` command and the MCP `route` tool call this so the receipt
// shape is identical across surfaces.
func writeRouteReceipt(d router.Decision, class router.Class, desc, origin string, now time.Time) {
	if d.Lane == "" {
		warnIf(dispatch.Append(dispatchPath(), dispatch.Record{
			TS: now, OutcomeClass: "route_recommendation", Origin: origin,
			TaskClass: string(class), RecRule: d.Rule, Admit: false,
			AdmitState: "all_masked", AdmitReason: d.Reason, Desc: desc,
		}), "dispatch append (route deferral)")
		return
	}
	warnIf(dispatch.Append(dispatchPath(), dispatch.Record{
		TS: now, OutcomeClass: "route_recommendation", Origin: origin,
		TaskClass: string(class), RecLane: d.Lane, RecModel: d.Model, RecRule: d.Rule,
		Admit: true, AdmitState: d.QuotaState[d.Lane], Desc: desc,
	}), "dispatch append (route receipt)")
}

func routeJSON(d router.Decision) []byte {
	o := routeOut{
		Lane: d.Lane, Model: d.Model, Effort: d.Effort, Strategy: d.Strategy,
		QuotaState: d.QuotaState, Reason: d.Reason, Class: string(d.Class),
		Rule: d.Rule, DispatchVia: dispatchVia(d.Lane),
		Masked: d.Masked, Alternatives: d.Alternatives,
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

	d := buildRouteDecision(cfg, fzs, snap, class, *ctxTokens, now)

	// All-masked relegation: emit the standard deferral JSON with resume_at and
	// exit 3 (RS5). The consult is still a countable decision — receipt it.
	if d.Lane == "" {
		if !*noReceipt {
			writeRouteReceipt(d, class, *desc, *origin, now)
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
		writeRouteReceipt(d, class, *desc, *origin, now)
	}
	return nil
}
