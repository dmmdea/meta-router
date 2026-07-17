package strategy

import (
	"path/filepath"
	"testing"
	"time"
)

// S3R-7: Heartbeat stamps HeartbeatAt so a live supervisor keeps state.json
// fresh; a dead one stops → the field goes stale and is detectable.
func TestHeartbeatStampsHeartbeatAt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	if err := Heartbeat(dir, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	st, _ := Load(dir)
	if st.HeartbeatAt == nil || !st.HeartbeatAt.Equal(t0.Add(time.Second)) {
		t.Fatalf("HeartbeatAt not stamped: %+v", st.HeartbeatAt)
	}
}

// S3R-7 stale detection: a dispatch in running/working whose HeartbeatAt is past
// the threshold is Stale; a fresh one (or a terminal one) is not.
func TestStaleDetection(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	_ = Mutate(dir, func(s *State) { s.State = "running" }, t0)
	_ = Heartbeat(dir, t0)

	now := t0.Add(10 * time.Minute)
	st, _ := Load(dir)
	if !Stale(st, now, time.Minute) {
		t.Fatal("a running dispatch with a 10-min-old heartbeat must be stale")
	}
	// Fresh heartbeat → not stale.
	_ = Heartbeat(dir, now)
	st2, _ := Load(dir)
	if Stale(st2, now.Add(time.Second), time.Minute) {
		t.Fatal("a freshly-heartbeated dispatch must NOT be stale")
	}
	// Terminal state → never stale (nothing to resume).
	_ = Mutate(dir, func(s *State) { s.State = "done" }, now)
	st3, _ := Load(dir)
	if Stale(st3, now.Add(time.Hour), time.Minute) {
		t.Fatal("a terminal (done) dispatch must never be stale")
	}
}

// A running dispatch that never heartbeated (HeartbeatAt nil) but was created a
// while ago is stale (it fell over before its first beat) — measured from
// UpdatedAt as the liveness floor.
func TestStaleWhenNeverHeartbeated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	_ = Mutate(dir, func(s *State) { s.State = "running" }, t0)
	st, _ := Load(dir)
	if !Stale(st, t0.Add(10*time.Minute), time.Minute) {
		t.Fatal("a running dispatch that never heartbeated and is old must be stale")
	}
}

// F1 (supervisor-exclusion lease): a fresh dispatch has no lease → the first
// supervisor ACQUIRES it. A second acquire while a LIVE lease is held (fresh
// heartbeat) REFUSES — no second Execute, no double-dispatch. A STALE lease
// (holder genuinely dead, past threshold) is reclaimable — a competing supervisor
// steals it. This is the R10 guard: on laptop-sleep a live-but-frozen supervisor
// must NOT be stolen while its heartbeat is fresh.
func TestSupervisorLeaseRefusesLiveStealsStale(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	threshold := time.Minute

	// (free) the first supervisor (pid 111) acquires the lease.
	got, err := AcquireSupervisorLease(dir, 111, t0, threshold)
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("a free lease must be acquirable by the first supervisor")
	}
	// Simulate the live holder driving: it heartbeats.
	_ = Mutate(dir, func(s *State) { s.State = "running" }, t0)
	_ = Heartbeat(dir, t0)

	// (a) a SECOND supervisor (pid 222) tries to acquire while the holder's
	// heartbeat is FRESH → REFUSE. Only a hair past the beat, well within threshold.
	got2, err := AcquireSupervisorLease(dir, 222, t0.Add(2*time.Second), threshold)
	if err != nil {
		t.Fatal(err)
	}
	if got2 {
		t.Fatal("a second supervisor must REFUSE a lease held by a LIVE holder (fresh heartbeat) — no second Execute (R10)")
	}
	// The lease still belongs to the live holder (111), untouched.
	st, _ := Load(dir)
	if st.SupervisorPID != 111 {
		t.Fatalf("a refused acquire must not steal the live lease, pid=%d want 111", st.SupervisorPID)
	}

	// (b) time passes past the threshold with NO heartbeat → the holder is
	// genuinely dead → a competing supervisor STEALS the stale lease.
	late := t0.Add(10 * time.Minute)
	got3, err := AcquireSupervisorLease(dir, 333, late, threshold)
	if err != nil {
		t.Fatal(err)
	}
	if !got3 {
		t.Fatal("a STALE lease (dead holder, past threshold) must be reclaimable")
	}
	st2, _ := Load(dir)
	if st2.SupervisorPID != 333 {
		t.Fatalf("stealing a stale lease must record the new holder, pid=%d want 333", st2.SupervisorPID)
	}
}

// F1: the SAME pid re-acquiring its own lease is idempotent (re-entrant) — a
// supervisor that already holds the lease keeps it, never refuses itself.
func TestSupervisorLeaseReentrantSamePID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	if got, _ := AcquireSupervisorLease(dir, 111, t0, time.Minute); !got {
		t.Fatal("first acquire must succeed")
	}
	_ = Heartbeat(dir, t0)
	if got, _ := AcquireSupervisorLease(dir, 111, t0.Add(time.Second), time.Minute); !got {
		t.Fatal("the SAME pid must be able to re-acquire its own live lease (re-entrant)")
	}
}

// F1: releasing the lease clears the holder so the next supervisor can acquire —
// but a release by a DIFFERENT pid (not the holder) must NOT clear a live lease.
func TestSupervisorLeaseRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	_, _ = AcquireSupervisorLease(dir, 111, t0, time.Minute)

	// A non-holder release is a no-op (must not free a live lease).
	if err := ReleaseSupervisorLease(dir, 999, t0); err != nil {
		t.Fatal(err)
	}
	if st, _ := Load(dir); st.SupervisorPID != 111 {
		t.Fatalf("a non-holder release must not clear the lease, pid=%d want 111", st.SupervisorPID)
	}
	// The holder releases → lease free.
	if err := ReleaseSupervisorLease(dir, 111, t0); err != nil {
		t.Fatal(err)
	}
	if st, _ := Load(dir); st.SupervisorPID != 0 {
		t.Fatalf("the holder's release must clear the lease, pid=%d want 0", st.SupervisorPID)
	}
	// Now a fresh supervisor can acquire.
	if got, _ := AcquireSupervisorLease(dir, 222, t0.Add(time.Second), time.Minute); !got {
		t.Fatal("after release the lease must be acquirable again")
	}
}

// S3R cancel sentinel: RequestCancel drops a marker; CancelRequested reads it;
// a fresh dispatch has no marker.
func TestCancelSentinel(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	_ = WriteInitial(dir, IR{Goal: "g", Steps: []Step{step(0)}}, "d1", t0)
	if CancelRequested(dir) {
		t.Fatal("a fresh dispatch must not be cancel-requested")
	}
	if err := RequestCancel(dir, t0); err != nil {
		t.Fatal(err)
	}
	if !CancelRequested(dir) {
		t.Fatal("RequestCancel must make CancelRequested true")
	}
}
