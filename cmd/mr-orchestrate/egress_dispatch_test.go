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
	cfg := map[string]any{"glm_allow_repos": allow}
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

// BLOCKER 1, end to end at the dispatcher: --add-dir is VARIADIC in the shipped
// binary, so `--add-dir <allowed> <client>` handed the client checkout to a
// third-party provider while the receipt certified "repository context
// allowlisted". A plain typo produced it.
func TestGLMDispatchRefusesASecondVariadicAddDir(t *testing.T) {
	base := t.TempDir()
	allowed := filepath.Join(base, "allowed")
	client := filepath.Join(base, "client-secret")
	for _, d := range []string{allowed, client} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Control: the single-directory form is allowed, so the test below is
	// measuring the arity and not just a blanket refusal of --add-dir.
	if _, code := glmDryRun(t, allowed, []string{"--add-dir", allowed}, []string{allowed}); code != 0 {
		t.Fatalf("control: an allowlisted --add-dir must pass, got exit %d", code)
	}
	got, code := glmDryRun(t, allowed, []string{"--add-dir", allowed, client}, []string{allowed})
	if code != exitEgressDenied {
		t.Fatalf("the SECOND variadic --add-dir value must be gated: got exit %d (want %d)\n%v", code, exitEgressDenied, got)
	}
	if b, _ := json.Marshal(got); !strings.Contains(string(b), "client-secret") {
		t.Fatalf("the denial must name the path it refused: %s", b)
	}
}

// Deny-by-default on the same channel: a flag the gate does not model is
// forwarded verbatim to the provider and may carry a path, so it is refused
// rather than assumed harmless.
func TestGLMDispatchRefusesAnUnmodelledExtraFlag(t *testing.T) {
	repo := t.TempDir()
	got, code := glmDryRun(t, repo, []string{"--some-future-flag", "value"}, []string{repo})
	if code != exitEgressDenied {
		t.Fatalf("an unmodelled --extra flag must be refused: got exit %d\n%v", code, got)
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
