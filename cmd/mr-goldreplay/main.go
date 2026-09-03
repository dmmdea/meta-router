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
	"sort"
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
	OutcomeClass string `json:"outcome_class"` // ok | dispatched-not-ok | deferred | error | verify_error | exit-N | <lane outcome>; holes per policyeval.IsEvidence
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

// rowIsEvidence delegates to policyeval.IsEvidence — the ONE evidence
// definition the whole stack shares (resume set here, mr-scorecard's ran,
// the B15 canary). Three drifting copies of the list are how error rows were
// simultaneously "already recorded" and "not evidence" (review 2026-08-12).
func rowIsEvidence(r Row) bool {
	return policyeval.IsEvidence(r.Dispatched, r.OutcomeClass)
}

// cellKey identifies a cell WITHOUT its effort — the index the effort-drift
// detector uses to tell "this (task,lane,model,trial) was recorded under a
// different effort" (a re-key: re-dispatching it re-spends) from "this cell
// was never recorded at any effort" (a genuinely new cell: a new lane, task,
// class filter or trial — a first measurement, not a re-spend). A new MODEL
// pin misses this index too, but it is NOT free: the model tier below owns
// it (see identKey). Keep this list identical to detectDrift's.
func cellKey(task, lane, model string, trial int) string {
	return fmt.Sprintf("%s|%s|%s|%d", task, lane, model, trial)
}

// modelUnrecorded is a DISPLAY string for a recorded row that carries no
// model, used only when formatting the refusal — never as an index key.
//
// It exists because "recorded under a different MODEL ()" named nothing and
// prescribed an impossible fix (review round 4). It is deliberately NOT
// stored in modelsByIdent the way policyeval.EffortUnrecorded is stored on
// the effort axis: the effort sentinel is a real, PINNABLE config value
// ("run at no recorded effort" is a choice an operator can make), whereas a
// model pin is mandatory and non-empty, so "no model" is not a configuration
// anyone can select. Indexing this string alongside real pins made it
// selectable — pinning it literally MATCHED a model-less row, silenced both
// drift tiers, and appended evidence under a fabricated model, turning a
// message fix into a hole in the money guard (review round 5). Blank rows
// index the raw "" instead, which requireConfigPins rejects as a pin, so no
// pin can ever match one.
const modelUnrecorded = "(no model recorded)"

// displayModel renders a recorded model for an operator-facing message.
func displayModel(m string) string {
	if m == "" {
		return modelUnrecorded
	}
	return m
}

// identKey identifies a cell without model OR effort — the index the
// MODEL-drift detector reads. A planned cell whose (task,lane,trial) is
// recorded only under other models is either a deliberate new-model
// measurement or a pin typo, and the two are indistinguishable mechanically —
// so the guard fails closed with -re-measure as the deliberate override. A
// wrong model pin is this tool's own documented root incident (204 sonnet
// rows recorded under opus decisions), and dropping v0.31.0's aggregate
// "resume matched NOTHING" refusal without this tier left a typo'd
// -claude-model free to re-dispatch the entire table unattended
// (review 2026-08-12, round 4).
func identKey(task, lane string, trial int) string {
	return fmt.Sprintf("%s|%s|%d", task, lane, trial)
}

// resumeState is what the output oracle already records, in the three shapes
// the resume and drift guards read.
type resumeState struct {
	done          map[string]bool            // full (task,lane,model,effort,trial) keys
	effortsByCell map[string]map[string]bool // cellKey → recorded efforts
	modelsByIdent map[string]map[string]bool // identKey → recorded models
}

