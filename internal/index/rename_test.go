package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shrink the backoff so the retry tests stay fast.
func fastRetries(t *testing.T) {
	t.Helper()
	origBase, origMax := renameBaseDelay, renameMaxDelay
	renameBaseDelay, renameMaxDelay = time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { renameBaseDelay, renameMaxDelay = origBase, origMax })
}

// The bug this fixes: on Windows the replace fails while another process has the
// destination open (a concurrent mr-hook read / a second session's refresh).
// A transient failure must be retried, not fail the whole refresh.
func TestRenameAtomic_RetriesTransientFailure(t *testing.T) {
	fastRetries(t)
	orig := osRename
	t.Cleanup(func() { osRename = orig })

	calls := 0
	osRename = func(src, dst string) error {
		calls++
		if calls < 3 {
			return errors.New("The process cannot access the file because it is being used by another process.")
		}
		return nil
	}

	if err := renameAtomic("a", "b"); err != nil {
		t.Fatalf("a transient sharing violation must be retried to success, got: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected 3 attempts (2 failures + 1 success), got %d", calls)
	}
}

// A genuinely stuck destination must still surface an error — retrying forever
// would hang SessionStart.
func TestRenameAtomic_GivesUpAndReportsError(t *testing.T) {
	fastRetries(t)
	orig := osRename
	t.Cleanup(func() { osRename = orig })

	calls := 0
	want := errors.New("locked forever")
	osRename = func(src, dst string) error { calls++; return want }

	err := renameAtomic("a", "b")
	if !errors.Is(err, want) {
		t.Fatalf("want the underlying error back, got: %v", err)
	}
	if calls != renameAttempts {
		t.Fatalf("expected %d attempts, got %d", renameAttempts, calls)
	}
}

// The happy path must still actually move the file (one call, no sleeping).
func TestRenameAtomic_RealRenameSucceedsFirstTry(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameAtomic(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "payload" {
		t.Fatalf("dst not written: %q err=%v", got, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("src should be gone after a rename")
	}
}
