package admission

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// RS5 for the free-provider windows: an exhausted DAY window with no anchor
// resumes at the next 00:00 UTC (the reset is knowable); an exhausted TRIAL
// pool resumes in 24h (no reset exists; re-check daily).
func TestDeniedDayAndTrialWithoutResetGetEstimatedResume(t *testing.T) {
	day := ledger.Bucket{Lane: "groq", Window: ledger.WinDay, UsedPct: 99, Source: "provider"}
	d := Decide([]ledger.Bucket{day}, "groq", now, Thresholds{80, 95})
	if d.Admit || !d.ResumeAt.Equal(ledger.NextDailyReset(now)) || !strings.Contains(d.Reason, "estimated") {
		t.Fatalf("day: %+v", d)
	}
	trial := ledger.Bucket{Lane: "nim", Window: ledger.WinTrial, UsedPct: 100, Source: "provider"}
	d = Decide([]ledger.Bucket{trial}, "nim", now, Thresholds{80, 95})
	if d.Admit || !d.ResumeAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("trial: %+v", d)
	}
}

// An estimate-sourced day window (config cap, no vendor signal) throttles but
// never exhausts — the free lanes' caps are priors (S2R-3).
func TestEstimateSourcedDayWindowThrottlesOnly(t *testing.T) {
	b := ledger.Bucket{Lane: "cloudflare", Window: ledger.WinDay, UsedPct: 99, Source: "shadow",
		CapSource: ledger.CapSourceEstimate, ResetsAt: ledger.NextDailyReset(now)}
	d := Decide([]ledger.Bucket{b}, "cloudflare", now, Thresholds{80, 95})
	if !d.Admit || d.State != Throttled {
		t.Fatalf("estimate-sourced 99%% must throttle, not deny: %+v", d)
	}
}
