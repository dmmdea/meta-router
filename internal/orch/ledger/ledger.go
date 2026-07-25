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
	"path/filepath"
	"sync"
	"time"
)

type WindowKind string

const (
	Win5h WindowKind = "5h"
	Win7d WindowKind = "7d"
)

type Bucket struct {
	Lane string `json:"lane"`
	// Subject scopes the bucket to a credential subject (account); "" is the
	// default subject. W1 keys for this so W2's multi-account profiles don't
	// have to retrofit the key shape (cheap to key now, painful later).
	Subject string     `json:"subject,omitempty"`
	Window  WindowKind `json:"window"`
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
	// ProviderSource records WHICH provider signal wrote UsedPct:
	// ProviderSourcePoll (vendor usage endpoint) or ProviderSourceDrop (the
	// statusline tee). "" = unspecified (a lane 429 or a pre-2026-07-25 row).
	// Precedence needs this: the two surfaces disagreed by 22pp and 14h of
	// anchor in the same instant, and last-writer-wins let the weaker one win.
	ProviderSource string `json:"provider_source,omitempty"`
}

// Provider signal kinds for Bucket.ProviderSource, in ascending authority.
// A vendor DENIAL outranks a vendor usage reading, which outranks the
// statusline tee's second-hand numbers.
const (
	ProviderSourceDrop  = "drop"  // statusline tee (second-hand, no internal timestamp)
	ProviderSourcePoll  = "poll"  // vendor usage endpoint
	ProviderSourceLimit = "limit" // vendor said NO (a 429 observed at dispatch)
)

// AuthorityTTL bounds how long a higher-authority observation outranks a lower
// one. Authority EXPIRES: an absolute rule would freeze a lane at the last poll
// for a whole 7d window if the poller broke (expired token, network), and the
// router would keep admitting on days-old data. After the TTL the freshest
// observation wins regardless of source.
const AuthorityTTL = 15 * time.Minute

func sourceRank(s string) int {
	switch s {
	case ProviderSourceLimit:
		return 3
	case ProviderSourcePoll:
		return 2
	case ProviderSourceDrop:
		return 1
	}
	return 0 // unspecified (pre-2026-07-25 rows)
}

// Observation is one provider-reported window fact. ObservedAt is when the
// VENDOR reported it — never the ingest instant: stamping ingest time is what
// let a 50h-old statusline fixture keep looking fresh to the E6 staleness
// alarm (audit 2026-07-25).
type Observation struct {
	Lane, Subject string
	Window        WindowKind
	UsedPct       float64
	ResetsAt      time.Time
	ObservedAt    time.Time
	Source        string // ProviderSourcePoll | ProviderSourceDrop
}

// Observe applies a provider observation under bounded SOURCE PRECEDENCE and
// returns whether it landed plus the reason when it did not (a silent refusal
// is unobservable, which is how the original poisoning hid). Rules:
//   - a LOWER-authority source cannot overwrite a HIGHER-authority one while
//     the incumbent is still fresh (within AuthorityTTL) — a statusline drop
//     must not overwrite a live vendor poll, and neither may re-open a window
//     the vendor just denied;
//   - authority EXPIRES (AuthorityTTL), so a broken poller cannot freeze a lane
//     at stale data for the rest of the window;
//   - no observation REWINDS an equal-or-higher-authority one (an older reading
//     never replaces a newer one for the same live window);
//   - a FUTURE ObservedAt is CLAMPED to now. An unclamped future stamp both
//     locked out every later poll and blinded the E6 staleness alarm (which
//     keys on ObservedAt) — reachable from a timestamp-preserving restore, a
//     synced state dir, or a backwards clock correction.
//
// An expired bucket is rolled first, so a refusal only ever protects live data.
func (l *Ledger) Observe(o Observation, now time.Time) (bool, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(o.Lane, o.Subject, o.Window)
	b.roll(now)
	observedAt := o.ObservedAt
	if observedAt.IsZero() || observedAt.After(now) {
		observedAt = now // no observation is from the future
	}
	if b.Source == "provider" {
		incumbentFresh := !b.ObservedAt.IsZero() && now.Sub(b.ObservedAt) <= AuthorityTTL
		if incumbentFresh && sourceRank(o.Source) < sourceRank(b.ProviderSource) {
			return false, o.Source + " outranked by a fresh " + b.ProviderSource + " observation"
		}
		if !b.ObservedAt.IsZero() && observedAt.Before(b.ObservedAt) &&
			sourceRank(o.Source) <= sourceRank(b.ProviderSource) {
			return false, o.Source + " observation is older than the one it would replace"
		}
	}
	b.UsedPct = o.UsedPct
	b.ResetsAt = o.ResetsAt
	b.Source = "provider"
	b.ObservedAt = observedAt
	b.ProviderSource = o.Source
	return true, ""
}

