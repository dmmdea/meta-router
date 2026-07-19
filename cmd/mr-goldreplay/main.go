// Command mr-goldreplay builds the V2 all-lanes oracle replay table: every
// gold task × every requested lane × N trials, each dispatch routed through
// mr-orchestrate run (so the quota ledger meters it and a receipt lands) and
// each output judged by the task's verifier — the pure engine in-process, or
// mr-goldverify for execution tasks. Rows append to oracle.jsonl; the runner
// is RESUMABLE (existing rows are skipped), and a deferred admission (exit 3)
// is recorded and skipped, never hammered.
//
//	mr-goldreplay -goldset <path> -lanes local,claude -trials 1 [-out oracle.jsonl]
//
// The replay-oracle Direct Method is the field-standard router eval (slice-4
// brief §3.3; decision record Q8): this dense R[task][lane] table IS the
// reward model every counterfactual policy is evaluated against.
package main

import (
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

// Row is one oracle observation.
type Row struct {
	TS           string `json:"ts"`
	Task         string `json:"task"`
	Class        string `json:"class"`
	Lane         string `json:"lane"`
	Model        string `json:"model"`
	Trial        int    `json:"trial"`
	Dispatched   bool   `json:"dispatched"`
	OutcomeClass string `json:"outcome_class"` // ok | deferred | error | <lane outcome>
	VerifierPass bool   `json:"verifier_pass"`
	LatencyMs    int64  `json:"latency_ms"`
	Note         string `json:"note,omitempty"`
}

func rowKey(task, lane string, trial int) string {
	return fmt.Sprintf("%s|%s|%d", task, lane, trial)
}

// loadDone reads an existing oracle file and returns the set of recorded
// (task,lane,trial) keys, so a rerun only fills the holes.
func loadDone(path string) map[string]bool {
	done := map[string]bool{}
	b, err := os.ReadFile(path)
	if err != nil {
		return done
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Row
		if json.Unmarshal([]byte(line), &r) == nil && r.Task != "" {
			done[rowKey(r.Task, r.Lane, r.Trial)] = true
		}
	}
	return done
}

// extractDiff pulls the unified diff out of an agent's output (prompts demand
// one, but prose may surround it). Empty when no diff marker is present.
func extractDiff(text string) string {
	for _, marker := range []string{"diff --git", "\n--- a/"} {
		if i := strings.Index(text, marker); i >= 0 {
			return strings.TrimLeft(text[i:], "\n")
		}
	}
	return ""
}

// routerClass maps a gold-task class to the router's task-class vocabulary
// (receipt/classifier input only — the lane is forced by the replay).
func routerClass(goldClass string) string {
	switch goldClass {
	case "agentic-coding", "quick-edit":
		return "workhorse-coding"
	case "research":
		return "deep-reasoning"
	case "extraction":
		return "mechanical-text"
	case "review":
		return "verify-gate"
	}
	return ""
}

func main() {
	goldset := flag.String("goldset", "testdata/routing-goldset.jsonl", "gold-set JSONL (point at the private repo's copy)")
	outPath := flag.String("out", "oracle.jsonl", "oracle table output (appended; resume skips recorded rows)")
	lanesFlag := flag.String("lanes", "local", "comma-separated lanes: local,claude,codex,glm")
	trials := flag.Int("trials", 1, "trials per (task,lane); resume adds more later (Q8 CI-width stopping)")
	tasksFlag := flag.String("tasks", "", "comma-separated task IDs filter (empty = all)")
	classesFlag := flag.String("classes", "", "comma-separated gold classes filter (empty = all)")
	orchBin := flag.String("orchestrate", defaultHomeBin("mr-orchestrate.exe"), "mr-orchestrate binary")
	verifyBin := flag.String("goldverify", defaultHomeBin("mr-goldverify.exe"), "mr-goldverify binary (exec tasks)")
	reposFlag := flag.String("repos", "", "logical repo overrides for exec tasks: name=path,...")
	claudeModel := flag.String("claude-model", "claude-sonnet-5", "model pin for the claude lane")
	codexModel := flag.String("codex-model", "gpt-5.5", "model pin for the codex lane")
	glmModel := flag.String("glm-model", "glm-5.2", "model pin for the glm lane")
	localModel := flag.String("local-model", "gemma4-cascade", "model tag for the local lane")
	timeoutSec := flag.Int("timeout", 900, "per-dispatch timeout (seconds)")
	maxNotional := flag.Float64("max-notional", 10, "claude-lane notional guard ceiling (real coding tasks exceed the $2 default)")
	claudeExtra := flag.String("claude-extra", "--dangerously-skip-permissions",
		"extra claude-lane flags via run -extra (headless replay agents work tool-enabled in disposable worktrees; empty to disable)")
	flag.Parse()

	tasks, err := goldtask.Load(*goldset)
	if err != nil {
		fatal("goldset load: %v", err)
	}
	if err := goldtask.Validate(tasks); err != nil {
		fatal("goldset invalid: %v", err)
	}
	taskFilter := csvSet(*tasksFlag)
	classFilter := csvSet(*classesFlag)
	laneModel := map[string]string{
		"claude": *claudeModel, "codex": *codexModel, "glm": *glmModel, "local": *localModel,
	}
	var lanes []string
	for _, l := range strings.Split(*lanesFlag, ",") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if _, ok := laneModel[l]; !ok {
			fatal("unknown lane %q", l)
		}
		lanes = append(lanes, l)
	}

	done := loadDone(*outPath)
	out, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("open out: %v", err)
	}
	defer out.Close()

	total, run, skip := 0, 0, 0
	for _, t := range tasks {
		if len(taskFilter) > 0 && !taskFilter[t.ID] {
			continue
		}
		if len(classFilter) > 0 && !classFilter[t.Class] {
			continue
		}
		for _, lane := range lanes {
			for trial := 1; trial <= *trials; trial++ {
				total++
				if done[rowKey(t.ID, lane, trial)] {
					skip++
					continue
				}
				row := replayOne(t, lane, laneModel[lane], trial, *orchBin, *verifyBin, *reposFlag, *timeoutSec, *maxNotional, *claudeExtra)
				b, _ := json.Marshal(row)
				fmt.Fprintln(out, string(b))
				run++
				fmt.Printf("[%s %s trial %d] dispatched=%v outcome=%s pass=%v (%dms) %s\n",
					row.Task, row.Lane, row.Trial, row.Dispatched, row.OutcomeClass, row.VerifierPass, row.LatencyMs, row.Note)
			}
		}
	}
	fmt.Printf("\nreplay complete: %d cells (%d run now, %d already recorded) → %s\n", total, run, skip, *outPath)
}

