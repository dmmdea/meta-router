package ledger

import (
	"os"
	"path/filepath"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func TestProviderOverridesShadow(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("claude", "7d", 50_000, t0)
	l.ObserveProvider("claude", "7d", 37.5, t0.Add(72*time.Hour), t0)
	b, ok := l.Bucket("claude", "7d")
	if !ok || b.Source != "provider" || b.UsedPct != 37.5 {
		t.Fatalf("provider must override shadow: %+v", b)
	}
	if b.ShadowTokens != 50_000 {
		t.Fatalf("shadow tokens must be preserved for calibration: %+v", b)
	}
}

func TestShadowDerivesPctOnlyWithCapacity(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("claude", "5h", 10_000, t0)
	b, _ := l.Bucket("claude", "5h")
	if b.UsedPct != -1 {
		t.Fatalf("unknown capacity must report -1, got %v", b.UsedPct)
	}
	l.SetCapacity("claude", "5h", 200_000)
	l.AddShadow("claude", "5h", 30_000, t0.Add(time.Minute))
	b, _ = l.Bucket("claude", "5h")
	if b.UsedPct != 20 { // (10k+30k)/200k
		t.Fatalf("want 20%%, got %v", b.UsedPct)
	}
}

// RS4: 5h buckets self-anchor on first shadow usage (ccusage block-anchoring).
func TestShadow5hAnchorsOnFirstUse(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("claude", "5h", 1_000, t0)
	b, _ := l.Bucket("claude", "5h")
	if !b.ResetsAt.Equal(t0.Add(5 * time.Hour)) {
		t.Fatalf("5h shadow bucket must anchor ResetsAt=firstUse+5h, got %v", b.ResetsAt)
	}
}

// RS4: a shadow-only 5h bucket ROLLS at anchor+5h and re-anchors on next use
// (the un-rolled variant grew ShadowTokens forever -> phantom exhaustion).
func TestShadow5hRollsAtAnchor(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("claude", "5h", 1_000, t0)
	l.AddShadow("claude", "5h", 500, t0.Add(6*time.Hour)) // past t0+5h anchor
	b, _ := l.Bucket("claude", "5h")
	if b.ShadowTokens != 500 {
		t.Fatalf("rolled bucket must hold only post-roll tokens, got %d", b.ShadowTokens)
	}
	if !b.ResetsAt.Equal(t0.Add(6*time.Hour + 5*time.Hour)) {
		t.Fatalf("rolled 5h bucket must re-anchor at newUse+5h, got %v", b.ResetsAt)
	}
}

// RS4: 7d buckets are provider-anchored ONLY — shadow never derives a
// percentage (the blended weekly signal comes from the RS1 statusline tee)
// and never self-anchors.
func TestShadow7dNeverDerivesPct(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.SetCapacity("claude", "7d", 1_000_000)
	l.AddShadow("claude", "7d", 500_000, t0)
	b, _ := l.Bucket("claude", "7d")
	if b.UsedPct != -1 {
		t.Fatalf("7d shadow bucket must report UsedPct -1 until a provider observation, got %v", b.UsedPct)
	}
	if !b.ResetsAt.IsZero() {
		t.Fatalf("7d shadow bucket must not self-anchor, got %v", b.ResetsAt)
	}
	if b.ShadowTokens != 500_000 {
		t.Fatalf("7d shadow tokens must still accumulate for calibration, got %d", b.ShadowTokens)
	}
}

func TestWindowRollsAfterReset(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.ObserveProvider("claude", "5h", 90, t0.Add(time.Hour), t0)
	l.AddShadow("claude", "5h", 1000, t0.Add(2*time.Hour)) // past resets_at
	b, _ := l.Bucket("claude", "5h")
	if b.Source != "shadow" || b.ShadowTokens != 1000 {
		t.Fatalf("window must roll after resets_at: %+v", b)
	}
	if !b.ResetsAt.Equal(t0.Add(2*time.Hour + 5*time.Hour)) {
		t.Fatalf("rolled 5h bucket must re-anchor (RS4), got %v", b.ResetsAt)
	}
}

func TestSaveOpenRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	l := Open(p)
	l.ObserveProvider("claude", "7d", 12, t0.Add(time.Hour), t0)
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	l2 := Open(p)
	if b, ok := l2.Bucket("claude", "7d"); !ok || b.UsedPct != 12 {
		t.Fatalf("round-trip lost state: %+v", b)
	}
}

func TestOpenCorruptFailsOpen(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, warn := OpenChecked(p)
	if len(l.Snapshot()) != 0 {
		t.Fatalf("corrupt file must fail open to empty state")
	}
	if warn == "" {
		t.Fatal("corrupt file must produce a warning (fail-open contract requires a WARN)")
	}
	if _, warn2 := OpenChecked(filepath.Join(t.TempDir(), "missing.json")); warn2 != "" {
		t.Fatalf("a merely-missing file is normal, no warning: %q", warn2)
	}
}

