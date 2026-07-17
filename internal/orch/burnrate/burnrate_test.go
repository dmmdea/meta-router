package burnrate

import (
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

var t0 = time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)

func smp(lane, window string, agoMin int, used float64) calib.Sample {
	return calib.Sample{TS: t0.Add(-time.Duration(agoMin) * time.Minute), Lane: lane, Window: window, UsedPct: used}
}

func bkt(lane string, w ledger.WindowKind, used float64, resetsInMin int) ledger.Bucket {
	return ledger.Bucket{Lane: lane, Window: w, UsedPct: used, ResetsAt: t0.Add(time.Duration(resetsInMin) * time.Minute)}
}

// THE MIGRATED SLICE-1 GATE LEG (roadmap carry-forward -> slice-4 E1): burn-rate
// downshift precision/recall on labeled replayed traces. The labeled fixture below
// is the gate: Assess must classify EVERY case exactly (precision = recall = 1.0
// on the fixture). The burn multiplier is m = actualRate(lookback) / requiredRate,
// requiredRate = (100-used)/(hours until reset) — m > 1 means the measured
// trajectory exhausts the window BEFORE reset (a real account fact, R14-compliant).
func TestAssessLabeledFixtureGate(t *testing.T) {
	cases := []struct {
		name    string
		samples []calib.Sample
		b       ledger.Bucket
		want    Level
	}{
		{
			// 40% used, reset in 6h -> required 10 pct/h. Last hour burned 0->40
			// (40 pct/h): m=4 >= FastX(3) over the 1h lookback -> FAST.
			name: "fast_burn",
			samples: []calib.Sample{
				smp("claude", "5h", 60, 0), smp("claude", "5h", 30, 20), smp("claude", "5h", 0, 40)},
			b:    bkt("claude", ledger.Win5h, 40, 360),
			want: LevelFast,
		},
		{
			// 55% used, reset in 5h -> required 9 pct/h. Last 2h burned 25->55
			// (15 pct/h): fast(1h) m=1.67 < 3 misses; med(3h) m=1.67 >= 1.5 -> MEDIUM.
			name: "medium_burn",
			samples: []calib.Sample{
				smp("claude", "5h", 120, 25), smp("claude", "5h", 60, 40), smp("claude", "5h", 0, 55)},
			b:    bkt("claude", ledger.Win5h, 55, 300),
			want: LevelMedium,
		},
		{
			// 7d window: 50% used, reset in 72h -> required 0.694 pct/h. 12h burned
			// 40->50 (0.833 pct/h): m=1.2 >= SlowX(1.2) over the 12h lookback -> SLOW.
			name: "slow_burn",
			samples: []calib.Sample{
				smp("claude", "7d", 720, 40), smp("claude", "7d", 360, 45), smp("claude", "7d", 0, 50)},
			b:    bkt("claude", ledger.Win7d, 50, 4320),
			want: LevelSlow,
		},
		{
			// Exactly on pace (10 pct/h vs required 10 pct/h): m=1 everywhere,
			// and the slow lookback's span guard (2h < 12h/4) skips it -> NONE.
			name: "nominal",
			samples: []calib.Sample{
				smp("claude", "5h", 120, 20), smp("claude", "5h", 60, 30), smp("claude", "5h", 0, 40)},
			b:    bkt("claude", ledger.Win5h, 40, 360),
			want: LevelNone,
		},
		{
			// Samples straddle a reset (90% -> 2%): only the post-reset suffix may
			// be used (pre-reset history is a different budget epoch) -> the short
			// clean suffix burns 6 pct/h vs required 20 -> NONE.
			name: "post_reset",
			samples: []calib.Sample{
				smp("claude", "5h", 120, 80), smp("claude", "5h", 60, 90),
				smp("claude", "5h", 30, 2), smp("claude", "5h", 0, 5)},
			b:    bkt("claude", ledger.Win5h, 5, 285),
			want: LevelNone,
		},
		{
			// Burned hard 10h ago then flat: fast/med lookbacks lack fresh points,
			// the 12h average still exceeds pace -> SLOW (advisory only; the router
			// demotes at >= Medium, so a recovered lane is NOT re-routed).
			name: "recovering_is_at_most_slow",
			samples: []calib.Sample{
				smp("claude", "7d", 600, 20), smp("claude", "7d", 480, 55),
				smp("claude", "7d", 240, 58), smp("claude", "7d", 0, 60)},
			b:    bkt("claude", ledger.Win7d, 60, 2880),
			want: LevelSlow,
		},
		{name: "empty_trace_is_none", samples: nil, b: bkt("claude", ledger.Win5h, 40, 360), want: LevelNone},
		{
			// Cross-source epoch skew: the trace's newest row (80%) is from a spent
			// budget epoch (a post-reset drop was ingested UNTRACED, so the trace
			// never saw the reset), but the bucket already reflects the fresh epoch
			// (UsedPct 2). The trace predates the bucket's budget epoch → no valid
			// trajectory data for the CURRENT epoch → NONE (must not brake a freshly
			// reset lane, R14). epochSuffix alone can't catch this (no drop is
			// visible WITHIN the trace); the guard cross-checks trace vs bucket.
			name: "trace_predates_bucket_reset_is_none",
			samples: []calib.Sample{
				smp("claude", "5h", 120, 20), smp("claude", "5h", 60, 80)},
			b:    bkt("claude", ledger.Win5h, 2, 285),
			want: LevelNone,
		},
	}
	for _, c := range cases {
		if got := Assess(c.samples, c.b, t0, Defaults()); got != c.want {
			t.Errorf("%s: Assess = %d, want %d", c.name, got, c.want)
		}
	}
}

// Degenerate buckets never produce a level: unknown used_pct (-1), no reset
// anchor, reset in the past, already-exhausted (>=100: admission's job).
func TestAssessDegenerateBucketsAreNone(t *testing.T) {
	s := []calib.Sample{smp("claude", "5h", 60, 0), smp("claude", "5h", 0, 40)}
	cases := []ledger.Bucket{
		bkt("claude", ledger.Win5h, -1, 360),
		{Lane: "claude", Window: ledger.Win5h, UsedPct: 40},
		bkt("claude", ledger.Win5h, 40, -10),
		bkt("claude", ledger.Win5h, 100, 360),
	}
	for i, b := range cases {
		if got := Assess(s, b, t0, Defaults()); got != LevelNone {
			t.Errorf("case %d: degenerate bucket must be LevelNone, got %d", i, got)
		}
	}
}

// Samples from OTHER lanes/windows never contaminate an assessment.
func TestAssessFiltersLaneAndWindow(t *testing.T) {
	s := []calib.Sample{
		smp("glm", "5h", 60, 0), smp("glm", "5h", 0, 40), // other lane burns fast
		smp("claude", "7d", 60, 0), smp("claude", "7d", 0, 40)} // other window burns fast
	if got := Assess(s, bkt("claude", ledger.Win5h, 40, 360), t0, Defaults()); got != LevelNone {
		t.Errorf("cross-lane/window contamination: got %d, want LevelNone", got)
	}
}
