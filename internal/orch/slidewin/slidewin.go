// Package slidewin is the W6 sliding-window dispatch limiter for the LOCAL
// lane. The local box serves interactive work too (llama-swap hot-swaps
// models per request), so a burst of headless dispatches can wedge it for the
// operator; a SLIDING window caps the rate without the boundary-burst a
// fixed window allows (2N dispatches straddling a reset instant).
//
// A denial is a LOCAL rate limit — typed so it can never be recorded as a
// vendor 429 (the W6 local-vs-upstream distinction): nothing here touches the
// ledger, and the caller stamps the outcome's RateLimitOrigin "local".
//
// Pure core over a stamp slice; persistence is a small JSON file. An
// unreadable or corrupt state file fails OPEN with a warning (a limiter that
// cannot read its state must not invent denials), mirroring exclusion.Load.
package slidewin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/lockfile"
)

// Window is the persisted state: dispatch instants inside the sliding span.
type Window struct {
	Stamps []time.Time `json:"stamps"`
}

// Allow prunes stamps older than span, then answers whether one more dispatch
// fits under limit. When it does, the returned Window includes now; when it
// does not, retryAt is the instant the OLDEST in-window stamp ages out — the
// earliest moment a retry can succeed. Pure: the caller persists.
func Allow(w Window, now time.Time, limit int, span time.Duration) (ok bool, retryAt time.Time, out Window) {
	cutoff := now.Add(-span)
	kept := w.Stamps[:0:0]
	for _, s := range w.Stamps {
		if s.After(cutoff) {
			kept = append(kept, s)
		}
	}
	if len(kept) < limit {
		return true, time.Time{}, Window{Stamps: append(kept, now)}
	}
	oldest := kept[0]
	for _, s := range kept[1:] {
		if s.Before(oldest) {
			oldest = s
		}
	}
	return false, oldest.Add(span), Window{Stamps: kept}
}

// Load reads limiter state. Absent → empty window (normal). Unreadable or
// corrupt → empty window plus a warning: fail open, never silently.
func Load(path string) (Window, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Window{}, ""
		}
		return Window{}, fmt.Sprintf("local limiter state unreadable (failing open): %v", err)
	}
	var w Window
	if err := json.Unmarshal(b, &w); err != nil {
		return Window{}, fmt.Sprintf("local limiter state corrupt (failing open): %v", err)
	}
	return w, ""
}

// Save persists atomically. Pid-unique tmp + brief rename retry, for the same
// Windows rename-under-reader reason as exclusion.Save.
func Save(path string, w Window) error {
	b, err := json.Marshal(w)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	var rerr error
	for i := 0; i < 5; i++ {
		if rerr = os.Rename(tmp, path); rerr == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	_ = os.Remove(tmp)
	return rerr
}

// Decide is the LOCKED admit-or-deny for one dispatch: load, prune, decide,
// and persist the new stamp — all under the cross-process lock, so two
// concurrent dispatches can never both pass at limit−1 (the lost-update race
// an unlocked read-modify-write allows is precisely a burst, the limiter's
// target case). The load warning (if any) is returned for surfacing; a
// corrupt file is replaced by clean state on the next allowed dispatch.
func Decide(path string, now time.Time, limit int, span time.Duration) (ok bool, retryAt time.Time, warn string, err error) {
	release, lerr := lockfile.Acquire(path+".lock", 2*time.Second, 30*time.Second)
	if lerr != nil {
		return false, time.Time{}, "", lerr
	}
	defer release()
	w, warn := Load(path)
	ok, retryAt, next := Allow(w, now, limit, span)
	if ok || warn != "" {
		if serr := Save(path, next); serr != nil {
			return ok, retryAt, warn, serr
		}
	}
	return ok, retryAt, warn, nil
}
