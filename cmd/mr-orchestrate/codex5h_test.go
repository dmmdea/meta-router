package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/codexlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// The LIVE bucket shape on 2026-09-07 (ledger.json on Qube): the 5h window
// carrying the config estimate at 230%+ modeled against a vendor week at 27%.
func liveCodexLedger(t *testing.T, now time.Time) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		l.SetCapacityEstimate("codex", ledger.Win5h, 80_000)
		l.AnchorIfUnset("codex", ledger.Win5h, now.Add(4*time.Hour), now)
		l.AddShadow("codex", ledger.Win5h, 184_641, now) // → ~230%
		l.ObserveProvider("codex", ledger.Win7d, 27, now.Add(3*24*time.Hour), now)
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
	if b.UsedPct < 200 || b.CapSource != ledger.CapSourceEstimate || b.Source != "shadow" {
		t.Fatalf("premise: the live shape is an estimate-sourced 5h at 230%%: %+v", b)
	}
	return p
}

func armedFacts(now time.Time, has5h, saw bool) (orchcfg.Config, pollState) {
	cfg := orchcfg.Defaults()
	cfg.Codex5hEstimateOff = true
	at := now.Add(-time.Hour)
	return cfg, pollState{CodexPlan: "prolite", CodexHas5h: &has5h, CodexSaw5hElsewhere: &saw, CodexFactsAt: &at}
}

// Armed + fresh + corroborated: the 5h estimate is withdrawn, the bucket
// stays at -1 AFTER the call returns (AddShadow no longer re-derives), the
// floor keeps accumulating, the anchor stays, the lane reads open at the
// vendor's 27 — and it is stable across dispatches, not one-shot.
func TestCodex5hSuppressionFromTheLiveShape(t *testing.T) {
	now := tnow
	p := liveCodexLedger(t, now)
	cfg, ps := armedFacts(now, false, true)
	gate := codex5hEstimateGate(cfg, ps, now)
	if !gate.Suppress || gate.Reason == "" {
		t.Fatalf("gate must suppress with a receipt: %+v", gate)
	}
	usage := codexlane.Usage{Input: 120_000, Output: 500}
	var shadowAfterFirst int64
	for i := 0; i < 2; i++ {
		if err := ledger.Update(p, func(l *ledger.Ledger) {
			applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: usage}, cfg, now.Add(time.Duration(i)*time.Minute), gate)
		}); err != nil {
			t.Fatal(err)
		}
		b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
		if b.UsedPct != -1 || b.CapTokens != 0 || b.CapSource != "" {
			t.Fatalf("pass %d: 5h must stay uncapped/-1 after the call returns: %+v", i, b)
		}
		if b.ShadowTokens <= 184_641 || !b.ResetsAt.Equal(now.Add(4*time.Hour)) {
			t.Fatalf("pass %d: shadow must grow and the anchor must stay: %+v", i, b)
		}
		if i == 0 {
			shadowAfterFirst = b.ShadowTokens
		} else if b.ShadowTokens <= shadowAfterFirst {
			t.Fatalf("second pass must keep accumulating: %d then %d", shadowAfterFirst, b.ShadowTokens)
		}
	}
	states := laneStates(ledger.Open(p).Snapshot(), nil, cfg, now.Add(2*time.Minute))
	if st := states["codex"]; st.State != "open" || st.WorstPct != 27 {
		t.Fatalf("codex must read open at the vendor's week (27), got %+v", st)
	}
}

// Guards, each red if inverted.
func TestCodex5hGateGuards(t *testing.T) {
	now := tnow
	// Q10: a bare absence never suppresses.
	if cfg, ps := armedFacts(now, false, false); codex5hEstimateGate(cfg, ps, now).Suppress {
		t.Fatal("bare absence (no sibling 5h window) must not suppress")
	}
	// The vendor reports a 5h window: nothing to suppress.
	if cfg, ps := armedFacts(now, true, true); codex5hEstimateGate(cfg, ps, now).Suppress {
		t.Fatal("has_5h_window=true must not suppress")
	}
	// Stale facts fall through to today's behaviour.
	cfg, ps := armedFacts(now, false, true)
	old := now.Add(-time.Duration(cfg.QuotaStaleHours+1) * time.Hour)
	ps.CodexFactsAt = &old
	if g := codex5hEstimateGate(cfg, ps, now); g.Suppress {
		t.Fatal("stale facts must not suppress")
	}
	// No facts at all.
	if g := codex5hEstimateGate(cfg, pollState{}, now); g.Suppress {
		t.Fatal("absent facts must not suppress")
	}
	// Knob off: identical to main whatever the facts say.
	cfg2, ps2 := armedFacts(now, false, true)
	cfg2.Codex5hEstimateOff = false
	if g := codex5hEstimateGate(cfg2, ps2, now); g.Suppress {
		t.Fatal("B8: the knob off must never suppress")
	}
}

