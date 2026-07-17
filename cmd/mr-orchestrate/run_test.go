package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/claudelane"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/jitter"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

var tnow = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
var th = admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95}

func TestGateDeniedIsRelegation(t *testing.T) {
	bs := []ledger.Bucket{{Lane: "claude", Window: "7d", UsedPct: 97, ResetsAt: tnow.Add(48 * time.Hour), Source: "provider"}}
	g := gate(bs, "claude", "sonnet", fuses.Seed(), tnow, orchcfg.Defaults(), false, th)
	if g.Admit {
		t.Fatalf("97%% weekly must deny: %+v", g)
	}
	var d deferral
	if err := json.Unmarshal(deferralJSON(g), &d); err != nil {
		t.Fatal(err)
	}
	if !d.Deferred || d.ResumeAt == nil || !d.ResumeAt.Equal(tnow.Add(48*time.Hour)) {
		t.Fatalf("deferral must carry a valid resume_at: %+v", d)
	}
}

// RS7/R10: credits billing mode hard-stops the lane even with headroom —
// quota must never silently convert to dollar spend.
func TestGateCreditsModeHardStops(t *testing.T) {
	cfg := orchcfg.Config{ClaudeBillingMode: orchcfg.BillingCredits}
	g := gate(nil, "claude", "sonnet", fuses.Seed(), tnow, cfg, false, th)
	if g.Admit || g.State != "hard_stop" || !strings.Contains(g.Reason, "R10") {
		t.Fatalf("credits mode must hard-stop: %+v", g)
	}
}

// Fail-safe: a garbled billing mode (hand-edit typo) must hard-stop, not
// silently behave as the permissive subscription mode.
func TestGateUnknownBillingModeHardStops(t *testing.T) {
	cfg := orchcfg.Config{ClaudeBillingMode: "Credits"}
	g := gate(nil, "claude", "sonnet", fuses.Seed(), tnow, cfg, false, th)
	if g.Admit || g.State != "hard_stop" {
		t.Fatalf("unknown billing mode must fail safe: %+v", g)
	}
}

// R11: the operator outranks the ledger.
func TestGateForceOverridesExhaustion(t *testing.T) {
	bs := []ledger.Bucket{{Lane: "claude", Window: "7d", UsedPct: 99, ResetsAt: tnow.Add(time.Hour), Source: "provider"}}
	g := gate(bs, "claude", "sonnet", fuses.Seed(), tnow, orchcfg.Defaults(), true, th)
	if !g.Admit || !g.Forced || !strings.Contains(g.Reason, "FORCED") {
		t.Fatalf("--force must admit with a loud reason: %+v", g)
	}
}

// A2R-#1 (INVERTED — the old test asserted the WRONG behavior): the billing-
// mode hard-stop is FORCE-PROOF. R10: --force must NEVER convert quota into
// real dollar spend — the ONLY place --force yields is the GLM 1313 latch.
// A credits billing mode denies UNCONDITIONALLY, even under --force.
func TestGateForceDoesNotBypassCreditsHardStop(t *testing.T) {
	cfg := orchcfg.Config{ClaudeBillingMode: orchcfg.BillingCredits}
	g := gate(nil, "claude", "sonnet", fuses.Seed(), tnow, cfg, true, th)
	if g.Admit {
		t.Fatalf("--force must NOT bypass the credits billing hard-stop (R10, real spend): %+v", g)
	}
	if g.State != "hard_stop" {
		t.Fatalf("forced credits mode must stay hard_stop: %+v", g)
	}
	if g.Forced {
		t.Fatalf("a force-proof hard-stop must not be marked Forced (it was not overridden): %+v", g)
	}
}

// A2R-#1: a garbled billing mode (hand-edit typo "Credits") is also force-
// proof — fail-safe: an unrecognized mode must never become the permissive
// subscription mode, and --force cannot lift it either.
func TestGateForceDoesNotBypassGarbledBillingMode(t *testing.T) {
	cfg := orchcfg.Config{ClaudeBillingMode: "Credits"} // typo, not the canonical constant
	g := gate(nil, "claude", "sonnet", fuses.Seed(), tnow, cfg, true, th)
	if g.Admit {
		t.Fatalf("--force must NOT bypass a garbled billing mode (fail-safe, R10): %+v", g)
	}
	if g.State != "hard_stop" {
		t.Fatalf("garbled billing mode under force must stay hard_stop: %+v", g)
	}
}

