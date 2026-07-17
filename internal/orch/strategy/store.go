package strategy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type StepState struct {
	OutcomeClass string     `json:"outcome_class,omitempty"` // "" = not yet run
	ResultRef    string     `json:"result_ref,omitempty"`
	Attempt      int        `json:"attempt,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"` // set at dispatch; the crash-recovery signal
	TS           *time.Time `json:"ts,omitempty"`         // finished-at
	ResumeAt     *time.Time `json:"resume_at,omitempty"`
	// Lane is the RESOLVED lane recorded at mark-started (S3R-6): the crash-recovery
	// signal for "what lane was this started step going to spend on." Recovery keys
	// idempotency on THIS resolved lane, never the explicit LaneHint (a router-decided
	// node that resolved to `local` is idempotent; keying on the empty hint would
	// wrongly block it). Empty when a step has not been marked-started.
	Lane string `json:"lane,omitempty"`
	// VerifyVerdict is a verifier node's tier-2 JUDGMENT ("yes"/"no"/"unsure"),
	// extracted from its result by the prod runner. ADVISORY: it NEVER changes
	// OutcomeClass (the tier-1 receipt gate governs — a weak local judge must not
	// hard-reject a completed cloud answer). A non-"yes" terminal verdict FLAGS the
	// dispatch (State.VerifyFlag) so the operator sees "completed, but the
	// independent check disagreed" — the signal solo cannot give. Empty for
	// non-verifier nodes and when no structured verdict could be parsed.
	VerifyVerdict string `json:"verify_verdict,omitempty"`
	VerifyReason  string `json:"verify_reason,omitempty"`
}

type State struct {
	DispatchID string             `json:"dispatch_id"`
	State      string             `json:"state"` // pending|running|working|deferred|done|failed|blocked|cancelled
	IR         IR                 `json:"ir"`
	StepStatus map[int]*StepState `json:"step_status"`
	ResultRef  string             `json:"result_ref,omitempty"`
	CreatedAt  time.Time          `json:"created_at"`
	UpdatedAt  time.Time          `json:"updated_at"`
	// HeartbeatAt is the S3R-7 supervisor-liveness signal: a live detached
	// supervisor refreshes it on a ticker while it drives the DAG. When the
	// supervisor dies before a terminal state (crash / taskkill / laptop sleep)
	// the field stops advancing → Stale() detects it and strategy_status /
	// --sweep re-invoke recovery instead of the state lying "running" forever.
	// Nil until the first beat (a dispatch that fell over before beating is stale
	// from UpdatedAt).
	HeartbeatAt *time.Time `json:"heartbeat_at,omitempty"`
	// SupervisorPID is the F1 supervisor-exclusion lease: the PID of the process
	// currently driving Execute over this DAG. A supervisor MUST acquire the lease
	// (AcquireSupervisorLease) before driving — if it is held by a LIVE holder
	// (fresh HeartbeatAt) a competitor REFUSES (no second Execute, no R10
	// double-spend); a STALE lease (dead holder past staleThreshold) is stolen. The
	// lease is heartbeated via the existing HeartbeatAt so a live holder is never
	// stolen — even a laptop-sleep-frozen supervisor, whose beat resumes on wake.
	// Zero = free. Cleared on release (terminal / defer).
	SupervisorPID int `json:"supervisor_pid,omitempty"`
	// VerifyFlag / VerifyVerdict / VerifyReason are the dispatch-level tier-2 verify
	// surface, set at finalize from the terminal verifier node's judgment. VerifyFlag
	// is true when the terminal verdict is non-affirmative (not "yes") — an ADVISORY
	// flag, never a state change (a flagged dispatch still finalizes done/failed per
	// the tier-1 gate). strategy_status surfaces {verdict,reason,flagged} front-and-
	// center so the operator sees an independent verification signal solo can't give.
	VerifyFlag    bool   `json:"verify_flag,omitempty"`
	VerifyVerdict string `json:"verify_verdict,omitempty"`
	VerifyReason  string `json:"verify_reason,omitempty"`
}

type JournalEntry struct {
	TS     time.Time `json:"ts"`
	Event  string    `json:"event"`
	StepID int       `json:"step_id"`
	// Detail is optional free-text carried by events that need it — e.g. a
	// `replan` event names the from-lane→to-lane re-lane and the quality
	// tradeoff (S3R-3c / S3R-10b). Empty for the plain step_started/step_finished
	// events, so the existing journal shape is unchanged.
	Detail string `json:"detail,omitempty"`
}

// Lock timing mirrors ledger.acquireLock. Exposed as package vars (not consts)
// only so tests can shorten the wait; production leaves them at the ledger's
// values (3s wait, 30s stale-steal).
var (
	lockWait  = 3 * time.Second
	lockStale = 30 * time.Second
)

// NewDispatchID mints a crypto/rand 16-byte hex id (32 chars). Stdlib-only,
// Windows-first, unguessable + greppable (Bernstein head-hash chaining is out, §7).
func NewDispatchID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// WriteInitial creates the dispatch dir and writes state.json (state=pending)
// under the O_EXCL lock, pre-seeding an empty StepStatus per step. It fails if a
// state.json already exists (a fresh dispatch_id never collides; a collision is
// a bug) and RETURNS every error (S3R-6: a dropped initial write must fail loud).
func WriteInitial(dir string, ir IR, id string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ss := make(map[int]*StepState, len(ir.Steps))
	for _, s := range ir.Steps {
		ss[s.ID] = &StepState{}
	}
	st := State{DispatchID: id, State: "pending", IR: ir, StepStatus: ss, CreatedAt: now, UpdatedAt: now}
	return withLock(dir, func() error {
		if _, err := os.Stat(statePath(dir)); err == nil {
			return fmt.Errorf("state.json already exists for %s", id)
		}
		return saveState(dir, st)
	})
}

