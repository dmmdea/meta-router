// Package exclusion is the W6 self-healing lane breaker: a lane whose
// ADAPTER keeps failing (spawn_error — the binary/infra is broken on OUR
// side; parse_error — its output contract broke; for the local lane also
// api_error — a harness tier fault) is excluded from routing for a backoff
// that doubles on each further failure and clears on the first success.
//
// Scope discipline: this breaker owns INFRASTRUCTURE health only. Quota
// states (rate_limit, throttled, exhausted) belong to admission/ledger;
// refusals and policy belong to the lanes. Excluding on a vendor's quota
// signal here would double-count pressure the router already prices.
//
// Self-healing is structural: when the backoff expires the lane simply
// routes again (a natural half-open probe); one success deletes the entry.
// Absence-vs-undetermined (clean-ship): an unreadable or corrupt state file
// yields an EMPTY set plus a warning string — failing open (never excluding
// on undetermined state) is the availability-safe direction, and the warn
// makes the degradation visible instead of silent.
package exclusion

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Entry is one lane's breaker state.
type Entry struct {
	Fails    int       `json:"fails"`               // consecutive qualifying failures
	Until    time.Time `json:"until,omitzero"`      // excluded before this instant (zero = armed but not excluding)
	LastFail time.Time `json:"last_fail,omitzero"`  // observability only
	LastNote string    `json:"last_note,omitempty"` // the failure class that armed it
}

// Set maps lane → breaker state.
type Set map[string]Entry

// Options are the backoff priors. Config data, never constants of nature:
// zero values fall back to Defaults() (the no-blog-lore-constants rule).
type Options struct {
	Threshold int           // failures before the first exclusion (a single transient must not brake a lane)
	Base      time.Duration // first exclusion length
	Cap       time.Duration // backoff ceiling
}

func Defaults() Options {
	return Options{Threshold: 2, Base: time.Minute, Cap: 30 * time.Minute}
}

func (o Options) normalized() Options {
	d := Defaults()
	if o.Threshold <= 0 {
		o.Threshold = d.Threshold
	}
	if o.Base <= 0 {
		o.Base = d.Base
	}
	if o.Cap <= 0 {
		o.Cap = d.Cap
	}
	return o
}

// backoff is Base doubled per exclusion beyond the threshold, capped.
func backoff(failsPastThreshold int, opt Options) time.Duration {
	d := opt.Base
	for i := 1; i < failsPastThreshold; i++ {
		d *= 2
		if d >= opt.Cap {
			return opt.Cap
		}
	}
	if d > opt.Cap {
		return opt.Cap
	}
	return d
}

// RecordFailure notes one qualifying failure for lane and arms/extends the
// exclusion once Threshold is reached. Pure over the Set; the caller persists.
func RecordFailure(s Set, lane, note string, now time.Time, opt Options) Set {
	if s == nil {
		s = Set{}
	}
	opt = opt.normalized()
	e := s[lane]
	e.Fails++
	e.LastFail = now
	e.LastNote = note
	if past := e.Fails - opt.Threshold + 1; past >= 1 {
		e.Until = now.Add(backoff(past, opt))
	}
	s[lane] = e
	return s
}

// RecordSuccess clears the lane's breaker entirely — one healthy dispatch is
// the self-healing signal.
func RecordSuccess(s Set, lane string) Set {
	if s == nil {
		return s
	}
	delete(s, lane)
	return s
}

// Excluded reports whether lane is currently excluded, and until when.
func Excluded(s Set, lane string, now time.Time) (bool, time.Time) {
	e, ok := s[lane]
	if !ok || e.Until.IsZero() {
		return false, time.Time{}
	}
	if now.Before(e.Until) {
		return true, e.Until
	}
	return false, time.Time{}
}

// Reason is the human string surfaced on a masked lane.
func Reason(s Set, lane string) string {
	e := s[lane]
	return fmt.Sprintf("excluded after %d adapter failure(s) (last: %s); self-heals at %s",
		e.Fails, e.LastNote, e.Until.UTC().Format(time.RFC3339))
}

// Lanes returns the set's lanes sorted, for deterministic surfaces.
func Lanes(s Set) []string {
	out := make([]string, 0, len(s))
	for l := range s {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// Load reads the breaker state. ABSENT file → empty set, no warning (nothing
// has failed yet — the normal state). Unreadable or corrupt → empty set PLUS
// a warning: fail open, but never silently (a breaker that cannot read its
// state must not exclude anything, and must say so).
func Load(path string) (Set, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Set{}, ""
		}
		return Set{}, fmt.Sprintf("exclusion state unreadable (failing open, no lanes excluded): %v", err)
	}
	var s Set
	if err := json.Unmarshal(b, &s); err != nil {
		return Set{}, fmt.Sprintf("exclusion state corrupt (failing open, no lanes excluded): %v", err)
	}
	if s == nil {
		s = Set{}
	}
	return s, ""
}

// Save persists the set atomically (tmp + rename) so a torn write can never
// produce the corrupt-state warning path on the next load.
func Save(path string, s Set) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
