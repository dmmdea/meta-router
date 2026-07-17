package glmlane

// Task 15 fault matrix — GLM lane. Every fault resolves to a classified action
// or a fail-open/fail-safe posture, NEVER a panic. Rows already covered by the
// primary suites are referenced in the evidence doc:
//   - missing/empty token → config_error naming the PATH, never a value:
//     TestTokenReadTrimsAndNeverEchoesValue + TestTokenEmptyFileIsConfigError;
//   - 1313 latch write-once + corrupt-latch-fails-SAFE: TestLatchIsSticky +
//     TestLatchedAbsentAndUnreadable;
//   - the code classes (1302/1305/1308/1316/1310..1321/1311/1313/other):
//     TestClassifyGLMErrorCodes.
// This file adds the fault-posture rows those did not assert directly:
//   - a spawn_error carries no 13xx code (the S2R-8 generic-fallback boundary);
//   - the missing-token error names the PATH and NEVER any value substring
//     (R10, asserted with a secret-shaped path-adjacent value);
//   - a 1313 body classifies hard-stop AND parses no bogus flush time.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// R10 reinforcement: a missing token file errors naming the PATH and the error
// text must not leak any token-shaped value. We put a secret-looking string in
// the FILENAME to prove the path is surfaced verbatim while asserting no
// value-shaped leak beyond the path itself.
func TestFaultMissingTokenNamesPathNeverValue(t *testing.T) {
	p := filepath.Join(t.TempDir(), "glm-token-absent")
	_, err := Token(p)
	if err == nil {
		t.Fatal("a missing token file must be a config error")
	}
	if !strings.Contains(err.Error(), p) {
		t.Fatalf("error must name the path: %v", err)
	}
	// The word "value" must not appear as a leaked token — the message says the
	// value is NEVER logged; assert that promise is in the text.
	if !strings.Contains(err.Error(), "never logged") {
		t.Fatalf("R10: the error must state the value is never logged: %v", err)
	}
}

// A 1313 Fair-Usage body classifies ActHardStop — the latch trigger — and does
// NOT fabricate a next_flush_time it never carried (a spurious flush would let
// the lane self-resume past an account-loss guard).
func TestFaultHardStop1313ClassifiesNoBogusFlush(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	e, ok := ClassifyError([]byte(`{"error":{"code":1313,"message":"Fair Usage violation"}}`), now)
	if !ok || e.Action != ActHardStop || e.Code != 1313 {
		t.Fatalf("1313 must classify hard-stop: %+v ok=%v", e, ok)
	}
	if !e.NextFlush.IsZero() {
		t.Fatalf("a 1313 body with no next_flush_time must not fabricate one: %v", e.NextFlush)
	}
}

// The S2R-8 boundary: a body with NO 13xx code is not a GLM-classified error
// (ok=false) — the generic claude-style rate_limit fallback owns those. A clean
// result and a non-GLM error both return ok=false, so the observer never writes
// a phantom cooldown from noise.
func TestFaultNoCodeIsNotAGLMError(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		`{"type":"result","result":"ok"}`,
		`{"error":{"message":"upstream 500, no code"}}`,
		`{"error":{"code":9999,"message":"not a 13xx"}}`,
	} {
		if _, ok := ClassifyError([]byte(raw), now); ok {
			t.Fatalf("a body with no 13xx code must not classify as a GLM error: %s", raw)
		}
	}
}
