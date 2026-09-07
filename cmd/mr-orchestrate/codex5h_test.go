package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/codexlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
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