// Knob off ⇒ the ledger is what main writes: the seed lands and the
// percentage derives exactly as before. Compared as the SORTED bucket set,
// not raw bytes: ledger.Save ranges a map, so main's own file order is
// nondeterministic (two runs of main differ in bytes too).
func TestCodex5hKnobOffIsByteIdenticalToMain(t *testing.T) {
	now := tnow
	run := func(gate codex5hGate) string {
		p := filepath.Join(t.TempDir(), "ledger.json")
		if err := ledger.Update(p, func(l *ledger.Ledger) {
			applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, orchcfg.Defaults(), now, gate)
			applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, orchcfg.Defaults(), now.Add(time.Minute), gate)
		}); err != nil {
			t.Fatal(err)
		}
		b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
		if b.CapTokens != 40_000 || b.CapSource != ledger.CapSourceEstimate || b.UsedPct <= 0 {
			t.Fatalf("today's behaviour: the estimate seeds and derives: %+v", b)
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var bs []ledger.Bucket
		if err := json.Unmarshal(raw, &bs); err != nil {
			t.Fatal(err)
		}
		sort.Slice(bs, func(i, j int) bool { return bs[i].Lane+"|"+string(bs[i].Window) < bs[j].Lane+"|"+string(bs[j].Window) })
		canon, err := json.MarshalIndent(bs, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		return string(canon)
	}
	cfg, ps := armedFacts(now, false, true)
	cfg.Codex5hEstimateOff = false
	if a, b := run(codex5hGate{}), run(codex5hEstimateGate(cfg, ps, now)); a != b {
		t.Fatalf("knob off must be identical to the zero gate:\n%s\n---\n%s", a, b)
	}
}

// A provider snapshot still overrides the suppressed bucket, and a real 429
// still exhausts the lane (S2R-3's denial path is intact).
func TestCodex5hSuppressionKeepsProviderAndLimitAuthority(t *testing.T) {
	now := tnow
	p := liveCodexLedger(t, now)
	cfg, ps := armedFacts(now, false, true)
	gate := codex5hEstimateGate(cfg, ps, now)
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, cfg, now, gate)
		l.ObserveProvider("codex", ledger.Win5h, 63, now.Add(2*time.Hour), now.Add(time.Minute))
		applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, cfg, now.Add(2*time.Minute), gate)
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
	if b.Source != "provider" || b.UsedPct != 63 {
		t.Fatalf("a provider reading must survive the suppression: %+v", b)
	}
	// Real veto.
	p2 := liveCodexLedger(t, now)
	if err := ledger.Update(p2, func(l *ledger.Ledger) {
		applyCodexOutcome(l, codexlane.Outcome{Class: "rate_limit"}, cfg, now, gate)
	}); err != nil {
		t.Fatal(err)
	}
	if g := laneGate(ledger.Open(p2).Snapshot(), "codex", now.Add(time.Minute), th, false); g.Admit {
		t.Fatalf("a real rate limit must still exhaust under suppression: %+v", g)
	}
}

// Self-revert: disarm after a suppressed run and the very next dispatch
// re-seeds the estimate.
func TestCodex5hDisarmReseeds(t *testing.T) {
	now := tnow
	p := liveCodexLedger(t, now)
	cfg, ps := armedFacts(now, false, true)
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, cfg, now, codex5hEstimateGate(cfg, ps, now))
	}); err != nil {
		t.Fatal(err)
	}
	cfg.Codex5hEstimateOff = false
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, cfg, now.Add(time.Minute), codex5hEstimateGate(cfg, ps, now.Add(time.Minute)))
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
	if b.CapTokens != 40_000 || b.CapSource != ledger.CapSourceEstimate || b.UsedPct <= 0 {
		t.Fatalf("disarming must re-seed the estimate on the next dispatch: %+v", b)
	}
}

// ── review round 2 ────────────────────────────────────────────────────────