// Concurrent Updates must serialize on the lock file — no lost updates.
func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func() {
			done <- Update(p, func(l *Ledger) {
				l.AddShadow("claude", Win5h, 100, t0)
			})
		}()
	}
	for i := 0; i < 10; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b, _ := Open(p).Bucket("claude", Win5h)
	if b.ShadowTokens != 1000 {
		t.Fatalf("lost update: want 1000 shadow tokens across 10 writers, got %d", b.ShadowTokens)
	}
}

// Slice 2: GLM/codex weekly buckets have no provider percentage surface —
// they self-anchor via AnchorIfUnset and derive once capped AND anchored
// (RS4 generalized, not repealed: an UNANCHORED window still never derives).
func TestAnchorIfUnsetSetsResetOnceAndDerives(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.SetCapacity("glm", Win7d, 400) // prompt-units: Lite weekly = 5×80
	l.AnchorIfUnset("glm", Win7d, t0.Add(7*24*time.Hour), t0)
	l.AddShadow("glm", Win7d, 100, t0)
	b, _ := l.Bucket("glm", Win7d)
	if b.UsedPct != 25 { // 100/400 — capped AND anchored ⇒ derives (generalized RS4)
		t.Fatalf("anchored+capped weekly must derive, got %v", b.UsedPct)
	}
	l.AnchorIfUnset("glm", Win7d, t0.Add(99*time.Hour), t0) // second anchor must NOT move it
	if b, _ := l.Bucket("glm", Win7d); !b.ResetsAt.Equal(t0.Add(7 * 24 * time.Hour)) {
		t.Fatalf("anchor must be set-once: %v", b.ResetsAt)
	}
}

func TestAnchorNeverOverridesProvider(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.ObserveProvider("claude", Win7d, 40, t0.Add(48*time.Hour), t0)
	l.AnchorIfUnset("claude", Win7d, t0.Add(time.Hour), t0)
	if b, _ := l.Bucket("claude", Win7d); !b.ResetsAt.Equal(t0.Add(48 * time.Hour)) {
		t.Fatalf("provider anchor is authoritative: %v", b.ResetsAt)
	}
}

func TestClaude7dStillNeverDerivesFromShadowAlone(t *testing.T) { // RS4 regression pin
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.SetCapacity("claude", Win7d, 1_000_000)
	l.AddShadow("claude", Win7d, 500_000, t0) // capped but UNANCHORED
	if b, _ := l.Bucket("claude", Win7d); b.UsedPct != -1 {
		t.Fatalf("unanchored 7d must stay -1 (RS4), got %v", b.UsedPct)
	}
}

// S2R-7 (Task 8): the stream rate_limit_event carries a PROVIDER-TRUE reset —
// it REPLACES a self-anchored estimate when they disagree; set-once semantics
// (AnchorIfUnset) apply only between estimates. A full provider observation
// (reset AND percentage, ObserveProvider) still outranks an anchor-only signal.
func TestAnchorAuthoritativeOverwritesEstimateNotProvider(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("claude", Win5h, 1_000, t0) // RS4 self-anchor: estimate t0+5h
	trueReset := t0.Add(4*time.Hour + 30*time.Minute)
	l.AnchorAuthoritative("claude", Win5h, trueReset, t0)
	if b, _ := l.Bucket("claude", Win5h); !b.ResetsAt.Equal(trueReset) {
		t.Fatalf("provider-true reset must REPLACE the self-anchored estimate: %v", b.ResetsAt)
	}
	l.AnchorIfUnset("claude", Win5h, t0.Add(9*time.Hour), t0) // estimate never clobbers
	if b, _ := l.Bucket("claude", Win5h); !b.ResetsAt.Equal(trueReset) {
		t.Fatalf("a later estimate must never clobber an authoritative anchor: %v", b.ResetsAt)
	}
	// Provider observations carry reset AND percentage — an anchor-only signal
	// must not detach the pair.
	l.ObserveProvider("claude", Win5h, 42, t0.Add(3*time.Hour), t0)
	l.AnchorAuthoritative("claude", Win5h, trueReset, t0)
	if b, _ := l.Bucket("claude", Win5h); !b.ResetsAt.Equal(t0.Add(3*time.Hour)) || b.UsedPct != 42 {
		t.Fatalf("a provider observation must survive an authoritative anchor: %+v", b)
	}
}

// S2R-3: config-guess capacities are marked estimate-sourced so admission can
// throttle-but-never-exhaust on them; a fitted/measured SetCapacity clears
// the mark.
func TestSetCapacityEstimateMarksAndSetCapacityClears(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.SetCapacityEstimate("codex", Win5h, 40_000)
	b, _ := l.Bucket("codex", Win5h)
	if b.CapTokens != 40_000 || b.CapSource != CapSourceEstimate || b.CapVersion != 1 {
		t.Fatalf("estimate cap must mark cap_source: %+v", b)
	}
	l.SetCapacity("codex", Win5h, 55_000) // fitted replaces the guess
	if b, _ := l.Bucket("codex", Win5h); b.CapSource != "" || b.CapVersion != 2 {
		t.Fatalf("measured cap must clear the estimate mark: %+v", b)
	}
}

