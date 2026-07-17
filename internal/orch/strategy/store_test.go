package strategy

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)

func TestNewDispatchIDIsHex32AndUnique(t *testing.T) {
	a, err := NewDispatchID()
	if err != nil || len(a) != 32 {
		t.Fatalf("id=%q err=%v (want 32 hex chars)", a, err)
	}
	for _, c := range a {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("non-hex char in id: %q", a)
		}
	}
	b, _ := NewDispatchID()
	if a == b {
		t.Fatal("ids must be unique")
	}
}

func TestWriteInitialLoadMutateRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	if err := WriteInitial(dir, ir, "d1", t0); err != nil {
		t.Fatal(err)
	}
	st, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "pending" || st.DispatchID != "d1" || len(st.IR.Steps) != 2 {
		t.Fatalf("bad initial state: %+v", st)
	}
	if st.StepStatus[0] == nil || st.StepStatus[1] == nil {
		t.Fatal("step_status must be pre-seeded per step")
	}
	// Mutate is a locked read-modify-write.
	if err := Mutate(dir, func(s *State) {
		s.State = "running"
		s.StepStatus[0].OutcomeClass = "ok"
	}, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	st2, _ := Load(dir)
	if st2.State != "running" || st2.StepStatus[0].OutcomeClass != "ok" {
		t.Fatalf("mutate did not persist: %+v", st2)
	}
	if !st2.UpdatedAt.After(st.UpdatedAt) {
		t.Fatal("UpdatedAt must advance on mutate")
	}
}

func TestJournalAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	if err := Journal(dir, "step_started", 0, t0); err != nil {
		t.Fatal(err)
	}
	if err := Journal(dir, "step_finished", 0, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if lines := len([]rune(string(b))); lines == 0 {
		t.Fatal("journal must have content")
	}
	// two newline-terminated lines
	if got := countLines(b); got != 2 {
		t.Fatalf("journal lines = %d, want 2", got)
	}
}

func countLines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}

// WriteInitial must refuse to clobber an existing dispatch — a fresh id never
// collides, so a collision is a bug that must surface, not silently overwrite.
func TestWriteInitialRefusesToClobber(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ir := IR{Goal: "g", Steps: []Step{step(0)}}
	if err := WriteInitial(dir, ir, "d1", t0); err != nil {
		t.Fatal(err)
	}
	if err := WriteInitial(dir, ir, "d1", t0); err == nil {
		t.Fatal("a second WriteInitial over an existing state.json must error, never clobber")
	}
}

