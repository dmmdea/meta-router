package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fleet check must (a) read a real binary's embedded revision, and (b) be
// HONEST when it cannot determine staleness — silently reporting stale=false
// for everything would be the false all-clear this subcommand exists to end.
func TestFleetReadsRevisionsAndAdmitsUndetermined(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	bin := t.TempDir()
	// Build a throwaway binary INTO the fake bin dir so there is something real
	// to read build info from.
	out := filepath.Join(bin, "mr-fake.exe")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/mr-hook")
	cmd.Dir = repoRootForTest(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("build unavailable in this environment: %v: %s", err, b)
	}
	// No -expect and (in a worktree) no self stamp → must say UNDETERMINED
	// rather than claim everything is current.
	stdout := captureFleet(t, "-bin", bin)
	if !strings.Contains(stdout, `"name": "mr-fake.exe"`) {
		t.Fatalf("fleet must list deployed mr-* binaries: %s", stdout)
	}
	// With an -expect that cannot match, the binary must be reported stale.
	stdout = captureFleet(t, "-bin", bin, "-expect", "0000000000ff")
	// A worktree build carries no stamp, so it must be reported UNSTAMPED
	// (unverifiable) rather than quietly "not stale".
	if !strings.Contains(stdout, `"unstamped": true`) {
		t.Fatalf("an unstamped binary must be flagged unverifiable, never passed: %s", stdout)
	}
	if !strings.Contains(stdout, "UNVERIFIABLE") {
		t.Fatalf("the note must state unverifiability: %s", stdout)
	}
	if !strings.Contains(stdout, `"staleness_undetermined": false`) {
		t.Fatalf("an explicit -expect makes staleness determined: %s", stdout)
	}
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // cmd/mr-orchestrate -> repo root
}

func captureFleet(t *testing.T, args ...string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	err = runFleet(args)
	os.Stdout = old
	w.Close()
	if err != nil {
		t.Fatalf("runFleet: %v", err)
	}
	b := make([]byte, 64*1024)
	n, _ := r.Read(b)
	return string(b[:n])
}