// A crashed holder's stale lock must be stolen, not deadlock the ledger.
func TestUpdateStealsStaleLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	lock := p + ".lock"
	if err := os.WriteFile(lock, []byte("dead"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	if err := Update(p, func(l *Ledger) { l.AddShadow("claude", Win5h, 1, t0) }); err != nil {
		t.Fatalf("stale lock must be stolen: %v", err)
	}
}

// ClearShadow is the S2R-2 unit-migration hook: a glm bucket that predates
// prompt-unit metering carries token-scale shadow — clearing must zero the
// accumulation without fabricating a percentage or touching provider state.
func TestClearShadowResetsUnitScale(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	l.AddShadow("glm", Win5h, 13_061, t0) // token-scale contamination
	l.ClearShadow("glm", Win5h, t0)
	b, _ := l.Bucket("glm", Win5h)
	if b.ShadowTokens != 0 || b.UsedPct != -1 {
		t.Fatalf("cleared uncapped bucket must read 0 shadow, -1 pct: %+v", b)
	}
	// Provider observations outrank: clearing shadow must not disturb them.
	l.ObserveProvider("glm", Win5h, 42, t0.Add(time.Hour), t0)
	l.ClearShadow("glm", Win5h, t0)
	if b, _ := l.Bucket("glm", Win5h); b.UsedPct != 42 || b.Source != "provider" {
		t.Fatalf("provider signal must survive a shadow clear: %+v", b)
	}
}

func TestSubjectKeyDefaultCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.json")
	// Rows written WITHOUT a subject (the entire pre-W1 fleet) must load and
	// keep resolving to the same buckets the pre-subject code addressed.
	legacy := `[{"lane":"claude","window":"5h","used_pct":40,"resets_at":"2027-01-01T00:00:00Z","source":"provider","observed_at":"2026-07-23T00:00:00Z"}]`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	l, note := OpenChecked(path)
	if note != "" {
		t.Fatalf("legacy ledger must load clean, note=%q", note)
	}
	b, ok := l.Bucket("claude", Win5h)
	if !ok || b.UsedPct != 40 {
		t.Fatalf("legacy row must resolve on the default subject, got %+v ok=%v", b, ok)
	}
}

func TestSubjectKeyDistinct(t *testing.T) {
	if key("claude", "", Win5h) != key("claude", "default", Win5h) {
		t.Fatal("empty subject must normalize to default")
	}
	if key("claude", "acct2", Win5h) == key("claude", "default", Win5h) {
		t.Fatal("distinct subjects must produce distinct keys")
	}
}

// W2: distinct subjects never cross-contaminate; default paths unchanged.
func TestSubjectIsolation(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "ledger.json"))
	now := time.Now().UTC()
	reset := now.Add(3 * time.Hour)
	l.ObserveProvider("claude", Win5h, 80, reset, now)                     // default subject
	l.ObserveProviderSubject("claude", "acct2", Win5h, 10, reset, now)     // second account
	if b, _ := l.Bucket("claude", Win5h); b.UsedPct != 80 {
		t.Fatalf("default subject contaminated: %+v", b)
	}
	if b, ok := l.BucketSubject("claude", "acct2", Win5h); !ok || b.UsedPct != 10 || b.Subject != "acct2" {
		t.Fatalf("acct2 bucket wrong: %+v ok=%v", b, ok)
	}
	l.AddShadowSubject("claude", "acct2", Win5h, 500, now)
	if b, _ := l.Bucket("claude", Win5h); b.ShadowTokens != 0 {
		t.Fatalf("default shadow contaminated by subject write: %+v", b)
	}
	s := l.SnapshotSubject("claude", "acct2")
	if len(s) != 1 || s[0].Subject != "acct2" {
		t.Fatalf("SnapshotSubject must return only acct2 buckets: %+v", s)
	}
	if sd := l.SnapshotSubject("claude", ""); len(sd) != 1 || sd[0].UsedPct != 80 {
		t.Fatalf("default snapshot wrong: %+v", sd)
	}
}

// W2 byte-identical guard: the DEFAULT subject — however spelled at the write
// site ("" or the literal "default" from the implicit registry) — stores an
// EMPTY subject, so its omitempty field never appears in status/ledger JSON on
// a single-account machine (the regression the Opus review caught).
func TestDefaultSubjectStoredEmpty(t *testing.T) {
	l := Open(filepath.Join(t.TempDir(), "l.json"))
	now := time.Now().UTC()
	l.ObserveProviderSubject("codex", "default", Win7d, 46, now.Add(48*time.Hour), now)
	b, ok := l.Bucket("codex", Win7d) // default-subject reader
	if !ok || b.Subject != "" {
		t.Fatalf("literal \"default\" must be stored as empty subject, got %q", b.Subject)
	}
	js, _ := json.Marshal(b)
	if strings.Contains(string(js), `"subject"`) {
		t.Fatalf("single-account bucket must not serialize a subject field: %s", js)
	}
}
