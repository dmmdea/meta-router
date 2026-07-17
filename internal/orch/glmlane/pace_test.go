package glmlane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake clock: now() returns a mutable instant that ADVANCES on every sleep —
// Pace's wait math becomes fully deterministic.
type clock struct {
	t     time.Time
	slept []time.Duration
}

func (c *clock) now() time.Time { return c.t }
func (c *clock) sleep(d time.Duration) {
	c.slept = append(c.slept, d)
	c.t = c.t.Add(d)
}

func pcfg(minSec, jitterSec int64) PaceConfig {
	pc := DefaultPace(minSec, jitterSec)
	return pc
}

func TestPaceFirstDispatchNoWaitWritesStamp(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Now().UTC()}
	unlock, waited, err := Pace(dir, pcfg(20, 0), c.now, c.sleep)
	if err != nil || waited != 0 {
		t.Fatalf("first dispatch must not wait: waited=%v err=%v", waited, err)
	}
	if _, serr := os.Stat(filepath.Join(dir, stampName)); serr != nil {
		t.Fatalf("cadence stamp must be written at dispatch start: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(dir, lockName)); serr != nil {
		t.Fatalf("lock must be held during the dispatch: %v", serr)
	}
	unlock()
	if _, serr := os.Stat(filepath.Join(dir, lockName)); !os.IsNotExist(serr) {
		t.Fatalf("unlock must release the concurrency lock: %v", serr)
	}
}

