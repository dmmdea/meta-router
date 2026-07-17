package glmlane

// Shape honesty (plan Task 6, recorded): no live 429 body has been captured
// through the claude binary — forcing one would burn real quota and flirt
// with the 1313 strike class (NOT authorized). Every error body below is
// SYNTHETIC, derived from the fact-refresh error-code contract; the standing
// policy watch + the first real production 429 (dispatch-logged raw for
// fixture promotion) are the drift catchers.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClassifyGLMErrorCodes(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	flush := `{"error":{"code":1308,"message":"5h exhausted","next_flush_time":1783312200}}`
	tests := []struct {
		raw    string
		code   int
		action ErrAction
	}{
		{`{"error":{"code":"1302","message":"concurrency"}}`, 1302, ActRetry},
		{`{"error":{"code":1305}}`, 1305, ActRetry},
		{flush, 1308, ActCooldown},
		{`{"error":{"code":"1316","message":"tokens exhausted"}}`, 1316, ActCooldown},
		{`{"error":{"code":1310}}`, 1310, ActOffline},
		{`{"error":{"code":1317}}`, 1317, ActOffline},
		{`{"error":{"code":1321}}`, 1321, ActOffline},
		{`{"error":{"code":1311}}`, 1311, ActConfig},
		{`{"error":{"code":1313,"message":"fair usage"}}`, 1313, ActHardStop},
		{`{"error":{"code":1399}}`, 1399, ActUnknown},
	}
	for _, tc := range tests {
		e, ok := ClassifyError([]byte(tc.raw), now)
		if !ok || e.Code != tc.code || e.Action != tc.action {
			t.Fatalf("%s → %+v ok=%v", tc.raw, e, ok)
		}
	}
	if e, _ := ClassifyError([]byte(flush), now); !e.NextFlush.Equal(time.Unix(1783312200, 0).UTC()) {
		t.Fatalf("next_flush_time must parse: %v", e.NextFlush)
	}
	if _, ok := ClassifyError([]byte(`{"type":"result","result":"ok"}`), now); ok {
		t.Fatal("clean results must not classify as GLM errors")
	}
}

// The classifier is PURE extraction: next_flush_time parses verbatim even
// when it is already in the past. The staleness guard (a past reset must not
// anchor a cooldown that instantly rolls) lives at the observation site —
// see the cmd-level applyGLMOutcome tests.
func TestClassifyNextFlushParsesVerbatimEvenWhenPast(t *testing.T) {
	later := time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC) // well after 1783312200
	e, ok := ClassifyError([]byte(`{"error":{"code":1308,"next_flush_time":1783312200}}`), later)
	if !ok || !e.NextFlush.Equal(time.Unix(1783312200, 0).UTC()) {
		t.Fatalf("classifier must extract verbatim (guard is the observer's job): %+v ok=%v", e, ok)
	}
}

