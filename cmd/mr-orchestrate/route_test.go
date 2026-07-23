package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/spenddown"
)

var rnow = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

// glmKey uses the exported key derivation, never a literal — the subject-scoped
// key shape (W1) made hard-coded "lane|window" literals silently stale.
var glmKey = spenddown.Key(ledger.Bucket{Lane: "glm", Window: ledger.Win5h})

// laneStates: S2R-3 consumption. An estimate-sourced codex bucket over the
// exhaust threshold THROTTLES (never exhausted); local is always open (free
// lane, fail-open).
func TestLaneStatesSourceAware(t *testing.T) {
	snap := []ledger.Bucket{
		// codex 5h over exhaust threshold BUT estimate-sourced → throttled only
		{Lane: "codex", Window: "5h", UsedPct: 99, Source: "shadow", CapSource: ledger.CapSourceEstimate},
		// claude 7d real provider signal over exhaust → exhausted
		{Lane: "claude", Window: "7d", UsedPct: 97, Source: "provider", ResetsAt: rnow.Add(48 * time.Hour)},
	}
	ls := laneStates(snap, fuses.Seed(), orchcfg.Defaults(), rnow)
	if ls["codex"].State != "throttled" {
		t.Fatalf("estimate-sourced codex must throttle not exhaust (S2R-3): %+v", ls["codex"])
	}
	if ls["claude"].State != "exhausted" {
		t.Fatalf("real provider signal must exhaust: %+v", ls["claude"])
	}
	if ls["local"].State != "open" {
		t.Fatalf("local is always open (free lane, fail-open): %+v", ls["local"])
	}
}

// claude billing hard-stop (R10): credits mode hard-stops the claude lane at
// laneStates level so the router masks it before selection.
func TestLaneStatesClaudeBillingHardStop(t *testing.T) {
	cfg := orchcfg.Config{ClaudeBillingMode: orchcfg.BillingCredits}
	ls := laneStates(nil, fuses.Seed(), cfg, rnow)
	if ls["claude"].State != "hard_stop" {
		t.Fatalf("credits billing must hard-stop claude in laneStates (R10): %+v", ls["claude"])
	}
}

// buildRouteDecision end-to-end: with claude exhausted, hard-repo routes to
// glm-5.2 (the mask-before-selection path), and a receipt-shaped Decision
// carries the rule.
func TestBuildRouteDecisionMasksToGLM(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // hermeticity: buildRouteDecision loads the real quota trace + rank table
	snap := []ledger.Bucket{
		{Lane: "claude", Window: "7d", UsedPct: 99, Source: "provider", ResetsAt: rnow.Add(3 * time.Hour)},
	}
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.HardRepo, 0, rnow, spendDownReq{})
	if d.Lane != "glm" || d.Model != "glm-5.2" {
		t.Fatalf("claude exhausted → glm-5.2 for hard-repo: %+v", d)
	}
	if d.Rule == "" {
		t.Fatalf("decision must carry a rule: %+v", d)
	}
}

// S2R-11 rank-table assertion: an UNCLASSIFIABLE desc (no --class) routes to
// claude-opus-4-8, not workhorse-GLM. Classify → HardRepo → Opus.
func TestUnclassifiableDescRoutesToOpus(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // hermeticity: buildRouteDecision loads the real quota trace + rank table
	c, _ := router.Classify("do something vague and unusual", 5000, false)
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, c, 5000, rnow, spendDownReq{})
	if d.Model != "claude-opus-4-8" {
		t.Fatalf("unclassifiable desc must route to claude-opus-4-8 (S2R-11): %+v", d)
	}
}

// S2R-4(a): dispatch_via is "local-offload-mcp" when the winning lane is local,
// else "mr-orchestrate".
func TestDispatchViaField(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // hermeticity: buildRouteDecision loads the real quota trace + rank table
	// mechanical-text at small ctx → local wins → local-offload-mcp
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, router.MechanicalText, 2000, rnow, spendDownReq{})
	if d.Lane != "local" {
		t.Fatalf("precondition: mechanical-text small ctx should win local: %+v", d)
	}
	if got := dispatchVia(d.Lane); got != "local-offload-mcp" {
		t.Fatalf("local winner → local-offload-mcp, got %q", got)
	}
	if got := dispatchVia("glm"); got != "mr-orchestrate" {
		t.Fatalf("non-local winner → mr-orchestrate, got %q", got)
	}
}

