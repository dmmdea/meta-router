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
// buckets never arm a boost), and ARMING additionally requires a live windowed
// trace read (≥2 same-epoch samples): with an empty, single-point, or
// stale-epoch trace the latch can HOLD or DROP on the bucket's own measured
// pct but never arm or ramp — no-data-no-boost, mirroring E1's
// no-data-no-brake. The latch is epoch-scoped: a bucket whose ResetsAt moved
// is a NEW window that never armed, so the ramp restarts at 1. All thresholds
// are CONFIG priors (orchcfg → Options) awaiting live-trace calibration.
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

// Entry is one bucket's persisted latch: the current boost level, when it last
// changed (the cooldown anchor), and the budget epoch it belongs to.
type Entry struct {
	Level     int       `json:"level"`
	ChangedAt time.Time `json:"changed_at"`
	// ResetsAt stamps the bucket's reset moment the transition was earned in.
	// A bucket whose live ResetsAt differs is a NEW window that never armed —
	// Assess zeroes the stale entry so the ramp restarts at 1 (a fresh window
	// must never inherit a full-level boost from the last one; the paced-finish
	// rule is per window).
	ResetsAt time.Time `json:"resets_at"`
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

// SaveState writes the latch via a UNIQUE temp file + rename: a torn write
// never lands, and two concurrent writers (MCP server + CLI) cannot interleave
// on a shared temp name. Concurrent saves are still last-writer-wins on the
// whole map — acceptable because the next Assess recomputes every entry from
// live bucket/trace data, so a clobbered transition self-heals within one
// consult (it can cost one cooldown, never correctness).
func SaveState(path string, s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".spend-down-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
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
// arming and ramping are cooldown-gated AND require a live windowed trace read
// (ok below) — holding and dropping work off the bucket's own measured pct, so
// a dead trace can end a boost but never start one (the asymmetry is the
// safety direction both ways).
// EpochGuard zeroes a persisted latch LEVEL from a different budget window —
// the new window never armed, so the ramp must restart (a Level-2 latch
// surviving a reset would fire full-strength into a fresh window, exactly the
// un-paced blast Q2 refinement 5 forbids). An armed entry with a ZERO stamp is
// pre-epoch-stamp legacy state and is equally stale. ChangedAt is PRESERVED as
// the cooldown anchor: across a mid-window re-anchor (a provider observation
// replacing a self-anchored estimate) this keeps the anti-flap cooldown alive,
// and across a genuine reset it delays the first arm by at most one cooldown —
// the paced direction either way. Callers comparing against prev (the
// exclusion freeze) must compare against THIS view, not the raw entry.
func EpochGuard(prev Entry, b ledger.Bucket) Entry {
	if prev.Level == 0 && prev.ResetsAt.IsZero() {
		return prev // empty entry — nothing to guard
	}
	if prev.ResetsAt.Equal(b.ResetsAt) {
		return prev // same epoch — the latch is live
	}
	// Different epoch, or an ARMED legacy entry with no stamp: the level is
	// stale; only the cooldown anchor survives.
	return Entry{ChangedAt: prev.ChangedAt}
}

func Assess(samples []calib.Sample, b ledger.Bucket, prev Entry, now time.Time, opt Options) Entry {
	opt = Normalize(opt)
	prev = EpochGuard(prev, b)
	drop := func() Entry {
		if prev.Level == 0 {
			return prev // no transition — keep the cooldown anchor as-is
		}
		return Entry{Level: 0, ChangedAt: now, ResetsAt: b.ResetsAt}
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

	avg, last, rate, ok := windowedRead(samples, b, now, opt.AvgWindow)
	// The bucket's live pct is folded into BOTH the projection anchor and the
	// hysteresis reads: ledger shadow accounting advances on paths that append
	// no trace row, so a fresh in-window trace can sit far below the real
	// consumption — the trace alone must never out-vote a higher live bucket
	// (that asymmetry always errs against boosting).
	if b.UsedPct > last {
		last = b.UsedPct
	}
	hi := avg
	if b.UsedPct > hi {
		hi = b.UsedPct
	}

	// Forecast (Q2 refinement 2): project the trajectory to ResetsAt from the
	// NEWEST measured point (the backward-looking mean lags a rising trajectory
	// by ~rate·lookback/2 — projecting from it would overstate the unused
	// fraction in the risk direction).
	projected := last + rate*b.ResetsAt.Sub(now).Hours()
	if projected > 100 {
		projected = 100
	}
	if projected < 0 {
		projected = 0
	}
	if 100-projected < opt.FloorUnusedPct {
		return drop() // window on pace to be well-used — nothing to reclaim
	}

	// Hysteresis (Q2 refinement 4): drop on the HIGHER of trace-average and
	// live bucket pct; arm only when BOTH sit below the raise line.
	if hi > opt.DropPct {
		return drop()
	}
	cooled := prev.ChangedAt.IsZero() || now.Sub(prev.ChangedAt) >= opt.Cooldown
	if ok && hi < opt.RaisePct && cooled && prev.Level < opt.MaxBoost {
		return Entry{Level: prev.Level + 1, ChangedAt: now, ResetsAt: b.ResetsAt} // arm or ramp (refinement 5)
	}
	return prev // no windowed read, in-band, cooling, or at bound: hold
}

// windowedRead returns the averaged UsedPct over the trace's recent same-epoch
// samples, the newest sample's pct (the projection anchor), and the trajectory
// rate (pct/hour). ok reports a usable windowed read (≥2 same-epoch points) —
// the precondition for ARMING. On an empty, single-point, or stale-epoch trace
// (newest row's pct above the bucket's — pre-reset history; 1pct tolerance
// absorbs observation skew) it falls back to the bucket's own live pct with
// rate 0 and ok=false: enough to hold or drop, never to arm. Rows from an
// earlier epoch VISIBLE inside the lookback (a used_pct decrease) are cut by
// the same nondecreasing-suffix rule as burnrate before any math.
func windowedRead(samples []calib.Sample, b ledger.Bucket, now time.Time, look time.Duration) (avg, last, rate float64, ok bool) {
	cut := now.Add(-look)
	var pts []calib.Sample
	for _, s := range samples {
		if s.Lane == b.Lane && s.Window == string(b.Window) && !s.TS.After(now) && !s.TS.Before(cut) {
			pts = append(pts, s)
		}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].TS.Before(pts[j].TS) })
	// Epoch suffix (burnrate rule): drop everything before the last visible
	// reset — mixed-epoch points corrupt both the average and the rate.
	start := 0
	for i := 1; i < len(pts); i++ {
		if pts[i].UsedPct < pts[i-1].UsedPct {
			start = i
		}
	}
	pts = pts[start:]
	if len(pts) == 0 || pts[len(pts)-1].UsedPct > b.UsedPct+1.0 {
		return b.UsedPct, b.UsedPct, 0, false
	}
	sum := 0.0
	for _, p := range pts {
		sum += p.UsedPct
	}
	avg = sum / float64(len(pts))
	last = pts[len(pts)-1].UsedPct
	if len(pts) < 2 {
		return avg, last, 0, false // one live point holds/drops but never arms
	}
	if span := pts[len(pts)-1].TS.Sub(pts[0].TS); span < look/4 {
		// Same min-span guard as burnrate, applied to ok itself: two points
		// seconds apart (a rapid double consult) are effectively ONE
		// instantaneous observation — not the windowed read arming requires,
		// and never a basis to extrapolate an hourly rate.
		return avg, last, 0, false
	} else {
		rate = (pts[len(pts)-1].UsedPct - pts[0].UsedPct) / span.Hours()
	}
	return avg, last, rate, true
}
