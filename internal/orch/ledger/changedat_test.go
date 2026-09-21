package ledger

import (
	"path/filepath"
	"testing"
	"time"
)

// ChangedAt is the SEMANTIC freshness signal for consumers (the mr-hook quota
// banner). It must move only when a displayed value moves. The first attempt at
// banner gating keyed on the ledger file's mtime, and `route` rewrites the file on
// every consult whether or not anything changed -- so the gate almost never fired.
// These tests drive the real write path (UpdateChecked), not a hand-set timestamp.

func withClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := clock
	clock = func() time.Time { return at }
	t.Cleanup(func() { clock = prev })
}

func bucketAfter(t *testing.T, path string) Bucket {
	t.Helper()
	l, warn := OpenChecked(path)
	if warn != "" {
		t.Fatalf("open: %s", warn)
	}
	b, ok := l.Bucket("claude", Win5h)
	if !ok {
		t.Fatal("bucket missing")
	}
	return b
}

var tc0 = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// The bug scenario: the same percentage re-reported 20 minutes later must NOT look
// like news. Before this change there was no ChangedAt, and the file mtime moved.
func TestChangedAtNotRestampedWhenValueRepeats(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	withClock(t, tc0)
	if err := Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42, tc0.Add(3*time.Hour), tc0) }); err != nil {
		t.Fatal(err)
	}
	if got := bucketAfter(t, p).ChangedAt; !got.Equal(tc0) {
		t.Fatalf("first observation must stamp ChangedAt=%v, got %v", tc0, got)
	}
	later := tc0.Add(20 * time.Minute)
	withClock(t, later)
	if err := Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42, tc0.Add(3*time.Hour), later) }); err != nil {
		t.Fatal(err)
	}
	b := bucketAfter(t, p)
	if !b.ChangedAt.Equal(tc0) {
		t.Fatalf("re-reporting the same value restamped ChangedAt to %v (want %v)", b.ChangedAt, tc0)
	}
	if !b.ObservedAt.Equal(later) {
		t.Fatalf("ObservedAt must still advance (%v), it is the staleness alarm's signal", b.ObservedAt)
	}
}

func TestChangedAtStampedWhenPercentMoves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	withClock(t, tc0)
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42, tc0.Add(3*time.Hour), tc0) })
	later := tc0.Add(20 * time.Minute)
	withClock(t, later)
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 57, tc0.Add(3*time.Hour), later) })
	if got := bucketAfter(t, p).ChangedAt; !got.Equal(later) {
		t.Fatalf("a real change must stamp ChangedAt=%v, got %v", later, got)
	}
}

// The banner renders "%.0f%%": drift below one rendered unit is invisible to the
// reader, so it must not count as a change either.
func TestChangedAtIgnoresSubPercentDrift(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	withClock(t, tc0)
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42.1, tc0.Add(3*time.Hour), tc0) })
	withClock(t, tc0.Add(20*time.Minute))
	_ = Update(p, func(l *Ledger) {
		l.ObserveProvider("claude", Win5h, 42.3, tc0.Add(3*time.Hour), tc0.Add(20*time.Minute))
	})
	if got := bucketAfter(t, p).ChangedAt; !got.Equal(tc0) {
		t.Fatalf("42.1 -> 42.3 renders as 42%% both times; ChangedAt moved to %v", got)
	}
}

func TestChangedAtStampedWhenResetMoves(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	withClock(t, tc0)
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42, tc0.Add(3*time.Hour), tc0) })
	later := tc0.Add(20 * time.Minute)
	withClock(t, later)
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42, tc0.Add(5*time.Hour), later) })
	if got := bucketAfter(t, p).ChangedAt; !got.Equal(later) {
		t.Fatalf("a moved reset is a displayed change; ChangedAt=%v", got)
	}
}

// A window expiring is news (a lane recovers), and it happens inside a mutator's
// roll() -- the stamp must catch it without every mutator knowing about ChangedAt.
func TestChangedAtStampedByExpiryRoll(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	withClock(t, tc0)
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 90, tc0.Add(1*time.Hour), tc0) })
	after := tc0.Add(2 * time.Hour) // past the reset
	withClock(t, after)
	_ = Update(p, func(l *Ledger) { l.AddShadow("claude", Win5h, 10, after) })
	if got := bucketAfter(t, p).ChangedAt; !got.Equal(after) {
		t.Fatalf("an expiry roll changes what the banner shows; ChangedAt=%v", got)
	}
}

// ChangedAt must follow the CALLER's clock, not the wall clock behind it: tests,
// replays and backfills inject time, and a wall-clock stamp made two identical
// runs serialize differently (it broke TestCodex5hKnobOffIsByteIdenticalToMain,
// and was written in local time while every other field is UTC).
func TestChangedAtFollowsCallerClockNotWallClock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	wall := time.Date(2031, 1, 1, 0, 0, 0, 0, time.FixedZone("X", -5*3600))
	withClock(t, wall)
	caller := tc0
	_ = Update(p, func(l *Ledger) { l.ObserveProvider("claude", Win5h, 42, caller.Add(3*time.Hour), caller) })
	got := bucketAfter(t, p).ChangedAt
	if !got.Equal(caller) {
		t.Fatalf("ChangedAt=%v followed the wall clock; want the caller's %v", got, caller)
	}
	if got.Location() != time.UTC {
		t.Fatalf("ChangedAt must be UTC like every other field, got %v", got.Location())
	}
}
