package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/copilotlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

// The metering unit is ONE DISPATCH = ONE REQUEST, deliberately, because the
// vendor's totalPremiumRequests field has an unverified unit (measured
// 2026-09-01: 1 for claude-sonnet-5 and gpt-5.6-terra, 14 for
// gemini-3.6-flash on reproducible fresh sessions, inversely to nano-AIU).
// Taking 14 at face value would mask a lane that still has capacity — an
// artificial brake built on an unverified number (R14).
func TestCopilotMeteringIsOneRequestPerDispatch(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cfg := orchcfg.Defaults()
	for _, tc := range []struct {
		name     string
		o        copilotlane.Outcome
		wantMill int64
	}{
		{"vendor says 1", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 1}}, 1000},
		{"vendor says 14", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 14}}, 1000},
		{"ok with no checkpoint still meters", copilotlane.Outcome{Class: "ok"}, 1000},
	} {
		l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
		applyCopilotOutcome(l, tc.o, cfg, now)
		b, ok := l.Bucket("copilot", ledger.WinMonth)
		if !ok {
			t.Fatalf("%s: no monthly bucket", tc.name)
		}
		if b.ShadowTokens != tc.wantMill {
			t.Fatalf("%s: metered %d milli-requests, want %d (one dispatch = one request)", tc.name, b.ShadowTokens, tc.wantMill)
		}
		if b.CapTokens != cfg.CopilotMonthlyRequests*1000 {
			t.Fatalf("%s: cap = %d, want the configured allowance", tc.name, b.CapTokens)
		}
		if want := ledger.NextMonthlyReset(now); !b.ResetsAt.Equal(want) {
			t.Fatalf("%s: resets %s, want the calendar boundary %s", tc.name, b.ResetsAt, want)
		}
	}
}

// A rate-limit outcome records a provider-observed limit at the calendar
// reset — premium allowances have no rolling recovery.
func TestCopilotRateLimitObservesTheCalendarReset(t *testing.T) {
	now := time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	applyCopilotOutcome(l, copilotlane.Outcome{Class: "rate_limit"}, orchcfg.Defaults(), now)
	b, ok := l.Bucket("copilot", ledger.WinMonth)
	if !ok || !b.ResetsAt.Equal(ledger.NextMonthlyReset(now)) {
		t.Fatalf("rate_limit must anchor the calendar reset: %+v ok=%v", b, ok)
	}
}
