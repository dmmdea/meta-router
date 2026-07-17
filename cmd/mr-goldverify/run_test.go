package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/goldtask"
)

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixtureRepo builds a two-commit repo: commit A has a buggy Sum (no test);
// commit B fixes it AND adds sum_test.go (the held-out verifier).
// Returns (repoPath, parentHash, resolvingHash, fixDiff) where fixDiff is B's
// non-test change as a unified diff — the "perfect candidate" patch.
func fixtureRepo(t *testing.T) (string, string, string, []byte) {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	mod := "module fixture\n\ngo 1.26\n"
	buggy := "package fixture\n\nfunc Sum(xs []int) int {\n\ttotal := 1\n\tfor _, x := range xs {\n\t\ttotal += x\n\t}\n\treturn total\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sum.go"), []byte(buggy), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "A: buggy")
	parent := git(t, dir, "rev-parse", "HEAD")

	fixed := strings.Replace(buggy, "total := 1", "total := 0", 1)
	test := "package fixture\n\nimport \"testing\"\n\nfunc TestSum(t *testing.T) {\n\tif got := Sum([]int{1, 2}); got != 3 {\n\t\tt.Fatalf(\"Sum=%d want 3\", got)\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "sum.go"), []byte(fixed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sum_test.go"), []byte(test), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "B: fix + held-out test")
	resolving := git(t, dir, "rev-parse", "HEAD")

	// The candidate patch = B's change EXCLUDING the test file.
	cmd := exec.Command("git", "diff", parent, resolving, "--", ":!*_test.go")
	cmd.Dir = dir
	diff, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture diff: %v", err)
	}
	return dir, parent, resolving, diff
}

func fixtureTask(parent, resolving string) goldtask.Task {
	return goldtask.Task{
		ID: "FIX-01", Class: "quick-edit", Split: "tuning", Repo: "fixture",
		Prompt: "fix Sum",
		Verify: goldtask.VerifySpec{
			Kind: "vgo", Repo: "fixture", Parent: parent, Resolving: resolving,
			TestFiles: []string{"sum_test.go"}, Pkgs: []string{"./..."},
			PreGate: []goldtask.Key{{Name: "diff", Pattern: `(^|\n)diff --git`}},
		},
	}
}

func TestRunExecFixture(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo, parent, resolving, fixDiff := fixtureRepo(t)
	task := fixtureTask(parent, resolving)

	// (a) the real fix (non-test diff) passes the held-out test.
	v := runExec(task, fixDiff, repo, 300)
	if !v.Pass {
		t.Fatalf("real fix should PASS, got: %s", v.Detail)
	}

	// (b) an empty patch fails (the held-out test catches the bug).
	v = runExec(task, nil, repo, 300)
	if v.Pass {
		t.Fatal("empty patch should FAIL")
	}

	// (c) a patch that touches the held-out test file is rejected (leakage guard).
	leak := []byte("diff --git a/sum_test.go b/sum_test.go\n--- a/sum_test.go\n+++ b/sum_test.go\n@@ -0,0 +1 @@\n+// cheat\n")
	v = runExec(task, leak, repo, 300)
	if v.Pass {
		t.Fatal("test-touching patch should FAIL (leakage guard)")
	}
	if !strings.Contains(v.Detail, "test file") {
		t.Fatalf("leakage failure should name the test-file guard, got: %s", v.Detail)
	}

	// (d) AllowedFiles constraint: restrict to a file the fix does not touch.
	task.Verify.AllowedFiles = []string{"other.go"}
	v = runExec(task, fixDiff, repo, 300)
	if v.Pass {
		t.Fatal("out-of-allowlist diff should FAIL")
	}
}
