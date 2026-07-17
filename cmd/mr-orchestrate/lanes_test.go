package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/claudelane"
	"github.com/dmmdea/meta-router/internal/orch/codexlane"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/glmlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
)

var fixtureUsage = codexlane.Usage{Input: 18487, CachedInput: 18176, Output: 5} // 4047 millicredits @ ×15

// S2R-3 wiring: the codex 5h cap is a CONFIG GUESS (Plus band × degradation
// factor) — modeled exhaustion may only ever THROTTLE. Denial semantics need
// a real provider signal.
func TestApplyCodexOutcomeModeledExhaustionOnlyThrottles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		for i := 0; i < 12; i++ { // 12×4047 ≈ 48.6k > the 40k millicredit cap
			applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, orchcfg.Defaults(), tnow)
		}
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("codex", ledger.Win5h)
	if b.CapTokens != 40_000 || b.CapSource != ledger.CapSourceEstimate {
		t.Fatalf("5h cap must be the config estimate, marked as such: %+v", b)
	}
	if b.UsedPct < 95 {
		t.Fatalf("test premise: modeled usage must cross the exhaust threshold, got %v", b.UsedPct)
	}
	g := laneGate(ledger.Open(p).Snapshot(), "codex", tnow.Add(time.Minute), th, false)
	if !g.Admit || g.State != "throttled" {
		t.Fatalf("S2R-3: estimate-sourced modeled exhaustion must THROTTLE, never deny: %+v", g)
	}
}

// A real provider veto (429 / turn.failed → rate_limit) exhausts the lane
// with the RS5 conservative resume — the NEXT invocation defers.
func TestApplyCodexOutcomeRateLimitExhaustsLane(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyCodexOutcome(l, codexlane.Outcome{Class: "rate_limit"}, orchcfg.Defaults(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	g := laneGate(ledger.Open(p).Snapshot(), "codex", tnow.Add(time.Minute), th, false)
	if g.Admit {
		t.Fatalf("lane must be exhausted after a real rate limit: %+v", g)
	}
	if !g.ResumeAt.Equal(tnow.Add(5 * time.Hour)) {
		t.Fatalf("RS5 conservative resume expected, got %v", g.ResumeAt)
	}
}

// The 7d window anchors at firstUse+7d but stays UNCAPPED (no weekly band is
// known for codex) — shadow accumulates for fitting, percentage stays -1.
func TestApplyCodexOutcomeAnchors7dUncapped(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyCodexOutcome(l, codexlane.Outcome{Class: "ok", Usage: fixtureUsage}, orchcfg.Defaults(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	b, ok := ledger.Open(p).Bucket("codex", ledger.Win7d)
	if !ok || !b.ResetsAt.Equal(tnow.Add(7*24*time.Hour)) {
		t.Fatalf("7d must anchor at firstUse+7d: %+v", b)
	}
	if b.UsedPct != -1 || b.ShadowTokens != 4047 {
		t.Fatalf("7d stays -1 (uncapped) while shadow accumulates: %+v", b)
	}
}

// glmFixtureOutcome parses the COMMITTED live glm-5.2 capture (13k+ tokens) —
// the raw material for the S2R-2 unit-invariant tests.
func glmFixtureOutcome(t *testing.T) (claudelane.Outcome, []byte) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "glm", "result-glm-5.2.json"))
	if err != nil {
		t.Fatalf("glm fixture missing: %v", err)
	}
	o := claudelane.Parse(b)
	if o.Class != "ok" || o.TotalTokens() < 10_000 {
		t.Fatalf("fixture premise: a real 13k-token capture, got %+v", o)
	}
	return o, b
}

// S2R-2: GLM ledger units are PROMPTS from the FIRST dispatch — the fixture
// outcome carries 13k+ real tokens, and NONE of them may token-scale into the
// glm buckets. tnow is 12:00 UTC (off-peak) with the seeded glm-offpeak-promo
// fuse active ⇒ 1 unit per prompt.
func TestApplyGLMOutcomeMetersPromptUnitsNotTokens(t *testing.T) {
	o, raw := glmFixtureOutcome(t)
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
		applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []ledger.WindowKind{ledger.Win5h, ledger.Win7d} {
		b, ok := ledger.Open(p).Bucket("glm", w)
		if !ok || b.ShadowTokens != 2 {
			t.Fatalf("glm/%s must meter 2 PROMPT units, never 2×13k tokens (S2R-2): %+v", w, b)
		}
	}
}

// R11 on the non-claude gate: --force outranks the ledger, loudly.
func TestLaneGateForceOverridesWithAudit(t *testing.T) {
	bs := []ledger.Bucket{{Lane: "codex", Window: ledger.Win5h, UsedPct: 100,
		Source: "provider", ResetsAt: tnow.Add(time.Hour)}}
	g := laneGate(bs, "codex", tnow, th, true)
	if !g.Admit || !g.Forced || !strings.Contains(g.Reason, "FORCED") {
		t.Fatalf("--force must admit with a loud audit trail: %+v", g)
	}
}

// Task 7: one PEAK glm-5.2 prompt (07:00 UTC = 15:00 UTC+8) meters 3 units
// against the config-seeded caps: 5h cap 80, weekly cap 5×5h = 400
// (NEVER 10×), weekly anchored at firstUse+7d.
func TestApplyGLMOutcomeSeedsCapsAndMeters(t *testing.T) {
	tpeak := time.Date(2026, 7, 6, 7, 0, 0, 0, time.UTC)
	o, raw := glmFixtureOutcome(t)
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tpeak)
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("glm", ledger.Win5h)
	if b.ShadowTokens != 3 || b.CapTokens != 80 || b.UsedPct != 3.75 {
		t.Fatalf("peak prompt: 3 units / cap 80 / 3.75%%: %+v", b)
	}
	b7, _ := ledger.Open(p).Bucket("glm", ledger.Win7d)
	if b7.CapTokens != 400 || !b7.ResetsAt.Equal(tpeak.Add(7*24*time.Hour)) || b7.ShadowTokens != 3 {
		t.Fatalf("weekly: cap 400 (5×5h), anchored firstUse+7d, metered: %+v", b7)
	}
}

