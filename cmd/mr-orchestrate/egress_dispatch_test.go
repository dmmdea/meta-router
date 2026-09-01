package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// These are BEHAVIOURAL tests of the GLM dispatcher's data boundary, and they
// exist because the B14 canary alone was not enough.
//
// B14 asserts only that runGLMLane CONTAINS a call to egress.Plan. Review proved
// two compiling mutations that keep that call, keep the whole test suite green,
// keep B14 passing — and export the caller's repository anyway:
//
//	_ = planDir                                   // ignore the planned directory
//	_, planCleanup, ed := egress.Plan(...)        // discard it outright
//
// Both leave cwd as the caller's inherited directory while the receipt still
// records "prompt-only ENFORCED in a neutral directory". A structural check
// cannot see that; only asserting on the dispatcher's OUTPUT can. Every test
// below reads effective_cwd / the exit code, never the source.
//
// The dry-run path is used deliberately: it prints the same egress decision and
// the same effective_cwd the live path hands the child, so the boundary is
// provable without spending a single token.

// glmDryRun runs the dispatcher in dry-run mode against an isolated state dir
// with the given allowlist, and returns the parsed JSON plus the exit code.
func glmDryRun(t *testing.T, cwd string, extra []string, allow []string) (map[string]any, int) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)
	cfg := map[string]any{"glm_allow_repos": allow, "glm_retired": false} // retirement is config; these tests pin the egress boundary of the RE-ENABLED lane
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := runGLMLane(&out, "hello", "glm-5.2", "", cwd, 30, extra,
		false /*live*/, false /*force*/, "test", "egress behaviour", recFields{}, strategyFields{})
	if err != nil {
		t.Fatalf("runGLMLane: %v\n%s", err, out.String())
	}
	var got map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("dispatcher output is not JSON (%v):\n%s", jerr, out.String())
	}
	return got, code
}

// The CRITICAL, asserted on behaviour: with no allowlist and no requested cwd,
// the child must NOT run in the caller's directory.
func TestGLMDispatchNeverRunsInAnUnallowlistedInheritedCwd(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got, code := glmDryRun(t, "", nil, nil)
	if code != 0 {
		t.Fatalf("prompt-only must remain dispatchable, got exit %d: %v", code, got)
	}
	eff, _ := got["effective_cwd"].(string)
	if eff == "" {
		t.Fatal("effective_cwd is EMPTY — os/exec would run the child in the caller's directory, which is the exact leak this gate exists to close")
	}
	if strings.EqualFold(eff, wd) {
		t.Fatalf("the child would run in the caller's cwd %q despite it not being allowlisted", eff)
	}
	// Emptiness is asserted in the egress package's unit test, which still holds
	// the directory open. Here it is already gone: the dispatcher defers
	// planCleanup, so by the time it returns the scratch directory is removed.
	// That is the correct lifecycle — what this test can and must check is that
	// the substituted directory was a SCRATCH one, nowhere near the caller.
	if !strings.HasPrefix(strings.ToLower(eff), strings.ToLower(os.TempDir())) {
		t.Fatalf("the substituted directory must live under the OS temp dir, got %q (temp=%q)", eff, os.TempDir())
	}
	if within := strings.HasPrefix(strings.ToLower(eff), strings.ToLower(wd)); within {
		t.Fatalf("the substituted directory must not be inside the caller's tree: %q", eff)
	}
	if gate, _ := got["egress_gate"].(string); !strings.Contains(gate, "ENFORCED") {
		t.Fatalf("the receipt must say the guarantee was enforced, not assumed: %q", gate)
	}
}

// An allowlisted cwd is used as-is: the fix must not degenerate into "always a
// temp dir", which would silently break every legitimate repo dispatch.
func TestGLMDispatchUsesAnAllowlistedCwd(t *testing.T) {
	repo := t.TempDir()
	got, code := glmDryRun(t, repo, nil, []string{repo})
	if code != 0 {
		t.Fatalf("an allowlisted repo must dispatch, got exit %d: %v", code, got)
	}
	eff, _ := got["effective_cwd"].(string)
	if !strings.EqualFold(eff, repo) {
		t.Fatalf("an allowlisted cwd must be used as-is: got %q want %q", eff, repo)
	}
}

