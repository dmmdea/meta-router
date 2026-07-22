// Package spenddown is the slice-4 E2 spend-down assessor — the mirror image
// of burnrate (E1). Where E1 demotes a lane whose measured trajectory exhausts
// its window BEFORE reset, E2 promotes a lane whose window is on pace to WASTE
// budget at reset: a premium window that is under-utilized near its
// (lane-specific) reset gets a bounded rank BOOST for explicitly-tagged,
// already-queued batch work — never interactive consults, never fabricated
// work (nothing here dispatches; the boost only re-ranks a consult the caller
// already made for a queued, batch-tagged task).
//
// Q2-locked refinements (docs/specs/2026-07-15-v3-open-questions-resolved.md):
//  1. Completion-fit start gate — Fits(): the task's expected duration must end
//     Buffer before the bucket's reset; an unknown duration never fits.
//  2. Forecast trigger — the PROJECTED end-of-window unused fraction (windowed
//     trace rate extrapolated to ResetsAt), not the instantaneous UsedPct.
//  3. Lane-specific ResetsAt — per-bucket: the ledger anchors each window's own
//     reset (5h block-anchor, 7d provider/stream anchor), so the rolling-window
//     caveat (CC2) is carried by the bucket, not re-modeled here.
//  4. Anti-oscillation — hysteresis band (arm below RaisePct, disarm above
//     DropPct, hold in between), windowed/averaged quota reads (AvgWindow), and
//     a cooldown between rank RAISES (drops are immediate: stopping a dump is
//     the safety direction).
//  5. Ramped, bounded boost — arms at 1 and escalates one level per elapsed
//     cooldown while still qualifying, bounded at MaxBoost (the DoorDash
//     overdelivery lesson: a paced finish, never a blast).
//
// R14-compliant by construction: the only trigger is a real measured
// under-utilization (provider-sourced or fitted percentages — estimate-sourced
// buckets never arm a boost), and with an unknown pct, an unanchored window,
// or a window far from reset the assessor is a no-op. All thresholds are
// CONFIG priors (orchcfg → Options) awaiting live-trace calibration.
package spenddown

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// Options are the spend-down priors. Zero/invalid fields are normalized from
// Defaults (hand-edit-damage rule, consistent with orchcfg tier numerics).
type Options struct {
	FloorUnusedPct float64       // arm only when the projected end-of-window UNUSED fraction ≥ this
	Horizon        time.Duration // arm only within this of the bucket's reset
	RaisePct       float64       // hysteresis: arm below this averaged UsedPct
	DropPct        float64       // hysteresis: disarm above this averaged UsedPct
	Cooldown       time.Duration // minimum gap between rank raises (arm + ramp)
	Buffer         time.Duration // completion-fit: the task must end this far before reset
	MaxBoost       int           // ramp bound (rank-delta, never scalar)
	AvgWindow      time.Duration // windowed quota-read lookback for the average + rate
}

// Defaults are documented PRIORS (pending real-trace calibration, Q2): boost
// batch work onto a window projected to strand ≥30% of its budget, within 90
// minutes of reset, arming under 25% used / disarming over 35% (hysteresis),
// one rank level per 10-minute cooldown, bounded at 2, with a 10-minute
// completion buffer and 15-minute averaged reads.
func Defaults() Options {
	return Options{FloorUnusedPct: 30, Horizon: 90 * time.Minute,
		RaisePct: 25, DropPct: 35, Cooldown: 10 * time.Minute,
		Buffer: 10 * time.Minute, MaxBoost: 2, AvgWindow: 15 * time.Minute}
}

// Normalize repairs invalid options: non-positive fields backfill from
// Defaults, and an inverted hysteresis band (Raise ≥ Drop) resets BOTH — a
// broken band must fail safe to the documented priors, never half-apply.
func Normalize(o Options) Options {
	d := Defaults()
	if o.FloorUnusedPct <= 0 || o.FloorUnusedPct >= 100 {
		o.FloorUnusedPct = d.FloorUnusedPct
	}
	if o.Horizon <= 0 {
		o.Horizon = d.Horizon
	}
	if o.RaisePct <= 0 || o.DropPct <= 0 || o.RaisePct >= o.DropPct || o.DropPct >= 100 {
		o.RaisePct, o.DropPct = d.RaisePct, d.DropPct
	}
	if o.Cooldown <= 0 {
		o.Cooldown = d.Cooldown
	}
	if o.Buffer <= 0 {
		o.Buffer = d.Buffer
	}
	if o.MaxBoost <= 0 {
		o.MaxBoost = d.MaxBoost
	}
	if o.AvgWindow <= 0 {
		o.AvgWindow = d.AvgWindow
	}
	return o
}

// Entry is one bucket's persisted latch: the current boost level and when it
// last changed (the cooldown anchor).
type Entry struct {
	Level     int       `json:"level"`
	ChangedAt time.Time `json:"changed_at"`
}

