package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// Free-provider lanes render their day / trial windows like any other lane,
// in the fixed lane order, and only when they carry a live number.
func TestQuotaHintRendersFreeLaneWindows(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("groq", ledger.WinDay, 40, ledger.NextDailyReset(hnow), hnow)
		l.SetCapacityEstimate("nim", ledger.WinTrial, 1000_000)
		l.AnchorIfUnset("nim", ledger.WinTrial, hnow.AddDate(10, 0, 0), hnow)
		l.AddShadow("nim", ledger.WinTrial, 120_000, hnow)
		l.ObserveProvider("openrouter", ledger.WinDay, 100, ledger.NextDailyReset(hnow), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	for _, want := range []string{"groq day 40%", "nim trial 12%", "openrouter day 100% EXHAUSTED"} {
		if !strings.Contains(h, want) {
			t.Fatalf("hint missing %q: %s", want, h)
		}
	}
	if strings.Contains(h, "cloudflare") || strings.Contains(h, "gemini") {
		t.Fatalf("lanes without a bucket must not render: %s", h)
	}
	if strings.Index(h, "groq") > strings.Index(h, "openrouter") || strings.Index(h, "openrouter") > strings.Index(h, "nim") {
		t.Fatalf("lane order must be the fixed hintLanes order: %s", h)
	}
	// An expired day window is history, not pressure.
	later := ledger.NextDailyReset(hnow).Add(time.Minute)
	if h2 := quotaHint(later); strings.Contains(h2, "openrouter day 100%") {
		t.Fatalf("an expired day window must not render as live pressure: %s", h2)
	}
}
