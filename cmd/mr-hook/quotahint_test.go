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
	for _, want := range []string{"85%", "42%", "9%", "THROTTLED", "mr-orchestrate route", "mr-orchestrate.md"} {
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
// goroutine (it used to run AFTER the select resolved, outside the ~300ms
// budget). This pins the property that made the move safe: on a seeded ledger
// the hint is (a) unchanged in content and (b) computed FAR under the ~300ms
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
	// A file read is microseconds; a comfortable margin under the 300ms hook
	// deadline proves the hint fits inside the bounded goroutine.
	if elapsed > 100*time.Millisecond {
		t.Fatalf("quotaHint took %v — too slow to fold inside the ~300ms hook deadline", elapsed)
	}
}

// A bucket with UsedPct == -1 (unknown) renders '?' not a bogus number.
func TestQuotaHintUnknownRendersQuestionMark(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		// claude with a known signal so the hint has signal at all.
		l.ObserveProvider("claude", ledger.Win5h, 30, hnow.Add(3*time.Hour), hnow)
		// codex bucket with unknown pct (shadow floor unlearned).
		l.AddShadow("codex", ledger.Win7d, 0, hnow) // stays -1 (uncapped/unanchored)
	}); err != nil {
		t.Fatal(err)
	}
	h := quotaHint(hnow)
	if !strings.Contains(h, "?") {
		t.Fatalf("unknown window must render '?': %s", h)
	}
}