// CapSourceEstimate marks a CapTokens that is a config guess (e.g. the codex
// Plus 5h band × degradation factor), not a fitted or provider-derived value.
const CapSourceEstimate = "estimate"

// Expired reports whether the window's reset moment has PASSED — the bucket's
// percentage is then dead history, never live pressure. Every consumer that
// renders, ranks, or gates on UsedPct must consult this. admission, burnrate,
// spenddown and pace still carry their own equivalent inline checks (burnrate's
// differs at exact equality); the two that had NONE — the hook's quota banner
// and the router's worst-window tie-break — spent 6 days reporting a
// reset-25h-ago window as live throttle pressure. Semantics here match
// admission's inline check; migrating the other three is a separate,
// behavior-pinned change.
func (b Bucket) Expired(now time.Time) bool {
	return !b.ResetsAt.IsZero() && now.After(b.ResetsAt)
}

type Ledger struct {
	mu      sync.Mutex
	path    string
	buckets map[string]*Bucket // key: lane + "|" + subject + "|" + window
}

func subjectOrDefault(s string) string {
	if s == "" {
		return "default"
	}
	return s
}

func key(lane, subject string, w WindowKind) string {
	return lane + "|" + subjectOrDefault(subject) + "|" + string(w)
}

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
	l, warn, _ := openWithKind(path)
	return l, warn
}

// openWithKind is OpenChecked plus the distinction the quarantine needs:
// corrupt (unmarshal failed — the bytes are garbage) vs a transient I/O error.
func openWithKind(path string) (*Ledger, string, bool) {
	l, warn, corrupt, _ := openWithBytes(path)
	return l, warn, corrupt
}

// openWithBytes additionally returns the raw bytes so a caller can PRESERVE
// them. Quarantining by RENAME loses the file whenever the rename fails (the
// AV/backup sharing violation this design anticipates), because Save()
// overwrites the original either way.
func openWithBytes(path string) (*Ledger, string, bool, []byte) {
	l := &Ledger{path: path, buckets: map[string]*Bucket{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return l, "", false, nil
		}
		return l, "ledger unreadable, failing open to empty: " + err.Error(), false, nil
	}
	var list []Bucket
	if err := json.Unmarshal(b, &list); err != nil {
		return l, "ledger corrupt, failing open to empty: " + err.Error(), true, b
	}
	for i := range list {
		cp := list[i]
		l.buckets[key(cp.Lane, cp.Subject, cp.Window)] = &cp
	}
	return l, "", false, nil
}

// Update performs a cross-process-safe read-modify-write: acquire the sidecar
// lock file, open a FRESH ledger from disk, apply fn, save atomically. The
// in-process mutex only guards goroutines; concurrent INVOCATIONS (run +
// status + the scheduled probe) must route every write through Update or a
// last-writer-wins race silently drops shadow tokens. Reads for admission
// decisions may still use Open — a stale read fails open by design.
func Update(path string, fn func(*Ledger)) error {
	_, err := UpdateChecked(path, fn)
	return err
}