// loadDone reads an existing oracle file into the resume state, so a rerun
// only fills the holes and the drift guards can tell a re-key from a new cell.
//
// Lane and model are TRIMMED exactly as the planned side trims its pins
// (normalizePins) and the scorecard trims on ingest (config()): a padded
// field in a recorded row otherwise builds keys no planned cell can match,
// which blinds BOTH guards on the one seam with money attached
// (review 2026-08-12, round 4).
//
// The effort is normalized on the way in, the SAME way the planned cell's is:
// the 825 legacy rows carry no `effort` key at all, so without this every one
// of them lands under a key ending "...|" that no planned cell can match, and
// resume silently stops recognizing the entire existing table.
//
// Only EVIDENCE rows enter any index (a hole is re-attemptable, not recorded
// — see policyeval.IsEvidence): re-dispatching a hole was always going to
// happen, so it is not a re-spend the guards need to refuse.
func loadDone(path string) resumeState {
	rs := resumeState{
		done:          map[string]bool{},
		effortsByCell: map[string]map[string]bool{},
		modelsByIdent: map[string]map[string]bool{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return rs
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Row
		if json.Unmarshal([]byte(line), &r) == nil && r.Task != "" && rowIsEvidence(r) {
			lane, model := strings.TrimSpace(r.Lane), strings.TrimSpace(r.Model)
			eff := policyeval.NormalizeEffort(r.Effort)
			rs.done[rowKey(r.Task, lane, model, eff, r.Trial)] = true
			ck := cellKey(r.Task, lane, model, r.Trial)
			if rs.effortsByCell[ck] == nil {
				rs.effortsByCell[ck] = map[string]bool{}
			}
			rs.effortsByCell[ck][eff] = true
			ik := identKey(r.Task, lane, r.Trial)
			if rs.modelsByIdent[ik] == nil {
				rs.modelsByIdent[ik] = map[string]bool{}
			}
			// The RAW model, blank included: the index is a matching
			// namespace, and requireConfigPins rejects an empty pin, so a
			// blank entry is unmatchable by construction. The display string
			// is substituted only when formatting (see modelUnrecorded).
			rs.modelsByIdent[ik][model] = true
		}
	}
	return rs
}

// driftReport is what detectDrift found: the planned cells that would
// RE-dispatch already-recorded work, split by which pin drifted, plus the
// recorded values for the error messages.
type driftReport struct {
	effortDrift []plannedCell // same (task,lane,model,trial), different effort
	// modelDrift: recorded only at other REAL models — dispatching ADDS a
	// first measurement and re-spends nothing.
	modelDrift []plannedCell
	// blankModelDrift: recorded WITHOUT any model. Split from modelDrift
	// because the guard's usual sentence is WRONG for these: an unlabeled row
	// says nothing about which model produced it, so it may well BE a
	// measurement at the pin (the same legacy-capture argument the effort
	// branch makes), which makes this the one branch where following the
	// advice can RE-BUY recorded work. Kept PER-CELL, not as a report-level
	// flag: a single unlabeled row must not rewrite the message for the real
	// typo cells in the same run, which do need "never measured at"
	// (review 2026-08-12, round 6).
	blankModelDrift []plannedCell
	recordedEfforts []string
	recordedModels  []string // display form; a blank recorded model shows as modelUnrecorded
}

// allModelDrift is every model-tier cell, both flavors — the count the
// operator-facing totals report.
func (d driftReport) allModelDrift() int { return len(d.modelDrift) + len(d.blankModelDrift) }

func (d driftReport) any() bool { return len(d.effortDrift)+d.allModelDrift() > 0 }

// detectDrift returns the planned cells whose identity is already recorded
// under a DIFFERENT pin. This replaces the v0.31.0 aggregate `plan.Skipped ==
// 0` tell, which was wrong in both directions (review 2026-08-12): it
// over-fired on every legitimate first measurement of new cells against a
// populated oracle (a new lane, task, class filter or trial has no recorded
// counterpart — nothing re-spends), and it was silent on PARTIAL drift (pin
// one lane's real effort while another lane still matches — exactly what an
// operator produces when fixing the incident the guard was written for).
//
// Two tiers, because two pins can re-key a recorded cell:
//   - EFFORT drift: same (task,lane,model,trial) recorded under another
//     effort — the 476-cell incident shape (rows written before effort
//     capture resume as "unrecorded"; a real pin re-keys them all).
//   - MODEL drift: same (task,lane,trial) recorded only under other models.
//     A typo'd -claude-model makes every planned cell look brand new, so
//     without this tier NOTHING refuses and the whole table re-dispatches —
//     the exact unattended full re-spend the guard exists to stop, plus every
//     row lands mislabeled (the 204-sonnet-rows incident). A deliberate
//     new-model measurement trips it too; that is fail-closed by design, and
//     -re-measure is the deliberate override.
//
// A cell in NEITHER index is genuinely new and passes freely.
func detectDrift(run []plannedCell, rs resumeState) driftReport {
	var d driftReport
	seenEff, seenModel := map[string]bool{}, map[string]bool{}
	for _, c := range run {
		if effs := rs.effortsByCell[cellKey(c.Task, c.Config.Lane, c.Config.Model, c.Trial)]; len(effs) > 0 && !effs[c.Config.Effort] {
			d.effortDrift = append(d.effortDrift, c)
			for e := range effs {
				if !seenEff[e] {
					seenEff[e] = true
					d.recordedEfforts = append(d.recordedEfforts, e)
				}
			}
			continue
		}
		if models := rs.modelsByIdent[identKey(c.Task, c.Config.Lane, c.Trial)]; len(models) > 0 && !models[c.Config.Model] {
			// PER-CELL split: this cell's own recorded set decides which
			// sentence it earns. ANY blank in the set puts the cell in the
			// unlabeled bucket, because the re-buy risk is what the operator
			// must act on: an unlabeled row may be a measurement at this very
			// pin, whether or not the cell ALSO has labeled rows. Requiring
			// the set to be blank-only sent a mixed cell to the bucket whose
			// message omits that warning — failing toward under-warning about
			// spend, which is the wrong direction on this tool
			// (review 2026-08-12, round 6).
			if models[""] {
				d.blankModelDrift = append(d.blankModelDrift, c)
			} else {
				d.modelDrift = append(d.modelDrift, c)
			}
			for m := range models {
				if !seenModel[m] {
					seenModel[m] = true
					// Display form only at the formatting boundary; the index
					// itself keeps the raw value (see modelUnrecorded).
					d.recordedModels = append(d.recordedModels, displayModel(m))
				}
			}
		}
	}
	sort.Strings(d.recordedEfforts)
	sort.Strings(d.recordedModels)
	return d
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
// codex event JSONL carries it JSON-escaped in item.text (agent_message),
// claude/glm result JSON carries it in .result, and copilot's JSONL carries it
// in data.content on assistant.message envelopes (the payload is polymorphic
// per event type — decode it only on that type, mirroring copilotlane.Parse) —
// extracting a diff from the RAW stream yields escaped garbage ("git diff
// header lacks filename"). Returns "" when the output has no decodable agent
// text.
func decodeAgentText(stdout string) string {
	text, _ := decodeAgentStream(stdout)
	return text
}

// decodeAgentStream is decodeAgentText plus the copilot SERVED model, when the
// stream names one (assistant.message data.model). `--model auto` resolves
// vendor-side, so the model that answered is evidence the pin alone cannot
// carry; it lands on the row note rather than the cell key.
func decodeAgentStream(stdout string) (text, servedModel string) {
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
			Result string          `json:"result"`
			Data   json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case ev.Item != nil && ev.Item.Type == "agent_message" && ev.Item.Text != "":
			parts = append(parts, ev.Item.Text)
		case ev.Type == "result" && ev.Result != "":
			parts = append(parts, ev.Result)
		case ev.Type == "assistant.message" && len(ev.Data) > 0:
			var m struct {
				Content string `json:"content"`
				Model   string `json:"model"`
			}
			if json.Unmarshal(ev.Data, &m) == nil {
				if m.Content != "" {
					parts = append(parts, m.Content)
				}
				if m.Model != "" {
					servedModel = m.Model
				}
			}
		}
	}
	return strings.Join(parts, "\n"), servedModel
}

