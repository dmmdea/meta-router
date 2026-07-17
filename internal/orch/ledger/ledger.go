// Package ledger is the quota substrate: generic per-(lane,window) buckets fed
// by shadow accounting (always on, the floor) and any provider-reported
// percentages (authoritative — cross-device drift means local counts lie).
// Capacities are LEARNED, versioned quantities (three repricings in ~2 months);
// nothing here hardcodes a cap.
//
// Window anchoring (RS4, generalized in slice 2 — S2R-15): 5h buckets
// self-anchor on first shadow usage (ResetsAt = firstUse + 5h, ccusage
// block-anchoring) and re-anchor after each roll, so a shadow-only bucket can
// never grow ShadowTokens unbounded. A shadow bucket derives UsedPct whenever
// it is BOTH capped (CapTokens>0) and anchored (ResetsAt set) — lanes without
// a provider percentage surface (GLM prompt-units, codex millicredits) anchor
// their weekly windows via AnchorIfUnset and derive from there. An UNANCHORED
// window still never derives, and claude-7d never self-anchors from shadow
// alone (both RS4 floors, regression-pinned). Since Task 8 the claude
// provider-true signals are TWO: the RS1 statusline tee (WIRED — ObserveProvider
// with a percentage) and the stream rate_limit_event (AnchorAuthoritative — a
// true reset WITHOUT a percentage, which REPLACES a self-anchored estimate;
// set-once applies only between estimates, S2R-7). A claude-7d bucket
// therefore derives from shadow once it is BOTH stream-anchored and
// capacity-fitted (calib); a merely-capped one still reports -1.
//
// Capacity provenance (S2R-3): caps seeded from config guesses are marked
// CapSource="estimate" via SetCapacityEstimate — admission may THROTTLE on an
// estimate-derived percentage but never EXHAUST; denial semantics need a real
// provider signal (ObserveProvider). Fitted/measured SetCapacity clears the
// mark.
package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type WindowKind string

const (
	Win5h WindowKind = "5h"
	Win7d WindowKind = "7d"
)

type Bucket struct {
	Lane         string     `json:"lane"`
	Window       WindowKind `json:"window"`
	UsedPct      float64    `json:"used_pct"` // 0..100; -1 = unknown
	ResetsAt     time.Time  `json:"resets_at"`
	Source       string     `json:"source"` // "provider" | "shadow"
	ObservedAt   time.Time  `json:"observed_at"`
	ShadowTokens int64      `json:"shadow_tokens"`
	CapTokens    int64      `json:"cap_tokens"` // learned capacity estimate; 0 = unlearned
	CapVersion   int        `json:"cap_version"`
	// CapSource marks the capacity's provenance: "" = fitted/measured,
	// CapSourceEstimate = config guess (S2R-3: throttle-only, never exhaust).
	CapSource string `json:"cap_source,omitempty"`
}

// CapSourceEstimate marks a CapTokens that is a config guess (e.g. the codex
// Plus 5h band × degradation factor), not a fitted or provider-derived value.
const CapSourceEstimate = "estimate"

type Ledger struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*Bucket // key: lane + "|" + window
}

func key(lane string, w WindowKind) string { return lane + "|" + string(w) }

// Open loads the ledger at path; a missing or corrupt file fails open to an
// empty ledger (the shadow floor rebuilds from subsequent runs).
func Open(path string) *Ledger {
	l, _ := OpenChecked(path)
	return l
}

// OpenChecked is Open plus a non-empty warning when the file existed but was
// unreadable/corrupt — the fail-open contract requires the caller to WARN,
// not to silently zero accumulated state.
func OpenChecked(path string) (*Ledger, string) {
	l := &Ledger{path: path, buckets: map[string]*Bucket{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, ""
		}
		return l, "ledger unreadable, failing open to empty: " + err.Error()
	}
	var list []Bucket
	if err := json.Unmarshal(b, &list); err != nil {
		return l, "ledger corrupt, failing open to empty: " + err.Error()
	}
	for i := range list {
		cp := list[i]
		l.buckets[key(cp.Lane, cp.Window)] = &cp
	}
	return l, ""
}

// Update performs a cross-process-safe read-modify-write: acquire the sidecar
// lock file, open a FRESH ledger from disk, apply fn, save atomically. The
// in-process mutex only guards goroutines; concurrent INVOCATIONS (run +
// status + the scheduled probe) must route every write through Update or a
// last-writer-wins race silently drops shadow tokens. Reads for admission
// decisions may still use Open — a stale read fails open by design.
func Update(path string, fn func(*Ledger)) error {
	unlock, err := acquireLock(path+".lock", 3*time.Second, 30*time.Second)
	if err != nil {
		return err
	}
	defer unlock()
	l := Open(path)
	fn(l)
	return l.Save()
}