// The emitted route JSON carries the §6c six named keys + additive fields
// including dispatch_via and class/rule.
func TestRouteJSONContract(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // hermeticity: buildRouteDecision loads the real quota trace + rank table
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, router.HardRepo, 0, rnow, spendDownReq{})
	b := routeJSON(d)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lane", "model", "effort", "strategy", "quota_state", "reason"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("§6c contract missing key %q: %s", k, b)
		}
	}
	if m["dispatch_via"] != "mr-orchestrate" {
		t.Fatalf("dispatch_via must be present (S2R-4): %s", b)
	}
	if m["strategy"] != "solo" {
		t.Fatalf("strategy must be solo in v0: %s", b)
	}
}

// TestRunRouteEndToEnd: temp state, seeded ledger with claude exhausted, the
// route command writes a route_recommendation receipt with rec_rule + origin.
func TestRunRouteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	// Seed a ledger with claude 7d exhausted (real provider signal).
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win7d, 99, rnow.Add(3*time.Hour), rnow)
	}); err != nil {
		t.Fatal(err)
	}
	if err := runRoute([]string{"--class", "hard-repo", "--desc", "refactor across packages", "--origin", "route"}); err != nil {
		t.Fatal(err)
	}
	// The receipt must be a route_recommendation with rec_rule set + origin.
	raw, err := os.ReadFile(filepath.Join(dir, "dispatch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b := string(raw)
	if !strings.Contains(b, "route_recommendation") {
		t.Fatalf("receipt must be route_recommendation: %s", b)
	}
	if !strings.Contains(b, "\"rec_rule\"") {
		t.Fatalf("receipt must carry rec_rule: %s", b)
	}
	if !strings.Contains(b, "\"origin\":\"route\"") {
		t.Fatalf("receipt must carry the origin (S2R-1): %s", b)
	}
}

// S2R-1: the --origin flag defaults to "cli".
func TestRunRouteDefaultOriginCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	if err := runRoute([]string{"--class", "workhorse-coding", "--desc", "x"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "dispatch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	b := string(raw)
	if !strings.Contains(b, "\"origin\":\"cli\"") {
		t.Fatalf("default origin must be cli (S2R-1): %s", b)
	}
}

// TestRouteAllMaskedExit3Shape: when everything is masked the deferral JSON
// carries resume_at (the buildRouteDecision returns Lane:"" + ResumeAt).
func TestRouteAllMaskedShape(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // hermeticity: buildRouteDecision loads the real quota trace + rank table
	snap := []ledger.Bucket{
		{Lane: "claude", Window: "7d", UsedPct: 99, Source: "provider", ResetsAt: rnow.Add(4 * time.Hour)},
		{Lane: "glm", Window: "7d", UsedPct: 99, Source: "provider", ResetsAt: rnow.Add(2 * time.Hour)},
	}
	// hard-repo lists claude, claude, glm; local not in the class. With claude +
	// glm exhausted, all candidates are masked.
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.HardRepo, 0, rnow, spendDownReq{})
	if d.Lane != "" {
		t.Fatalf("all masked must relegate (Lane empty): %+v", d)
	}
	if !d.ResumeAt.Equal(rnow.Add(2 * time.Hour)) {
		t.Fatalf("relegation must carry earliest resume: %v", d.ResumeAt)
	}
}

// E1 wiring: a fast-burning lane's downshift is computed from the quota trace on
// the route path and demotes it; the kill-switch disables the whole mechanism.
func TestBuildRouteDecisionAppliesBurnDownshift(t *testing.T) {
	now := time.Now().UTC()
	snap := []ledger.Bucket{{Lane: "glm", Window: ledger.Win5h, UsedPct: 40,
		ResetsAt: now.Add(6 * time.Hour), Source: "provider", ObservedAt: now}}
	samples := []calib.Sample{
		{TS: now.Add(-time.Hour), Lane: "glm", Window: "5h", UsedPct: 0},
		{TS: now, Lane: "glm", Window: "5h", UsedPct: 40}} // 40 pct/h vs required 10 -> m=4 -> fast
	down := burnDownshiftByLane(snap, samples, orchcfg.Config{}, now)
	if down["glm"] < 2 {
		t.Fatalf("fast-burning glm must reach downshift >= 2, got %d", down["glm"])
	}
	off := burnDownshiftByLane(snap, samples, orchcfg.Config{BurnDownshiftOff: true}, now)
	if len(off) != 0 {
		t.Fatalf("kill-switch must disable downshift entirely, got %v", off)
	}
}

// F4 real integration: buildRouteDecision (not burnDownshiftByLane) loads the
// quota trace + ledger from the state paths and demotes a fast-burning rank-1
// lane. workhorse-coding Seed ranks glm#1 > claude-sonnet-5#2; a glm 5h bucket
// on pace to blow (0→40% in the last hour vs required ~10 pct/h) downshifts glm
// to eff-rank 2, tying claude#2 — the usedPct tiebreak (glm 40% vs claude 0%)
// then hands the win to claude. This exercises the ACTUAL route-path wiring
// (calib.Load(quotaTracePath()) + router.Load(rankTablePath())) end to end.
func TestBuildRouteDecisionEndToEndBurnDownshift(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	now := time.Now().UTC()

	// Real quota-trace.jsonl at the exact path buildRouteDecision reads: glm 5h
	// burning 0→40% over the last hour (40 pct/h) — a fast-burn shape.
	trace := "" +
		`{"ts":"` + now.Add(-time.Hour).Format(time.RFC3339Nano) + `","lane":"glm","window":"5h","used_pct":0}` + "\n" +
		`{"ts":"` + now.Format(time.RFC3339Nano) + `","lane":"glm","window":"5h","used_pct":40}` + "\n"
	if err := os.WriteFile(quotaTracePath(), []byte(trace), 0o644); err != nil {
		t.Fatal(err)
	}

	// Seed the ledger with the matching glm 5h bucket: 40% used, resets in 6h →
	// required ~10 pct/h, so the 40 pct/h trace is m=4 (fast).
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		l.ObserveProvider("glm", ledger.Win5h, 40, now.Add(6*time.Hour), now)
	}); err != nil {
		t.Fatal(err)
	}
	var snap []ledger.Bucket
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) { snap = l.Snapshot() }); err != nil {
		t.Fatal(err)
	}

	// workhorse-coding: Seed ranks glm#1, claude-sonnet-5#2. The fast burn must
	// demote glm below claude on the real route path.
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.Workhorse, 0, now, spendDownReq{})
	if d.Lane == "glm" {
		t.Fatalf("fast-burning rank-1 glm must be demoted off the win on the real route path: %+v", d)
	}
	if d.Lane != "claude" {
		t.Fatalf("expected the demotion to hand the win to rank-2 claude, got %+v", d)
	}
}

