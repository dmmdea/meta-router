// Command mr-goldreplay builds the V2 all-lanes oracle replay table: every
// gold task × every requested lane × N trials, each dispatch routed through
// mr-orchestrate run (so the quota ledger meters it and a receipt lands) and
// each output judged by the task's verifier — the pure engine in-process, or
// mr-goldverify for execution tasks. Rows append to oracle.jsonl; the runner
// is RESUMABLE (existing rows are skipped), and a deferred admission (exit 3)
// is recorded and skipped, never hammered.
//
// Every replayed lane MUST carry an explicit model pin (exit 2 otherwise): the
// oracle row records the pin, so an unpinned lane writes evidence under a model
// nobody chose.
//
//	mr-goldreplay -goldset <path> -lanes local,claude -trials 1 \
//	  -local-model gemma4-cascade -claude-model claude-sonnet-5 [-out oracle.jsonl]
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
	"github.com/dmmdea/meta-router/internal/policyeval"
)

const version = "0.1.0"

// Row is one oracle observation.
type Row struct {
	TS           string `json:"ts"`
	Task         string `json:"task"`
	Class        string `json:"class"`
	Lane         string `json:"lane"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Trial        int    `json:"trial"`
	Dispatched   bool   `json:"dispatched"`
	OutcomeClass string `json:"outcome_class"` // ok | deferred | error | <lane outcome>
	VerifierPass bool   `json:"verifier_pass"`
	LatencyMs    int64  `json:"latency_ms"`
	Note         string `json:"note,omitempty"`
}

// rowKey identifies an oracle cell. MODEL AND EFFORT ARE PART OF THE IDENTITY:
// a mandatory pin that is not in the key is a no-op, because resume then treats
// a row recorded under a DIFFERENT configuration as already-done. Review
// 2026-07-27 reproduced exactly that for the model — with a claude-sonnet-5 row
// present, a rerun with the corrected `-claude-model claude-opus-4-8` reported
// "0 run now, 1 already recorded" and exited 0, leaving the mislabelled row
// standing while looking fixed. Effort joins the key here because it is now
// both RECORDED on the row and APPLIED to the dispatch (replayArgs); recording
// it without sending it would have been strictly worse than omitting it.
func rowKey(task, lane, model, effort string, trial int) string {
	return fmt.Sprintf("%s|%s|%s|%s|%d", task, lane, model, effort, trial)
}

// rowIsEvidence is the ONE definition of "this cell was measured", shared by
// the resume set here and mirrored by mr-scorecard's oracleRow.ran. A cell
// that did not dispatch, deferred, or ended in an error/exit-N is a HOLE:
// never counted as measurement, and always re-attemptable.
func rowIsEvidence(r Row) bool {
	if !r.Dispatched {
		return false
	}
	switch r.OutcomeClass {
	case "deferred", "error", "spawn_error", "config_error", "parse_error", "api_error":
		return false
	}
	return !strings.HasPrefix(r.OutcomeClass, "exit-")
}

// loadDone reads an existing oracle file and returns the set of recorded
// (task,lane,model,effort,trial) keys, so a rerun only fills the holes.
//
// The effort is normalized on the way in, the SAME way the planned cell's is:
// the 825 legacy rows carry no `effort` key at all, so without this every one
// of them lands under a key ending "...|" that no planned cell can match, and
// resume silently stops recognizing the entire existing table.
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
		// The SAME evidence definition the scorecard applies (oracleRow.ran).
		// Treating only "deferred" as a hole left error/exit-N cells
		// simultaneously "already recorded" (resume never refills them) and
		// "not evidence" (the scorecard and B15 ignore them) — a hole that
		// can never fill. They are holes on BOTH sides now, so a rerun
		// re-attempts them; -plan-only shows the count before it costs
		// anything (review 2026-08-12).
		if json.Unmarshal([]byte(line), &r) == nil && r.Task != "" && rowIsEvidence(r) {
			// A deferred row is a HOLE (admission was closed), not an
			// observation — resume must refill it when the window reopens.
			done[rowKey(r.Task, r.Lane, r.Model, policyeval.NormalizeEffort(r.Effort), r.Trial)] = true
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
	// Effort follows the model's rule exactly, and for the same reason: the row
	// records the pin, so an unpassed flag writes evidence under a configuration
	// nobody chose. A lane with no effort dial (local) is declared explicitly as
	// `unrecorded` — saying "this ran at no recorded effort" is a deliberate act,
	// where a silent default is precisely the mislabelling this gate exists for.
	effortHelp := func(lane string) string {
		return "effort pin for the " + lane + " lane (REQUIRED when -lanes includes " + lane +
			"; pass " + policyeval.EffortUnrecorded + " when the lane has no effort dial)"
	}
	claudeEffort := flag.String("claude-effort", "", effortHelp("claude"))
	codexEffort := flag.String("codex-effort", "", effortHelp("codex"))
	glmEffort := flag.String("glm-effort", "", effortHelp("glm"))
	localEffort := flag.String("local-effort", "", effortHelp("local"))
	migrateEffortPath := flag.String("migrate-effort", "",
		"MIGRATION MODE: stamp effort=\""+policyeval.EffortUnrecorded+"\" on every row of this oracle file that has none, then exit (idempotent; writes <path>.tmp and renames)")
	timeoutSec := flag.Int("timeout", 900, "per-dispatch timeout (seconds)")
	maxNotional := flag.Float64("max-notional", 10, "claude-lane notional guard ceiling (real coding tasks exceed the $2 default)")
	claudeExtra := flag.String("claude-extra", "--dangerously-skip-permissions",
		"extra claude-lane flags via run -extra (headless replay agents work tool-enabled in disposable worktrees; empty to disable)")
	planOnly := flag.Bool("plan-only", false, "print the cells this run WOULD dispatch and exit 0 — the zero-spend way to check a resume before an unattended run")
	reMeasure := flag.Bool("re-measure", false, "proceed even when the resume set matched nothing (deliberate full re-spend)")
	flag.Parse()

	// Migration is a pure file rewrite: it must not require a goldset, lanes or
	// pins, and it must never dispatch anything.
	if *migrateEffortPath != "" {
		changed, err := migrateEffortFile(*migrateEffortPath)
		if err != nil {
			fatal("migrate-effort: %v", err)
		}
		fmt.Printf("migrate-effort: %d row(s) stamped effort=%q → %s\n",
			changed, policyeval.EffortUnrecorded, *migrateEffortPath)
		return
	}

	tasks, err := goldtask.Load(*goldset)
	if err != nil {
		fatal("goldset load: %v", err)
	}
	if err := goldtask.Validate(tasks); err != nil {
		fatal("goldset invalid: %v", err)
	}
	taskFilter := csvSet(*tasksFlag)
	classFilter := csvSet(*classesFlag)
	// Trim for USE, not just for validation. The gate below checks
	// TrimSpace(pin); if the raw value were carried on, a padded pin would pass
	// validation and then be RECORDED and DISPATCHED untrimmed, producing a
	// model string unequal to every other row's — the same mislabelling this
	// gate exists to prevent, one layer down (review 2026-07-27).
	rawPins := map[string]policyeval.Config{
		"claude": {Lane: "claude", Model: *claudeModel, Effort: *claudeEffort},
		"codex":  {Lane: "codex", Model: *codexModel, Effort: *codexEffort},
		"glm":    {Lane: "glm", Model: *glmModel, Effort: *glmEffort},
		"local":  {Lane: "local", Model: *localModel, Effort: *localEffort},
	}
	// rawPins is passed on UNNORMALIZED and that is deliberate: buildRunPlan does
	// its own normalization for the dispatch decision, and requireConfigPins
	// TrimSpaces internally. Normalizing here too would be dead code that looks
	// load-bearing — an equivalent mutant a future reader would try to "fix".
	lanes := parseLanes(*lanesFlag)
	for _, l := range lanes {
		if _, ok := rawPins[l]; !ok {
			fatal("unknown lane %q", l)
		}
	}
	if missing := requireConfigPins(lanes, rawPins); len(missing) > 0 {
		fatal("model AND effort pins required for lane(s) %s: the oracle row records the PINS, so "+
			"replaying a lane without them writes evidence under a configuration nobody chose (this "+
			"is how 204 claude rows recorded sonnet-5 while the rank table dispatched opus-4-8). "+
			"Pass %s", strings.Join(missing, ", "), pinFlagsFor(missing))
	}

	done := loadDone(*outPath)
	out, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("open out: %v", err)
	}
	defer out.Close()

	plan := buildRunPlan(tasks, *lanesFlag, rawPins, *trials, taskFilter, classFilter, done)

	// The plan is what a replay WOULD do, decided before anything dispatches —
	// so SAY it before dispatching, not in a summary line after the loop that
	// an unattended weekly driver never reads.
	fmt.Fprintf(os.Stderr, "plan: %d cells (%d to run, %d already recorded)\n",
		plan.Total, len(plan.Run), plan.Skipped)
	// The mismatch is detected BEFORE the plan-only exit. -plan-only is the
	// pre-flight the guard's own error message tells you to run, so it must
	// SHOW the condition the guard trips on; returning first made the safety
	// tool structurally blind to the hazard it exists to reveal.
	resumeMismatch := len(done) > 0 && plan.Skipped == 0 && len(plan.Run) > 0
	if len(done) > 0 {
		fmt.Fprintf(os.Stderr, "resume: %d recorded cell(s) in the output oracle, %d matched this run's keys\n", len(done), plan.Skipped)
	}
	if resumeMismatch {
		fmt.Fprintf(os.Stderr, "RESUME MISMATCH: none of the %d recorded cells match the (task, lane, model, EFFORT, trial) keys this run would write\n", len(done))
	}
	if *planOnly {
		fmt.Printf("plan-only: %d cells (%d would run, %d already recorded, %d recorded in file) → %s\n",
			plan.Total, len(plan.Run), plan.Skipped, len(done), *outPath)
		if resumeMismatch {
			fmt.Printf("plan-only: RESUME MISMATCH — a live run would REFUSE (pass -re-measure to override)\n")
		}
		return
	}

	// RESUME-KEY MISMATCH GUARD. The resume key includes effort. An oracle
	// recorded before effort capture resumes as "unrecorded"; pinning a real
	// effort makes every key miss, so nothing is skipped and the ENTIRE table
	// re-dispatches — 476 cloud cells on this project's live oracle, tool
	// enabled, unattended, discovered only by the bill (review 2026-08-12).
	//
	// The tell is unambiguous: a non-empty resume set that matched NOTHING.
	// That is either a genuine first run against someone else's oracle or a
	// key mismatch, and the safe reading is the expensive one.
	if resumeMismatch {
		if !*reMeasure {
			fatal("resume matched NOTHING: the output oracle already holds %d recorded cells, "+
				"but none match the (task, lane, model, EFFORT, trial) keys this run would write, "+
				"so all %d cells would re-dispatch and re-spend. The usual cause is an effort pin "+
				"that disagrees with the recorded rows (rows written before effort capture resume "+
				"as %q). Check with -plan-only, pin the effort the rows actually carry, or pass "+
				"-re-measure if re-spending is genuinely what you want.",
				len(done), len(plan.Run), policyeval.EffortUnrecorded)
		}
		fmt.Fprintf(os.Stderr, "WARNING: -re-measure: resume matched nothing; re-dispatching all %d cells\n", len(plan.Run))
	}

	for _, c := range plan.Run {
		row := replayOne(c.GoldTask, c.Config, c.Trial, *orchBin, *verifyBin, *reposFlag, *timeoutSec, *maxNotional, *claudeExtra)
		b, _ := json.Marshal(row)
		fmt.Fprintln(out, string(b))
		fmt.Printf("[%s %s/%s/%s trial %d] dispatched=%v outcome=%s pass=%v (%dms) %s\n",
			row.Task, row.Lane, row.Model, row.Effort, row.Trial, row.Dispatched, row.OutcomeClass, row.VerifierPass, row.LatencyMs, row.Note)
	}
	fmt.Printf("\nreplay complete: %d cells (%d run now, %d already recorded) → %s\n",
		plan.Total, len(plan.Run), plan.Skipped, *outPath)
}

// plannedCell is one (task, config, trial) the replay will dispatch.
type plannedCell struct {
	Task     string
	Config   policyeval.Config
	Trial    int
	GoldTask goldtask.Task
}

// Lane and Model read the planned cell's config; the fields moved into Config
// when the evidence cell became (lane, model, effort).
func (c plannedCell) Lane() string  { return c.Config.Lane }
func (c plannedCell) Model() string { return c.Config.Model }

// runPlan is what a replay WOULD do, decided before anything is dispatched.
type runPlan struct {
	Run     []plannedCell
	Skipped int
	Total   int
}

// buildRunPlan resolves filters, the per-lane model pin and the resume set into
// the exact cells that will be dispatched.
//
// This is a FUNCTION rather than an inline loop because the defects lived at
// the CALL SITES, not in the helpers. Review 2026-07-27 mutation-tested three
// reverts against the previous shape — the resume lookup dropping the model,
// `normalizePins` deleted, `parseLanes` replaced by `strings.Split` — and all
// three compiled and left the ENTIRE repo suite green, while a binary built
// from the first one re-dispatched all 224 already-recorded cells. Unit tests
// on rowKey/normalizePins/parseLanes could not see any of it. This repo already
// holds its canaries to call-site mutation testing (see internal/canary's
// StripGoComments note); the same standard belongs here.
// It takes the RAW -lanes string and the RAW pins and normalizes them itself,
// deliberately: if it accepted already-normalized inputs, deleting the
// normalization from main() would leave every test green again (MUT-2/MUT-3).
// The dispatch decision owns its own normalization.
func buildRunPlan(tasks []goldtask.Task, lanesCSV string, rawPins map[string]policyeval.Config,
	trials int, taskFilter, classFilter, done map[string]bool) runPlan {
	lanes := parseLanes(lanesCSV)
	laneCfg := normalizePins(rawPins)
	var p runPlan
	for _, t := range tasks {
		if len(taskFilter) > 0 && !taskFilter[t.ID] {
			continue
		}
		if len(classFilter) > 0 && !classFilter[t.Class] {
			continue
		}
		for _, lane := range lanes {
			for trial := 1; trial <= trials; trial++ {
				p.Total++
				cfg := laneCfg[lane]
				cfg.Lane = lane // the -lanes list owns the lane, not the pin map's key
				if done[rowKey(t.ID, cfg.Lane, cfg.Model, cfg.Effort, trial)] {
					p.Skipped++
					continue
				}
				p.Run = append(p.Run, plannedCell{
					Task: t.ID, Config: cfg, Trial: trial, GoldTask: t})
			}
		}
	}
	return p
}

// replayArgs builds the `mr-orchestrate run` argv for one cell. It is a
// separate function because the defect it guards is invisible in the row: an
// effort we RECORD but never SEND produces a row asserting a configuration that
// never ran — strictly worse than recording no effort at all, because it looks
// like evidence. Testing the row is not enough; the ARGV is the claim.
//
// EffortUnrecorded is a marker, not an effort: it must never be dispatched, or
// the orchestrator forwards the literal string "unrecorded" to the CLI. The
// lane arg builders emit --effort / model_reasoning_effort only when non-empty,
// so omitting it here is exactly "the lane's own default ran", which is what
// the unrecorded marker means.
func replayArgs(taskID string, cfg policyeval.Config, prompt string, maxNotional float64) []string {
	args := []string{"run", prompt, "-lane", cfg.Lane, "-model", cfg.Model}
	if cfg.Effort != policyeval.EffortUnrecorded && strings.TrimSpace(cfg.Effort) != "" {
		args = append(args, "-effort", cfg.Effort)
	}
	return append(args, "-live", "-origin", "goldreplay", "-desc", "goldreplay "+taskID,
		"-max-notional-usd", fmt.Sprintf("%g", maxNotional))
}

// replayOne runs one (task,config,trial) cell end to end.
func replayOne(t goldtask.Task, cfg policyeval.Config, trial int, orchBin, verifyBin, repos string, timeoutSec int, maxNotional float64, claudeExtra string) Row {
	lane, model := cfg.Lane, cfg.Model
	row := Row{TS: time.Now().UTC().Format(time.RFC3339), Task: t.ID, Class: t.Class,
		Lane: lane, Model: model, Effort: cfg.Effort, Trial: trial}
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

	args := replayArgs(t.ID, cfg, t.Prompt, maxNotional)
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

// normalizePins trims every pin so the value VALIDATED is the value RECORDED
// and DISPATCHED. Returns a new map; the caller's is not mutated.
//
// Effort goes through policyeval.NormalizeEffort — the SAME function the oracle
// reader and the scorecard's rank-table side use. Symmetry is the whole point:
// normalizing one side only leaves the other's keys ending "...|", which match
// nothing and are permanently uncoverable.
func normalizePins(in map[string]policyeval.Config) map[string]policyeval.Config {
	out := make(map[string]policyeval.Config, len(in))
	for lane, pin := range in {
		out[lane] = policyeval.Config{
			Lane:   strings.TrimSpace(pin.Lane),
			Model:  strings.TrimSpace(pin.Model),
			Effort: policyeval.NormalizeEffort(pin.Effort),
		}
	}
	return out
}

// parseLanes splits the -lanes list, trimming blanks and DEDUPING while
// preserving first-seen order. Without the dedupe, `-lanes claude,claude`
// replays the same cell twice in one run (the second dispatch is not yet in
// `done`, which is loaded once before the loop) and repeats the flag name in
// the pin error.
func parseLanes(csv string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(csv, ",") {
		l = strings.TrimSpace(l)
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// requireConfigPins returns the lanes being replayed that carry an incomplete
// config pin — missing model OR missing effort — in the caller's lane order
// (deterministic message). Only lanes actually in the run are required, so
// `-lanes claude` needs no glm pin.
//
// It reads the RAW pins deliberately. Running it after normalization would be
// vacuous: NormalizeEffort turns blank into EffortUnrecorded, so a lane whose
// effort was never passed would silently pass the gate and record `unrecorded`
// as though the operator had chosen it.
func requireConfigPins(lanes []string, laneCfg map[string]policyeval.Config) []string {
	var missing []string
	for _, l := range lanes {
		c := laneCfg[l]
		if strings.TrimSpace(c.Model) == "" || strings.TrimSpace(c.Effort) == "" {
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
		flags = append(flags, "-"+l+"-model <id> -"+l+"-effort <level|"+policyeval.EffortUnrecorded+">")
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
