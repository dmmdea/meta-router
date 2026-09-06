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

// The metering unit is the VENDOR'S per-dispatch premium-request figure,
// floored at one request (recalibrated 2026-09-05: the one-per-dispatch unit
// let a gold probe spend the whole month while the ledger showed 60% left).
func TestCopilotMeteringUsesTheVendorFigureFlooredAtOne(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	cfg := orchcfg.Defaults()
	for _, tc := range []struct {
		name     string
		o        copilotlane.Outcome
		wantMill int64
	}{
		{"vendor says 1", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 1}}, 1000},
		{"vendor says 3 (luna-class)", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 3}}, 3000},
		{"vendor says 14", copilotlane.Outcome{Class: "ok", Usage: copilotlane.Usage{PremiumRequests: 14}}, 14000},
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
			t.Fatalf("%s: metered %d milli-requests, want %d (vendor figure, floored at one)", tc.name, b.ShadowTokens, tc.wantMill)
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