// State is the persisted latch set, keyed lane+"|"+window (the ledger's key
// shape).
type State map[string]Entry

// Key returns the State key for a bucket.
func Key(b ledger.Bucket) string { return b.Lane + "|" + string(b.Window) }

// LoadState reads the latch file fail-open: missing or corrupt → empty (the
// latch rebuilds from subsequent assessments; losing it costs one cooldown,
// never correctness).
func LoadState(path string) State {
	raw, err := os.ReadFile(path)
	if err != nil {
		return State{}
	}
	var s State
	if json.Unmarshal(raw, &s) != nil || s == nil {
		return State{}
	}
	return s
}

// SaveState writes the latch atomically (temp + rename) so a torn write can
// never corrupt the file into a fail-open reset mid-band.
func SaveState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), ".spend-down.tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Fits is the completion-fit start gate (Q2 refinement 1): the task's expected
// duration must end Buffer before the bucket's reset. An unknown duration
// (est ≤ 0) never fits — the gate stays closed without the data to clear it
// (fail-quiet, mirroring E1's no-data-no-brake in the promote direction:
// no-data-no-boost).
func Fits(b ledger.Bucket, est time.Duration, now time.Time, opt Options) bool {
	if est <= 0 || b.ResetsAt.IsZero() {
		return false
	}
	return !now.Add(est).Add(Normalize(opt).Buffer).After(b.ResetsAt)
}

// Assess grades one bucket's latch transition from the quota trace + the
// bucket's live numbers. Pure given prev — it never touches disk; the caller
// owns Load/Save. Disqualified or over-band buckets drop to 0 immediately;
// arming and ramping are cooldown-gated.
func Assess(samples []calib.Sample, b ledger.Bucket, prev Entry, now time.Time, opt Options) Entry {
	opt = Normalize(opt)
	drop := func() Entry {
		if prev.Level == 0 {
			return prev // no transition — keep the cooldown anchor as-is
		}
		return Entry{Level: 0, ChangedAt: now}
	}

	// Disqualifiers: no real measured fact → no boost (R14).
	if b.UsedPct < 0 || b.UsedPct >= 100 || b.ResetsAt.IsZero() || !b.ResetsAt.After(now) {
		return drop()
	}
	if b.Source != "provider" && b.CapSource == ledger.CapSourceEstimate {
		// Estimate-derived percentage: may inform throttling elsewhere (S2R-3)
		// but never arms a spend-down boost — boosting on a config guess could
		// dump real quota into a window that was never actually idle.
		return drop()
	}
	if b.ResetsAt.Sub(now) > opt.Horizon {
		return drop() // not near reset — spend-down is an end-of-window mechanism
	}

	avg, rate := windowedRead(samples, b, now, opt.AvgWindow)

	// Forecast (Q2 refinement 2): project the averaged trajectory to ResetsAt.
	projected := avg + rate*b.ResetsAt.Sub(now).Hours()
	if projected > 100 {
		projected = 100
	}
	if 100-projected < opt.FloorUnusedPct {
		return drop() // window on pace to be well-used — nothing to reclaim
	}

	// Hysteresis (Q2 refinement 4).
	if avg > opt.DropPct {
		return drop()
	}
	cooled := prev.ChangedAt.IsZero() || now.Sub(prev.ChangedAt) >= opt.Cooldown
	if avg < opt.RaisePct && cooled && prev.Level < opt.MaxBoost {
		return Entry{Level: prev.Level + 1, ChangedAt: now} // arm or ramp (refinement 5)
	}
	return prev // in-band, cooling, or at bound: hold
}

// windowedRead returns the averaged UsedPct over the trace's recent samples
// plus the trajectory rate (pct/hour), falling back to the bucket's own live
// pct with rate 0 when the trace is empty, single-point, or stale-epoch
// (newest row's pct above the bucket's — pre-reset history, the burnrate epoch
// guard; 1pct tolerance absorbs observation skew).
func windowedRead(samples []calib.Sample, b ledger.Bucket, now time.Time, look time.Duration) (avg, rate float64) {
	cut := now.Add(-look)
	var pts []calib.Sample
	for _, s := range samples {
		if s.Lane == b.Lane && s.Window == string(b.Window) && !s.TS.After(now) && !s.TS.Before(cut) {
			pts = append(pts, s)
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].TS.Before(pts[j].TS) })
	if len(pts) == 0 || pts[len(pts)-1].UsedPct > b.UsedPct+1.0 {
		return b.UsedPct, 0
	}
	sum := 0.0
	for _, p := range pts {
		sum += p.UsedPct
	}
	avg = sum / float64(len(pts))
	if len(pts) >= 2 {
		if span := pts[len(pts)-1].TS.Sub(pts[0].TS); span >= look/4 {
			// Same min-span guard as burnrate: two points a minute apart must
			// not extrapolate an hourly rate.
			rate = (pts[len(pts)-1].UsedPct - pts[0].UsedPct) / span.Hours()
		}
	}
	return avg, rate
}
