package index

import (
	"os"
	"time"
)

// Test seams: swapped out so the retry loop can be exercised deterministically
// without contriving a real Windows sharing violation.
var (
	osRename        = os.Rename
	renameAttempts  = 8
	renameBaseDelay = 5 * time.Millisecond
	renameMaxDelay  = 100 * time.Millisecond
)

// renameAtomic replaces dst with src, retrying briefly on failure.
//
// POSIX rename(2) replaces the destination even while another process holds it
// open, so a single call is enough there. Windows does not: MoveFileEx fails
// with a sharing violation while ANY process has dst open, and that collides
// with normal operation here — two Claude sessions starting at once each run
// `mr-index refresh` at SessionStart while mr-hook is reading index.json for a
// prompt. Observed 2026-07-13: "The process cannot access the file because it is
// being used by another process", which failed that refresh outright.
//
// The readers hold the file for milliseconds, so a short backoff turns a lost
// refresh into an imperceptible wait. Worst case is 5+10+20+40+80+100+100 = 355ms
// of sleeping across 8 attempts — well inside the async SessionStart refresh
// budget, and never on the per-prompt hook path.
//
// Only CONTENDED failures are worth retrying. A missing source or directory can
// never succeed no matter how long we wait, so those surface immediately instead
// of burning the backoff.
func renameAtomic(src, dst string) error {
	delay := renameBaseDelay
	var err error
	for i := 0; i < renameAttempts; i++ {
		if err = osRename(src, dst); err == nil {
			return nil
		}
		if os.IsNotExist(err) {
			return err // permanent: nothing to move, or the dir is gone
		}
		if i == renameAttempts-1 {
			break // don't sleep after the final attempt
		}
		time.Sleep(delay)
		if delay *= 2; delay > renameMaxDelay {
			delay = renameMaxDelay
		}
	}
	return err
}
