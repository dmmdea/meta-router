package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"os"
	"path/filepath"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/claudelane"
	"github.com/dmmdea/meta-router/internal/orch/glmlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// A 429 result must flow into an exhausted lane with a valid resume: parse →
// rate_limit, provider observation → Decide denies with ResumeAt.
func TestFault429ThenExhaustedWithResume(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"","total_cost_usd":0,"num_turns":0}`)
	if o := claudelane.Parse(raw); o.Class != "rate_limit" {
		t.Fatalf("want rate_limit, got %q", o.Class)
	}
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	l := ledger.Open(filepath.Join(t.TempDir(), "ledger.json"))
	resets := now.Add(90 * time.Minute)
	l.ObserveProvider("claude", ledger.Win5h, 100, resets, now)
	d := admission.Decide(l.Snapshot(), "claude", now, admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95})
	if d.Admit || !d.ResumeAt.Equal(resets) {
		t.Fatalf("post-429 observation must exhaust with the provider reset: %+v", d)
	}
}

// Corrupt ledger on disk fails open: empty state, gate admits with the
// no-signal reason — the ledger being broken never blocks work (fail-open, R8).
func TestFaultCorruptLedgerFailsOpenAndAdmits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")
	if err := os.WriteFile(p, []byte("###corrupt###"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, warn := ledger.OpenChecked(p)
	if warn == "" {
		t.Fatal("corrupt ledger must produce a warning for the fail-open contract")
	}
	g := gate(l.Snapshot(), "claude", "sonnet", nil, time.Now().UTC(), orchcfg.Defaults(), false, admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95})
	if !g.Admit || g.State != "open" {
		t.Fatalf("corrupt ledger must fail open and admit: %+v", g)
	}
	if g.Reason != "" && !strings.Contains(g.Reason, "no signal") && g.Reason != "" {
		t.Fatalf("unexpected reason: %q", g.Reason)
	}
}

// Deferral JSON (exit-3 path) round-trips with resume even when the reset was
// estimated (RS5).
func TestFaultDeferralJSONAlwaysHasResume(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	bs := []ledger.Bucket{{Lane: "claude", Window: ledger.Win5h, UsedPct: 99, Source: "shadow"}}
	g := gate(bs, "claude", "sonnet", nil, now, orchcfg.Defaults(), false, admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95})
	if g.Admit {
		t.Fatalf("must deny: %+v", g)
	}
	if g.ResumeAt.IsZero() {
		t.Fatalf("RS5: even without a provider reset the deferral must carry resume: %+v", g)
	}
	if !strings.Contains(string(deferralJSON(g)), "resume_at") {
		t.Fatal("deferral JSON must expose resume_at")
	}
}

// ---- Task-15 additions: MCP transport faults + glm gate/latch integration ----

// MCP garbage stdin line: the server logs a parse error to stderr and KEEPS
// serving — the very next well-formed request still gets a response. A garbage
// line must never end the loop or crash the transport.
func TestFaultMCPGarbageLineKeepsServing(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	in := strings.Join([]string{
		`this is not json at all`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`, // must still be answered
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatalf("a garbage line must not error the server: %v", err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":1`) || !strings.Contains(lines[0], `"result":{}`) {
		t.Fatalf("server must skip the garbage line and answer the next request: %q", out.String())
	}
}

// MCP oversized line (> the 4MB scanner buffer): bufio.Scanner returns
// bufio.ErrTooLong; serveMCP surfaces it as a scan error and returns cleanly —
// NO panic, NO partial-line corruption. A bounded buffer is the DoS guard.
func TestFaultMCPOversizedLineBoundedNoCrash(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	// 5MB single line with no newline — exceeds the 4MB Scanner buffer.
	huge := make([]byte, 5*1024*1024)
	for i := range huge {
		huge[i] = 'a'
	}
	var out bytes.Buffer
	err := serveMCP(bytes.NewReader(huge), &out)
	if err == nil {
		t.Fatal("an oversized line must surface the bounded-buffer scan error (bufio.ErrTooLong), not be silently accepted")
	}
	// The process is still standing (no panic) and wrote nothing bogus to the
	// transport for the un-parseable oversized line.
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("no response should be emitted for an oversized/unscannable line: %q", out.String())
	}
}

// glm 1308 cooldown → the 5h window is exhausted until next_flush_time; the NEXT
// gate on the glm lane then DEFERS (admission denies with a valid resume). This
// pins the end-to-end "next invocation defers" behavior through applyGLMOutcome
// + glmGate, on a SYNTHETIC 1308 body (labeled — no live 429 exists).
func TestFaultGLM1308CooldownThenNextDefers(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	flush := now.Add(90 * time.Minute).Unix()
	raw := []byte(`{"error":{"code":1308,"message":"5h exhausted","next_flush_time":` +
		itoa(flush) + `}}`)
	cfg := orchcfg.Defaults()
	// Apply the 1308 outcome: the observer must exhaust glm/5h to the flush time.
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		e, classified := applyGLMOutcome(l, claudelane.Outcome{Class: "rate_limit"}, raw, "glm-5.2", cfg, nil, now)
		if !classified || e.Code != 1308 {
			t.Fatalf("1308 body must classify cooldown: %+v classified=%v", e, classified)
		}
	}); err != nil {
		t.Fatal(err)
	}
	// The NEXT gate must DEFER — admission denies with the flush-time resume.
	l, _ := ledger.OpenChecked(statepaths.Ledger())
	g := glmGate(l.Snapshot(), glmAlertPath(), now, defaultThresholds, false)
	if g.Admit {
		t.Fatalf("after a 1308 cooldown the next glm dispatch must defer: %+v", g)
	}
	if g.ResumeAt.IsZero() {
		t.Fatalf("the deferral must carry a resume (the flush time / RS5): %+v", g)
	}
}