// Config threshold overrides reach the assessor (blog-lore-constant ban: the
// priors are JSON-tunable, not code-locked).
func TestBurnDownshiftThresholdOverrides(t *testing.T) {
	now := time.Now().UTC()
	snap := []ledger.Bucket{{Lane: "glm", Window: ledger.Win5h, UsedPct: 40,
		ResetsAt: now.Add(6 * time.Hour), Source: "provider", ObservedAt: now}}
	samples := []calib.Sample{
		{TS: now.Add(-time.Hour), Lane: "glm", Window: "5h", UsedPct: 25},
		{TS: now, Lane: "glm", Window: "5h", UsedPct: 40}} // 15 pct/h vs required 10 -> m=1.5
	strict := burnDownshiftByLane(snap, samples, orchcfg.Config{BurnFastX: 1.4}, now)
	if strict["glm"] != 3 {
		t.Fatalf("BurnFastX=1.4 must classify m=1.5 as fast, got %d", strict["glm"])
	}
	// R14 floor: a sub-1 override (m<1 can never legitimately brake — that is an
	// UNDER-pace lane) must be IGNORED and fall back to the default. BurnFastX=0.5
	// must NOT take effect: it falls back to default FastX=3, so m=1.5 yields no
	// fast level; MedX stays at its default 1.5, so m=1.5 >= 1.5 → medium (2).
	floor := burnDownshiftByLane(snap, samples, orchcfg.Config{BurnFastX: 0.5}, now)
	if floor["glm"] == 3 {
		t.Fatalf("sub-1 BurnFastX=0.5 must be ignored (fall back to default 3), not classify m=1.5 as fast: got %d", floor["glm"])
	}
	if floor["glm"] != 2 {
		t.Fatalf("with FastX defaulted and MedX default 1.5, m=1.5 must be medium (2), got %d", floor["glm"])
	}
}