// S3R-5 (crash integrity): a FAILED write must leave the PRIOR committed
// state.json fully intact and loadable — the whole idempotent-recovery guarantee
// rests on this. This test FORCES a real torn write: it pre-creates the exact
// per-PID temp path saveState will use AS A DIRECTORY, so writeAndSync's
// os.OpenFile(O_CREATE|O_TRUNC|O_WRONLY) on it fails mid-save. It then asserts
// (a) the failing Mutate RETURNS an error (never swallows the drop, S3R-6) and
// (b) the prior committed state.json is byte-for-byte intact and loadable.
//
// This is NON-vacuous: it FAILS if saveState's atomic temp→fsync→rename is
// stripped to a direct os.WriteFile(state.json), because a direct-write failure
// would truncate/corrupt the live state.json instead of leaving the prior one.
func TestS3R5CrashLeavesPriorStateIntact(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ir := IR{Goal: "g", Steps: []Step{step(0), step(1, 0)}}
	if err := WriteInitial(dir, ir, "d1", t0); err != nil {
		t.Fatal(err)
	}
	// Establish a known-good committed state.
	if err := Mutate(dir, func(s *State) { s.State = "running"; s.StepStatus[0].OutcomeClass = "ok" }, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	good, err := Load(dir)
	if err != nil || good.State != "running" || good.StepStatus[0].OutcomeClass != "ok" {
		t.Fatalf("precondition: good state not committed: %+v err=%v", good, err)
	}
	goodBytes, err := os.ReadFile(statePath(dir))
	if err != nil {
		t.Fatal(err)
	}

	// FORCE the write to fail: occupy the exact temp path saveState will target
	// (statePath + ".tmp." + pid) with a DIRECTORY. os.OpenFile on a directory for
	// writing fails, so writeAndSync errors and saveState aborts BEFORE the rename —
	// the atomic machinery must leave the prior state.json untouched.
	tmp := statePath(dir) + ".tmp." + itoaPID()
	if err := os.Mkdir(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory can't be removed by saveState's os.Remove(tmp) cleanup
	// either, so the block persists for the whole failing save.
	if err := os.WriteFile(filepath.Join(tmp, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// This Mutate MUST fail (the temp write can't proceed) and MUST surface the
	// error — never a silent drop (S3R-6).
	err = Mutate(dir, func(s *State) { s.State = "corrupted-should-never-commit" }, t0.Add(2*time.Minute))
	if err == nil {
		t.Fatal("a failed temp write must RETURN an error, never silently drop the state write (S3R-6)")
	}

	// The prior committed state.json must be byte-for-byte intact and loadable —
	// the failed save never touched the live file (proves the temp→rename atomicity).
	afterBytes, rerr := os.ReadFile(statePath(dir))
	if rerr != nil {
		t.Fatalf("a failed write must leave the prior state.json readable: %v", rerr)
	}
	if string(afterBytes) != string(goodBytes) {
		t.Fatalf("a failed write CORRUPTED the prior state.json — atomic machinery stripped?\nwant:\n%s\ngot:\n%s", goodBytes, afterBytes)
	}
	after, lerr := Load(dir)
	if lerr != nil || after.State != "running" || after.StepStatus[0].OutcomeClass != "ok" {
		t.Fatalf("prior state must survive a failed write intact: %+v err=%v", after, lerr)
	}

	// Clean up the block, then a fresh successful mutate atomically replaces and
	// stays clean+loadable (the machinery still works after a recovered failure).
	if err := os.RemoveAll(tmp); err != nil {
		t.Fatal(err)
	}
	if err := Mutate(dir, func(s *State) { s.State = "done" }, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	final, ferr := Load(dir)
	if ferr != nil || final.State != "done" {
		t.Fatalf("atomic replace must produce a clean loadable state after recovery: %+v err=%v", final, ferr)
	}
}

// itoaPID returns the current PID as a decimal string, matching saveState's temp
// filename suffix (fmt.Sprintf("%s.tmp.%d", ..., os.Getpid())).
func itoaPID() string { return strconv.Itoa(os.Getpid()) }

// S3R-6 (no silent drops): a lock-busy Mutate must RETURN an error, never
// silently discard the state write. We hold the lock by pre-creating a FRESH
// (non-stale) lock file, then assert Mutate surfaces the lock-busy error inside
// the bounded wait window.
func TestS3R6MutateReturnsErrorWhenLockBusy(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ir := IR{Goal: "g", Steps: []Step{step(0)}}
	if err := WriteInitial(dir, ir, "d1", t0); err != nil {
		t.Fatal(err)
	}
	// Keep the test fast: shrink the bounded wait so a held lock surfaces the
	// error in ~150ms instead of the production 3s (behavior identical).
	savedWait, savedStale := lockWait, lockStale
	lockWait, lockStale = 150*time.Millisecond, 30*time.Second
	defer func() { lockWait, lockStale = savedWait, savedStale }()

	// Hold the lock with a fresh mtime so the stale-steal never reclaims it
	// within the bounded wait. A held lock must make Mutate FAIL LOUD.
	lock := statePath(dir) + ".lock"
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(lock)

	err = Mutate(dir, func(s *State) { s.State = "running" }, t0.Add(time.Minute))
	if err == nil {
		t.Fatal("Mutate under a held lock must return a lock-busy error, never silently drop the write")
	}
	// And the state must be UNCHANGED (the drop was surfaced, not applied).
	st, _ := Load(dir)
	if st.State != "pending" {
		t.Fatalf("a failed Mutate must not have partially applied: state=%q", st.State)
	}
}