func TestLatchIsSticky(t *testing.T) {
	p := filepath.Join(t.TempDir(), "glm-alert.json")
	if err := LatchAlert(p, GLMErr{Code: 1313, Action: ActHardStop, Raw: "fair usage"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	note, latched := Latched(p)
	if !latched || !strings.Contains(note, "1313") {
		t.Fatalf("1313 must latch: %q %v", note, latched)
	}
	// Write-once: a second violation must NOT overwrite the first timestamp
	// (the first violation is the evidence).
	if err := LatchAlert(p, GLMErr{Code: 1313, Action: ActHardStop, Raw: "second strike"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if note2, _ := Latched(p); note2 != note {
		t.Fatalf("latch must be write-once: %q → %q", note, note2)
	}
}

// A2R-#7: LatchAlert is an ATOMIC create-once (O_CREATE|O_EXCL), not a
// Stat-then-Write TOCTOU. A second LatchAlert carrying a DIFFERENT code MUST
// NOT alter the file — the first violation's evidence stands, byte-for-byte.
func TestLatchAlertAtomicFirstViolationStands(t *testing.T) {
	p := filepath.Join(t.TempDir(), "glm-alert.json")
	if err := LatchAlert(p, GLMErr{Code: 1313, Action: ActHardStop}, time.Unix(1000, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	// Second violation: different code, different timestamp — must be a no-op.
	if err := LatchAlert(p, GLMErr{Code: 1399, Action: ActUnknown}, time.Unix(9999, 0).UTC()); err != nil {
		t.Fatalf("a second LatchAlert on an existing latch must return nil, got %v", err)
	}
	second, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("latch file mutated by a second violation (TOCTOU): %q → %q", first, second)
	}
	if !strings.Contains(string(first), "1313") {
		t.Fatalf("the FIRST violation's code must be the recorded evidence: %q", first)
	}
}

// A2R-#7 (concurrency): N racers latch DISTINCT codes at once. With an atomic
// O_CREATE|O_EXCL create-once, exactly ONE write lands and the file is
// stable from the first successful write — no torn/overwritten content. Under
// the old Stat-then-Write TOCTOU two racers could both pass the Stat and both
// WriteFile, clobbering the first-violation evidence (and racing under -race).
func TestLatchAlertConcurrentCreateOnce(t *testing.T) {
	p := filepath.Join(t.TempDir(), "glm-alert.json")
	const n = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(code int) {
			defer wg.Done()
			<-start
			_ = LatchAlert(p, GLMErr{Code: code, Action: ActHardStop}, time.Unix(int64(code), 0).UTC())
		}(1300 + i)
	}
	close(start)
	wg.Wait()
	// Exactly one file, valid JSON, exactly one winner's code recorded.
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("a latch file must exist after the race: %v", err)
	}
	var v struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("latch file must be intact valid JSON after the race (no torn write): %v\n%s", err, b)
	}
	if v.Code < 1300 || v.Code >= 1300+n {
		t.Fatalf("latch code must be one racer's code, got %d", v.Code)
	}
}

func TestLatchedAbsentAndUnreadable(t *testing.T) {
	if _, latched := Latched(filepath.Join(t.TempDir(), "missing.json")); latched {
		t.Fatal("no alert file → not latched")
	}
	// 1313 is the one place that fails SAFE, not open: an existing-but-corrupt
	// latch file stays latched (account-loss class; ack clears it explicitly).
	p := filepath.Join(t.TempDir(), "glm-alert.json")
	if err := os.WriteFile(p, []byte("###corrupt###"), 0o644); err != nil {
		t.Fatal(err)
	}
	if note, latched := Latched(p); !latched || note == "" {
		t.Fatalf("corrupt latch file must stay latched with a readable note: %q %v", note, latched)
	}
}

// A2R-#2: an existing-but-UNREADABLE latch (not IsNotExist — e.g. a directory
// at the path, an ACL/sharing-violation read error) must FAIL SAFE and stay
// LATCHED. The old code collapsed ALL read errors to not-latched, silently
// re-admitting a lane one strike from a ban. Only os.IsNotExist means "no
// latch"; every other read error is treated as latched.
func TestLatchedExistingButUnreadableStaysLatched(t *testing.T) {
	// A directory at the latch path: os.ReadFile fails with a non-IsNotExist
	// error (EISDIR/"is a directory") — the fail-safe branch must latch.
	dir := filepath.Join(t.TempDir(), "glm-alert.json")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	note, latched := Latched(dir)
	if !latched {
		t.Fatalf("an existing-but-unreadable latch (dir at path) must stay LATCHED (fail-safe), got latched=%v note=%q", latched, note)
	}
	if note == "" {
		t.Fatalf("the fail-safe latch must carry a readable note: %q", note)
	}
	// And the genuinely-missing case must STILL report not-latched.
	if _, l := Latched(filepath.Join(t.TempDir(), "nope.json")); l {
		t.Fatal("a genuinely-missing latch file must report not-latched")
	}
}
