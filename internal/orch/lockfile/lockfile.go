// Package lockfile is the cross-process mutex for small state files that see
// concurrent read-modify-write from parallel dispatches (W6: exclusions.json,
// local-limiter.json). Same O_CREATE|O_EXCL + stale-steal convention as the
// ledger's internal lock — extracted rather than copied a third time.
package lockfile

import (
	"fmt"
	"os"
	"time"
)

// Acquire takes lockPath (conventionally <state file>+".lock"), waiting up to
// wait and stealing locks older than stale (a crashed holder must not wedge
// the mechanism forever). Returns the release func.
func Acquire(lockPath string, wait, stale time.Duration) (func(), error) {
	deadline := time.Now().Add(wait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			return func() { os.Remove(lockPath) }, nil
		}
		if fi, serr := os.Stat(lockPath); serr == nil && time.Since(fi.ModTime()) > stale {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("state lock busy: %s", lockPath)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
