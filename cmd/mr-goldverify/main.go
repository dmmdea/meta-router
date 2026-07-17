// Command mr-goldverify runs a gold task's EXECUTION verifier (V-GO / V-PY /
// V-PESTER): git worktree at the task's parent commit → apply the candidate's
// unified diff (rejecting any hunk that touches a held-out test file — the
// oracle-leakage guard) → copy the held-out test files from the resolving
// commit → run the tests → verdict. The pure kinds never reach this command;
// their engine lives in internal/goldtask and runs in-suite.
//
//	mr-goldverify -task AC-04 -patch fix.diff [-goldset testdata/routing-goldset.jsonl]
//	              [-repos name=path,...] [-timeout 600]
//
// Exit 0 = PASS, 1 = FAIL, 2 = usage/infra error. Verdict JSON on stdout.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/goldtask"
)

const version = "0.1.0"

// Verdict is the execution verifier's result for one task.
type Verdict struct {
	Task   string `json:"task"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
}

// touchesTestFile reports whether the unified diff names any held-out test
// file. Checked BEFORE apply: a candidate that edits the verifier is an
// automatic fail (oracle-leakage guard), never an accident to absorb.
func touchesTestFile(patch []byte, testFiles []string) string {
	text := string(patch)
	for _, tf := range testFiles {
		slash := filepath.ToSlash(tf)
		if strings.Contains(text, "b/"+slash) || strings.Contains(text, "a/"+slash) {
			return slash
		}
	}
	return ""
}

// run executes one command in dir with a deadline, returning combined output.
func run(ctx context.Context, dir string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func fail(task, stage string, out []byte, err error) Verdict {
	tail := strings.TrimSpace(string(out))
	if len(tail) > 800 {
		tail = "…" + tail[len(tail)-800:]
	}
	return Verdict{Task: task, Pass: false, Detail: fmt.Sprintf("%s: %v\n%s", stage, err, tail)}
}

// runExec is the whole execution gate for one task.
func runExec(t goldtask.Task, patch []byte, repoPath string, timeoutSec int) Verdict {
	v := t.Verify
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	// Leakage guard first — cheapest and non-negotiable.
	if tf := touchesTestFile(patch, v.TestFiles); tf != "" {
		return Verdict{Task: t.ID, Pass: false,
			Detail: fmt.Sprintf("leakage guard: candidate diff touches held-out test file %s", tf)}
	}

	wt := filepath.Join(os.TempDir(), fmt.Sprintf("goldverify-%s-%d", strings.ToLower(t.ID), os.Getpid()))
	if out, err := run(ctx, repoPath, "git", "worktree", "add", "--detach", wt, v.Parent); err != nil {
		return fail(t.ID, "worktree add", out, err)
	}
	defer func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer ccancel()
		_, _ = run(cctx, repoPath, "git", "worktree", "remove", "--force", wt)
	}()

	// Apply the candidate diff (an empty patch is legal — it just fails tests).
	if len(strings.TrimSpace(string(patch))) > 0 {
		pf := filepath.Join(wt, ".candidate.diff")
		if err := os.WriteFile(pf, patch, 0o644); err != nil {
			return fail(t.ID, "write patch", nil, err)
		}
		if out, err := run(ctx, wt, "git", "apply", "--whitespace=nowarn", ".candidate.diff"); err != nil {
			return fail(t.ID, "git apply", out, err)
		}
		_ = os.Remove(pf)
	}

	// AllowedFiles constraint — checked BEFORE the held-out tests land so the
	// copied verifier files can never trip it.
	if len(v.AllowedFiles) > 0 {
		out, err := run(ctx, wt, "git", "diff", "--name-only", "HEAD")
		if err != nil {
			return fail(t.ID, "diff --name-only", out, err)
		}
		allowed := map[string]bool{}
		for _, a := range v.AllowedFiles {
			allowed[filepath.ToSlash(a)] = true
		}
		for _, f := range strings.Fields(string(out)) {
			if !allowed[filepath.ToSlash(f)] {
				return Verdict{Task: t.ID, Pass: false,
					Detail: fmt.Sprintf("allowed-files: candidate changed %s (allowed: %v)", f, v.AllowedFiles)}
			}
		}
	}

	// Drop in the held-out test files from the resolving commit.
	for _, tf := range v.TestFiles {
		out, err := run(ctx, repoPath, "git", "show", v.Resolving+":"+filepath.ToSlash(tf))
		if err != nil {
			return fail(t.ID, "extract held-out test "+tf, out, err)
		}
		dst := filepath.Join(wt, tf)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fail(t.ID, "mkdir for "+tf, nil, err)
		}
		if err := os.WriteFile(dst, out, 0o644); err != nil {
			return fail(t.ID, "write held-out test "+tf, nil, err)
		}
	}

	// Run the gate.
	switch v.Kind {
	case "vgo":
		vetArgs := append([]string{"vet"}, v.Pkgs...)
		if out, err := run(ctx, wt, "go", vetArgs...); err != nil {
			return fail(t.ID, "go vet", out, err)
		}
		testArgs := append([]string{"test", "-count=1"}, v.Pkgs...)
		if out, err := run(ctx, wt, "go", testArgs...); err != nil {
			return fail(t.ID, "go test", out, err)
		}
	case "vpy", "vpester":
		parts := strings.Fields(v.TestCmd)
		if len(parts) == 0 {
			return Verdict{Task: t.ID, Pass: false, Detail: "empty test_cmd"}
		}
		if out, err := run(ctx, wt, parts[0], parts[1:]...); err != nil {
			return fail(t.ID, v.Kind+" tests", out, err)
		}
	default:
		return Verdict{Task: t.ID, Pass: false, Detail: "kind " + v.Kind + " is not an execution verifier"}
	}
	return Verdict{Task: t.ID, Pass: true, Detail: "held-out tests green"}
}

// repoDir resolves a logical repo name: -repos overrides win; meta-router is
// the module itself; anything else defaults to a sibling checkout.
func repoDir(name string, overrides map[string]string) string {
	if p, ok := overrides[name]; ok {
		return p
	}
	if name == "meta-router" {
		return "."
	}
	return filepath.Join("..", name)
}

func main() {
	goldset := flag.String("goldset", "testdata/routing-goldset.jsonl", "gold-set JSONL")
	taskID := flag.String("task", "", "task ID to verify (required)")
	patchPath := flag.String("patch", "", "candidate unified-diff file (empty = null candidate)")
	reposFlag := flag.String("repos", "", "logical repo overrides: name=path,name=path")
	timeoutSec := flag.Int("timeout", 600, "gate timeout (seconds)")
	flag.Parse()

	overrides := map[string]string{}
	for _, kv := range strings.Split(*reposFlag, ",") {
		if k, val, ok := strings.Cut(strings.TrimSpace(kv), "="); ok {
			overrides[k] = val
		}
	}

	ts, err := goldtask.Load(*goldset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goldset load: %v\n", err)
		os.Exit(2)
	}
	var task *goldtask.Task
	for i := range ts {
		if ts[i].ID == *taskID {
			task = &ts[i]
			break
		}
	}
	if task == nil {
		fmt.Fprintf(os.Stderr, "task %q not in goldset (%d tasks)\n", *taskID, len(ts))
		os.Exit(2)
	}
	if task.Verify.Kind == "pure" {
		fmt.Fprintln(os.Stderr, "pure task — use the in-suite engine, not mr-goldverify")
		os.Exit(2)
	}

	var patch []byte
	if *patchPath != "" {
		patch, err = os.ReadFile(*patchPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "patch read: %v\n", err)
			os.Exit(2)
		}
	}

	verdict := runExec(*task, patch, repoDir(task.Verify.Repo, overrides), *timeoutSec)
	b, _ := json.MarshalIndent(verdict, "", "  ")
	fmt.Println(string(b))
	if !verdict.Pass {
		os.Exit(1)
	}
}
