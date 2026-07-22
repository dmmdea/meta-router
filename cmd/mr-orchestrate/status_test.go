package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

func TestBuildStatus(t *testing.T) {
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	bs := []ledger.Bucket{
		{Lane: "claude", Window: "5h", UsedPct: 20, ResetsAt: now.Add(time.Hour), Source: "provider"},
		{Lane: "claude", Window: "7d", UsedPct: 97, ResetsAt: now.Add(48 * time.Hour), Source: "provider"},
	}
	fzs := []fuses.Fuse{
		{Name: "fable-carveout", ExpiresAt: time.Date(2026, 7, 7, 7, 0, 0, 0, time.UTC)}, // expired at now
		{Name: "weekly-boost-50pct", ExpiresAt: time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)},
	}
	st := buildStatus(bs, fzs, orchcfg.Defaults(), now, nil, nil)

	lane, ok := st.Lanes["claude"]
	if !ok {
		t.Fatal("claude lane missing")
	}
	if lane.State != "exhausted" || lane.ResumeAt == nil || !lane.ResumeAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("97%% weekly must exhaust with resume: %+v", lane)
	}
	if len(lane.Windows) != 2 {
		t.Fatalf("want both windows listed, got %+v", lane.Windows)
	}
	if len(st.ActiveFuses) != 1 || st.ActiveFuses[0].Name != "weekly-boost-50pct" {
		t.Fatalf("expired fuse must drop out: %+v", st.ActiveFuses)
	}
	if st.BillingMode != orchcfg.BillingSubscription {
		t.Fatalf("billing mode must surface: %+v", st.BillingMode)
	}
}

// Task 8: maybeFit is SetCapacity's production caller — an agreeing fit lands
// versioned; a fit within 10% of the current cap causes no churn; a >10%
// drifted fit replaces it.
func TestMaybeFitAppliesAgreedFitWithoutChurn(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	mk := func(pct float64, shadow int64) calib.Sample {
		return calib.Sample{Lane: "claude", Window: "7d", UsedPct: pct, ShadowTokens: shadow}
	}
	samples := []calib.Sample{mk(25, 250_000), mk(30, 310_000), mk(40, 390_000), mk(50, 505_000), mk(60, 590_000)} // ≈1M
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))

	notes := maybeFit(l, samples, now)
	if len(notes) != 1 || !strings.Contains(notes[0], "capacity fitted claude/7d = 1000000 tokens (n=5 samples)") {
		t.Fatalf("agreeing 7d samples must fit exactly one window with the note: %v", notes)
	}
	b, _ := l.Bucket("claude", ledger.Win7d)
	if b.CapTokens != 1_000_000 || b.CapVersion != 1 || b.CapSource != "" {
		t.Fatalf("fit must SetCapacity (versioned, measured — clears any estimate mark): %+v", b)
	}

	if notes := maybeFit(l, samples, now); len(notes) != 0 {
		t.Fatalf("a re-fit within 10%% of the current cap must not churn: %v", notes)
	}
	if b, _ := l.Bucket("claude", ledger.Win7d); b.CapVersion != 1 {
		t.Fatalf("cap version churned without a >10%% change: %+v", b)
	}

	drift := []calib.Sample{mk(25, 300_000), mk(30, 372_000), mk(40, 468_000), mk(50, 606_000), mk(60, 708_000)} // ≈1.2M
	if notes := maybeFit(l, drift, now); len(notes) != 1 {
		t.Fatalf("a >10%% drifted fit must apply: %v", notes)
	}
	if b, _ := l.Bucket("claude", ledger.Win7d); b.CapTokens != 1_200_000 || b.CapVersion != 2 {
		t.Fatalf("drifted fit must land versioned: %+v", b)
	}
}

// Too few / disagreeing / sub-MinPct samples must leave the ledger untouched
// (fail-open: no fit is a normal outcome, not an error).
func TestMaybeFitNoFitNoTouch(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	few := []calib.Sample{{Lane: "claude", Window: "7d", UsedPct: 40, ShadowTokens: 400_000}}
	if notes := maybeFit(l, few, now); len(notes) != 0 {
		t.Fatalf("under MinSamples must not fit: %v", notes)
	}
	if b, ok := l.Bucket("claude", ledger.Win7d); ok && b.CapTokens != 0 {
		t.Fatalf("no fit must not set capacity: %+v", b)
	}
	if notes := maybeFit(l, nil, now); len(notes) != 0 {
		t.Fatalf("an absent trace (nil samples) must be a silent no-op: %v", notes)
	}
}

// E1 surfacing: a downshifted lane shows burn_downshift in status JSON.
func TestBuildStatusSurfacesBurnDownshift(t *testing.T) {
	now := time.Now().UTC()
	bs := []ledger.Bucket{{Lane: "glm", Window: ledger.Win5h, UsedPct: 40,
		ResetsAt: now.Add(6 * time.Hour), Source: "provider", ObservedAt: now}}
	st := buildStatus(bs, nil, orchcfg.Config{}, now, map[string]int{"glm": 3}, nil)
	if st.Lanes["glm"].BurnDownshift != 3 {
		t.Fatalf("burn_downshift must surface, got %d", st.Lanes["glm"].BurnDownshift)
	}
}

// E6: quota_health is ALWAYS emitted; a missing trace carries the unfed-fitter
// note; stale provider buckets are named.
func TestStatusQuotaHealthBlock(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	bs := []ledger.Bucket{{Lane: "claude", Window: ledger.Win5h, UsedPct: 10,
		ResetsAt: now.Add(time.Hour), Source: "provider", ObservedAt: now.Add(-60 * time.Hour)}}
	qh := buildQuotaHealth(bs, "Z:\\does\\not\\exist.jsonl", orchcfg.Config{QuotaStaleHours: 48}, now)
	if qh == nil || qh.TraceExists || !qh.TraceStale {
		t.Fatalf("missing trace must yield a stale quota_health block, got %+v", qh)
	}
	if qh.TraceNote == "" {
		t.Fatalf("the unfed-fitter note must be present when the trace is missing/empty")
	}
	if len(qh.StaleBuckets) != 1 {
		t.Fatalf("the 60h-old provider bucket must be named, got %v", qh.StaleBuckets)
	}
}