// S2R-2 migration: a pre-existing glm bucket with TOKEN-scale shadow (pre-
// unit-convention state) is cleared exactly once, at first capacity seeding —
// otherwise 13k "prompts" against an 80-prompt cap bricks the lane on run one.
func TestApplyGLMOutcomeMigratesTokenScaleState(t *testing.T) {
	o, raw := glmFixtureOutcome(t)
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		l.AddShadow("glm", ledger.Win5h, 13_061, tnow.Add(-time.Hour)) // token-scale contamination
		l.AddShadow("glm", ledger.Win7d, 13_061, tnow.Add(-time.Hour))
		applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		w   ledger.WindowKind
		cap int64
	}{{ledger.Win5h, 80}, {ledger.Win7d, 400}} {
		b, _ := ledger.Open(p).Bucket("glm", tc.w)
		if b.ShadowTokens != 1 || b.CapTokens != tc.cap {
			t.Fatalf("glm/%s must clear token-scale state then meter 1 unit against cap %d: %+v", tc.w, tc.cap, b)
		}
	}
}

// S2R-2 real-path semantics: the invariant must hold through the SAME
// statepaths-resolved paths and cross-process ledger.Update transaction the
// binary uses — not only hand-built temp ledgers.
func TestGLMUnitInvariantRealPath(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	o, raw := glmFixtureOutcome(t)
	for i := 0; i < 2; i++ {
		if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
			applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Load(configPath()), fuses.Seed(), tnow)
		}); err != nil {
			t.Fatal(err)
		}
	}
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		t.Fatal(warn)
	}
	b, ok := l.Bucket("glm", ledger.Win5h)
	if !ok || b.ShadowTokens != 2 || b.CapTokens != 80 || b.UsedPct != 2.5 {
		t.Fatalf("real-path glm/5h must read 2 PROMPT units (2.5%% of 80) — the fixture's 13k tokens must be nowhere: %+v", b)
	}
	if b7, _ := l.Bucket("glm", ledger.Win7d); b7.ShadowTokens != 2 || b7.CapTokens != 400 || b7.ResetsAt.IsZero() {
		t.Fatalf("real-path glm/7d: 2 units, cap 400, anchored: %+v", b7)
	}
}