// Load reads state.json WITHOUT the lock — a stale read is fine for status
// surfaces (S3R-7 strategy_status) and the crash-recovery re-derivation reads a
// committed file. The atomic replace in saveState guarantees a reader sees
// either the whole prior file or the whole new one, never a torn one, so a
// lockless read can never observe a half-written state.
func Load(dir string) (State, error) {
	b, err := os.ReadFile(statePath(dir))
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, err
	}
	if st.StepStatus == nil {
		st.StepStatus = map[int]*StepState{}
	}
	return st, nil
}

// Mutate is the locked read-modify-write: fresh load UNDER the lock, apply fn,
// bump UpdatedAt, atomic crash-safe save. Every state.json write goes through
// here (or WriteInitial) so concurrent status reads never see a torn file and
// two writers never lose a write. S3R-6: a lock-busy or write failure is
// RETURNED so the executor fails loud or retries — it must never be discarded.
func Mutate(dir string, fn func(*State), now time.Time) error {
	return withLock(dir, func() error {
		st, err := loadLocked(dir)
		if err != nil {
			return err
		}
		fn(&st)
		st.UpdatedAt = now
		return saveState(dir, st)
	})
}

// Journal appends one event line (append-only; not the authoritative outcome —
// that stays dispatch.jsonl). RETURNS its error (S3R-6).
func Journal(dir, event string, stepID int, now time.Time) error {
	return JournalDetail(dir, event, stepID, "", now)
}

// JournalDetail is Journal with a free-text detail (S3R-3c/S3R-10b: the replan
// event carries the from-lane→to-lane re-lane + the quality tradeoff). RETURNS
// its error (S3R-6). Journal is the detail-less convenience wrapper.
func JournalDetail(dir, event string, stepID int, detail string, now time.Time) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "journal.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(JournalEntry{TS: now, Event: event, StepID: stepID, Detail: detail})
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// loadLocked is Load called while the caller already holds the lock — a fresh
// read of the committed file for the read-modify-write cycle.
func loadLocked(dir string) (State, error) { return Load(dir) }

// saveState writes state.json crash-safely (S3R-5): marshal → write a per-PID
// temp → fsync the temp → atomic replace → fsync the parent dir. The fsyncs
// order the bytes-then-rename durably so a crash between the write and the
// rename leaves the PRIOR state.json intact (never a torn half-write), and a
// crash after the rename leaves the NEW state durably on disk. The rename gets a
// short bounded retry on the Windows "access denied" that fires when a
// concurrent reader/indexer/AV holds a handle across the replace
// (golang/go#8914/#29106) — stdlib only, no external atomic-replace dep.
func saveState(dir string, st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", statePath(dir), os.Getpid())
	if err := writeAndSync(tmp, data); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := renameWithRetry(tmp, statePath(dir)); err != nil {
		os.Remove(tmp)
		return err
	}
	return fsyncDir(dir)
}

// writeAndSync writes data to path and fsyncs the FILE before returning, so the
// bytes are durable before the rename makes them the live state.
func writeAndSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// renameWithRetry does the atomic replace, retrying briefly on a Windows
// sharing-violation "access denied" (a concurrent reader/indexer/AV holding a
// handle across the replace). On POSIX os.Rename already atomically replaces.
func renameWithRetry(oldp, newp string) error {
	const attempts = 20
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.Rename(oldp, newp); err == nil {
			return nil
		}
		if !isAccessDenied(err) {
			return err
		}
		time.Sleep(25 * time.Millisecond) // bounded: ~0.5s total before giving up loud
	}
	return fmt.Errorf("state.json replace kept failing (concurrent handle held?): %w", err)
}

// isAccessDenied reports whether err is the Windows sharing-violation the
// replace retries on. os.IsPermission covers ERROR_ACCESS_DENIED (5); the
// substring guard also catches ERROR_SHARING_VIOLATION (32) which surfaces as a
// text-only message on some Go versions.
func isAccessDenied(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if os.IsPermission(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "being used by another process") ||
		strings.Contains(msg, "sharing violation")
}

// fsyncDir fsyncs the parent directory so the rename entry itself is durable
// (S3R-5). On Windows opening a directory for Sync is not supported; the NTFS
// rename is journaled by the filesystem, so a dir-fsync is a POSIX-only
// durability step and a Windows failure is not fatal.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if runtime.GOOS == "windows" {
			return nil
		}
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		if runtime.GOOS == "windows" {
			return nil // directory fsync unsupported on Windows; NTFS journals the rename
		}
		return err
	}
	return nil
}

// withLock mirrors ledger.acquireLock (O_EXCL create-once + stale-steal): a
// crashed holder's lock (older than lockStale) is reclaimed so a resume never
// wedges. A lock still held within the bounded wait RETURNS a lock-busy error
// (S3R-6) so the caller fails loud rather than silently dropping the write.
func withLock(dir string, fn func() error) error {
	lock := statePath(dir) + ".lock"
	deadline := time.Now().Add(lockWait)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d %s", os.Getpid(), time.Now().UTC().Format(time.RFC3339))
			f.Close()
			break
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > lockStale {
			_ = os.Remove(lock)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("strategy state lock busy: %s", lock)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer os.Remove(lock)
	return fn()
}
