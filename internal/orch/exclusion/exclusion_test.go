package exclusion

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// W6 canary (self-healing exclusion): the breaker arms only at the threshold,
// backs off progressively, caps, and one success clears it. Reverting the
// backoff doubling, the threshold, or the success-clears rule goes red here.
func TestBreakerProgressionAndSelfHeal(t *testing.T) {
	opt := Defaults() // threshold 2, base 1m, cap 30m
	s := Set{}

	// One transient failure must NOT brake a lane.
	s = RecordFailure(s, "local", "spawn_error", t0, opt)
	if on, _ := Excluded(s, "local", t0); on {
		t.Fatal("a single failure must not exclude (threshold 2)")
	}

	// Second failure arms: excluded for Base.
	s = RecordFailure(s, "local", "spawn_error", t0.Add(time.Second), opt)
	on, until := Excluded(s, "local", t0.Add(2*time.Second))
	if !on || !until.Equal(t0.Add(time.Second).Add(time.Minute)) {
		t.Fatalf("second failure must exclude for Base: on=%v until=%v", on, until)
	}

	// Backoff expiry self-heals structurally: no longer excluded.
	if on, _ := Excluded(s, "local", until.Add(time.Second)); on {
		t.Fatal("expired backoff must admit the lane again (half-open probe)")
	}

	// Third failure doubles: 2m.
	s = RecordFailure(s, "local", "spawn_error", t0.Add(2*time.Minute), opt)
	_, until3 := Excluded(s, "local", t0.Add(2*time.Minute+time.Second))
	if want := t0.Add(2 * time.Minute).Add(2 * time.Minute); !until3.Equal(want) {
		t.Fatalf("third failure must double the backoff: until=%v want=%v", until3, want)
	}

	// Backoff caps at Cap regardless of failure count.
	for i := 0; i < 12; i++ {
		s = RecordFailure(s, "local", "spawn_error", t0.Add(time.Hour), opt)
	}
	_, untilCap := Excluded(s, "local", t0.Add(time.Hour+time.Second))
	if want := t0.Add(time.Hour).Add(opt.Cap); !untilCap.Equal(want) {
		t.Fatalf("backoff must cap at %v: until=%v want=%v", opt.Cap, untilCap, want)
	}

	// One success clears everything.
	s = RecordSuccess(s, "local")
	if on, _ := Excluded(s, "local", t0.Add(time.Hour+2*time.Second)); on {
		t.Fatal("success must clear the breaker")
	}
	if _, tracked := s["local"]; tracked {
		t.Fatal("success must delete the entry, not just close it")
	}
}

// Absent state is a clean empty set; unreadable/corrupt state fails OPEN with
// a warning — undetermined breaker state must never exclude a lane, and must
// never do so silently (clean-ship absent-vs-undetermined).
func TestLoadStates(t *testing.T) {
	dir := t.TempDir()
	s, warn := Load(filepath.Join(dir, "absent.json"))
	if len(s) != 0 || warn != "" {
		t.Fatalf("absent: %v %q", s, warn)
	}
	p := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(p, []byte("{nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, warn = Load(p)
	if len(s) != 0 || warn == "" {
		t.Fatal("corrupt state must fail open WITH a warning")
	}
	// Round-trip.
	set := RecordFailure(Set{}, "glm", "parse_error", t0, Defaults())
	rt := filepath.Join(dir, "state.json")
	if err := Save(rt, set); err != nil {
		t.Fatal(err)
	}
	got, warn := Load(rt)
	if warn != "" || got["glm"].Fails != 1 {
		t.Fatalf("round-trip: %+v %q", got, warn)
	}
}
