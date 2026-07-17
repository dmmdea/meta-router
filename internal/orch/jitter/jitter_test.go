package jitter

import (
	"math/rand"
	"testing"
	"time"
)

// E4 (admission.go hands smoothing to slice-4): N deferred callers sharing one
// ResumeAt must not re-hammer the reset boundary synchronously. RetryAt spreads
// retries uniformly into [resumeAt, resumeAt+window); resume_at itself stays
// truthful — retry_at is the ADDITIVE scheduling hint.
func TestRetryAtBoundsAndDeterminism(t *testing.T) {
	resume := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	r := rand.New(rand.NewSource(42))
	seen := map[time.Time]bool{}
	for i := 0; i < 200; i++ {
		got := RetryAt(resume, 90*time.Second, r)
		if got.Before(resume) || !got.Before(resume.Add(90*time.Second)) {
			t.Fatalf("retry %d out of [resume, resume+90s): %s", i, got)
		}
		seen[got] = true
	}
	if len(seen) < 50 {
		t.Fatalf("200 draws must spread (thundering-herd), got %d distinct", len(seen))
	}
	// Determinism under a seeded source (test reproducibility contract).
	a := RetryAt(resume, 90*time.Second, rand.New(rand.NewSource(7)))
	b := RetryAt(resume, 90*time.Second, rand.New(rand.NewSource(7)))
	if !a.Equal(b) {
		t.Fatalf("seeded RetryAt must be deterministic: %s != %s", a, b)
	}
}

func TestRetryAtDegenerateWindow(t *testing.T) {
	resume := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if got := RetryAt(resume, 0, nil); !got.Equal(resume) {
		t.Fatalf("zero window must return resumeAt unchanged, got %s", got)
	}
}

// RetryAtStable must be idempotent for a fixed (key, resumeAt): a scheduler that
// polls strategy_status repeatedly has to see the SAME retry_at every read, or it
// can never settle on a wake time. This is the non-idempotency bug (plain RetryAt
// with a nil source re-rolls on every call).
func TestRetryAtStableIsIdempotentPerKey(t *testing.T) {
	resume := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	first := RetryAtStable(resume, DefaultWindow, "dispatch-abc")
	for i := 0; i < 100; i++ {
		if got := RetryAtStable(resume, DefaultWindow, "dispatch-abc"); !got.Equal(first) {
			t.Fatalf("read %d re-rolled: %s != %s (must be stable across reads)", i, got, first)
		}
	}
	if first.Before(resume) || !first.Before(resume.Add(DefaultWindow)) {
		t.Fatalf("retry_at %s must land in [resume, resume+window)", first)
	}
}

// ...but DISTINCT dispatches sharing one reset boundary must still spread — the
// E4 anti-herd purpose. Stability is per-dispatch, not a global constant.
func TestRetryAtStableDecorrelatesAcrossKeys(t *testing.T) {
	resume := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	seen := map[time.Time]bool{}
	for i := 0; i < 200; i++ {
		seen[RetryAtStable(resume, DefaultWindow, "dispatch-"+string(rune('A'+i%26))+string(rune('0'+i/26)))] = true
	}
	if len(seen) < 50 {
		t.Fatalf("200 distinct keys must spread into the window, got %d distinct", len(seen))
	}
}

// A changed resume boundary (re-estimate) legitimately re-phases the offset, so
// the same key against a different resumeAt need not collide — but must stay in
// bounds relative to its own resumeAt.
func TestRetryAtStableTracksResumeBoundary(t *testing.T) {
	r1 := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	r2 := r1.Add(3 * time.Hour)
	got := RetryAtStable(r2, DefaultWindow, "k")
	if got.Before(r2) || !got.Before(r2.Add(DefaultWindow)) {
		t.Fatalf("retry_at %s must be relative to its own resumeAt %s", got, r2)
	}
}

func TestRetryAtStableDegenerateWindow(t *testing.T) {
	resume := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if got := RetryAtStable(resume, 0, "k"); !got.Equal(resume) {
		t.Fatalf("zero window must return resumeAt unchanged, got %s", got)
	}
}
