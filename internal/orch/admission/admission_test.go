package admission

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

var now = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

func bucket(w ledger.WindowKind, pct float64, resets time.Time) ledger.Bucket {
	return ledger.Bucket{Lane: "claude", Window: w, UsedPct: pct, ResetsAt: resets, Source: "provider"}
}

func TestWorstWindowGoverns(t *testing.T) {
	bs := []ledger.Bucket{bucket("5h", 20, now.Add(time.Hour)), bucket("7d", 97, now.Add(48*time.Hour))}
	d := Decide(bs, "claude", now, Thresholds{80, 95})
	if d.Admit || d.State != "exhausted" {
		t.Fatalf("97%% weekly must exhaust the lane: %+v", d)
	}
	if !d.ResumeAt.Equal(now.Add(48 * time.Hour)) {
		t.Fatalf("resume must anchor to the exhausted window's reset: %+v", d)
	}
}

func TestThrottledAdmitsWithReason(t *testing.T) {
	bs := []ledger.Bucket{bucket("5h", 85, now.Add(time.Hour))}
	d := Decide(bs, "claude", now, Thresholds{80, 95})
	if !d.Admit || d.State != "throttled" || d.Reason == "" {
		t.Fatalf("throttled should admit-with-flag: %+v", d)
	}
}

func TestNoSignalIsOpen(t *testing.T) {
	bs := []ledger.Bucket{{Lane: "claude", Window: "5h", UsedPct: -1}}
	if d := Decide(bs, "claude", now, Thresholds{80, 95}); !d.Admit || d.State != "open" {
		t.Fatalf("unknown usage must fail open: %+v", d)
	}
}

// RS5: a denial with no ResetsAt on the exhausted bucket still MUST carry a
// usable ResumeAt — 5h falls back to now+5h, marked as an estimate.
func TestDenied5hWithoutResetGetsEstimatedResume(t *testing.T) {
	bs := []ledger.Bucket{bucket("5h", 99, time.Time{})}
	d := Decide(bs, "claude", now, Thresholds{80, 95})
	if d.Admit {
		t.Fatalf("99%% must deny: %+v", d)
	}
	if !d.ResumeAt.Equal(now.Add(5 * time.Hour)) {
		t.Fatalf("RS5: missing 5h reset must estimate now+5h, got %v", d.ResumeAt)
	}
	if !strings.Contains(d.Reason, "estimated") {
		t.Fatalf("RS5: estimated resume must be marked in Reason: %q", d.Reason)
	}
}

// RS5: 7d without a known reset estimates now+24h (re-check daily, conservative).
func TestDenied7dWithoutResetGetsEstimatedResume(t *testing.T) {
	bs := []ledger.Bucket{bucket("7d", 99, time.Time{})}
	d := Decide(bs, "claude", now, Thresholds{80, 95})
	if d.Admit {
		t.Fatalf("99%% must deny: %+v", d)
	}
	if !d.ResumeAt.Equal(now.Add(24 * time.Hour)) {
		t.Fatalf("RS5: missing 7d reset must estimate now+24h, got %v", d.ResumeAt)
	}
	if !strings.Contains(d.Reason, "estimated") {
		t.Fatalf("RS5: estimated resume must be marked in Reason: %q", d.Reason)
	}
}

// Earliest effective resume governs when several windows are exhausted.
func TestEarliestResumeGoverns(t *testing.T) {
	bs := []ledger.Bucket{
		bucket("5h", 99, now.Add(3*time.Hour)),
		bucket("7d", 99, now.Add(48*time.Hour)),
	}
	d := Decide(bs, "claude", now, Thresholds{80, 95})
	if !d.ResumeAt.Equal(now.Add(3 * time.Hour)) {
		t.Fatalf("want earliest reset to govern, got %v", d.ResumeAt)
	}
}

// S2R-3: a bucket whose numbers are estimate-sourced (config-guess capacity ×
// degradation guess) may THROTTLE but never EXHAUST — denial semantics
// activate only after a REAL provider signal anchors the window (R4/R14: no
// artificial brakes; only real account consumption and provider stops gate).
func TestEstimateSourcedBucketThrottlesNeverExhausts(t *testing.T) {
	bs := []ledger.Bucket{{Lane: "codex", Window: ledger.Win5h, UsedPct: 121,
		Source: "shadow", CapSource: ledger.CapSourceEstimate, ResetsAt: now.Add(time.Hour)}}
	d := Decide(bs, "codex", now, Thresholds{80, 95})
	if !d.Admit || d.State != Throttled {
		t.Fatalf("estimate-sourced modeled exhaustion must throttle, never deny: %+v", d)
	}
	if !strings.Contains(d.Reason, "estimate") {
		t.Fatalf("the throttle reason must say WHY denial is withheld: %q", d.Reason)
	}
	// The same estimate-capped bucket after a real provider veto still denies.
	bs[0].Source = "provider"
	bs[0].UsedPct = 100
	if d := Decide(bs, "codex", now, Thresholds{80, 95}); d.Admit || d.State != Exhausted {
		t.Fatalf("a real provider signal must exhaust even an estimate-capped bucket: %+v", d)
	}
}

func TestOtherLaneIgnored(t *testing.T) {
	bs := []ledger.Bucket{bucket("7d", 99, now.Add(time.Hour))}
	if d := Decide(bs, "codex", now, Thresholds{80, 95}); !d.Admit || d.State != "open" {
		t.Fatalf("another lane's buckets must not gate this lane: %+v", d)
	}
}

// A bucket whose reset moment has passed is stale history — it must never
// gate admission (found live: the GLM 5h window stayed "exhausted" forever
// because nothing rolled the read path past resets_at).
func TestExpiredWindowNeverGates(t *testing.T) {
	now := time.Now().UTC()
	bs := []ledger.Bucket{{
		Lane: "glm", Window: ledger.Win5h, UsedPct: 100,
		ResetsAt: now.Add(-10 * time.Minute), Source: "provider",
	}}
	d := Decide(bs, "glm", now, Thresholds{80, 95})
	if !d.Admit || d.State != Open {
		t.Fatalf("expired window gated admission: %+v", d)
	}
}

// W2: one subject's exhaustion must never mask another's headroom.
func TestDecideSubjectIsolation(t *testing.T) {
	now := time.Now().UTC()
	reset := now.Add(2 * time.Hour)
	bs := []ledger.Bucket{
		{Lane: "claude", Window: ledger.Win5h, UsedPct: 97, ResetsAt: reset, Source: "provider", ObservedAt: now},                     // default: exhausted
		{Lane: "claude", Subject: "acct2", Window: ledger.Win5h, UsedPct: 5, ResetsAt: reset, Source: "provider", ObservedAt: now}, // acct2: wide open
	}
	th := Thresholds{ThrottlePct: 80, ExhaustPct: 95}
	if d := Decide(bs, "claude", now, th); d.State != Exhausted {
		t.Fatalf("default subject must read exhausted, got %+v", d)
	}
	if d := DecideSubject(bs, "claude", "acct2", now, th); d.State != Open {
		t.Fatalf("acct2 must read open (never masked by default's exhaustion), got %+v", d)
	}
}
