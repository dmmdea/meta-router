package slidewin

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// W6 canary (sliding-window limiter): under the limit admits and records; at
// the limit denies with retryAt = the instant the oldest stamp ages out; a
// SLIDING window admits again as stamps age (no fixed-window boundary burst).
// Reverting the prune, the limit check, or the retryAt computation goes red.
func TestSlidingWindowLimiter(t *testing.T) {
	const limit = 3
	span := time.Minute
	w := Window{}
	var ok bool
	for i := 0; i < limit; i++ {
		ok, _, w = Allow(w, t0.Add(time.Duration(i)*time.Second), limit, span)
		if !ok {
			t.Fatalf("dispatch %d under the limit must be allowed", i)
		}
	}
	// Fourth inside the window: denied, retry when the OLDEST (t0) ages out.
	ok, retryAt, w := Allow(w, t0.Add(10*time.Second), limit, span)
	if ok {
		t.Fatal("at the limit must deny")
	}
	if want := t0.Add(span); !retryAt.Equal(want) {
		t.Fatalf("retryAt must be oldest+span: %v want %v", retryAt, want)
	}
	if len(w.Stamps) != limit {
		t.Fatalf("a denial must not record a stamp: %d", len(w.Stamps))
	}
	// SLIDING: retryAt is documented as "the earliest moment a retry can
	// succeed" — probe at EXACTLY retryAt, not a tick later, so a boundary
	// inversion in the prune cannot make the API's own contract lie.
	ok, _, w = Allow(w, retryAt, limit, span)
	if !ok {
		t.Fatal("a retry at exactly retryAt must be admitted (the contract's own words)")
	}
	// And stamps older than span are pruned from persisted state.
	for _, s := range w.Stamps {
		if s.Before(retryAt.Add(time.Millisecond).Add(-span)) {
			t.Fatalf("stamp %v should have been pruned", s)
		}
	}
}

func TestLoadStates(t *testing.T) {
	dir := t.TempDir()
	w, warn := Load(filepath.Join(dir, "absent.json"))
	if len(w.Stamps) != 0 || warn != "" {
		t.Fatalf("absent: %+v %q", w, warn)
	}
	p := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(p, []byte("]["), 0o644); err != nil {
		t.Fatal(err)
	}
	w, warn = Load(p)
	if len(w.Stamps) != 0 || warn == "" {
		t.Fatal("corrupt limiter state must fail open WITH a warning")
	}
	if err := Save(p, Window{Stamps: []time.Time{t0}}); err != nil {
		t.Fatal(err)
	}
	w, warn = Load(p)
	if warn != "" || len(w.Stamps) != 1 || !w.Stamps[0].Equal(t0) {
		t.Fatalf("round-trip: %+v %q", w, warn)
	}
}
