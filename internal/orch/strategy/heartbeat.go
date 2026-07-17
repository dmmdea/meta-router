package strategy

import (
	"os"
	"path/filepath"
	"time"
)

// liveStates are the non-terminal dispatch states a supervisor is expected to be
// actively driving — the only states where a missing heartbeat means "the
// supervisor died," not "the DAG is already finished." (S3R-7)
var liveStates = map[string]bool{"running": true, "working": true}

// Heartbeat refreshes HeartbeatAt to now under the state lock (S3R-7). A live
// detached supervisor calls this on a ticker; a dead one stops, so the field
// goes stale and Stale() flags the dispatch for resume. It is a normal Mutate,
// so it never corrupts a concurrent status read (atomic replace).
func Heartbeat(dir string, now time.Time) error {
	return Mutate(dir, func(s *State) {
		t := now
		s.HeartbeatAt = &t
	}, now)
}

// HeartbeatOwned refreshes HeartbeatAt to now ONLY while pid still holds the
// supervisor lease. Once the lease was stolen (SupervisorPID != pid), the beat
// is a no-op — a dead-but-woken supervisor must NOT refresh the new holder's
// lease (F1b-2 / R10). Returns whether it still owns the lease.
func HeartbeatOwned(dir string, pid int, now time.Time) (owned bool, err error) {
	err = Mutate(dir, func(s *State) {
		if s.SupervisorPID != pid {
			owned = false
			return
		}
		owned = true
		t := now
		s.HeartbeatAt = &t
	}, now)
	return owned, err
}

// Stale reports whether a dispatch is in a live state (running/working) but has
// not heartbeated within threshold — i.e. its supervisor has probably died and
// nothing is re-invoking recovery. The liveness clock is HeartbeatAt when set,
// else UpdatedAt (a dispatch that fell over before its first beat is stale from
// its last state write). A terminal dispatch is never stale (nothing to resume).
func Stale(st State, now time.Time, threshold time.Duration) bool {
	if !liveStates[st.State] {
		return false
	}
	last := st.UpdatedAt
	if st.HeartbeatAt != nil {
		last = *st.HeartbeatAt
	}
	return now.Sub(last) > threshold
}

// leaseHolderLive reports whether the F1 supervisor lease is held by a LIVE
// holder: a PID is recorded AND its liveness clock (HeartbeatAt, else UpdatedAt
// for a just-acquired lease that has not beaten yet) is within threshold. A stale
// holder (past threshold) is treated as dead — its lease is stealable. Keyed on
// the lease itself, NOT dispatch state, so a lease held during the brief
// pending→running window is still honored.
func leaseHolderLive(st State, now time.Time, threshold time.Duration) bool {
	if st.SupervisorPID == 0 {
		return false // free
	}
	last := st.UpdatedAt
	if st.HeartbeatAt != nil {
		last = *st.HeartbeatAt
	}
	return now.Sub(last) <= threshold
}

// AcquireSupervisorLease is the F1 cross-process supervisor-exclusion gate. It is
// the read-decide-write done ATOMICALLY under the state lock (via Mutate), so two
// supervisors racing to drive one dispatch cannot both acquire. Semantics:
//
//   - free (no holder)                        → acquire (record pid), return true.
//   - held by THIS pid                        → re-entrant, keep it, return true.
//   - held by a LIVE other holder (fresh beat) → REFUSE, return false (no second
//     Execute — the R10 guard; a laptop-sleep-frozen-but-live supervisor whose
//     heartbeat is still within threshold is never stolen).
//   - held by a STALE other holder (dead, past threshold) → STEAL (record pid),
//     return true (the old supervisor is genuinely gone).
//
// The acquirer then heartbeats via startHeartbeat so its lease stays live for the
// whole Execute. threshold is injectable so tests run fast.
func AcquireSupervisorLease(dir string, pid int, now time.Time, threshold time.Duration) (bool, error) {
	acquired := false
	err := Mutate(dir, func(s *State) {
		if s.SupervisorPID == pid {
			acquired = true // re-entrant: already ours
			return
		}
		if leaseHolderLive(*s, now, threshold) {
			acquired = false // a LIVE other holder — refuse (R10)
			return
		}
		// free or stale (dead) holder → take it. Stamp a fresh liveness clock so a
		// competitor doesn't immediately see the just-acquired lease as stale.
		s.SupervisorPID = pid
		hb := now
		s.HeartbeatAt = &hb
		acquired = true
	}, now)
	if err != nil {
		return false, err
	}
	return acquired, nil
}

// ReleaseSupervisorLease clears the lease IFF this pid holds it (the holder is
// done driving — terminal state or a defer). A release by a non-holder is a no-op
// so a stale-steal followed by the dead original's late release can't free a NEW
// live holder's lease.
func ReleaseSupervisorLease(dir string, pid int, now time.Time) error {
	return Mutate(dir, func(s *State) {
		if s.SupervisorPID == pid {
			s.SupervisorPID = 0
		}
	}, now)
}

// cancelMarkerPath is the between-step cancel sentinel file (S3R cancel floor:
// slice-3 supports between-wave cancellation, not a hard mid-node kill).
func cancelMarkerPath(dir string) string { return filepath.Join(dir, "cancel") }

// RequestCancel drops the cancel sentinel the supervisor checks between waves.
// A running node finishes; no new node starts; the supervisor sets state
// "cancelled". Idempotent — a second request is a no-op rewrite.
func RequestCancel(dir string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(cancelMarkerPath(dir), []byte(now.UTC().Format(time.RFC3339)), 0o644)
}

// CancelRequested reports whether the cancel sentinel exists. Any stat error
// other than a clean "exists" reads as not-requested (fail-open: a filesystem
// hiccup must not spuriously cancel a live dispatch).
func CancelRequested(dir string) bool {
	_, err := os.Stat(cancelMarkerPath(dir))
	return err == nil
}
