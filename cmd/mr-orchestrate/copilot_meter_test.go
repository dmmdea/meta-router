package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/copilotlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

// The metering unit is the VENDOR'S per-dispatch figure — AI CREDITS on the
// token-based plan (verified 2026-09-06 against copilot_internal/user and the
// billing report) — floored at one credit, against the configured monthly
// credit allowance until a poll lands the measured entitlement.
func TestCopilotMeteringUsesTheVendorFigureFlooredAtOne(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cfg := orchcfg.Defaults()
	for _, tc := range []struct {
		name     string
		o        copilotlane.Outcome
		wantMill int64
	}{
		{"vendor says 1 credit", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 1}}, 1000},
		{"vendor says 3 credits (luna-class)", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 3}}, 3000},
		{"vendor says 14 credits (gemini-flash)", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 14}}, 14000},
		{"ok with no checkpoint still meters one", copilotlane.Outcome{Class: "ok"}, 1000},
		{"not-ok but vendor charged", copilotlane.Outcome{Class: "incomplete", Usage: copilotlane.Usage{PremiumRequests: 2}}, 2000},
	} {
		l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
		applyCopilotOutcome(l, tc.o, cfg, now)
		b, ok := l.Bucket("copilot", ledger.WinMonth)
		if !ok {
			t.Fatalf("%s: no monthly bucket", tc.name)
		}
		if b.ShadowTokens != tc.wantMill {
			t.Fatalf("%s: metered %d milli-credits, want %d (vendor figure, floored at one credit)", tc.name, b.ShadowTokens, tc.wantMill)
		}
		if b.CapTokens != 1500*1000 || b.CapTokens != cfg.CopilotMonthlyCredits*1000 {
			t.Fatalf("%s: cap = %d, want the 1,500-credit Pro allowance as milli-credits (estimate until a poll lands)", tc.name, b.CapTokens)
		}
		if b.CapSource != ledger.CapSourceEstimate {
			t.Fatalf("%s: a config allowance must be marked estimate (throttle-only) until the poll measures it, got %q", tc.name, b.CapSource)
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

// The default pin is a measured 1x model; `auto` (which the vendor resolved to
// a ~3x model on 78% of a 168-cell probe) is opt-in only.
func TestCopilotDefaultModelIsOneXNotAuto(t *testing.T) {
	if orchcfg.Defaults().CopilotModel != "gpt-5.6-terra" || orchcfg.CopilotDefaultModel == "auto" {
		t.Fatalf("default copilot model must be the measured 1x pin, got %q", orchcfg.Defaults().CopilotModel)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := orchcfg.Load(p); c.CopilotModel != orchcfg.CopilotDefaultModel {
		t.Fatalf("empty config must normalize to the 1x default, got %q", c.CopilotModel)
	}
}