// glm 1313 latch → gate DENIES; --force does NOT bypass (the one place force
// yields, account-loss class); `probe --ack-glm` (os.Remove of the alert file)
// RESTORES the lane. Full integration of the latch composition (S2R-17).
func TestFaultGLM1313LatchDeniesForceYieldsAckRestores(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if err := glmlane.LatchAlert(glmAlertPath(), glmlane.GLMErr{Code: 1313, Action: glmlane.ActHardStop}, now); err != nil {
		t.Fatal(err)
	}
	l, _ := ledger.OpenChecked(statepaths.Ledger())

	// Latched: gate denies with a hard_stop that names the ack path.
	g := glmGate(l.Snapshot(), glmAlertPath(), now, defaultThresholds, false)
	if g.Admit || g.State != "hard_stop" || !strings.Contains(g.Reason, "ack-glm") {
		t.Fatalf("1313 latch must deny with a hard_stop naming the ack path: %+v", g)
	}

	// --force does NOT bypass the 1313 latch (unlike every other deny).
	gf := glmGate(l.Snapshot(), glmAlertPath(), now, defaultThresholds, true)
	if gf.Admit {
		t.Fatalf("--force must NOT bypass the 1313 latch (account-loss guard): %+v", gf)
	}

	// `probe --ack-glm` clears the latch (the R11 two-step). Simulate its effect.
	if err := os.Remove(glmAlertPath()); err != nil {
		t.Fatal(err)
	}
	gr := glmGate(l.Snapshot(), glmAlertPath(), now, defaultThresholds, false)
	if !gr.Admit {
		t.Fatalf("after --ack-glm clears the latch the lane must admit: %+v", gr)
	}
}

// A corrupt glm-alert.json fails SAFE (stays latched), NOT open — the 1313 latch
// is the one place fail-safe beats fail-open (account-loss class; the explicit
// ack is the only way out). This is the S2R-17-validated composition; the plan's
// Task-15 line saying "corrupt → fail-open WARN treated unlatched" is superseded
// by S2R-17 (recorded as a deviation in the evidence doc).
func TestFaultGLMCorruptAlertFailsSafeLatched(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	if err := os.MkdirAll(filepath.Dir(glmAlertPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(glmAlertPath(), []byte("###corrupt###"), 0o644); err != nil {
		t.Fatal(err)
	}
	l, _ := ledger.OpenChecked(statepaths.Ledger())
	g := glmGate(l.Snapshot(), glmAlertPath(), now, defaultThresholds, false)
	if g.Admit {
		t.Fatalf("a corrupt 1313 latch must stay latched (fail-SAFE), not admit: %+v", g)
	}
}

// codex missing binary at the CMD layer → a spawn_error RECEIPT is appended
// (not a bare error): the lane runs the gate (admits), provisions the home,
// spawns → spawn_error Outcome (nil err), and dispatch-logs it. The receipt is
// the replay/coverage substrate — a spawn failure must still be COUNTABLE.
func TestFaultCodexMissingBinaryAppendsSpawnErrorReceipt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	// Seed a fake operator ~/.codex/auth.json so EnsureHome succeeds and we
	// reach the spawn (the missing-auth config_error is TestEnsureHomeFailsLoud).
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"tokens":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "") // strip codex from the search path → spawn_error

	var out bytes.Buffer
	code, err := runCodexLane(&out, "hi", "gpt-5.5", "high", "", 30, nil, true, true, false, "cli", "fault test", recFields{}, strategyFields{})
	if err != nil {
		t.Fatalf("a spawn_error is a classified outcome, not a config error return: %v", err)
	}
	if code != exitNotOK {
		t.Fatalf("a spawn_error must exit %d (dispatched but outcome not ok), got %d", exitNotOK, code)
	}
	// The receipt must be appended with outcome_class=spawn_error.
	recs := loadReceipts(dispatchPath())
	if len(recs) != 1 || recs[0].OutcomeClass != "spawn_error" {
		t.Fatalf("a spawn_error must append exactly one receipt classified spawn_error: %+v", recs)
	}
	if recs[0].Lane != "codex" || recs[0].Origin != "cli" {
		t.Fatalf("receipt must carry lane+origin: %+v", recs[0])
	}
}

// itoa avoids importing strconv just for one int64→string in the fixture body.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