// THE Q10 CASE THE CORROBORATION LEG CANNOT SEE. On Plus, wham omits the
// main-allowance 5h window the plan really HAS while the Spark sibling block
// still reports one — so every other leg holds and the estimate would be
// withdrawn on a plan that has a 5h window. Only the plan allowlist stops it.
func TestCodex5hRefusesAPlanThatIsNotOnTheMeasuredAllowlist(t *testing.T) {
	now := tnow
	cfg, ps := armedFacts(now, false, true) // has5h=false, corroborated: legs 1-3 hold
	for _, plan := range []string{"plus", "pro", "team", ""} {
		ps.CodexPlan = plan
		g := codex5hEstimateGate(cfg, ps, now)
		if g.Suppress {
			t.Fatalf("plan %q is not on the allowlist and must never suppress: %+v", plan, g)
		}
		if plan != "" && !strings.Contains(g.Reason, "not in codex_5h_estimate_off_plans") {
			t.Fatalf("plan %q: the reason must name the allowlist: %s", plan, g.Reason)
		}
	}
	// The measured plan still suppresses, so the leg is a filter, not an off switch.
	ps.CodexPlan = "prolite"
	if g := codex5hEstimateGate(cfg, ps, now); !g.Suppress {
		t.Fatalf("the measured plan must still suppress: %+v", g)
	}
	// An explicitly EMPTY allowlist is a deliberate "no plan may suppress".
	cfg.Codex5hEstimateOffPlans = []string{}
	if g := codex5hEstimateGate(cfg, ps, now); g.Suppress {
		t.Fatalf("an empty allowlist must suppress nothing: %+v", g)
	}
}

// A short-span window that was PRESENT but unreadable reaches has_5h_window
// =false by the same path as a real absence. Unreadable is not absent.
func TestCodex5hRefusesAnUndecodableShortWindow(t *testing.T) {
	now := tnow
	cfg, ps := armedFacts(now, false, true)
	und := true
	ps.CodexUndecodable5h = &und
	g := codex5hEstimateGate(cfg, ps, now)
	if g.Suppress || !strings.Contains(g.Reason, "could not decode") {
		t.Fatalf("an undecodable short window must refuse: %+v", g)
	}
	und = false
	if g := codex5hEstimateGate(cfg, ps, now); !g.Suppress {
		t.Fatalf("a decodable absence still suppresses: %+v", g)
	}
	// A pre-v0.40.6 poll-state has no such field at all: nil is not "true".
	ps.CodexUndecodable5h = nil
	if g := codex5hEstimateGate(cfg, ps, now); !g.Suppress {
		t.Fatalf("nil undecodable (older poll-state) must not block: %+v", g)
	}
}

// Facts written by v0.40.5 have the plan and has_5h_window but not the
// corroboration field: say THAT, instead of telling the operator to poll
// while a fresh observed_at sits next to the message.
func TestCodex5hDistinguishesPreUpgradeFactsFromNoFacts(t *testing.T) {
	now := tnow
	cfg, ps := armedFacts(now, false, true)
	ps.CodexSaw5hElsewhere = nil
	if g := codex5hEstimateGate(cfg, ps, now); g.Suppress || !strings.Contains(g.Reason, "predate v0.40.6") {
		t.Fatalf("pre-upgrade facts need their own reason: %+v", g)
	}
	if g := codex5hEstimateGate(cfg, pollState{}, now); g.Suppress || !strings.Contains(g.Reason, "poll first") {
		t.Fatalf("no facts at all keeps the poll-first reason: %+v", g)
	}
}

// Facts stamped in the FUTURE (a clock skew between fleet nodes, or a
// hand-edited poll-state) are not fresh facts.
func TestCodex5hRefusesFutureStampedFacts(t *testing.T) {
	now := tnow
	cfg, ps := armedFacts(now, false, true)
	future := now.Add(2 * time.Hour)
	ps.CodexFactsAt = &future
	if g := codex5hEstimateGate(cfg, ps, now); g.Suppress || !strings.Contains(g.Reason, "stale") {
		t.Fatalf("future-stamped facts must refuse: %+v", g)
	}
}