func TestPaceEnforcesMinInterval(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Now().UTC()}
	last := c.t.Add(-5 * time.Second)
	if err := os.WriteFile(filepath.Join(dir, stampName), []byte(last.Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, waited, err := Pace(dir, pcfg(20, 0), c.now, c.sleep)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if waited != 15*time.Second {
		t.Fatalf("5s elapsed of a 20s interval ⇒ wait 15s, got %v", waited)
	}
}

func TestPaceElapsedIntervalNoWait(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Now().UTC()}
	last := c.t.Add(-60 * time.Second)
	if err := os.WriteFile(filepath.Join(dir, stampName), []byte(last.Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, waited, err := Pace(dir, pcfg(20, 20), c.now, c.sleep)
	if err != nil || waited != 0 {
		t.Fatalf("a stale stamp must not wait: waited=%v err=%v", waited, err)
	}
	unlock()
}

func TestPaceJitterWithinBounds(t *testing.T) {
	dir := t.TempDir()
	c := &clock{t: time.Now().UTC()}
	if err := os.WriteFile(filepath.Join(dir, stampName), []byte(c.t.Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}
	unlock, waited, err := Pace(dir, pcfg(20, 20), c.now, c.sleep)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if waited < 20*time.Second || waited > 40*time.Second {
		t.Fatalf("zero elapsed ⇒ wait ∈ [min, min+jitter] = [20s,40s], got %v", waited)
	}
}

// Max concurrency 1: a held lock blocks the second dispatcher until released.
func TestPaceConcurrencyLockBlocksThenAcquires(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, lockName)
	if err := os.WriteFile(lock, []byte("holder"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now().UTC()}
	polls := 0
	sleep := func(d time.Duration) {
		c.sleep(d)
		polls++
		if polls == 2 { // the in-flight dispatch finishes
			os.Remove(lock)
		}
	}
	unlock, _, err := Pace(dir, pcfg(0, 0), c.now, sleep)
	if err != nil || polls < 2 {
		t.Fatalf("second dispatcher must wait for the lock then acquire: polls=%d err=%v", polls, err)
	}
	unlock()
}

// A crashed holder's lock (older than LockStale) is stolen, not waited on.
func TestPaceLockStaleSteal(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, lockName)
	if err := os.WriteFile(lock, []byte("crashed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lock, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now().UTC()}
	unlock, _, err := Pace(dir, pcfg(0, 0), c.now, c.sleep)
	if err != nil {
		t.Fatalf("stale lock must be stolen: %v", err)
	}
	unlock()
}

// Lock wait exhausted → a named error the caller relegates on (never a hang).
func TestPaceLockBusyTimesOut(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lockName), []byte("busy"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &clock{t: time.Now().UTC()}
	pc := pcfg(0, 0)
	pc.LockWait = time.Second
	// A2R-#5: with the wait loop re-checking staleness each poll, LockStale must
	// stay ABOVE the (fresh) held lock's age or it would be stolen instead of
	// timing out. The just-written lock is age ~0; a large LockStale keeps it held.
	pc.LockStale = time.Hour
	_, _, err := Pace(dir, pc, c.now, c.sleep)
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("exhausted lock wait must error for relegation: %v", err)
	}
}

// A2R-#5(a): a lock whose holder is actively HEARTBEATING is NOT stolen, even
// after LockStale elapses. Real (short) durations: heartbeat 10ms refreshes the
// mtime faster than the 40ms stale threshold, so a second dispatcher waiting on
// it keeps seeing a fresh lock and never steals it — it times out on LockWait
// instead (the live holder wins). This is the exact S2R-6 ban guard: two
// simultaneous GLM requests (the ban PATTERN) must never happen.
func TestPaceLiveLockNotStolenPastLockStale(t *testing.T) {
	dir := t.TempDir()
	// Holder acquires and keeps heartbeating (default real time.Now/time.Sleep).
	holderPc := DefaultPace(0, 0)
	holderPc.Heartbeat = 10 * time.Millisecond
	holderPc.LockStale = 40 * time.Millisecond
	unlock, _, err := Pace(dir, holderPc, time.Now, time.Sleep)
	if err != nil {
		t.Fatalf("holder must acquire: %v", err)
	}
	defer unlock()

	// A second dispatcher: short LockWait (< a few heartbeats), same stale
	// threshold. Because the holder keeps the mtime fresh, the waiter never sees
	// a stale lock → it relegates (busy) rather than stealing a LIVE lock.
	waiterPc := DefaultPace(0, 0)
	waiterPc.Heartbeat = 10 * time.Millisecond
	waiterPc.LockStale = 40 * time.Millisecond
	waiterPc.LockWait = 120 * time.Millisecond
	waiterPc.Poll = 5 * time.Millisecond
	u2, _, werr := Pace(dir, waiterPc, time.Now, time.Sleep)
	if werr == nil {
		u2()
		t.Fatal("a LIVE heartbeated lock must NOT be stolen — the waiter must relegate (busy), got a successful acquire")
	}
	if !strings.Contains(werr.Error(), "busy") {
		t.Fatalf("waiter must relegate with a busy error: %v", werr)
	}
}

// A2R-#5(b): a GENUINELY stale lock (a crashed holder — no heartbeat, mtime
// aged past LockStale) IS reclaimed. Written directly with an aged mtime and no
// heartbeat goroutine behind it.
func TestPaceGenuinelyStaleLockReclaimed(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, lockName)
	if err := os.WriteFile(lock, []byte("crashed"), 0o644); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-500 * time.Millisecond)
	if err := os.Chtimes(lock, aged, aged); err != nil {
		t.Fatal(err)
	}
	pc := DefaultPace(0, 0)
	pc.Heartbeat = 10 * time.Millisecond
	pc.LockStale = 40 * time.Millisecond // 500ms-old lock is well past this
	pc.Poll = 5 * time.Millisecond
	pc.LockWait = 200 * time.Millisecond
	unlock, _, err := Pace(dir, pc, time.Now, time.Sleep)
	if err != nil {
		t.Fatalf("a genuinely dead lock (aged past LockStale) must be reclaimed: %v", err)
	}
	unlock()
}

// A2R-#5(c): the LockWait/LockStale ordering must not false-relegate a waiter
// before a genuinely-dead lock is reclaimable. With LockWait > LockStale a
// waiter facing a dead lock ALWAYS reaches the stale threshold and steals it,
// rather than timing out first (the old LockWait 15m < LockStale 20m bug meant
// a dead lock in the 15–20m window relegated a waiter that should have stolen).
func TestPaceLockWaitExceedsLockStaleSoDeadLockReclaimable(t *testing.T) {
	pc := DefaultPace(20, 20)
	if pc.LockWait <= pc.LockStale {
		t.Fatalf("LockWait (%v) must exceed LockStale (%v) so a dead lock is reclaimable before a waiter relegates", pc.LockWait, pc.LockStale)
	}
	// End-to-end: a lock aged into the (LockStale, LockWait) window is stolen,
	// not timed out on. Aged to 60ms with stale=40ms, wait=200ms.
	dir := t.TempDir()
	lock := filepath.Join(dir, lockName)
	if err := os.WriteFile(lock, []byte("dead"), 0o644); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-60 * time.Millisecond)
	if err := os.Chtimes(lock, aged, aged); err != nil {
		t.Fatal(err)
	}
	wpc := DefaultPace(0, 0)
	wpc.Heartbeat = 10 * time.Millisecond
	wpc.LockStale = 40 * time.Millisecond
	wpc.LockWait = 200 * time.Millisecond
	wpc.Poll = 5 * time.Millisecond
	unlock, _, err := Pace(dir, wpc, time.Now, time.Sleep)
	if err != nil {
		t.Fatalf("a dead lock inside (LockStale, LockWait) must be stolen, not relegated: %v", err)
	}
	unlock()
}