// UpdateChecked is Update plus OpenChecked's warning — the caller MUST surface
// it. Update discarded it, so a corrupt ledger was silently replaced by an
// empty one on the next write (audit 2026-07-25: exit 0, zero stderr, six
// buckets gone; GLM has no poller, so its consumed weekly window became
// re-spendable unmetered).
//
// On a CORRUPT file (unmarshal failure) the original bytes are COPIED to
// ledger.json.corrupt-<UTC stamp> before the fresh state is written: the damage
// stays inspectable. A transient read error is NOT quarantined — renaming on a
// sharing violation (Defender, backup) would cause the very loss this prevents.
// Either way the update still applies and Save still runs: refusing to write
// would dispatch with no shadow accounting (Bible B4).
func UpdateChecked(path string, fn func(*Ledger)) (string, error) {
	unlock, err := acquireLock(path+".lock", 3*time.Second, 30*time.Second)
	if err != nil {
		return "", err
	}
	defer unlock()
	l, warn, corrupt, raw := openWithBytes(path)
	if corrupt {
		// COPY, never rename: Save() overwrites path regardless, so preservation
		// must not depend on a rename succeeding.
		dst := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
		if werr := os.WriteFile(dst, raw, 0o644); werr != nil {
			warn += " (quarantine failed: " + werr.Error() + ")"
		} else {
			warn += " — original bytes copied to " + filepath.Base(dst)
		}
	}
	fn(l)
	return warn, l.Save()
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

func (l *Ledger) get(lane, subject string, w WindowKind) *Bucket {
	k := key(lane, subject, w)
	b, ok := l.buckets[k]
	if !ok {
		// The default subject is STORED as "" so its omitempty field stays
		// absent — byte-identical to pre-W2 (the poll loop threads the literal
		// "default" label from the implicit registry; key() already merges the
		// two spellings, so canonicalizing here is safe and every reader
		// normalizes via subjectOrDefault).
		if subject == "default" {
			subject = ""
		}
		b = &Bucket{Lane: lane, Subject: subject, Window: w, UsedPct: -1}
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
		b.ProviderSource = "" // a new window inherits no authority from the old one
	}
}

func (l *Ledger) ObserveProvider(lane string, w WindowKind, usedPct float64, resetsAt, now time.Time) {
	l.ObserveProviderSubject(lane, "", w, usedPct, resetsAt, now)
}

// ObserveProviderSubject is ObserveProvider on an explicit credential subject
// (W2; "" = default). Distinct subjects never share a bucket.
func (l *Ledger) ObserveProviderSubject(lane, subject string, w WindowKind, usedPct float64, resetsAt, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, subject, w)
	b.roll(now)
	b.UsedPct = usedPct
	b.ResetsAt = resetsAt
	b.Source = "provider"
	b.ObservedAt = now
}

// ObserveLimit records a vendor DENIAL (a 429 seen at dispatch) for a window:
// used_pct 100 with the resume the caller derived, carrying ProviderSourceLimit
// so a lower-authority reading cannot silently re-open the window inside its
// AuthorityTTL. It can never itself be refused for lack of authority — nothing
// outranks a denial — so callers need not check a result.
func (l *Ledger) ObserveLimit(lane, subject string, w WindowKind, resetsAt, now time.Time) {
	l.Observe(Observation{Lane: lane, Subject: subject, Window: w, UsedPct: 100,
		ResetsAt: resetsAt, ObservedAt: now, Source: ProviderSourceLimit}, now)
}

// BucketSubject is Bucket on an explicit subject (W2; "" = default).
func (l *Ledger) BucketSubject(lane, subject string, w WindowKind) (Bucket, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key(lane, subject, w)]
	if !ok {
		return Bucket{}, false
	}
	return *b, true
}

// AddShadowSubject is AddShadow on an explicit subject (W2; "" = default) —
// dispatch outcomes accrue to the subject that carried them.
func (l *Ledger) AddShadowSubject(lane, subject string, w WindowKind, tokens int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addShadowLocked(l.get(lane, subject, w), w, tokens, now)
}

// SnapshotSubject returns copies of one lane+subject's buckets (W2 selection
// input: per-subject admission + slack read exactly one subject's windows).
func (l *Ledger) SnapshotSubject(lane, subject string) []Bucket {
	l.mu.Lock()
	defer l.mu.Unlock()
	want := subjectOrDefault(subject)
	var out []Bucket
	for _, b := range l.buckets {
		if b.Lane == lane && subjectOrDefault(b.Subject) == want {
			out = append(out, *b)
		}
	}
	return out
}

// AnchorIfUnset sets a window's reset moment when none is known, WITHOUT
// touching the usage signal. Callers: GLM/codex weekly self-anchoring (these
// lanes have no provider percentage surface) and the stream rate_limit_event's
// provider-true reset anchor. Provider-sourced buckets are never re-anchored.
func (l *Ledger) AnchorIfUnset(lane string, w WindowKind, resetsAt, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.get(lane, "", w)
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
	b := l.get(lane, "", w)
	b.roll(now)
	if b.Source != "provider" {
		b.ResetsAt = resetsAt
	}
}

func (l *Ledger) AddShadow(lane string, w WindowKind, tokens int64, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addShadowLocked(l.get(lane, "", w), w, tokens, now)
}

// addShadowLocked is AddShadow's body, shared with the subject variant.
// Caller holds l.mu.
func (l *Ledger) addShadowLocked(b *Bucket, w WindowKind, tokens int64, now time.Time) {
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
	b := l.get(lane, "", w)
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
	b := l.get(lane, "", w)
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
	b := l.get(lane, "", w)
	b.CapTokens = capTokens
	b.CapVersion++
	b.CapSource = CapSourceEstimate
}

func (l *Ledger) Bucket(lane string, w WindowKind) (Bucket, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key(lane, "", w)]
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