// E2 wiring end-to-end: a BATCH-tagged consult with an armed latch on an
// under-utilized near-reset glm window boosts glm to parity (eff rank 1) where
// it wins the depletion tie against claude; the same consult untagged — or
// without a duration for the completion-fit gate — routes claude un-boosted,
// and neither of those ever touches the persisted latch.
func TestBuildRouteDecisionSpendDownBoost(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	snap := []ledger.Bucket{
		{Lane: "claude", Window: "5h", UsedPct: 40, Source: "provider", ResetsAt: rnow.Add(3 * time.Hour)},
		{Lane: "glm", Window: "5h", UsedPct: 10, Source: "provider", ResetsAt: rnow.Add(time.Hour)},
	}
	// Pre-seeded latch: glm armed at 2 in the CURRENT epoch with a fresh
	// cooldown anchor → the consult HOLDS it at 2 (hard-repo glm is seed rank
	// 3 → eff 1). Holding needs no trace; only arming/ramping does.
	seed := spenddown.State{glmKey: {Level: 2, ChangedAt: rnow.Add(-time.Minute), ResetsAt: rnow.Add(time.Hour)}}
	if err := spenddown.SaveState(spendDownPath(), seed); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(spendDownPath())
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.HardRepo, 0, rnow, spendDownReq{Batch: true, Est: 30 * time.Minute, Persist: true})
	if d.Lane != "glm" || d.SpendDownBoost != 2 || !strings.Contains(d.Reason, "spend-down") {
		t.Fatalf("batch consult must boost armed glm to the winning tie: %+v", d)
	}
	if d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.HardRepo, 0, rnow, spendDownReq{}); d.Lane != "claude" || d.SpendDownBoost != 0 {
		t.Fatalf("interactive consult must never boost: %+v", d)
	}
	if d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.HardRepo, 0, rnow, spendDownReq{Batch: true, Persist: true}); d.Lane != "claude" || d.SpendDownBoost != 0 {
		t.Fatalf("unknown duration must close the completion-fit gate: %+v", d)
	}
	after, _ := os.ReadFile(spendDownPath())
	if string(before) != string(after) {
		t.Fatalf("no consult above transitions the latch, so the file must be byte-identical: before %s after %s", before, after)
	}
}

// glmTrace fabricates two same-epoch trace rows inside the averaging window so
// the latch is allowed to ARM (arming requires a live windowed read).
func glmTrace(now time.Time) []calib.Sample {
	return []calib.Sample{
		{TS: now.Add(-10 * time.Minute), Lane: "glm", Window: "5h", UsedPct: 9},
		{TS: now, Lane: "glm", Window: "5h", UsedPct: 10},
	}
}

