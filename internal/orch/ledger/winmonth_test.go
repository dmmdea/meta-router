package ledger

import (
	"path/filepath"
	"testing"
	"time"
)

// NextMonthlyReset: strictly-after semantics at the boundary, year rollover,
// and UTC discipline (a local-time caller must not shift the anchor).
func TestNextMonthlyReset(t *testing.T) {
	cases := []struct{ now, want string }{
		{"2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z"}, // exactly at a reset → the NEXT one
		{"2026-09-15T13:45:00Z", "2026-10-01T00:00:00Z"},
		{"2026-12-31T23:59:59Z", "2027-01-01T00:00:00Z"}, // year rollover
		{"2026-01-31T12:00:00Z", "2026-02-01T00:00:00Z"}, // short-month safety
	}
	for _, c := range cases {
		now, _ := time.Parse(time.RFC3339, c.now)
		want, _ := time.Parse(time.RFC3339, c.want)
		if got := NextMonthlyReset(now); !got.Equal(want) {
			t.Fatalf("NextMonthlyReset(%s) = %s, want %s", c.now, got, want)
		}
	}
	// non-UTC input anchors to the UTC calendar, not the local one
	loc := time.FixedZone("UTC+13", 13*3600)
	local := time.Date(2026, 10, 1, 5, 0, 0, 0, loc) // still Sep 30 in UTC
	if got := NextMonthlyReset(local); !got.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("local-time caller shifted the anchor: %s", got)
	}
}

// A WinMonth bucket rolls at its reset like any window: expired shadow usage
// clears, and the caller's re-anchor lands the next calendar boundary.
func TestWinMonthBucketRollsAtCalendarReset(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	sep := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	l.AnchorIfUnset("copilot", WinMonth, NextMonthlyReset(sep), sep)
	l.AddShadow("copilot", WinMonth, 5000, sep)
	if b, ok := l.Bucket("copilot", WinMonth); !ok || b.ShadowTokens != 5000 {
		t.Fatalf("pre-roll bucket wrong: %+v ok=%v", b, ok)
	}
	oct := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	l.AddShadow("copilot", WinMonth, 1000, oct) // first spend of the new month rolls the window
	b, ok := l.Bucket("copilot", WinMonth)
	if !ok || b.ShadowTokens != 1000 {
		t.Fatalf("post-roll bucket must hold only the new month's spend: %+v ok=%v", b, ok)
	}
}
