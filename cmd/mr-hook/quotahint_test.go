package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/glmlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

var hnow = time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)

// TestQuotaHintFromLedgerFile: a seeded ledger with claude 7d at 85% (provider)
// + codex 5h at 9% produces a hint carrying the percentages, THROTTLED for the
// ≥80 window, and the route pointer.
func TestQuotaHintFromLedgerFile(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win7d, 85, hnow.Add(48*time.Hour), hnow) // throttle row
		l.ObserveProvider("claude", ledger.Win5h, 42, hnow.Add(3*time.Hour), hnow)
		l.ObserveProvider("codex", ledger.Win5h, 9, hnow.Add(2*time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if h == "" {
		t.Fatal("expected a hint with signal, got empty")
	}
	for _, want := range []string{"85%", "42%", "9%", "THROTTLED", "mr-orchestrate route"} {
		if !strings.Contains(h, want) {
			t.Fatalf("hint missing %q: %s", want, h)
		}
	}
	// claude worst window is 85 (≥80) → THROTTLED; codex 9 → not throttled.
	if !strings.Contains(h, "claude") || !strings.Contains(h, "codex") {
		t.Fatalf("hint must name lane-state rows: %s", h)
	}
}

// TestQuotaHintFailsSilent: no state dir → "" (fail-open absolute); a corrupt
// ledger → "".
func TestQuotaHintFailsSilent(t *testing.T) {
	// No state dir / no ledger file.
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if h := quotaHint(hnow); h != "" {
		t.Fatalf("no ledger file must yield empty hint (fail-open): %q", h)
	}
	// Corrupt ledger.
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	if err := os.MkdirAll(filepath.Dir(statepaths.Ledger()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statepaths.Ledger(), []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if h := quotaHint(hnow); h != "" {
		t.Fatalf("corrupt ledger must yield empty hint (fail-open): %q", h)
	}
}

// TestQuotaHintNeverNamesALane: the hint reports STATE and points at the oracle
// but NEVER names a rank-table model (policy-freedom pin, §6c). No model tokens
// beyond the lane-state words {claude, codex, glm}.
func TestQuotaHintNeverNamesALane(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 50, hnow.Add(3*time.Hour), hnow)
		l.ObserveProvider("codex", ledger.Win5h, 88, hnow.Add(2*time.Hour), hnow)
		l.ObserveProvider("glm", ledger.Win5h, 20, hnow.Add(time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if h == "" {
		t.Fatal("expected a hint")
	}
	for _, forbidden := range []string{"opus", "sonnet", "gpt-5.5", "gpt-5", "glm-5.2", "glm-4.7", "qwythos", "gemma", "haiku"} {
		if strings.Contains(strings.ToLower(h), forbidden) {
			t.Fatalf("hint names a model %q (policy leak): %s", forbidden, h)
		}
	}
}

// GLM 1313 latch renders a HARD-STOP marker, not a percentage.
func TestQuotaHintGLMHardStop(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 30, hnow.Add(3*time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	if err := glmlane.LatchAlert(statepaths.GLMAlert(), glmlane.GLMErr{Code: 1313, Action: glmlane.ActHardStop}, hnow); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if !strings.Contains(h, "HARD-STOP(1313)") {
		t.Fatalf("latched glm must render HARD-STOP(1313): %s", h)
	}
}

// A2R-#11: the quota hint is now computed INSIDE the hook's deadline-bounded
// goroutine (it used to run AFTER the select resolved, outside the configured (default 300ms)
// budget). This pins the property that made the move safe: on a seeded ledger
// the hint is (a) unchanged in content and (b) computed FAR under the configured (default 300ms)
// hook budget, so folding it inside the deadline cannot blow it.
func TestQuotaHintFitsWithinHookDeadlineBudget(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win7d, 85, hnow.Add(48*time.Hour), hnow)
		l.ObserveProvider("codex", ledger.Win5h, 9, hnow.Add(2*time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	// Content unchanged (same seeded case as TestQuotaHintFromLedgerFile).
	start := time.Now()
	h := quotaHint(hnow)
	elapsed := time.Since(start)
	if h == "" || !strings.Contains(h, "85%") || !strings.Contains(h, "mr-orchestrate route") {
		t.Fatalf("moving the hint must not change its output: %q", h)
	}
	// A file read is microseconds; a comfortable margin under even the default 300ms hook
	// deadline proves the hint fits inside the bounded goroutine.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("quotaHint took %v — too slow to fold inside the hook deadline (300ms binary default; production settings.json runs -timeout-ms 1000)", elapsed)
	}
}

// A bucket with UsedPct == -1 (unknown) renders '?' not a bogus number.
func TestQuotaHintUnknownRendersQuestionMark(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		// PARTIAL knowledge within ONE lane is the case where "?" carries
		// meaning: a known 5h next to an unlearned 7d. (Re-pointed 2026-07-25:
		// this test previously got its "?" from a lane whose ONLY window was
		// unknown — and such a lane is now omitted entirely, because a row of
		// bare "?" marks is an absent signal, and the hook's fail-open is
		// absolute. The intent — unknown windows render "?" — is unchanged.)
		l.ObserveProvider("claude", ledger.Win5h, 30, hnow.Add(3*time.Hour), hnow)
		l.AddShadow("claude", ledger.Win7d, 0, hnow) // stays -1 (uncapped/unanchored)
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if !strings.Contains(h, "5h 30%") {
		t.Fatalf("the known window must render its number: %s", h)
	}
	if !strings.Contains(h, "7d ?") {
		t.Fatalf("an unknown window beside a known one must render '?': %s", h)
	}
}

// REGRESSION (audit 2026-07-25): the banner rendered an EXPIRED window's
// percentage as live pressure. Live Qube state on that day was claude 5h 85%
// with resets_at 25h in the past and 7d 17% live: the hook printed
// "claude 5h 85% · 7d 17% THROTTLED" while `route` (which does guard expiry)
// saw an open lane. An expired window must render "?" and must not contribute
// a state word.
func TestQuotaHintExpiredWindowIsNotLivePressure(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		// 5h: dead 25h ago at 85% (the poisoning row). 7d: live at 17%.
		l.ObserveProvider("claude", ledger.Win5h, 85, hnow.Add(-25*time.Hour), hnow.Add(-30*time.Hour))
		l.ObserveProvider("claude", ledger.Win7d, 17, hnow.Add(96*time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if strings.Contains(h, "85") {
		t.Fatalf("expired window's percentage must not be rendered: %s", h)
	}
	if !strings.Contains(h, "5h ?") {
		t.Fatalf("expired window must render as unknown (5h ?): %s", h)
	}
	if strings.Contains(h, "THROTTLED") || strings.Contains(h, "EXHAUSTED") {
		t.Fatalf("a lane whose only pressure is an expired window must carry no state word: %s", h)
	}
	if !strings.Contains(h, "7d 17%") {
		t.Fatalf("the live window must still render: %s", h)
	}
}

// REGRESSION (review 2026-07-25): when EVERY window is expired/unknown the
// render produced a row of bare "?" marks and injected it — "fail-open
// absolute" means an absent signal injects NOTHING, and a banner of question
// marks reads like a live report in every prompt.
func TestQuotaHintAllUnknownInjectsNothing(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 85, hnow.Add(-25*time.Hour), hnow.Add(-30*time.Hour))
		l.ObserveProvider("codex", ledger.Win5h, 271, hnow.Add(-43*time.Hour), hnow.Add(-44*time.Hour))
		l.ObserveProvider("glm", ledger.Win5h, 100, hnow.Add(-40*time.Hour), hnow.Add(-41*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if h := quotaHint(hnow); h != "" {
		t.Fatalf("no live number anywhere ⇒ inject nothing, got: %s", h)
	}
}

// A lane with NO live number must not contribute an all-"?" row while another
// lane still has real signal.
func TestQuotaHintOmitsFullyUnknownLanes(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win7d, 17, hnow.Add(96*time.Hour), hnow) // live
		l.ObserveProvider("codex", ledger.Win5h, 271, hnow.Add(-43*time.Hour), hnow.Add(-44*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if !strings.Contains(h, "claude 7d 17%") {
		t.Fatalf("the live lane must render: %s", h)
	}
	if strings.Contains(h, "codex") {
		t.Fatalf("a lane with no live number must be omitted, not rendered as ?: %s", h)
	}
}

// TestQuotaHintStaleLedgerSuppressed: the hint re-rendered IDENTICAL content on
// every single prompt (observed live 2026-09-21: "copilot month 100% EXHAUSTED ·
// nim trial 6%" on every turn of a session). Repeating an unchanged number is a
// tick, not a delta, and it costs context on every turn while changing no
// decision. The ledger's own mtime is the stateless freshness signal: if nothing
// has metered a window recently, there is no new pressure to report.
//
// Stateless BY DESIGN -- mr-hook writes no state in production and that
// invariant is preserved here.
func TestQuotaHintStaleLedgerSuppressed(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 42, hnow.Add(3*time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	stale := hnow.Add(-30 * time.Minute)
	if err := os.Chtimes(statepaths.Ledger(), stale, stale); err != nil {
		t.Fatal(err)
	}
	if h := quotaHint(hnow); h != "" {
		t.Fatalf("stale ledger (30m untouched) must suppress the hint, got: %s", h)
	}
}

// TestQuotaHintFreshLedgerRenders: the complement -- a ledger touched moments ago
// carries live pressure and MUST still render. Without this, the gate above
// could pass by suppressing everything.
func TestQuotaHintFreshLedgerRenders(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 42, hnow.Add(3*time.Hour), hnow)
	}); err != nil {
		t.Fatal(err)
	}
	fresh := hnow.Add(-1 * time.Minute)
	if err := os.Chtimes(statepaths.Ledger(), fresh, fresh); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if h == "" {
		t.Fatal("fresh ledger must still render the hint")
	}
	if !strings.Contains(h, "42%") {
		t.Fatalf("fresh hint lost its signal: %s", h)
	}
}

// TestQuotaHintGLMLatchExemptFromStaleness: REGRESSION GUARD.
//
// The first cut of the staleness gate ran os.Stat(ledger) before rows were
// built. The GLM hard-stop latch is deliberately ledger-INDEPENDENT -- it
// renders with no buckets and with no ledger file at all -- so the stat failed
// and the gate silently swallowed a 1313 account-protection warning
// (caught by TestHookMismatchKeepsQuotaHintE2E).
//
// The latch is account protection, not quota reporting. It must survive any
// freshness gate, including the no-ledger-at-all case seeded here.
func TestQuotaHintGLMLatchExemptFromStaleness(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	// Only a latch. No ledger file whatsoever.
	if err := os.WriteFile(statepaths.GLMAlert(), []byte(`{"note":"test latch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statepaths.Ledger()); err == nil {
		t.Fatal("precondition: this test requires NO ledger file")
	}
	h := quotaHint(hnow)
	if !strings.Contains(h, "glm HARD-STOP(1313)") {
		t.Fatalf("the GLM hard-stop latch must never be suppressed by the staleness gate, got: %q", h)
	}
}