// F13 e2e: a live rate_limit outcome must leave the lane exhausted with a
// valid resume — the NEXT invocation's gate defers instead of hammering.
func TestApplyRunOutcomeRateLimitExhaustsLane(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // applyRunOutcome reads dropPath(); isolate from real state
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyRunOutcome(l, claudelane.Outcome{Class: "rate_limit"}, tnow)
	}); err != nil {
		t.Fatal(err)
	}
	g := gate(ledger.Open(p).Snapshot(), "claude", "sonnet", fuses.Seed(), tnow.Add(time.Minute), orchcfg.Defaults(), false, th)
	if g.Admit {
		t.Fatalf("lane must be exhausted after a live 429: %+v", g)
	}
	if !g.ResumeAt.Equal(tnow.Add(5 * time.Hour)) {
		t.Fatalf("429 exhaustion must carry the RS5 conservative resume, got %v", g.ResumeAt)
	}
}

func TestApplyRunOutcomeOKAddsShadowBothWindows(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	p := filepath.Join(t.TempDir(), "ledger.json")
	o := claudelane.Outcome{Class: "ok", ModelUsage: map[string]claudelane.ModelUse{
		"claude-sonnet-5": {InputTokens: 1000, OutputTokens: 50},
	}}
	if err := ledger.Update(p, func(l *ledger.Ledger) { applyRunOutcome(l, o, tnow) }); err != nil {
		t.Fatal(err)
	}
	l := ledger.Open(p)
	for _, w := range []ledger.WindowKind{ledger.Win5h, ledger.Win7d} {
		if b, ok := l.Bucket("claude", w); !ok || b.ShadowTokens != 1050 {
			t.Fatalf("%s window must carry 1050 shadow tokens: %+v", w, b)
		}
	}
}

// R10: fable exits the runtime lane set when the carve-out fuse expires —
// the fuse note is a runtime rule, not display copy.
func TestGateFableDeniedAfterCarveoutExpiry(t *testing.T) {
	after := time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	g := gate(nil, "claude", "claude-fable-5", fuses.Seed(), after, orchcfg.Defaults(), false, th)
	if g.Admit || g.State != "model_retired" {
		t.Fatalf("fable post-7/7 must be denied: %+v", g)
	}
	before := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if g := gate(nil, "claude", "claude-fable-5", fuses.Seed(), before, orchcfg.Defaults(), false, th); !g.Admit {
		t.Fatalf("fable pre-7/7 (carve-out active) must admit: %+v", g)
	}
	if g := gate(nil, "claude", "sonnet", fuses.Seed(), after, orchcfg.Defaults(), false, th); !g.Admit {
		t.Fatalf("non-fable models unaffected by the fable fuse: %+v", g)
	}
}

// F4 (S3R-8 fail-safe inversion): the post-run exit code must check the OUTCOME
// FAILURE first. A genuinely-failed-but-costly claude call (Class != "ok" AND
// NotionalUSD > max) must return exit-5 (not_ok), NEVER exit-4 (ok_notional) —
// exit-4 is kindOK downstream and would launder a hard failure into a success
// that satisfies deps and can finalize a failed dispatch as done. The notional
// guard (exit-4) fires ONLY for a SUCCESSFUL call that was costly.
func TestClaudeExitCodeFailedCostlyIsNotOK(t *testing.T) {
	cases := []struct {
		name        string
		class       string
		notional    float64
		maxNotional float64
		want        int
	}{
		// The bug case: failed AND over the cap → not_ok, never notional.
		{"failed-and-costly is not_ok, not notional", "api_error", 5.0, 2.0, exitNotOK},
		{"refusal-and-costly is not_ok", "refusal", 9.0, 2.0, exitNotOK},
		// A successful but costly call → the notional warning (exit-4).
		{"ok-and-costly is notional", "ok", 5.0, 2.0, exitNotional},
		// Ordinary success under the cap → clean.
		{"ok-under-cap is clean", "ok", 0.5, 2.0, 0},
		// A failure under the cap → not_ok (the pre-existing behavior).
		{"failed-under-cap is not_ok", "api_error", 0.5, 2.0, exitNotOK},
	}
	for _, c := range cases {
		if got := claudeExitCode(c.class, c.notional, c.maxNotional); got != c.want {
			t.Errorf("%s: claudeExitCode(%q, %.2f, %.2f) = %d, want %d",
				c.name, c.class, c.notional, c.maxNotional, got, c.want)
		}
	}
}

// E4: a deferral carries the truthful resume_at AND the jittered retry_at.
func TestDeferralJSONCarriesJitteredRetryAt(t *testing.T) {
	resume := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	b := deferralJSON(gateResult{ResumeAt: resume, Reason: "test"})
	var d struct {
		ResumeAt *time.Time `json:"resume_at"`
		RetryAt  *time.Time `json:"retry_at"`
	}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	if d.ResumeAt == nil || !d.ResumeAt.Equal(resume) {
		t.Fatalf("resume_at must stay truthful, got %v", d.ResumeAt)
	}
	if d.RetryAt == nil || d.RetryAt.Before(resume) || !d.RetryAt.Before(resume.Add(jitter.DefaultWindow)) {
		t.Fatalf("retry_at must be jittered into [resume, resume+window), got %v", d.RetryAt)
	}
}