// spendDownBoostByLane: an idle near-reset window with a live trace ARMS
// (level 1) and persists the latch; a non-persisting consult previews the same
// boost without writing; an active E1 downshift or a non-open lane state
// blocks the boost AND freezes the ramp; the kill-switch disables everything.
func TestSpendDownLatchArmsAndPersists(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	snap := []ledger.Bucket{
		{Lane: "glm", Window: "5h", UsedPct: 10, Source: "provider", ResetsAt: rnow.Add(time.Hour)},
	}
	open := map[string]router.LaneState{"glm": {State: "open"}}

	if b := spendDownBoostByLane(snap, glmTrace(rnow), orchcfg.Defaults(), nil, open, 30*time.Minute, rnow, false); b["glm"] != 1 {
		t.Fatalf("non-persisting consult must still preview the armed boost: %v", b)
	}
	if _, err := os.Stat(spendDownPath()); err == nil {
		t.Fatal("persist=false must not write the latch file")
	}

	boost := spendDownBoostByLane(snap, glmTrace(rnow), orchcfg.Defaults(), nil, open, 30*time.Minute, rnow, true)
	if boost["glm"] != 1 {
		t.Fatalf("idle near-reset glm must arm at 1: %v", boost)
	}
	st := spenddown.LoadState(spendDownPath())
	if e := st[glmKey]; e.Level != 1 || !e.ChangedAt.Equal(rnow) {
		t.Fatalf("latch transition must persist: %+v", st)
	}

	// E1 conflict: the brake wins, and the latch FREEZES (no background ramp).
	// later is past the cooldown (a ramp would fire if eligible) but still
	// well before the bucket's reset.
	later := rnow.Add(30 * time.Minute)
	snapLater := []ledger.Bucket{
		{Lane: "glm", Window: "5h", UsedPct: 10, Source: "provider", ResetsAt: rnow.Add(time.Hour)},
	}
	if b := spendDownBoostByLane(snapLater, glmTrace(later), orchcfg.Defaults(), map[string]int{"glm": 2}, open, 30*time.Minute, later, true); b["glm"] != 0 {
		t.Fatalf("an E1 downshift must block the boost (brake wins): %v", b)
	}
	if e := spenddown.LoadState(spendDownPath())[glmKey]; e.Level != 1 {
		t.Fatalf("a boost-excluded lane must hold its latch, never ramp in the background: %+v", e)
	}

	// Non-open admission state (throttled elsewhere) equally blocks the boost —
	// a 7d-idle bucket must not steer batch work onto a throttled lane.
	throttled := map[string]router.LaneState{"glm": {State: "throttled"}}
	if b := spendDownBoostByLane(snapLater, glmTrace(later), orchcfg.Defaults(), nil, throttled, 30*time.Minute, later, true); b["glm"] != 0 {
		t.Fatalf("a throttled lane must never boost: %v", b)
	}

	if b := spendDownBoostByLane(snap, glmTrace(rnow), orchcfg.Config{SpendDownOff: true}, nil, open, 30*time.Minute, rnow, true); len(b) != 0 {
		t.Fatalf("kill-switch must disable spend-down: %v", b)
	}
}

// spendDownArmedByLane (the status view) is READ-ONLY: it reports the armed
// level, elides zero levels, honors the kill-switch, and never writes the
// latch file.
func TestSpendDownArmedByLaneReadOnly(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	snap := []ledger.Bucket{
		{Lane: "glm", Window: "5h", UsedPct: 10, Source: "provider", ResetsAt: rnow.Add(time.Hour)},
		{Lane: "claude", Window: "5h", UsedPct: 40, Source: "provider", ResetsAt: rnow.Add(3 * time.Hour)},
	}
	armed := spendDownArmedByLane(snap, glmTrace(rnow), orchcfg.Defaults(), rnow)
	if armed["glm"] != 1 {
		t.Fatalf("status must show the would-be armed level: %v", armed)
	}
	if _, elided := armed["claude"]; elided {
		t.Fatalf("zero levels must be elided (omitempty parity): %v", armed)
	}
	if _, err := os.Stat(spendDownPath()); err == nil {
		t.Fatal("the status view must never write the latch file")
	}
	if a := spendDownArmedByLane(snap, glmTrace(rnow), orchcfg.Config{SpendDownOff: true}, rnow); a != nil {
		t.Fatalf("kill-switch must return nil: %v", a)
	}
}

// NEW-1 (delta review): the exclusion freeze compares against the
// EPOCH-GUARDED prev — a boost-excluded lane holding a stale-epoch latch must
// end at level 0, not sneak an arm through the raw-prev comparison.
func TestExcludedLaneCannotArmAcrossEpochFlip(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	snap := []ledger.Bucket{
		{Lane: "glm", Window: "5h", UsedPct: 10, Source: "provider", ResetsAt: rnow.Add(time.Hour)},
	}
	// Stale latch from an earlier window at level 1.
	seed := spenddown.State{glmKey: {Level: 1, ChangedAt: rnow.Add(-2 * time.Hour), ResetsAt: rnow.Add(-time.Hour)}}
	if err := spenddown.SaveState(spendDownPath(), seed); err != nil {
		t.Fatal(err)
	}
	throttled := map[string]router.LaneState{"glm": {State: "throttled"}}
	if b := spendDownBoostByLane(snap, glmTrace(rnow), orchcfg.Defaults(), nil, throttled, 30*time.Minute, rnow, true); b["glm"] != 0 {
		t.Fatalf("excluded lane must not boost: %v", b)
	}
	if e := spenddown.LoadState(spendDownPath())[glmKey]; e.Level != 0 {
		t.Fatalf("excluded lane after an epoch flip must persist level 0, got %+v", e)
	}
}
