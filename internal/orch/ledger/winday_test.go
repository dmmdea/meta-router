package ledger

import (
	"testing"
	"time"
)

// NextDailyReset: strictly-after at the boundary, month and year rollover.
func TestNextDailyReset(t *testing.T) {
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 9, 6, 23, 59, 59, 0, time.UTC), time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)}, // exactly midnight → NEXT midnight
		{time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 12, 31, 1, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)},
		// A non-UTC clock is normalised: 2026-09-06 20:30 -05:00 is 01:30Z on the 7th → the 8th.
		{time.Date(2026, 9, 6, 20, 30, 0, 0, time.FixedZone("BOG", -5*3600)), time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := NextDailyReset(c.now); !got.Equal(c.want) {
			t.Fatalf("NextDailyReset(%s) = %s, want %s", c.now, got, c.want)
		}
	}
}

// A day bucket rolls at its reset like every other window, and a trial
// bucket — never anchored — never rolls and never derives a percentage until
// it is capped AND anchored (RS4 holds: an unanchored window never derives).
func TestDayRollsTrialNever(t *testing.T) {
	now := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)
	l := &Ledger{buckets: map[string]*Bucket{}}
	l.SetCapacityEstimate("groq", WinDay, 1000_000)
	l.AnchorIfUnset("groq", WinDay, NextDailyReset(now), now)
	l.AddShadow("groq", WinDay, 250_000, now)
	if b, _ := l.Bucket("groq", WinDay); b.UsedPct != 25 || b.CapSource != CapSourceEstimate {
		t.Fatalf("day bucket: %+v", b)
	}
	// Past midnight: rolled on the next touch.
	later := NextDailyReset(now).Add(time.Minute)
	l.AddShadow("groq", WinDay, 1000, later)
	if b, _ := l.Bucket("groq", WinDay); b.ShadowTokens != 1000 || !b.ResetsAt.IsZero() {
		t.Fatalf("day bucket must roll at 00:00 UTC: %+v", b)
	}

	l.SetCapacityEstimate("nim", WinTrial, 1000_000)
	l.AddShadow("nim", WinTrial, 5_000, now)
	if b, _ := l.Bucket("nim", WinTrial); b.UsedPct != -1 || !b.ResetsAt.IsZero() {
		t.Fatalf("an unanchored trial pool never derives (RS4): %+v", b)
	}
	// Anchoring a trial far in the future lets it derive; it still does not roll.
	l.AnchorIfUnset("nim", WinTrial, now.AddDate(10, 0, 0), now)
	l.AddShadow("nim", WinTrial, 5_000, now.AddDate(1, 0, 0))
	if b, _ := l.Bucket("nim", WinTrial); b.UsedPct != 1 || b.ShadowTokens != 10_000 {
		t.Fatalf("trial pool accumulates across a year without rolling: %+v", b)
	}
}