// servedNote appends served=<model> to a row note for lanes whose pin is
// resolved vendor-side (copilot `auto`). Empty served model = no annotation.
func servedNote(note, served string) string {
	if served == "" {
		return note
	}
	tag := "served=" + served
	if note == "" {
		return tag
	}
	return note + "; " + tag
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
	lanesFlag := flag.String("lanes", "local", "comma-separated lanes: local,claude,codex,glm,copilot")
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
	// copilot: `auto` is a legitimate pin — it is the config the router
	// dispatches (orchcfg CopilotModel) and the vendor resolves it per turn; the
	// SERVED model is recorded on the row's note (served=<model>) so the
	// evidence keeps its attribution without splitting the oracle cell.
	copilotModel := flag.String("copilot-model", "", "model pin for the copilot lane (REQUIRED when -lanes includes copilot; 'auto' is the deployable config, served model lands in the note)")
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
	copilotEffort := flag.String("copilot-effort", "", effortHelp("copilot")) // no effort dial: pass unrecorded, deliberately
	localEffort := flag.String("local-effort", "", effortHelp("local"))
	migrateEffortPath := flag.String("migrate-effort", "",
		"MIGRATION MODE: stamp effort=\""+policyeval.EffortUnrecorded+"\" on every row of this oracle file that has none, then exit (idempotent; writes <path>.tmp and renames)")
	timeoutSec := flag.Int("timeout", 900, "per-dispatch timeout (seconds)")
	maxNotional := flag.Float64("max-notional", 10, "claude-lane notional guard ceiling (real coding tasks exceed the $2 default)")
	claudeExtra := flag.String("claude-extra", "--dangerously-skip-permissions",
		"extra claude-lane flags via run -extra (headless replay agents work tool-enabled in disposable worktrees; empty to disable)")
	planOnly := flag.Bool("plan-only", false, "print the cells this run WOULD dispatch and exit 0 — the zero-spend way to check a resume before an unattended run")
	reMeasure := flag.Bool("re-measure", false, "override the drift guard: dispatch planned cells even when their identity is already recorded under a different effort or model (deliberate re-measurement / new-model measurement)")
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
		"glm":     {Lane: "glm", Model: *glmModel, Effort: *glmEffort},
		"copilot": {Lane: "copilot", Model: *copilotModel, Effort: *copilotEffort},
		"local":   {Lane: "local", Model: *localModel, Effort: *localEffort},
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

	rs := loadDone(*outPath)

	plan := buildRunPlan(tasks, *lanesFlag, rawPins, *trials, taskFilter, classFilter, rs.done)

	// The plan is what a replay WOULD do, decided before anything dispatches —
	// so SAY it before dispatching, not in a summary line after the loop that
	// an unattended weekly driver never reads.
	fmt.Fprintf(os.Stderr, "plan: %d cells (%d to run, %d already recorded)\n",
		plan.Total, len(plan.Run), plan.Skipped)
	// Drift is detected BEFORE the plan-only exit. -plan-only is the
	// pre-flight the guard's own error message tells you to run, so it must
	// SHOW the condition the guard trips on; returning first made the safety
	// tool structurally blind to the hazard it exists to reveal.
	drift := detectDrift(plan.Run, rs)
	if len(rs.done) > 0 {
		fmt.Fprintf(os.Stderr, "resume: %d recorded cell(s) in the output oracle, %d matched this run's keys\n", len(rs.done), plan.Skipped)
	}
	if len(drift.effortDrift) > 0 {
		c := drift.effortDrift[0]
		fmt.Fprintf(os.Stderr, "EFFORT DRIFT: %d planned cell(s) are already recorded under a different effort (recorded: %s; e.g. %s planned at %q)\n",
			len(drift.effortDrift), strings.Join(drift.recordedEfforts, ","), cellKey(c.Task, c.Config.Lane, c.Config.Model, c.Trial), c.Config.Effort)
	}
	if len(drift.modelDrift) > 0 {
		c := drift.modelDrift[0]
		fmt.Fprintf(os.Stderr, "MODEL DRIFT: %d planned cell(s) whose (task,lane,trial) is already recorded under a different model (recorded: %s; e.g. %s planned at model %q)\n",
			len(drift.modelDrift), strings.Join(drift.recordedModels, ","), identKey(c.Task, c.Config.Lane, c.Trial), c.Config.Model)
	}
	if len(drift.blankModelDrift) > 0 {
		c := drift.blankModelDrift[0]
		fmt.Fprintf(os.Stderr, "MODEL DRIFT (unlabeled): %d planned cell(s) whose (task,lane,trial) is recorded WITHOUT a model — dispatching may RE-BUY recorded work (e.g. %s planned at model %q)\n",
			len(drift.blankModelDrift), identKey(c.Task, c.Config.Lane, c.Trial), c.Config.Model)
	}
	if *planOnly {
		fmt.Printf("plan-only: %d cells (%d would run, %d already recorded, %d recorded in file) → %s\n",
			plan.Total, len(plan.Run), plan.Skipped, len(rs.done), *outPath)
		// The two tiers get DIFFERENT sentences because they are different
		// facts. An effort-tier hit really would re-dispatch a recorded cell;
		// a model-tier cell has never been recorded at that model, so it
		// would append a first measurement and re-spend nothing. Asserting
		// the re-spend for both is the same over-claim the round-3 fixes
		// removed from the old aggregate guard (review round 4).
		if len(drift.effortDrift) > 0 {
			fmt.Printf("plan-only: EFFORT DRIFT — %d cell(s) would RE-dispatch cells already recorded at a different effort (%s), re-spending what the oracle already paid for; a live run would REFUSE (pass -re-measure to override)\n",
				len(drift.effortDrift), strings.Join(drift.recordedEfforts, ","))
		}
		if n := len(drift.modelDrift); n > 0 {
			fmt.Printf("plan-only: MODEL DRIFT — %d cell(s) would record a (task,lane,trial) at a model it has never been measured at (recorded: %s); a live run would REFUSE (pass -re-measure to override)\n",
				n, strings.Join(drift.recordedModels, ","))
		}
		// The blank-model shape gets its own line here too. The round-6 fix
		// landed on the refusal but not on this preflight, so -plan-only —
		// the tool the refusal tells you to run FIRST — still asserted the
		// cell was never measured at the pin, which for an unlabeled row is
		// exactly what nobody knows (review round 6).
		if n := len(drift.blankModelDrift); n > 0 {
			fmt.Printf("plan-only: MODEL DRIFT (unlabeled rows) — %d cell(s) are recorded WITHOUT a model, so dispatching may RE-BUY work already paid for; a live run would REFUSE (pass -re-measure to override)\n", n)
		}
		return
	}

	// DRIFT GUARD. The resume key is (task,lane,model,EFFORT,trial), so a pin
	// that disagrees with the recorded rows re-keys them all and the ENTIRE
	// table re-dispatches — 476 cloud cells on this project's live oracle,
	// tool enabled, unattended, discovered only by the bill (review
	// 2026-08-12). The tell is PER-CELL, not aggregate (see detectDrift), in
	// two tiers: an effort re-key of recorded cells, and a model re-key —
	// deliberate new-model measurement or a pin typo, mechanically
	// indistinguishable, so it fails closed with -re-measure as the
	// deliberate override. Genuinely new cells pass freely.
	if drift.any() {
		if !*reMeasure {
			fatal("%s", driftRefusal(drift))
		}
		fmt.Fprintf(os.Stderr, "WARNING: -re-measure: dispatching %d cell(s) whose identity is already recorded under a different pin (%d effort, %d model, %d unlabeled-model)\n",
			len(drift.effortDrift)+drift.allModelDrift(), len(drift.effortDrift), len(drift.modelDrift), len(drift.blankModelDrift))
	}

	// Opened AFTER the plan-only exit and the drift guard: O_CREATE before
	// them left a 0-byte oracle behind every preflight — a file -migrate-effort
	// then refuses as "not an oracle", and a typo'd -out silently seeded a
	// second empty oracle beside the real one (review 2026-08-12).
	out, err := os.OpenFile(*outPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		fatal("open out: %v", err)
	}
	defer out.Close()

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

// driftRefusal renders the guard's refusal. It is a FUNCTION so the message
// itself is testable: the two tiers assert different facts and the blank-model
// case asserts a third, and reverting any of them to a generic "re-spend"
// sentence left the whole suite green (review round 5). On a tool with money
// attached the advice IS the product.
func driftRefusal(drift driftReport) string {
	var parts []string
	if n := len(drift.effortDrift); n > 0 {
		parts = append(parts, fmt.Sprintf("%d cell(s) are already recorded at a different EFFORT (%s), so dispatching them "+
			"RE-SPENDS what the oracle already paid for — the usual cause is an effort pin that disagrees with the "+
			"recorded rows (rows written before effort capture resume as %q); pin the effort the rows actually carry",
			n, strings.Join(drift.recordedEfforts, ","), policyeval.EffortUnrecorded))
	}
	if n := len(drift.modelDrift); n > 0 {
		// NOT a re-spend: these cells have never been recorded at this model,
		// so a dispatch APPENDS a first measurement. The hazard is that the
		// two causes are mechanically indistinguishable.
		parts = append(parts, fmt.Sprintf("%d cell(s) whose (task,lane,trial) is recorded only at other MODELS (%s), so dispatching "+
			"them ADDS a first measurement at a model this cell was never measured at — either a pin typo "+
			"(fix the -<lane>-model flag, which would also mislabel the rows) or a deliberate new-model measurement",
			n, strings.Join(drift.recordedModels, ",")))
	}
	if n := len(drift.blankModelDrift); n > 0 {
		// The one case where "never measured at this model" would be a LIE:
		// an unlabeled row does not say which model produced it, so it may
		// already be a measurement at this very pin — and then -re-measure
		// re-buys recorded work. Emitted ALONGSIDE the sentence above rather
		// than replacing it, so a run containing both shapes gets both
		// diagnoses (review round 6).
		parts = append(parts, fmt.Sprintf("%d cell(s) whose (task,lane,trial) is recorded WITHOUT a model. An unlabeled row "+
			"does not say which model produced it, so it may already be a measurement at this pin — dispatching "+
			"could re-buy work the oracle already paid for. No pin can match a blank recorded model (an empty "+
			"pin is rejected), so the options are: stamp the rows with the model that produced them, or pass "+
			"-re-measure accepting the possible re-spend", n))
	}
	return fmt.Sprintf("drift: %s. Check with -plan-only, or pass -re-measure if measuring under the new pin is deliberate. "+
		"Cells with no recorded (task,lane,trial) counterpart do not trip this guard.",
		strings.Join(parts, "; and "))
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

	// copilot: the pin may be `auto`; record which model actually answered.
	// Deferred to after the exit switch so a spawn/exit failure keeps its own
	// note; every later return path below goes through this closure.
	if lane == "copilot" {
		_, served := decodeAgentStream(stdout)
		defer func() { row.Note = servedNote(row.Note, served) }()
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
		// Same seam as the goldverify infra branch below: OUR machinery
		// failed, not the agent's diff — a hole, never a measured failure.
		row.OutcomeClass = "verify_error"
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
	applyVerifyOutcome(&row, vOut, vErr)
	return row
}

// applyVerifyOutcome folds mr-goldverify's process result into the row.
// Exactly three shapes exist:
//   - exit 0: the diff verified — a measured PASS;
//   - exit 1: goldverify ran and the diff failed — a measured FAILURE, with
//     WHY in the note, never silent;
//   - anything else (missing/stale binary, a spawn failure, an unexpected
//     exit): the VERIFIER'S infrastructure failed, which says nothing about
//     the agent's diff. Leaving outcome_class "ok" here recorded a measured
//     failure (dispatched:true, verifier_pass:false), so a missing goldverify
//     binary scored an entire replay as incompetent — the hole-as-failure
//     defect at the verify seam (review 2026-08-12). verify_error is a HOLE
//     (policyeval.IsEvidence): never evidence, always re-attemptable.
func applyVerifyOutcome(row *Row, vOut []byte, vErr error) {
	if vErr == nil {
		row.VerifierPass = true
		return
	}
	if ee, ok := vErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		row.VerifierPass = false
		row.Note = "verify-fail: " + verdictDetail(vOut)
		return
	}
	row.OutcomeClass = "verify_error"
	row.Note = "goldverify: " + firstLine(vOut, vErr)
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
