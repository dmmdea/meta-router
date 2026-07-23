package pace

import (
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

func bucket(w ledger.WindowKind, used float64, resetIn time.Duration, now time.Time) ledger.Bucket {
	return ledger.Bucket{Lane: "claude", Window: w, UsedPct: used, ResetsAt: now.Add(resetIn), Source: "provider", ObservedAt: now}
}

func TestSlackMidWindowOnPace(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	// 5h window, half elapsed (resets in 2.5h), 50% used → slack 0.
	b := bucket(ledger.Win5h, 50, 150*time.Minute, now)
	s, ok := Slack(b, now)
	if !ok || s < -0.001 || s > 0.001 {
		t.Fatalf("on-pace slack should be ~0, got %v ok=%v", s, ok)
	}
}

func TestSlackNegativeWhenBurningAhead(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	// half elapsed but 80% used → slack -0.30.
	b := bucket(ledger.Win5h, 80, 150*time.Minute, now)
	s, ok := Slack(b, now)
	if !ok || s > -0.29 || s < -0.31 {
		t.Fatalf("want ~-0.30, got %v ok=%v", s, ok)
	}
}

func TestSlackUnknownUsedPct(t *testing.T) {
	now := time.Now()
	b := bucket(ledger.Win5h, -1, time.Hour, now)
	if _, ok := Slack(b, now); ok {
		t.Fatal("unknown used_pct must not produce a slack")
	}
}

func TestSlackUnanchored(t *testing.T) {
	now := time.Now()
	b := ledger.Bucket{Lane: "claude", Window: ledger.Win5h, UsedPct: 10}
	if _, ok := Slack(b, now); ok {
		t.Fatal("unanchored bucket must not produce a slack")
	}
}

func TestSlackRolledWindow(t *testing.T) {
	now := time.Now()
	b := bucket(ledger.Win5h, 40, -time.Minute, now) // reset already passed
	if _, ok := Slack(b, now); ok {
		t.Fatal("rolled window must not produce a slack")
	}
}

func TestSlackClockSkewAnchorAhead(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	// Reset anchored further out than one window span: treat as window start.
	b := bucket(ledger.Win5h, 10, 6*time.Hour, now)
	s, ok := Slack(b, now)
	if !ok || s != -0.10 {
		t.Fatalf("anchor-ahead must clamp elapsed to 0 (slack -0.10), got %v ok=%v", s, ok)
	}
}

func TestBindingIsMin(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	bs := []ledger.Bucket{
		bucket(ledger.Win5h, 80, 150*time.Minute, now), // slack ~-0.30
		bucket(ledger.Win7d, 10, 84*time.Hour, now),    // half elapsed, 10% used → +0.40
	}
	s, ok := Binding(bs, now)
	if !ok || s > -0.29 || s < -0.31 {
		t.Fatalf("binding must be the min (~-0.30), got %v ok=%v", s, ok)
	}
}

func TestBindingNoneKnown(t *testing.T) {
	if _, ok := Binding([]ledger.Bucket{{Lane: "x", Window: ledger.Win5h, UsedPct: -1}}, time.Now()); ok {
		t.Fatal("no known windows → no binding slack")
	}
}

func TestDuration(t *testing.T) {
	if d, ok := Duration(ledger.Win5h); !ok || d != 5*time.Hour {
		t.Fatalf("5h: got %v ok=%v", d, ok)
	}
	if d, ok := Duration(ledger.Win7d); !ok || d != 7*24*time.Hour {
		t.Fatalf("7d: got %v ok=%v", d, ok)
	}
	if _, ok := Duration(ledger.WindowKind("bogus")); ok {
		t.Fatal("unknown window kind must be !ok")
	}
}