// 1313 latch: the ONE place --force yields (an account-loss event; R11
// override still exists via ack-then-run, a deliberate two-step). Ack clears.
func TestGLMGateDeniesWhileLatchedEvenForced(t *testing.T) {
	alert := filepath.Join(t.TempDir(), "glm-alert.json")
	if err := glmlane.LatchAlert(alert, glmlane.GLMErr{Code: 1313, Action: glmlane.ActHardStop}, tnow); err != nil {
		t.Fatal(err)
	}
	g := glmGate(nil, alert, tnow, th, true) // force=true — must NOT bypass
	if g.Admit || g.State != "hard_stop" || !strings.Contains(g.Reason, "ack-glm") {
		t.Fatalf("latched 1313 must hard-stop past --force, naming the ack: %+v", g)
	}
	if err := os.Remove(alert); err != nil { // the ack
		t.Fatal(err)
	}
	if g := glmGate(nil, alert, tnow, th, false); !g.Admit {
		t.Fatalf("ack must clear the latch: %+v", g)
	}
}

// Cooldown (1308/1316): the 5h window observes 100%% with the embedded
// next_flush_time as resume — the NEXT run defers with a valid resume.
func TestApplyGLMOutcomeCooldownDefersNextRun(t *testing.T) {
	flush := tnow.Add(2 * time.Hour)
	raw := []byte(`{"type":"result","is_error":true,"api_error_status":429,"result":"{\"error\":{\"code\":1308,\"message\":\"5h exhausted\",\"next_flush_time\":` + fmt.Sprint(flush.Unix()) + `}}"}`)
	o := claudelane.Parse(raw)
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	g := glmGate(ledger.Open(p).Snapshot(), filepath.Join(t.TempDir(), "none.json"), tnow.Add(time.Minute), th, false)
	if g.Admit || !g.ResumeAt.Equal(flush) {
		t.Fatalf("cooldown must deny the NEXT run with the provider flush as resume: %+v", g)
	}
}

// A STALE next_flush_time (already past) must not anchor a cooldown that
// instantly rolls — RS5 conservative estimate instead (staleness guard lives
// here at the observation site; the classifier extracts verbatim).
func TestApplyGLMOutcomeStaleFlushGetsRS5Estimate(t *testing.T) {
	raw := []byte(`{"error":{"code":1316,"next_flush_time":` + fmt.Sprint(tnow.Add(-time.Hour).Unix()) + `}}`)
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyGLMOutcome(l, claudelane.Outcome{Class: "rate_limit"}, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("glm", ledger.Win5h)
	if b.UsedPct != 100 || b.Source != "provider" || !b.ResetsAt.Equal(tnow.Add(5*time.Hour)) {
		t.Fatalf("stale flush ⇒ RS5 now+5h resume: %+v", b)
	}
}

// Offline class (1310/1317–1321): weekly window observes 100%% with the RS5
// conservative daily re-check when no flush is embedded.
func TestApplyGLMOutcomeOfflineObserves7d(t *testing.T) {
	raw := []byte(`{"error":{"code":1310}}`)
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyGLMOutcome(l, claudelane.Outcome{Class: "api_error"}, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("glm", ledger.Win7d)
	if b.UsedPct != 100 || b.Source != "provider" || !b.ResetsAt.Equal(tnow.Add(24*time.Hour)) {
		t.Fatalf("offline class ⇒ 7d exhausted, re-checked daily (RS5): %+v", b)
	}
}

// S2R-8: a GLM rate_limit with NO parseable 13xx body code gets the generic
// claude-style treatment (5h → 100%% + RS5 conservative resume) so the next
// invocation defers instead of re-hammering.
func TestApplyGLMOutcomeGenericRateLimitFallback(t *testing.T) {
	raw := []byte(`{"type":"result","is_error":true,"api_error_status":429,"result":""}`)
	o := claudelane.Parse(raw)
	if o.Class != "rate_limit" {
		t.Fatalf("test premise: outer 429 classifies rate_limit, got %q", o.Class)
	}
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		if _, classified := applyGLMOutcome(l, o, raw, "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow); classified {
			t.Fatal("no 13xx code present — must not classify")
		}
	}); err != nil {
		t.Fatal(err)
	}
	b, _ := ledger.Open(p).Bucket("glm", ledger.Win5h)
	if b.UsedPct != 100 || b.Source != "provider" || !b.ResetsAt.Equal(tnow.Add(5*time.Hour)) {
		t.Fatalf("S2R-8 generic fallback must exhaust 5h with RS5 resume: %+v", b)
	}
}

