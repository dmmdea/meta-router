package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fleet check must not depend on AMBIENT VCS stamping: Go omits it for
// worktree builds and supplies it for normal checkouts, so a test that asserts
// either state implicitly passes in one and fails in the other. (The first
// version of this test did exactly that and would have gone red on main after
// merge — the same environment-assumption class that failed CI on the W2 PR.)
// Every case below CONTROLS provenance explicitly with -buildvcs / -ldflags.
func TestFleetProvenance(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		// No skip: the subject of this test IS a built binary, and a skip is
		// indistinguishable from a pass.
		t.Fatalf("go toolchain required: %v", err)
	}
	root := repoRootForTest(t)

	build := func(t *testing.T, dir, name string, extra ...string) {
		t.Helper()
		args := append([]string{"build"}, extra...)
		args = append(args, "-o", filepath.Join(dir, name), "./cmd/mr-hook")
		cmd := exec.Command(goBin, args...)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %v: %v: %s", args, err, b)
		}
	}

	t.Run("unstamped is unverifiable, never a pass", func(t *testing.T) {
		dir := t.TempDir()
		build(t, dir, "mr-fake.exe", "-buildvcs=false")
		out := captureFleet(t, "-bin", dir, "-expect", "b9bd6c3ab8be")
		if !strings.Contains(out, `"unstamped": true`) {
			t.Fatalf("a binary with no revision must be flagged unverifiable: %s", out)
		}
		if !strings.Contains(out, "UNVERIFIABLE") {
			t.Fatalf("the note must state unverifiability: %s", out)
		}
	})

	t.Run("injected revision matches by prefix", func(t *testing.T) {
		dir := t.TempDir()
		const full = "b9bd6c3ab8bedeadbeef0123456789abcdef0123"
		build(t, dir, "mr-fake.exe", "-buildvcs=false", "-ldflags", "-X main.buildRev="+full)
		// A 7-char short sha MUST match a full stamp: comparing fixed-width
		// truncations reported every binary stale for the natural
		// `git rev-parse --short` invocation.
		out := captureFleet(t, "-bin", dir, "-expect", "b9bd6c3")
		if !strings.Contains(out, `"stale": false`) || !strings.Contains(out, `"unstamped": false`) {
			t.Fatalf("short-sha prefix must match the injected revision: %s", out)
		}
		out = captureFleet(t, "-bin", dir, "-expect", "0000000000ff")
		if !strings.Contains(out, `"stale": true`) {
			t.Fatalf("a non-matching reference must mark the binary stale: %s", out)
		}
	})

	t.Run("unreadable binary and empty dir never read as uniform", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "mr-broken.exe"), []byte("not an executable"), 0o644); err != nil {
			t.Fatal(err)
		}
		out := captureFleet(t, "-bin", dir, "-expect", "b9bd6c3")
		if strings.Contains(out, "uniform") {
			t.Fatalf("an unreadable binary must not read as a uniform fleet: %s", out)
		}
		if !strings.Contains(out, `"unreadable_count": 1`) {
			t.Fatalf("unreadable binaries must be counted: %s", out)
		}
		empty := t.TempDir()
		out = captureFleet(t, "-bin", empty, "-expect", "b9bd6c3")
		if strings.Contains(out, "uniform") || !strings.Contains(out, "NO mr-* BINARIES FOUND") {
			t.Fatalf("an empty bin dir verified nothing and must say so: %s", out)
		}
	})
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // cmd/mr-orchestrate -> repo root
}

// captureFleet runs the subcommand with stdout redirected to a temp FILE, not a
// pipe: an unread pipe deadlocks once the report exceeds the buffer, which would
// turn a larger fleet into a hang instead of a failure. os.Stdout is restored via
// defer so a panic cannot leave it dangling.
func captureFleet(t *testing.T, args ...string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "fleet-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	old := os.Stdout
	os.Stdout = f
	defer func() { os.Stdout = old }()
	if err := runFleet(args); err != nil {
		t.Fatalf("runFleet(%v): %v", args, err)
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