// The receipt half of "never silent". Admission returns an EMPTY reason
// whenever every window sits below the throttle band, and the old
// concatenation emitted a receipt starting with "; ".
func TestCodexAdmitReasonComposition(t *testing.T) {
	sup := codex5hGate{Suppress: true, Reason: "codex 5h estimate suppressed: plan prolite"}
	no := codex5hGate{Reason: "codex_5h_estimate_off not armed"}
	if got := codexAdmitReason("", sup); got != "codex 5h estimate suppressed: plan prolite" {
		t.Fatalf("an empty admission reason must yield the gate reason alone, no separator: %q", got)
	}
	if got := codexAdmitReason("rank 1 admitted (state=open)", sup); got != "rank 1 admitted (state=open); codex 5h estimate suppressed: plan prolite" {
		t.Fatalf("both parts join with one separator: %q", got)
	}
	if got := codexAdmitReason("rank 1 admitted;", sup); got != "rank 1 admitted; codex 5h estimate suppressed: plan prolite" {
		t.Fatalf("a trailing separator is not doubled: %q", got)
	}
	// NOT suppressing: byte-identical to the admission reason. The gate's
	// "why not" belongs in status, not on every codex receipt.
	for _, r := range []string{"", "rank 2 admitted"} {
		if got := codexAdmitReason(r, no); got != r {
			t.Fatalf("a non-suppressing gate must not touch the reason: %q → %q", r, got)
		}
	}
	if strings.HasPrefix(codexAdmitReason("", sup), ";") {
		t.Fatal("a receipt must never start with a bare separator")
	}
}

// An ARMED knob always has a status surface, even with no facts at all —
// otherwise the operator arms it, sees nothing change, and status is silent
// about the knob's existence.
func TestCodexPlanStatusRendersTheArmedGateWithNoFacts(t *testing.T) {
	cfg := orchcfg.Defaults()
	cfg.Codex5hEstimateOff = true
	st := codexPlanStatus(pollState{}, cfg, tnow)
	if st == nil {
		t.Fatal("an armed knob with no facts must still render a status block")
	}
	if !st.EstimateOffArmed || st.EstimateOffSuppressing || !strings.Contains(st.EstimateOffReason, "poll first") {
		t.Fatalf("armed + no facts: %+v", st)
	}
	// Disarmed and factless stays absent (no empty block on every status).
	cfg.Codex5hEstimateOff = false
	if st := codexPlanStatus(pollState{}, cfg, tnow); st != nil {
		t.Fatalf("disarmed + no facts must stay absent: %+v", st)
	}
}

// The status block's JSON tags are an operator-facing contract (scripts and
// the wiki read them); renaming one is a silent break.
func TestCodexPlanStatusJSONTagsArePinned(t *testing.T) {
	cfg := orchcfg.Defaults()
	cfg.Codex5hEstimateOff = true
	has, saw, und := false, true, false
	at := tnow.Add(-time.Hour)
	b, err := json.Marshal(codexPlanStatus(pollState{CodexPlan: "prolite", CodexHas5h: &has, CodexSaw5hElsewhere: &saw, CodexUndecodable5h: &und, CodexFactsAt: &at}, cfg, tnow))
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{`"plan_type"`, `"has_5h_window"`, `"saw_5h_elsewhere"`, `"undecodable_5h"`, `"observed_at"`,
		`"estimate_off_armed"`, `"estimate_off_suppressing"`, `"estimate_off_reason"`, `"note"`} {
		if !strings.Contains(string(b), tag) {
			t.Fatalf("status tag %s missing: %s", tag, b)
		}
	}
}

