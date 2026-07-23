// Package pace owns window-fraction math and the bindingPaceSlack scalar
// (claudexor budget-ledger lineage, reconciliation W1): per window,
// slack = elapsedFraction − usedRatio; a lane's BINDING slack is the minimum
// across its known windows. Negative slack = burning ahead of pace (E1's
// territory); large positive slack near reset = under-use (E2's territory).
// One comparable number across lanes.
//
// pace is pure and import-light so both the economics packages and the
// router wiring (via cmd) can share it without dragging network or state
// dependencies into the hot path (Bible B2).
package pace

import (
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// Duration maps a window kind to its span.
func Duration(w ledger.WindowKind) (time.Duration, bool) {
	switch w {
	case ledger.Win5h:
		return 5 * time.Hour, true
	case ledger.Win7d:
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}

// ElapsedFraction is how far through the window we are, 0..1. false when the
// bucket is unanchored, its window kind is unknown, or the reset has passed
// (a rolled window's fraction would be a lie).
func ElapsedFraction(b ledger.Bucket, now time.Time) (float64, bool) {
	d, ok := Duration(b.Window)
	if !ok || b.ResetsAt.IsZero() || !b.ResetsAt.After(now) {
		return 0, false
	}
	remaining := b.ResetsAt.Sub(now)
	if remaining > d {
		return 0, true // anchored ahead (clock skew): treat as window start
	}
	return 1 - remaining.Seconds()/d.Seconds(), true
}

// Slack is elapsedFraction − usedRatio for a bucket with a KNOWN used_pct.
func Slack(b ledger.Bucket, now time.Time) (float64, bool) {
	if b.UsedPct < 0 {
		return 0, false
	}
	ef, ok := ElapsedFraction(b, now)
	if !ok {
		return 0, false
	}
	return ef - b.UsedPct/100, true
}

// Binding is the minimum slack across the known windows — the window that
// binds first is the one that governs pacing. NOTE: callers currently pass a
// single lane's buckets on the default subject; when W2 introduces real
// subjects, per-subject slack needs the caller to filter by Subject too
// (recorded 2026-07-23 review liveness note — not a current defect).
func Binding(bs []ledger.Bucket, now time.Time) (float64, bool) {
	best, have := 0.0, false
	for _, b := range bs {
		s, ok := Slack(b, now)
		if !ok {
			continue
		}
		if !have || s < best {
			best, have = s, true
		}
	}
	return best, have
}