// replayOne runs one (task,lane,trial) cell end to end.
func replayOne(t goldtask.Task, lane, model string, trial int, orchBin, verifyBin, repos string, timeoutSec int, maxNotional float64, claudeExtra string) Row {
	row := Row{TS: time.Now().UTC().Format(time.RFC3339), Task: t.ID, Class: t.Class,
		Lane: lane, Model: model, Trial: trial}
	start := time.Now()

	// Exec tasks get a fresh agent worktree at the parent commit as cwd.
	cwd := ""
	if t.Verify.Kind != "pure" {
		repoPath := repoDir(t.Verify.Repo, repos)
		wt := filepath.Join(os.TempDir(), fmt.Sprintf("goldreplay-%s-%s-%d", strings.ToLower(t.ID), lane, trial))
		if out, err := gitC(repoPath, timeoutSec, "worktree", "add", "--detach", wt, t.Verify.Parent); err != nil {
			row.OutcomeClass = "error"
			row.Note = "agent worktree: " + firstLine(out, err)
			row.LatencyMs = time.Since(start).Milliseconds()
			return row
		}
		defer func() { _, _ = gitC(repoPath, 60, "worktree", "remove", "--force", wt) }()
		cwd = wt
	}

	args := []string{"run", t.Prompt, "-lane", lane, "-model", model, "-live",
		"-origin", "goldreplay", "-desc", "goldreplay " + t.ID,
		"-max-notional-usd", fmt.Sprintf("%g", maxNotional)}
	if lane == "claude" && claudeExtra != "" {
		args = append(args, "-extra", claudeExtra)
	}
	if rc := routerClass(t.Class); rc != "" {
		args = append(args, "-class", rc)
	}
	if cwd != "" {
		args = append(args, "-cwd", cwd)
	}
	cmd := exec.Command(orchBin, args...)
	outB, runErr := cmd.CombinedOutput()
	stdout := string(outB)
	row.LatencyMs = time.Since(start).Milliseconds()

	exit := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			row.OutcomeClass = "error"
			row.Note = "spawn: " + runErr.Error()
			return row
		}
	}
	switch exit {
	case 0:
		row.Dispatched = true
		row.OutcomeClass = "ok"
	case 3:
		row.OutcomeClass = "deferred" // admission closed — recorded, never hammered
		row.Note = firstLine(outB, nil)
		return row
	case 5:
		row.Dispatched = true
		row.OutcomeClass = "dispatched-not-ok"
	default:
		row.OutcomeClass = fmt.Sprintf("exit-%d", exit)
		row.Note = firstLine(outB, nil)
		return row
	}

	// Verify: pure in-process; exec via mr-goldverify on the extracted diff.
	if t.Verify.Kind == "pure" {
		row.VerifierPass = goldtask.PureCheck(t.Verify, stdout)
		return row
	}
	diff := extractDiff(stdout)
	if diff == "" {
		row.Note = "no diff in output"
		return row
	}
	pf := filepath.Join(os.TempDir(), fmt.Sprintf("goldreplay-%s-%s-%d.diff", strings.ToLower(t.ID), lane, trial))
	if err := os.WriteFile(pf, []byte(diff), 0o644); err != nil {
		row.Note = "write diff: " + err.Error()
		return row
	}
	defer os.Remove(pf)
	vArgs := []string{"-goldset", flagLookup("goldset"), "-task", t.ID, "-patch", pf}
	if repos != "" {
		vArgs = append(vArgs, "-repos", repos)
	}
	vc := exec.Command(verifyBin, vArgs...)
	vOut, vErr := vc.CombinedOutput()
	if vErr == nil {
		row.VerifierPass = true
	} else if ee, ok := vErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		row.VerifierPass = false
	} else {
		row.Note = "goldverify: " + firstLine(vOut, vErr)
	}
	return row
}

// ── small helpers ─────────────────────────────────────────────────────────

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(2)
}

func defaultHomeBin(name string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return name
	}
	return filepath.Join(home, ".meta-router", "bin", name)
}

func csvSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out[x] = true
		}
	}
	return out
}

func gitC(dir string, timeoutSec int, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func repoDir(name, overrides string) string {
	for _, kv := range strings.Split(overrides, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && k == name {
			return v
		}
	}
	if name == "meta-router" {
		return "."
	}
	return filepath.Join("..", name)
}

func firstLine(b []byte, err error) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" && err != nil {
		s = err.Error()
	}
	return s
}

// flagLookup returns a set flag's value (the goldset path for the verifier call).
func flagLookup(name string) string {
	f := flag.Lookup(name)
	if f == nil {
		return ""
	}
	return f.Value.String()
}