// 1313 surfaces to the caller for latching; retry/unknown classes write NO
// ledger observation (fail-open).
func TestApplyGLMOutcomeHardStopAndNoOpClasses(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		e, classified := applyGLMOutcome(l, claudelane.Outcome{Class: "api_error"},
			[]byte(`{"error":{"code":1313,"message":"fair usage"}}`), "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
		if !classified || e.Action != glmlane.ActHardStop {
			t.Fatalf("1313 must surface for the latch: %+v %v", e, classified)
		}
		for _, body := range []string{`{"error":{"code":1302}}`, `{"error":{"code":1399}}`} {
			applyGLMOutcome(l, claudelane.Outcome{Class: "api_error"}, []byte(body), "glm-5.2", orchcfg.Defaults(), fuses.Seed(), tnow)
		}
	}); err != nil {
		t.Fatal(err)
	}
	for _, w := range []ledger.WindowKind{ledger.Win5h, ledger.Win7d} {
		if b, _ := ledger.Open(p).Bucket("glm", w); b.Source == "provider" {
			t.Fatalf("retry/unknown/hard-stop classes must not fabricate provider observations on %s: %+v", w, b)
		}
	}
}

// A2R-#3: the 1313 latch is written from `raw` ALONE — it must be lock-
// independent of ledger.Update. latchGLMHardStop is the pure seam; a 1313 body
// latches, a non-1313 body does not.
func TestLatchGLMHardStopFromRawAlone(t *testing.T) {
	dir := t.TempDir()
	alert := filepath.Join(dir, "glm-alert.json")
	// A 1313 body latches, independent of any ledger.
	if !latchGLMHardStop([]byte(`{"error":{"code":1313,"message":"fair usage"}}`), alert, tnow) {
		t.Fatal("a 1313 raw body must latch")
	}
	if note, latched := glmlane.Latched(alert); !latched || !strings.Contains(note, "1313") {
		t.Fatalf("glm-alert.json must be written from raw alone: %q %v", note, latched)
	}
	// A non-1313 body (retry 1302) must NOT latch.
	alert2 := filepath.Join(t.TempDir(), "glm-alert.json")
	if latchGLMHardStop([]byte(`{"error":{"code":1302}}`), alert2, tnow) {
		t.Fatal("a non-1313 body must not latch")
	}
	if _, latched := glmlane.Latched(alert2); latched {
		t.Fatal("no latch file must exist for a non-hard-stop code")
	}
}

// A2R-#3 (the real defect): the 1313 latch must land even when ledger.Update
// FAILS. We simulate an unusable ledger by pointing its path at a DIRECTORY
// (Update's write fails), drive the latch from raw independently, and assert
// the alert is still written. Under the old code the latch decision lived
// INSIDE the Update closure, so a failed/lock-busy Update silently dropped the
// strike. The lock-independent path proves it no longer can.
func TestGLMLatchSurvivesLedgerUpdateFailure(t *testing.T) {
	dir := t.TempDir()
	// Ledger path is a directory → ledger.Update cannot write it.
	ledgerDir := filepath.Join(dir, "ledger.json")
	if err := os.Mkdir(ledgerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	updErr := ledger.Update(ledgerDir, func(l *ledger.Ledger) {})
	if updErr == nil {
		t.Skip("ledger.Update unexpectedly succeeded writing over a directory on this platform")
	}
	// The latch write is a SEPARATE path and must succeed regardless.
	alert := filepath.Join(dir, "glm-alert.json")
	raw := []byte(`{"error":{"code":1313,"message":"fair usage"}}`)
	if !latchGLMHardStop(raw, alert, tnow) {
		t.Fatal("the 1313 latch must fire independent of the failed ledger.Update")
	}
	if _, latched := glmlane.Latched(alert); !latched {
		t.Fatal("glm-alert.json must be written even though ledger.Update failed (strike must not be dropped)")
	}
}