// --extra at the dispatcher: a third-party lane refuses every path-bearing flag.
//
// Two rounds of review defeated the two designs that tried to GATE those paths
// instead (variadic arity, then a variadic collector left open across a
// skip-counted flag, plus a relative value the gate and the child resolve against
// different base directories). This asserts the posture that replaced them.
func TestGLMDispatchRefusesPathBearingExtras(t *testing.T) {
	repo := t.TempDir()
	client := filepath.Join(t.TempDir(), "client-secret")
	if err := os.MkdirAll(client, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{
		{"--add-dir", repo},                             // even the ALLOWLISTED dir
		{"--add-dir", repo, client},                     // round-2 variadic bypass
		{"--add-dir", "--output-format", client},        // round-3 skip-counter bypass
		{"--add-dir", "../client-secret"},               // round-3 relative-base bypass
		{"--mcp-config", filepath.Join(repo, "m.json")}, // never gated at all before
		{"--some-future-flag", "value"},                 // deny-by-default
	} {
		got, code := glmDryRun(t, repo, extra, []string{repo})
		if code != exitEgressDenied {
			t.Errorf("--extra %v must be refused: got exit %d, %v", extra, code, got)
		}
	}
}

// …while path-free flags still dispatch, or the lane is unusable for real calls.
func TestGLMDispatchAllowsPathFreeExtras(t *testing.T) {
	repo := t.TempDir()
	for _, extra := range [][]string{
		{"--dangerously-skip-permissions"},
		{"--verbose"},
		{"--effort", "high"},
	} {
		got, code := glmDryRun(t, repo, extra, []string{repo})
		if code != 0 {
			t.Errorf("--extra %v carries no path and must dispatch: got exit %d, %v", extra, code, got)
		}
	}
}

// An explicitly requested non-allowlisted repo is a deliberate ask and must be
// refused outright — never quietly neutralised into a temp dir.
func TestGLMDispatchRefusesAnExplicitNonAllowlistedCwd(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	other := filepath.Join(base, "other")
	for _, d := range []string{allowed, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, code := glmDryRun(t, other, nil, []string{allowed})
	if code != exitEgressDenied {
		t.Fatalf("an explicit non-allowlisted cwd must exit %d, got %d\n%v", exitEgressDenied, code, got)
	}
}

// A junction inside an allowlisted repo must be judged by its TARGET. This is
// the dispatcher-level companion to the egress package's unit test: mklink /J
// needs no elevation, and Go reports a junction as ModeIrregular, so
// filepath.EvalSymlinks returned it unchanged with a nil error.
func TestGLMDispatchRefusesAJunctionEscape(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	secret := filepath.Join(base, "client-secret")
	for _, d := range []string{allowed, secret} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(allowed, "link")
	if !dispatchDirLink(t, link, secret) {
		t.Skip("no unprivileged directory-link mechanism on this platform")
	}
	got, code := glmDryRun(t, link, nil, []string{allowed})
	if code != exitEgressDenied {
		t.Fatalf("a link out of the allowlisted tree must exit %d, got %d\n%v", exitEgressDenied, code, got)
	}
}

// dispatchDirLink mirrors the egress package's helper: a junction on Windows
// (mklink /J, no elevation needed — unlike mklink /D), a symlink elsewhere.
func dispatchDirLink(t *testing.T, link, target string) bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Logf("mklink /J unavailable: %v\n%s", err, out)
			return false
		}
		return true
	}
	if err := os.Symlink(target, link); err != nil {
		t.Logf("symlink unavailable: %v", err)
		return false
	}
	return true
}

// SECURITY.md states that EVERY dispatch records its gate decision on the receipt,
// "including quota deferrals and paced deferrals, which are decided after the gate
// has already ruled". Nothing tested that: the only egress_gate assertion in the
// suite was on dry-run stdout, so the deferral receipts could silently lose the
// field and the security document would quietly become false (review round 4).
func TestGLMDeferralReceiptCarriesTheEgressGate(t *testing.T) {
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)
	repo := t.TempDir()
	cfg, err := json.Marshal(map[string]any{"glm_allow_repos": []string{repo}, "glm_retired": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "config.json"), cfg, 0o644); err != nil {
		t.Fatal(err)
	}
	// Exhaust the glm 5h window so the quota gate defers AFTER the egress gate has
	// already allowed the dispatch.
	if err := ledger.Update(filepath.Join(state, "ledger.json"), func(l *ledger.Ledger) {
		l.SetCapacity("glm", ledger.Win5h, 10)
		l.AddShadow("glm", ledger.Win5h, 100, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code, err := runGLMLane(&out, "hello", "glm-5.2", "", repo, 30, nil,
		false, false, "test", "deferral receipt", recFields{}, strategyFields{})
	if err != nil {
		t.Fatalf("runGLMLane: %v", err)
	}
	if code != exitDeferred {
		t.Fatalf("test premise: the quota gate must defer, got exit %d: %s", code, out.String())
	}
	b, rerr := os.ReadFile(filepath.Join(state, "dispatch.jsonl"))
	if rerr != nil {
		t.Fatalf("no receipt written: %v", rerr)
	}
	var rec map[string]any
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if jerr := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); jerr != nil {
		t.Fatalf("receipt is not JSON: %v\n%s", jerr, b)
	}
	if rec["outcome_class"] != "deferred" {
		t.Fatalf("expected a deferral receipt, got %v", rec["outcome_class"])
	}
	gate, _ := rec["egress_gate"].(string)
	if gate == "" {
		t.Fatal("the deferral receipt dropped egress_gate — the gate ruled before quota did, and SECURITY.md promises every decision is recorded")
	}
	if !strings.Contains(gate, "allowlisted") {
		t.Fatalf("the receipt must carry the real basis, got %q", gate)
	}
}
