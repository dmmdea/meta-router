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
	"github.com/dmmdea/meta-router/internal/orch/childenv"
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
		if json.Unmarshal([]byte(line), &r) == nil && r.Task != "" && r.OutcomeClass != "deferred" {
			// A deferred row is a HOLE (admission was closed), not an
			// observation — resume must refill it when the window reopens.
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

// truncateDiff cuts a printed diff at the first line that violates unified-diff
// grammar (agents narrate after the final hunk; git apply calls that corrupt).
func truncateDiff(d string) string {
	if d == "" {
		return ""
	}
	prefixes := []string{"diff --git", "index ", "--- ", "+++ ", "@@ ", "+", "-", " ",
		"new file mode", "deleted file mode", "old mode", "new mode", "similarity ",
		"rename ", "copy ", "Binary files", "\\ No newline"}
	var out []string
	for _, line := range strings.Split(d, "\n") {
		if line == "" { // blank lines end a printed diff (context lines keep their leading space)
			break
		}
		ok := false
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				ok = true
				break
			}
		}
		if !ok {
			break
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// decodeAgentText recovers the agent's REAL text from structured lane stdout:
// codex event JSONL carries it JSON-escaped in item.text (agent_message), and
// claude/glm result JSON carries it in .result — extracting a diff from the
// RAW stream yields escaped garbage ("git diff header lacks filename").
// Returns "" when the output has no decodable agent text.
func decodeAgentText(stdout string) string {
	var parts []string
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Item *struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Result string `json:"result"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Item != nil && ev.Item.Type == "agent_message" && ev.Item.Text != "" {
			parts = append(parts, ev.Item.Text)
		} else if ev.Type == "result" && ev.Result != "" {
			parts = append(parts, ev.Result)
		}
	}
	return strings.Join(parts, "\n")
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
	// NO DEFAULTS. A default model pin is not a convenience, it is a mislabelled
	// oracle: the row records the PIN, so an unpassed flag writes evidence under
	// a model nobody chose. That is not hypothetical — `-claude-model` defaulted
	// to claude-sonnet-5 and the A2 weekly script passed -codex-model and
	// -glm-model but not -claude-model, so all 204 claude observations recorded
	// Sonnet 5 while the seed rank table dispatched claude-opus-4-8. The router's
	// Opus decisions were scored with Sonnet's results for the life of the table.
	// claudelane/args.go already refuses an unpinned model for exactly this
	// reason; the flag default reintroduced the trap one layer up (audit
	// 2026-07-27). Required per lane actually replayed — see requireModelPins.
	claudeModel := flag.String("claude-model", "", "model pin for the claude lane (REQUIRED when -lanes includes claude)")
	codexModel := flag.String("codex-model", "", "model pin for the codex lane (REQUIRED when -lanes includes codex)")
	glmModel := flag.String("glm-model", "", "model pin for the glm lane (REQUIRED when -lanes includes glm)")
	localModel := flag.String("local-model", "", "model tag for the local lane (REQUIRED when -lanes includes local)")
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
	if missing := requireModelPins(lanes, laneModel); len(missing) > 0 {
		fatal("model pin required for lane(s) %s: the oracle row records the PIN, so replaying "+
			"a lane without one writes evidence under a model nobody chose (this is how 204 claude "+
			"rows recorded sonnet-5 while the rank table dispatched opus-4-8). Pass %s",
			strings.Join(missing, ", "), pinFlagsFor(missing))
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
	// The replay spawns the ORCHESTRATOR, which then spawns lane binaries. It
	// scrubs again downstream, but a weekly unattended run is the last place to
	// rely on someone else's hygiene: an ambient ANTHROPIC_API_KEY here would be
	// one bug away from turning ~150 replay dispatches into metered spend (R10).
	cmd.Env = childenv.Scrub(os.Environ())
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
	case 6:
		// Egress-denied: the data-boundary gate refused to send this task's
		// working directory to a third-party lane. Like a deferral this is a
		// HOLE, not an observation. Recording it as a failure would poison the
		// oracle permanently — loadDone never refills a non-deferred row — and
		// would make every third-party exec cell read as incompetent, which
		// then drives the weekly A2 alarm and any B8 promotion eval off a lie
		// (review 2026-07-25). The replay worktree lives under TEMP, so this
		// fires for EVERY exec task on glm until TEMP (or an -agent-worktree
		// root) is allowlisted.
		row.OutcomeClass = "deferred"
		row.Note = "egress-denied (hole, not a failure): " + firstLine(outB, nil)
		return row
	case 5:
		row.Dispatched = true
		row.OutcomeClass = "dispatched-not-ok"
	default:
		row.OutcomeClass = fmt.Sprintf("exit-%d", exit)
		row.Note = firstLine(outB, nil)
		return row
	}

	// Verify: pure in-process; exec via mr-goldverify on the candidate diff.
	if t.Verify.Kind == "pure" {
		row.VerifierPass = goldtask.PureCheck(t.Verify, stdout)
		return row
	}
	// Prefer the WORKTREE diff: a tool-enabled agent edits files in place and
	// rarely prints a diff. `add -N` makes new files diffable; the leakage
	// guard downstream still rejects test-file tampering.
	diff := ""
	if cwd != "" {
		_, _ = gitC(cwd, 60, "add", "-N", ".")
		if b, err := gitC(cwd, 120, "diff", "HEAD"); err == nil && len(strings.TrimSpace(string(b))) > 0 {
			diff = string(b) // RAW bytes — git apply needs the trailing newline
		}
	}
	if diff == "" {
		// Fallback: the agent PRINTED its diff. Decode structured lane output
		// first (codex events / result JSON) so the diff isn't JSON-escaped,
		// then cut trailing prose — agents narrate after the last hunk and
		// git apply rejects it as a corrupt patch.
		if txt := decodeAgentText(stdout); txt != "" {
			diff = truncateDiff(extractDiff(txt))
		}
	}
	if diff == "" {
		diff = truncateDiff(extractDiff(stdout)) // last resort: raw stream
	}
	if diff == "" {
		row.Note = "no diff in output"
		return row
	}
	if !strings.HasSuffix(diff, "\n") {
		diff += "\n" // git apply requires the trailing newline
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
	vc.Env = childenv.Scrub(os.Environ())
	vOut, vErr := vc.CombinedOutput()
	if vErr == nil {
		row.VerifierPass = true
	} else if ee, ok := vErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		row.VerifierPass = false
		row.Note = "verify-fail: " + verdictDetail(vOut) // WHY it failed, never silent
	} else {
		row.Note = "goldverify: " + firstLine(vOut, vErr)
	}
	return row
}

// verdictDetail extracts the failure stage from goldverify's verdict JSON.
func verdictDetail(out []byte) string {
	var v struct {
		Detail string `json:"detail"`
	}
	if json.Unmarshal(out, &v) == nil && v.Detail != "" {
		s := strings.ReplaceAll(v.Detail, "\n", " | ")
		if len(s) > 220 {
			s = s[:220]
		}
		return s
	}
	return firstLine(out, nil)
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

// requireModelPins returns the lanes being replayed that carry no model pin, in
// the caller's lane order (deterministic message). Only lanes actually in the
// run are required, so `-lanes claude` needs no glm pin.
func requireModelPins(lanes []string, laneModel map[string]string) []string {
	var missing []string
	for _, l := range lanes {
		if strings.TrimSpace(laneModel[l]) == "" {
			missing = append(missing, l)
		}
	}
	return missing
}

// pinFlagsFor renders the flags the operator must pass, so the error is
// actionable rather than a diagnosis they have to translate.
func pinFlagsFor(lanes []string) string {
	flags := make([]string, 0, len(lanes))
	for _, l := range lanes {
		flags = append(flags, "-"+l+"-model <id>")
	}
	return strings.Join(flags, " ")
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