// ClearCapacity withdraws an ESTIMATE, never a fit. A capacity fitted from
// measured shadow usage is evidence, and S2R-3's exhaust gate depends on it.
func TestClearCapacityOnlyWithdrawsAnEstimateAndDoesNotChurn(t *testing.T) {
	now := tnow
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		l.SetCapacityEstimate("codex", ledger.Win5h, 40_000)
		l.AnchorIfUnset("codex", ledger.Win5h, now.Add(4*time.Hour), now)
		l.AddShadow("codex", ledger.Win5h, 8_000, now)
	}); err != nil {
		t.Fatal(err)
	}
	var first, second bool
	var v1, v2 int
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		first = l.ClearCapacity("codex", ledger.Win5h, now)
		b, _ := l.Bucket("codex", ledger.Win5h)
		v1 = b.CapVersion
		second = l.ClearCapacity("codex", ledger.Win5h, now)
		b, _ = l.Bucket("codex", ledger.Win5h)
		v2 = b.CapVersion
	}); err != nil {
		t.Fatal(err)
	}
	if !first || second {
		t.Fatalf("the first clear changes the bucket, the second is a no-op: %v / %v", first, second)
	}
	if v1 != v2 {
		t.Fatalf("cap_version must not churn on an already-cleared bucket: %d → %d", v1, v2)
	}
	b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
	if b.CapTokens != 0 || b.UsedPct != -1 || b.ShadowTokens != 8_000 {
		t.Fatalf("cleared: cap 0, pct -1, shadow KEPT: %+v", b)
	}

	// A capacity that is NOT an estimate (SetCapacity: what calib.Fit writes
	// from measured shadow usage) is evidence, and S2R-3's exhaust gate
	// depends on it. ClearCapacity must refuse to touch it.
	p2 := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p2, func(l *ledger.Ledger) {
		l.SetCapacity("codex", ledger.Win5h, 250_000) // fitted, CapSource ""
		l.AnchorIfUnset("codex", ledger.Win5h, now.Add(4*time.Hour), now)
		l.AddShadow("codex", ledger.Win5h, 50_000, now)
	}); err != nil {
		t.Fatal(err)
	}
	var cleared bool
	if err := ledger.Update(p2, func(l *ledger.Ledger) {
		cleared = l.ClearCapacity("codex", ledger.Win5h, now)
	}); err != nil {
		t.Fatal(err)
	}
	fb, _ := ledger.Open(p2).Bucket("codex", ledger.Win5h)
	if cleared || fb.CapTokens != 250_000 || fb.UsedPct <= 0 {
		t.Fatalf("a FITTED capacity is measured evidence and must survive: cleared=%v %+v", cleared, fb)
	}
}

// END TO END through runCodexLane: the gate is actually WIRED into dispatch
// and the suppression reaches the receipt. Severing the wiring (or dropping
// the admit_reason composition) is green without this — the suite has no
// other AdmitReason assertion, and a suppressed dispatch would then be
// indistinguishable from an ordinary one in dispatch.jsonl.
//
// Zero spend: PATH is stripped, so the lane reaches the spawn and classifies
// spawn_error, having already run the gate, cleared the capacity and written
// the receipt.
func TestCodex5hSuppressionIsWiredIntoDispatchAndReceipted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"tokens":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")

	// Armed config + fresh, corroborated, allowlisted facts on disk.
	cfg := orchcfg.Defaults()
	cfg.Codex5hEstimateOff = true
	writeJSON(t, filepath.Join(dir, "config.json"), cfg)
	now := time.Now()
	at := now.Add(-time.Minute)
	has, saw, und := false, true, false
	writeJSON(t, filepath.Join(dir, "poll-state.json"), pollState{
		CodexPlan: "prolite", CodexHas5h: &has, CodexSaw5hElsewhere: &saw, CodexUndecodable5h: &und, CodexFactsAt: &at,
	})
	// Seed the live phantom shape so the suppression has something to clear.
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.SetCapacityEstimate("codex", ledger.Win5h, 40_000)
		l.AnchorIfUnset("codex", ledger.Win5h, now.Add(4*time.Hour), now)
		l.AddShadow("codex", ledger.Win5h, 90_000, now)
	}); err != nil {
		t.Fatal(err)
	}
	if b, _ := ledger.Open(statepaths.Ledger()).Bucket("codex", ledger.Win5h); b.UsedPct <= 0 {
		t.Fatalf("premise: the phantom derives before the dispatch: %+v", b)
	}

	var out bytes.Buffer
	if _, err := runCodexLane(&out, "hi", "gpt-5.5", "high", "", 30, nil, true, true, false, "cli", "wiring test", recFields{}, strategyFields{}); err != nil {
		t.Fatalf("spawn_error is a classified outcome, not an error return: %v", err)
	}
	// The LEDGER proves the gate ran on the dispatch path.
	b, _ := ledger.Open(statepaths.Ledger()).Bucket("codex", ledger.Win5h)
	if b.CapTokens != 0 || b.UsedPct != -1 {
		t.Fatalf("the gate must be wired into dispatch: the 5h estimate is still capped: %+v", b)
	}
	// The RECEIPT proves the suppression is disclosed, and does not start
	// with a bare separator when admission had no reason of its own.
	recs := loadReceipts(dispatchPath())
	if len(recs) != 1 {
		t.Fatalf("one receipt expected: %+v", recs)
	}
	if !strings.Contains(recs[0].AdmitReason, "codex 5h estimate suppressed") {
		t.Fatalf("the receipt must name the suppression: %q", recs[0].AdmitReason)
	}
	if strings.HasPrefix(strings.TrimSpace(recs[0].AdmitReason), ";") {
		t.Fatalf("receipt must not start with a bare separator: %q", recs[0].AdmitReason)
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
