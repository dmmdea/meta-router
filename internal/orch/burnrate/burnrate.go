// Package burnrate is the slice-4 E1 burn-rate downshift assessor — the struck
// slice-1 gate leg landing where the master brief put it (scheduler work). It
// turns the quota-trace history into a graded downshift Level per (lane,window):
// the burn multiplier m = actualRate(lookback) / requiredRate, where requiredRate
// is the rate that consumes the REMAINING budget exactly at ResetsAt. m > 1 means
// the measured trajectory exhausts the window before reset. R14-compliant by
// construction: the only trigger is a real measured over-pace (an account fact),
// never a static reserve — with a nominal burn the assessor is a no-op, and with
// an empty trace (the current live state) it returns LevelNone everywhere.
//
// Thresholds/lookbacks are CONFIG priors (orchcfg -> Options), deliberately NOT
// the SRE 14.4x/6x/1x constants: those are calibrated against a 30-day error
// budget, which is dimensionally wrong for 5h/7d subscription windows. The
// exhaust-at-reset normalization used here makes m=1 the on-pace invariant for
// ANY window length; the operator's brief-§5 Q3 answer + real-trace calibration retune.
package burnrate

import (
	"sort"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// Level is the graded downshift signal. The router demotes (+1 rank, mirroring
// the throttle shadow price) at >= LevelMedium; LevelSlow is advisory-only.
type Level int

const (
	LevelNone   Level = iota
	LevelSlow         // sustained mild over-pace (long lookback) — advisory
	LevelMedium       // clear over-pace (medium lookback) — router demotes
	LevelFast         // severe over-pace (short lookback) — router demotes
)

// Options are the burn-multiple thresholds and lookbacks. Zero-valued fields are
// invalid; use Defaults() and override from orchcfg.
type Options struct {
	FastX, MedX, SlowX          float64
	FastLook, MedLook, SlowLook time.Duration
}

// Defaults are documented PRIORS (pending operator input + real-trace calibration):
// fast = burning 3x the exhaust-at-reset rate over the last hour; medium = 1.5x
// over 3h; slow = 1.2x over 12h.
func Defaults() Options {
	return Options{FastX: 3, MedX: 1.5, SlowX: 1.2,
		FastLook: time.Hour, MedLook: 3 * time.Hour, SlowLook: 12 * time.Hour}
}

// Assess grades one bucket's burn trajectory from the quota-trace samples.
// It never mutates anything; an unknown/degenerate bucket or an empty/stale
// trace is LevelNone (fail-quiet: no data = no brake, R14).
func Assess(samples []calib.Sample, b ledger.Bucket, now time.Time, opt Options) Level {
	if b.UsedPct < 0 || b.UsedPct >= 100 || b.ResetsAt.IsZero() || !b.ResetsAt.After(now) {
		return LevelNone // unknown, exhausted (admission's job), or unanchored
	}
	required := (100 - b.UsedPct) / b.ResetsAt.Sub(now).Hours() // pct/h to land exactly at reset
	if required <= 0 {
		return LevelNone
	}
	hist := epochSuffix(filterSorted(samples, b.Lane, string(b.Window), now))
	// Cross-source epoch guard (R14 fail-quiet): epochSuffix only sees resets
	// VISIBLE within the trace. When a post-reset drop was ingested UNTRACED
	// (run.go applyRunOutcome → quotasig changed-gate suppresses the post-reset
	// trace row), the trace's newest row can predate the bucket's fresh budget
	// epoch while carrying a much higher used_pct. That is stale-epoch data, not
	// a live over-pace — assessing it would falsely brake a freshly reset lane.
	// A 1pct tolerance absorbs benign observation skew.
	if len(hist) > 0 && hist[len(hist)-1].UsedPct > b.UsedPct+1.0 {
		return LevelNone // trace predates the bucket's current epoch — no valid trajectory
	}
	checks := []struct {
		look   time.Duration
		thresh float64
		level  Level
	}{
		{opt.FastLook, opt.FastX, LevelFast},
		{opt.MedLook, opt.MedX, LevelMedium},
		{opt.SlowLook, opt.SlowX, LevelSlow},
	}
	for _, c := range checks {
		if m, ok := multiple(hist, now, c.look, required); ok && m >= c.thresh {
			return c.level
		}
	}
	return LevelNone
}

// filterSorted keeps the bucket's own (lane,window) samples up to now, ascending.
func filterSorted(samples []calib.Sample, lane, window string, now time.Time) []calib.Sample {
	var out []calib.Sample
	for _, s := range samples {
		if s.Lane == lane && s.Window == window && !s.TS.After(now) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out
}

// epochSuffix returns the maximal suffix with nondecreasing UsedPct — samples
// before the last quota reset (where used_pct drops) belong to a spent budget
// epoch and must never feed the current trajectory.
func epochSuffix(s []calib.Sample) []calib.Sample {
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i].UsedPct < s[i-1].UsedPct {
			start = i
		}
	}
	return s[start:]
}

// multiple computes the burn multiple over one lookback. ok=false when the
// window lacks a usable pair: fewer than 2 points, or a span under look/4 (two
// points a minute apart must not extrapolate an hourly rate — noise guard).
func multiple(hist []calib.Sample, now time.Time, look time.Duration, required float64) (float64, bool) {
	cut := now.Add(-look)
	var pts []calib.Sample
	for _, s := range hist {
		if !s.TS.Before(cut) {
			pts = append(pts, s)
		}
	}
	if len(pts) < 2 {
		return 0, false
	}
	span := pts[len(pts)-1].TS.Sub(pts[0].TS)
	if span < look/4 {
		return 0, false
	}
	actual := (pts[len(pts)-1].UsedPct - pts[0].UsedPct) / span.Hours()
	return actual / required, true
}