// acquireLock takes an exclusive create-only lock file, waiting up to wait and
// stealing locks older than stale (crashed holder).
func acquireLock(lockPath string, wait, stale time.Duration) (func(), error) {
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
			return nil, fmt.Errorf("ledger lock busy: %s", lockPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (l *Ledger) get(lane string, w WindowKind) *Bucket {
	k := key(lane, w)
	b, ok := l.buckets[k]
	if !ok {
		b = &Bucket{Lane: lane, Window: w, UsedPct: -1}
		l.buckets[k] = b
	}
	return b
}

// roll clears a window whose reset moment has passed. The caller re-anchors
// 5h buckets on the next shadow usage; provider-sourced buckets wait for a
// fresh observation.
func (b *Bucket) roll(now time.Time) {
	if !b.ResetsAt.IsZero() && now.After(b.ResetsAt) {
		b.ShadowTokens = 0
		b.UsedPct = -1
		b.Source = "shadow"
		b.ResetsAt = time.Time{}
	}
}

func (l *Ledger) ObserveProvider(lane string, w WindowKind, usedPct float64, resetsAt, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.roll(now)
	b.UsedPct = usedPct
	b.ResetsAt = resetsAt
	b.Source = "provider"
	b.ObservedAt = now
}

// AnchorIfUnset sets a window's reset moment when none is known, WITHOUT
// touching the usage signal. Callers: GLM/codex weekly self-anchoring (these
// lanes have no provider percentage surface) and the stream rate_limit_event's
// provider-true reset anchor. Provider-sourced buckets are never re-anchored.
func (l *Ledger) AnchorIfUnset(lane string, w WindowKind, resetsAt, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.roll(now)
	if b.Source != "provider" && b.ResetsAt.IsZero() {
		b.ResetsAt = resetsAt
	}
}

// AnchorAuthoritative sets a window's reset moment from a PROVIDER-TRUE source
// (the stream rate_limit_event carries the account's real reset), REPLACING a
// self-anchored estimate when they disagree — S2R-7: set-once (AnchorIfUnset)
// applies only between estimates. Provider-sourced buckets keep their own
// observation untouched: ObserveProvider carries reset AND percentage, and an
// anchor-only signal must not detach the pair. Like AnchorIfUnset, the usage
// signal is never touched; derivation happens on the next AddShadow.
func (l *Ledger) AnchorAuthoritative(lane string, w WindowKind, resetsAt, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.roll(now)
	if b.Source != "provider" {
		b.ResetsAt = resetsAt
	}
}

func (l *Ledger) AddShadow(lane string, w WindowKind, tokens int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.roll(now)
	if w == Win5h && b.ResetsAt.IsZero() {
		b.ResetsAt = now.Add(5 * time.Hour) // RS4 self-anchor
	}
	b.ShadowTokens += tokens
	if b.Source != "provider" { // provider stays authoritative until its window rolls
		// Anchored-derivation rule (slice 2): capped AND anchored ⇒ derive.
		// RS4 is generalized, not repealed — an UNANCHORED window still never
		// derives, which is exactly the claude-7d case (never capped-and-
		// anchored by shadow alone; the tee provides its signal).
		if b.CapTokens > 0 && !b.ResetsAt.IsZero() {
			b.UsedPct = 100 * float64(b.ShadowTokens) / float64(b.CapTokens)
		} else {
			b.UsedPct = -1 // unanchored or uncapped: never derive (RS4)
		}
		b.Source = "shadow"
		b.ObservedAt = now
	}
}

// ClearShadow zeroes a bucket's shadow accumulation without fabricating a
// percentage or touching provider observations. Caller: the S2R-2 glm unit
// migration — a bucket that predates prompt-unit metering carries token-scale
// shadow that would poison a prompt-unit cap.
func (l *Ledger) ClearShadow(lane string, w WindowKind, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.ShadowTokens = 0
	if b.Source != "provider" {
		if b.CapTokens > 0 && !b.ResetsAt.IsZero() {
			b.UsedPct = 0
		} else {
			b.UsedPct = -1
		}
		b.ObservedAt = now
	}
}

// SetCapacity records a fitted/measured capacity, clearing any estimate mark
// (a measured value replaces a config guess — S2R-3).
func (l *Ledger) SetCapacity(lane string, w WindowKind, capTokens int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.CapTokens = capTokens
	b.CapVersion++
	b.CapSource = ""
}

// SetCapacityEstimate is SetCapacity for CONFIG-GUESS capacities (codex Plus
// band × degradation factor). S2R-3: buckets whose UsedPct derives from an
// estimate-sourced cap may THROTTLE admission but never EXHAUST it — denial
// activates only after a real provider signal anchors the window.
func (l *Ledger) SetCapacityEstimate(lane string, w WindowKind, capTokens int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, w)
	b.CapTokens = capTokens
	b.CapVersion++
	b.CapSource = CapSourceEstimate
}

func (l *Ledger) Bucket(lane string, w WindowKind) (Bucket, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key(lane, w)]
	if !ok {
		return Bucket{}, false
	}
	return *b, true
}

func (l *Ledger) Snapshot() []Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotLocked()
}

// snapshotLocked copies the buckets; the caller must hold mu.
func (l *Ledger) snapshotLocked() []Bucket {
	out := make([]Bucket, 0, len(l.buckets))
	for _, b := range l.buckets {
		out = append(out, *b)
	}
	return out
}

// Save writes the ledger atomically. The tmp name is per-process: a shared
// fixed tmp would let two concurrent savers interleave truncate+write and
// rename torn JSON into place.
func (l *Ledger) Save() error {
	l.mu.Lock()
	data, err := json.MarshalIndent(l.snapshotLocked(), "", "  ")
	l.mu.Unlock()
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d", l.path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}
