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
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dmmdea/meta-router/internal/goldtask"
	"github.com/dmmdea/meta-router/internal/orch/childenv"
	"github.com/dmmdea/meta-router/internal/orch/freelane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
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
	// DiffSource records WHERE the candidate diff came from: worktree (the
	// agent edited files in place and `git diff HEAD` captured them),
	// printed-decoded (decoded from the lane's structured stdout),
	// printed-raw (cut from the raw stream) or none (no diff at all). It is
	// set only where that branch actually yielded a non-empty diff. ABSENT
	// MEANS UNKNOWN, never "worktree": rows written before v0.40.4 carry no
	// provenance, and the AC-02/AC-08 astra cells are unattributable for
	// exactly that reason (B6). Not part of rowKey — resume must not re-key
	// on it (pinned by TestIntegrationDiffSourceDoesNotRekeyResume).
	DiffSource string `json:"diff_source,omitempty"`
	// DiffTruncatedLines counts the lines truncateDiff cut from a PRINTED
	// diff (trailing prose, a second diff, narration). Zero is omitted.
	// Receipt-everything: a truncation that leaves no trace is a silent cap.
	DiffTruncatedLines int `json:"diff_truncated_lines,omitempty"`
	// Quarantined names the instrument fix that retroactively made this row
	// a HOLE (-requarantine): the class stays as written, the row is no
	// longer evidence, resume refills it. QuarantineReason says why.
	Quarantined      string `json:"quarantined,omitempty"`
	QuarantineReason string `json:"quarantine_reason,omitempty"`
}

// Diff provenance values (Row.DiffSource).
const (
	diffSourceWorktree = "worktree"
	diffSourcePrinted  = "printed-decoded"
	diffSourceRaw      = "printed-raw"
	diffSourceNone     = "none"
)

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
	return policyeval.IsEvidence(r.Dispatched, r.OutcomeClass, r.Quarantined)
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
		if json.Unmarshal([]byte(line), &r) != nil || r.Task == "" {
			continue
		}
		// A QUARANTINED row is a hole (not in the resume set: the next sweep
		// refills it) that KEEPS ITS IDENTITY in the drift indexes: it was
		// recorded at this model and effort, so refilling it at the same pin
		// is a re-measurement, not a re-key. Without this, quarantining every
		// gpt-6-astra row removed astra from modelsByIdent while terra/luna
		// stayed, the model tier fired, and the next codex sweep died at
		// os.Exit(2) before dispatching anything — including the unrelated
		// cells it was launched for.
		ev := rowIsEvidence(r)
		if !ev && r.Quarantined == "" {
			continue
		}
		{
			lane, model := strings.TrimSpace(r.Lane), strings.TrimSpace(r.Model)
			eff := policyeval.NormalizeEffort(r.Effort)
			if ev {
				rs.done[rowKey(r.Task, lane, model, eff, r.Trial)] = true
			}
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
	out, _ := truncateDiffN(d)
	return out
}

// truncateDiffN is truncateDiff with a receipt: the number of lines it cut.
func truncateDiffN(d string) (string, int) {
	if d == "" {
		return "", 0
	}
	prefixes := []string{"diff --git", "index ", "--- ", "+++ ", "@@ ", "+", "-", " ",
		"new file mode", "deleted file mode", "old mode", "new mode", "similarity ",
		"rename ", "copy ", "Binary files", "\\ No newline"}
	lines := strings.Split(d, "\n")
	var out []string
	for _, line := range lines {
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
	// The receipt counts SUBSTANTIVE lines cut: blank separators and the
	// empty element strings.Split leaves after a trailing newline are not
	// content, and counting them made the same narration yield a different
	// number depending on how the stream ended.
	cut := 0
	for _, l := range lines[len(out):] {
		if strings.TrimSpace(l) != "" {
			cut++
		}
	}
	return strings.Join(out, "\n"), cut
}

// patchGrammarViolation scans a WORKTREE-captured patch for a line that is
// not unified-diff grammar. The capture is `git diff HEAD` stdout, so every
// line is a header, a hunk marker, a +/-/space content line or a
// "\ No newline" note — anything else is OUR machinery writing into the
// patch (the 2026-09-06 shape: git's CRLF warnings on the combined stream
// spliced mid-hunk, which then applied at exit 0 and wrote git's own
// warning text into a source file, producing a genuine-looking compile
// failure with no error in the note). A "Binary files" stanza is rejected
// too: the capture carries no --binary payload, so it cannot be re-applied
// and would fail at the verifier as a model failure. It scans the WHOLE
// stream, not the leading edge — leading junk applies at exit 0 and mid-hunk
// junk is the fatal one. Returns the offending line ("" = clean).
func patchGrammarViolation(diff string) string {
	prefixes := []string{"diff --git ", "index ", "--- ", "+++ ", "@@ ", "+", "-", " ",
		"new file mode", "deleted file mode", "old mode", "new mode", "similarity index",
		"dissimilarity index", "rename from", "rename to", "copy from", "copy to", "\\ No newline"}
	for _, line := range strings.Split(diff, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "Binary files ") {
			return line
		}
		ok := false
		for _, p := range prefixes {
			if strings.HasPrefix(line, p) {
				ok = true
				break
			}
		}
		if !ok {
			return line
		}
	}
	return ""
}

// captureExcludes is the DENY-list of build junk an agent's tool calls leave
// in its worktree: `go test`/`go build` caches redirected into the tree,
// Python caches, venvs, node_modules. Excluding them at the pathspec keeps
// their binary objects out of the candidate patch. A deny-list, never an
// extension allowlist: AC-06/AC-07 require creating untracked non-source
// fixtures (`internal/catalog/testdata/*.md`), so a `.go`-only filter would
// make those cells structurally unpassable forever.
var captureExcludes = []string{".go-cache", ".gocache", ".go-build-cache", ".cache",
	"__pycache__", ".pytest_cache", ".mypy_cache", ".venv", "node_modules"}

// capturePathspec is `-- . :(exclude)...` for both capture calls: the
// root-level name and the same name at any depth.
func capturePathspec() []string {
	ps := []string{"--", "."}
	for _, x := range captureExcludes {
		ps = append(ps, ":(exclude)"+x, ":(exclude,glob)**/"+x+"/**")
	}
	return ps
}

// captureWorktreeDiff is the candidate-diff capture at the worktree seam:
// `git add -N` (so new files are diffable) then `git diff HEAD`, stdout
// ONLY, both timed. Non-nil error = OUR machinery failed (a hole, never a
// scored row): add -N failed or timed out (a ctx-killed add -N leaves new
// files untracked — a silently truncated patch), or the diff failed or
// timed out (the old code's `err == nil` guard fell through to "no diff in
// output", which is SCORED). An empty diff with a nil error is the fail-open
// case: the agent printed its diff instead, and the caller falls through.
func captureWorktreeDiff(cwd string, timeoutSec int) (string, error) {
	// Notes carry the ERROR first, then the bounded stderr: under the live
	// autocrlf/safecrlf=warn shape stderr begins with CRLF warnings, and a
	// first-line note would name the warning instead of the failure.
	addArgs := append([]string{"add", "-N"}, capturePathspec()...)
	if _, stderr, err := gitOut(cwd, timeoutSec, addArgs...); err != nil {
		return "", fmt.Errorf("git add -N: %v; stderr: %s", err, boundedText(stderr, nil, 240))
	}
	diffArgs := append([]string{"diff", "HEAD"}, capturePathspec()...)
	stdout, stderr, err := gitOut(cwd, timeoutSec, diffArgs...)
	if err != nil {
		return "", fmt.Errorf("git diff HEAD: %v; stderr: %s", err, boundedText(stderr, nil, 240))
	}
	if len(strings.TrimSpace(string(stdout))) == 0 {
		return "", nil
	}
	return string(stdout), nil // RAW bytes — git apply needs the trailing newline
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
		var ev agentEvent
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		switch {
		case len(ev.Choices) > 0:
			// OpenAI chat-completion body (the free-provider lanes print the
			// vendor response verbatim): the answer is choices[0].message.content
			// and the served model is the body's model.
			if c := strings.TrimSpace(ev.Choices[0].Message.Content); c != "" {
				parts = append(parts, c)
			}
			if ev.Model != "" {
				servedModel = ev.Model
			}
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
	if len(parts) == 0 {
		// A vendor may pretty-print its body across lines; the line scan above
		// then sees no complete object. Decode the whole stream once.
		var ev agentEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &ev); err == nil && len(ev.Choices) > 0 {
			if c := strings.TrimSpace(ev.Choices[0].Message.Content); c != "" {
				parts = append(parts, c)
			}
			if ev.Model != "" {
				servedModel = ev.Model
			}
		}
	}
	return strings.Join(parts, "\n"), servedModel
}

// agentEvent is the union of the per-lane stdout shapes decodeAgentStream
// understands: codex item events, claude/glm result JSON, copilot envelopes,
// and the OpenAI chat-completion body the free-provider lanes print.
type agentEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Result  string          `json:"result"`
	Data    json.RawMessage `json:"data"`
	Model   string          `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// laneCWDArg renders the -cwd argument for a replay dispatch. The
// free-provider lanes get NONE: their transport is an HTTP call with no
// working directory — the lane cannot read a file, so a cwd would be a
// repository-context assertion the egress gate must judge (and refuse for a
// TEMP worktree) for a dispatch that would not have used it anyway. The agent
// worktree still exists for the verifier; the lane is scored on the diff it
// PRINTS, exactly like copilot's text dispatch.
func laneCWDArg(lane, cwd string) []string {
	if cwd == "" || freelane.IsLane(lane) {
		return nil
	}
	return []string{"-cwd", cwd}
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
	lanesFlag := flag.String("lanes", "local", "comma-separated lanes: local,claude,codex,glm,copilot,groq,cloudflare,openrouter,nim,gemini")
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
	// Free-provider lanes (W4): the same pin gate as every lane. The pin is the
	// vendor model id (openai/gpt-oss-120b on groq, @cf/... on cloudflare,
	// ...:free on openrouter); effort is forwarded as reasoning_effort only when
	// pinned, so `unrecorded` is the deliberate "vendor default ran" declaration.
	freeModelFlags, freeEffortFlags := map[string]*string{}, map[string]*string{}
	for _, lane := range freelane.Lanes {
		freeModelFlags[lane] = flag.String(lane+"-model", "", "model pin for the "+lane+" lane (REQUIRED when -lanes includes "+lane+"; the vendor model id)")
	}
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
	for _, lane := range freelane.Lanes {
		freeEffortFlags[lane] = flag.String(lane+"-effort", "", effortHelp(lane))
	}
	requarantinePath := flag.String("requarantine", "",
		"quarantine the rows the pre-0.40.4 capture defect poisoned (git stderr / build caches in the candidate patch, recorded as model failures): prints the matched rows grouped by lane/model with per-lane pass rates before → after. DRY RUN unless -apply. Additive: stamps quarantined + quarantine_reason, never deletes a row or rewrites a class; quarantined rows are holes the next sweep refills at the same pin")
	applyQuarantine := flag.Bool("apply", false, "with -requarantine: actually stamp the rows (requires the oracle committed clean in git; backup written beside it; aborts if the file changes while preparing)")
	quarantineStamp := flag.String("quarantine-stamp", "v0.40.4", "with -requarantine: the value written to `quarantined` — the instrument version whose fix made these rows holes")
	migrateEffortPath := flag.String("migrate-effort", "",
		"MIGRATION MODE: stamp effort=\""+policyeval.EffortUnrecorded+"\" on every row of this oracle file that has none, then exit (idempotent; writes <path>.tmp and renames)")
	timeoutSec := flag.Int("timeout", 900, "per-dispatch timeout (seconds)")
	maxNotional := flag.Float64("max-notional", 10, "claude-lane notional guard ceiling (real coding tasks exceed the $2 default)")
	claudeExtra := flag.String("claude-extra", "--dangerously-skip-permissions",
		"extra claude-lane flags via run -extra (headless replay agents work tool-enabled in disposable worktrees; empty to disable)")
	planOnly := flag.Bool("plan-only", false, "print the cells this run WOULD dispatch and exit 0 — the zero-spend way to check a resume before an unattended run")
	reMeasure := flag.Bool("re-measure", false, "override the drift guard: dispatch planned cells even when their identity is already recorded under a different effort or model (deliberate re-measurement / new-model measurement)")
	copilotBudgetPct := flag.Float64("copilot-budget-pct", 33, "probe spend cap for the copilot lane: a copilot cell is recorded as a deferred HOLE (resumable) once the ledger's month window is at or above this percentage; negative disables. Default a third of the month — a 168-cell probe under `auto` consumed the whole month on 2026-09-05")
	codexBudgetPct := flag.Float64("codex-budget-pct", 33, "probe spend cap for the codex lane: a codex cell is recorded as a deferred HOLE (resumable) once the ledger's 7d window (the wham poll's weekly allowance) is at or above this percentage; negative disables. Same lesson as the copilot cap: a roster probe must never drain the operator's week")
	flag.Parse()

	// Migration is a pure file rewrite: it must not require a goldset, lanes or
	// pins, and it must never dispatch anything.
	if *applyQuarantine && *requarantinePath == "" {
		fatal("-apply has no effect on its own: it gates -requarantine <oracle> only. Re-run with -requarantine, or drop -apply.")
	}
	if *requarantinePath != "" {
		rep, bak, err := requarantineFile(*requarantinePath, *quarantineStamp, *applyQuarantine)
		mode := qDryRun
		switch {
		case *applyQuarantine && err != nil:
			mode = qAborted
		case *applyQuarantine:
			mode = qApplied
		}
		fmt.Print(rep.render(*quarantineStamp, mode))
		if err != nil {
			// NOT applied: the header says it, and so does the exit line —
			// an -apply that fails after the report must never read like one
			// that finished.
			fatal("requarantine: NOT applied, the oracle is unchanged: %v", err)
		}
		if *applyQuarantine {
			if len(rep.Matched) == 0 {
				fmt.Printf("requarantine: nothing to stamp — 0 matching rows; %s was not opened for writing and no backup was made\n", *requarantinePath)
			} else {
				fmt.Printf("requarantine: %d row(s) stamped → %s (pre-state backup: %s)\n", len(rep.Matched), *requarantinePath, bak)
			}
		}
		return
	}
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
		"claude":  {Lane: "claude", Model: *claudeModel, Effort: *claudeEffort},
		"codex":   {Lane: "codex", Model: *codexModel, Effort: *codexEffort},
		"glm":     {Lane: "glm", Model: *glmModel, Effort: *glmEffort},
		"copilot": {Lane: "copilot", Model: *copilotModel, Effort: *copilotEffort},
		"local":   {Lane: "local", Model: *localModel, Effort: *localEffort},
	}
	for _, lane := range freelane.Lanes {
		rawPins[lane] = policyeval.Config{Lane: lane, Model: *freeModelFlags[lane], Effort: *freeEffortFlags[lane]}
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
		var row Row
		if hold, why := probeBudgetHold(c.Config.Lane, probeBudgets(*copilotBudgetPct, *codexBudgetPct), time.Now().UTC()); hold {
			row = Row{TS: time.Now().UTC().Format(time.RFC3339), Task: c.Task, Class: c.GoldTask.Class,
				Lane: c.Config.Lane, Model: c.Config.Model, Effort: c.Config.Effort, Trial: c.Trial,
				OutcomeClass: "deferred", Note: why}
		} else {
			row = replayOne(c.GoldTask, c.Config, c.Trial, *orchBin, *verifyBin, *reposFlag, *timeoutSec, *maxNotional, *claudeExtra)
		}
		b, _ := json.Marshal(row)
		fmt.Fprintln(out, string(b))
		src := ""
		if row.DiffSource != "" {
			src = " src=" + row.DiffSource
		}
		fmt.Printf("[%s %s/%s/%s trial %d] dispatched=%v outcome=%s pass=%v (%dms)%s %s\n",
			row.Task, row.Lane, row.Model, row.Effort, row.Trial, row.Dispatched, row.OutcomeClass, row.VerifierPass, row.LatencyMs, src, row.Note)
	}
	fmt.Printf("\nreplay complete: %d cells (%d run now, %d already recorded) → %s\n",
		plan.Total, len(plan.Run), plan.Skipped, *outPath)
}

// probeBudget is one lane's probe spend cap: the ledger window that carries
// the lane's non-recovering (or slowly recovering) allowance and the
// percentage at which cells become holes.
type probeBudget struct {
	Window ledger.WindowKind
	Pct    float64
}

// probeBudgets is the budget table the replay runs under. copilot: the
// calendar month (credits, 2026-09-05: 168 planned cells, 110 dispatched,
// month gone). codex: the weekly window the wham poll reports (the plan's
// primary allowance; on 2026-09-06 the operator's upgraded plan exposed ONLY
// that window). A negative pct disables that lane's cap.
func probeBudgets(copilotPct, codexPct float64) map[string]probeBudget {
	return map[string]probeBudget{
		"copilot": {Window: ledger.WinMonth, Pct: copilotPct},
		"codex":   {Window: ledger.Win7d, Pct: codexPct},
	}
}

// probeBudgetHold decides whether a cell must be held back as a deferred hole
// because the lane's budgeted window is already at or above the probe budget.
// Reads the orchestrator ledger directly (read-only; MR_ORCH_STATE-scoped like
// every other path here). Fail-open on a missing or unreadable ledger, on a
// window with no signal, and on an expired window — a probe with no quota
// signal at all is the pre-budget state, not a reason to hold. Lanes absent
// from the table are never held (free lanes cost nothing; local is local).
func probeBudgetHold(lane string, budgets map[string]probeBudget, now time.Time) (bool, string) {
	pb, budgeted := budgets[lane]
	if !budgeted || pb.Pct < 0 {
		return false, ""
	}
	l, warn := ledger.OpenChecked(statepaths.Ledger())
	if warn != "" {
		return false, ""
	}
	b, ok := l.Bucket(lane, pb.Window)
	if !ok || b.UsedPct < 0 {
		return false, ""
	}
	if !b.ResetsAt.IsZero() && !b.ResetsAt.After(now) {
		return false, "" // an expired window is history; the next dispatch re-anchors it
	}
	if b.UsedPct >= pb.Pct {
		return true, fmt.Sprintf("probe budget: %s %s at %.0f%% >= -%s-budget-pct %.0f (hole, resumable after the reset or with a higher budget)", lane, pb.Window, b.UsedPct, lane, pb.Pct)
	}
	return false, ""
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
//
// NAMED RESULT, deliberately: the copilot served-model annotation below runs
// in a defer so that every return path after the dispatch carries it. A defer
// can only reach the value being returned through a named result — with an
// unnamed one, `return row` copies the struct out BEFORE the defer mutates the
// local, and the annotation is silently lost. That is not hypothetical: the
// first live copilot smoke (EX-01, 2026-09-02) wrote a row with no served=
// note for exactly this reason; the integration test below pins it.
func replayOne(t goldtask.Task, cfg policyeval.Config, trial int, orchBin, verifyBin, repos string, timeoutSec int, maxNotional float64, claudeExtra string) (row Row) {
	lane, model := cfg.Lane, cfg.Model
	row = Row{TS: time.Now().UTC().Format(time.RFC3339), Task: t.ID, Class: t.Class,
		Lane: lane, Model: model, Effort: cfg.Effort, Trial: trial}
	start := time.Now()

	// Exec tasks get a fresh agent worktree at the parent commit as cwd.
	cwd := ""
	nonce := cellNonce()
	if t.Verify.Kind != "pure" {
		repoPath := repoDir(t.Verify.Repo, repos)
		wt := worktreePath(t.ID, lane, trial, nonce)
		if _, stderr, err := gitOut(repoPath, timeoutSec, "worktree", "add", "--detach", wt, t.Verify.Parent); err != nil {
			row.OutcomeClass = "error"
			// The FULL stderr, not firstLine: `worktree add` into an existing
			// path writes "Preparing worktree (detached HEAD …)" and THEN
			// "fatal: '<path>' already exists", and a first-line note kept
			// the success banner (10 such holes across four lanes, 2026-09-06).
			row.Note = "agent worktree: " + boundedText(stderr, err, 400)
			row.LatencyMs = time.Since(start).Milliseconds()
			return row
		}
		defer removeWorktree(repoPath, wt)
		cwd = wt
	}

	args := replayArgs(t.ID, cfg, t.Prompt, maxNotional)
	if lane == "claude" && claudeExtra != "" {
		args = append(args, "-extra", claudeExtra)
	}
	if rc := routerClass(t.Class); rc != "" {
		args = append(args, "-class", rc)
	}
	args = append(args, laneCWDArg(lane, cwd)...)
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
	// free lanes: the vendor's body names the model it served (openrouter's
	// `:free` pool routes to variants) — same attribution, same mechanism.
	if lane == "copilot" || freelane.IsLane(lane) {
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
	// guard downstream still rejects test-file tampering. The capture is
	// stdout-only, deny-listed and timed (captureWorktreeDiff): the
	// 2026-09-06 astra/luna/claude/copilot "git apply" cells were this seam
	// splicing git's stderr and the agent's build caches into the patch and
	// recording the corrupt result as a model failure.
	diff := ""
	if cwd != "" {
		b, err := captureWorktreeDiff(cwd, 120)
		if err != nil {
			// OUR machinery failed, not the agent's diff — a hole, never a
			// measured failure (the same seam as the write/goldverify holes).
			row.OutcomeClass = "verify_error"
			row.Note = "worktree capture: " + err.Error()
			return row
		}
		if b != "" {
			diff = b
			row.DiffSource = diffSourceWorktree
		}
	}
	if diff == "" {
		// Fallback: the agent PRINTED its diff. Decode structured lane output
		// first (codex events / result JSON) so the diff isn't JSON-escaped,
		// then cut trailing prose — agents narrate after the last hunk and
		// git apply rejects it as a corrupt patch.
		if txt := decodeAgentText(stdout); txt != "" {
			if d, cut := truncateDiffN(extractDiff(txt)); d != "" {
				diff, row.DiffSource, row.DiffTruncatedLines = d, diffSourcePrinted, cut
			}
		}
	}
	if diff == "" {
		if d, cut := truncateDiffN(extractDiff(stdout)); d != "" { // last resort: raw stream
			diff, row.DiffSource, row.DiffTruncatedLines = d, diffSourceRaw, cut
		}
	}
	if diff == "" {
		row.DiffSource = diffSourceNone
		row.Note = "no diff in output"
		return row
	}
	if row.DiffSource == diffSourceWorktree {
		// Pre-apply assertion — should never fire post-fix. A worktree
		// capture is git's own output; a non-grammar line in it is the
		// harness, and applying it would either fail (scored as the model's
		// failure) or, worse, apply at exit 0 with the junk inside a file.
		if bad := patchGrammarViolation(diff); bad != "" {
			row.OutcomeClass = "verify_error"
			row.Note = "worktree capture not a patch: " + boundedText([]byte(bad), nil, 200)
			return row
		}
	}
	if !strings.HasSuffix(diff, "\n") {
		diff += "\n" // git apply requires the trailing newline
	}
	pf := candidateDiffPath(t.ID, lane, trial, nonce)
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
	applyVerifyOutcome(&row, vOut, vErr, row.DiffSource)
	return row
}

// applyStageFailure reports whether goldverify's exit-1 verdict failed at
// the `git apply` stage (Verdict.Detail is "<stage>: <err>\n<tail>").
func applyStageFailure(vOut []byte) bool {
	var v struct {
		Detail string `json:"detail"`
	}
	return json.Unmarshal(vOut, &v) == nil && strings.HasPrefix(v.Detail, goldtask.StageGitApply+":")
}

// applyVerifyOutcome folds mr-goldverify's process result into the row.
// Exactly three shapes exist:
//   - exit 0: the diff verified — a measured PASS;
//   - exit 1: goldverify ran and the diff failed — a measured FAILURE, with
//     WHY in the note, never silent — EXCEPT a WORKTREE-sourced diff that
//     fails at the `git apply` stage. That diff was taken at the task's
//     parent commit and is re-applied to a fresh checkout of that same
//     commit, so it applies by construction unless WE corrupted it: a
//     harness fault, a hole. A PRINTED diff that fails to apply stays a
//     measured model failure — the agent printed a patch that does not
//     apply. This asymmetry is the anti-laundering rule (pinned by
//     TestHarnessCorruptedApplyIsHole): without it the fix would turn every
//     model's malformed patch into a hole too;
//   - anything else (missing/stale binary, a spawn failure, an unexpected
//     exit): the VERIFIER'S infrastructure failed, which says nothing about
//     the agent's diff. Leaving outcome_class "ok" here recorded a measured
//     failure (dispatched:true, verifier_pass:false), so a missing goldverify
//     binary scored an entire replay as incompetent — the hole-as-failure
//     defect at the verify seam (review 2026-08-12). verify_error is a HOLE
//     (policyeval.IsEvidence): never evidence, always re-attemptable.
//
// It ASSIGNS row.Note on the failing shapes, so a receipt set before the
// verifier call is clobbered on exactly the failing rows — receipts belong
// here or in the servedNote defer.
func applyVerifyOutcome(row *Row, vOut []byte, vErr error, diffSource string) {
	if vErr == nil {
		row.VerifierPass = true
		return
	}
	if ee, ok := vErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		if diffSource == diffSourceWorktree && applyStageFailure(vOut) {
			row.VerifierPass = false
			row.OutcomeClass = "verify_error"
			row.Note = "worktree diff failed to apply at its own parent (harness fault): " + verdictDetail(vOut)
			return
		}
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

// gitOut runs git with stdout and stderr captured SEPARATELY and the timeout
// honoured (the old gitC ignored its timeoutSec and returned CombinedOutput,
// which is how git's CRLF warnings became patch bytes). A timeout is
// reported as an error naming it, so callers route it to a hole.
func gitOut(dir string, timeoutSec int, args ...string) (stdout, stderr []byte, err error) {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gitExe(), args...)
	cmd.Dir = dir
	// git runs the repo's hooks with this environment; scrubbed like every
	// other spawn here (B13, R10).
	cmd.Env = childenv.Scrub(os.Environ())
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	// WaitDelay: after the kill, Wait must not block on a child that still
	// holds the inherited stdout/stderr pipe (a hook, or the Git for Windows
	// launcher's real git). Without it the timeout mapping below is
	// unreachable — the replay hangs at the cell with no row and no note,
	// the most silent shape possible (review 2026-09-07, reproduced against
	// C:\Program Files\Git\cmd\git.exe). Same precedent as the lane runners.
	cmd.WaitDelay = gitWaitDelay
	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("git %s: timeout after %ds", strings.Join(args, " "), timeoutSec)
	}
	return so.Bytes(), se.Bytes(), err
}

// gitWaitDelay bounds Wait after a context kill; a variable so the timeout
// test can shorten it.
var gitWaitDelay = 10 * time.Second

// gitExe resolves git ONCE. On Windows the PATH entry is the Git for Windows
// LAUNCHER (cmd\git.exe), which spawns the real mingw64\bin\git.exe as a
// child: a context kill stops the launcher and orphans the real git, which
// keeps running — holding the agent worktree's handles — and keeps the pipes
// open. Substituting the real binary when it sits beside the launcher makes
// the kill reach the process that does the work (measured: the mingw64
// binary returns at the deadline, the launcher hangs past it).
var gitExe = func() func() string {
	var once sync.Once
	resolved := "git"
	return func() string {
		once.Do(func() {
			p, err := exec.LookPath("git")
			if err != nil {
				return
			}
			resolved = p
			dir, base := filepath.Split(p)
			if strings.EqualFold(base, "git.exe") && strings.EqualFold(filepath.Base(filepath.Clean(dir)), "cmd") {
				real := filepath.Join(filepath.Dir(filepath.Clean(dir)), "mingw64", "bin", "git.exe")
				if st, err := os.Stat(real); err == nil && !st.IsDir() {
					resolved = real
				}
			}
		})
		return resolved
	}
}()

// cellNonce makes one cell's temp paths unique to THIS process and moment:
// two sweeps replaying the same (task,lane,trial) — or a rerun after a
// killed sweep left its tree behind — collided on the old deterministic
// path (`worktree add` → "already exists"), and `defer os.Remove(pf)` on
// the same-keyed candidate-diff path let one sweep delete the patch another
// was about to verify (A3, 2026-09-06).
var cellNonce = func() string {
	// pid + clock + a process counter: the Windows clock hands two
	// back-to-back calls the same nanosecond (measured in the test). A
	// variable so a test can pin the path and pre-create it.
	return fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), atomic.AddUint64(&nonceSeq, 1))
}

var nonceSeq uint64

// worktreeRoot is the parent of every agent worktree and candidate diff;
// a variable so tests can point it at their own temp dir.
var worktreeRoot = os.TempDir

func worktreePath(taskID, lane string, trial int, nonce string) string {
	return filepath.Join(worktreeRoot(), fmt.Sprintf("goldreplay-%s-%s-%d-%s", strings.ToLower(taskID), lane, trial, nonce))
}

func candidateDiffPath(taskID, lane string, trial int, nonce string) string {
	return filepath.Join(worktreeRoot(), fmt.Sprintf("goldreplay-%s-%s-%d-%s.diff", strings.ToLower(taskID), lane, trial, nonce))
}

// removeWorktree reclaims a cell's tree: `worktree remove --force`, then
// os.RemoveAll as the fallback (a held handle makes git return 255, delete
// the .git gitfile and drop the admin entry while leaving the files), the
// error SURFACED on stderr rather than discarded. It never prunes anything
// it did not create: a blanket `git worktree prune` mutates the operator's
// real repos, and a prefix sweep of the root would delete a concurrent
// sweep's live tree.
func removeWorktree(repoPath, wt string) {
	_, stderr, err := gitOut(repoPath, 60, "worktree", "remove", "--force", wt)
	if err == nil {
		if _, statErr := os.Stat(wt); os.IsNotExist(statErr) {
			return
		}
	}
	// git failed (or left the tree): say so even when RemoveAll then
	// succeeds — a failed `worktree remove` leaves a stale admin entry under
	// the repo's .git/worktrees that only expires with gc, and a silent
	// success here hides the handle leak that caused it.
	rmErr := os.RemoveAll(wt)
	// Two different faults, two different messages: git REFUSED (err != nil),
	// or git reported success and the tree is still on disk (a handle held
	// it open). Saying "remove failed" for the second sends the reader after
	// a git error that does not exist.
	cause := fmt.Sprintf("git worktree remove failed (%s)", boundedText(stderr, err, 200))
	if err == nil {
		cause = "git worktree remove returned 0 but the tree is still on disk (a handle held it open)"
	}
	fmt.Fprintf(warnOut, "warn: agent worktree %s: %s; os.RemoveAll: %v\n",
		wt, cause, rmErrText(rmErr))
}

// warnOut is where removeWorktree's warning goes — a variable so a test can
// prove the warning is actually emitted (a warning with no seam is a
// warning nothing can pin).
var warnOut io.Writer = os.Stderr

func rmErrText(err error) string {
	if err == nil {
		return "reclaimed the files (a stale admin entry may remain in .git/worktrees)"
	}
	return "FAILED: " + err.Error()
}

// boundedText renders a captured stream for a note, whole (newlines folded
// to " | ") and capped at max bytes; the error stands in for an empty stream.
func boundedText(b []byte, err error, max int) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", " | ")
	if s == "" && err != nil {
		s = err.Error()
	}
	if len(s) > max {
		// Cut on a rune boundary (git quotes non-ASCII paths) and SAY how
		// much was cut — a bare truncation is a silent cap.
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + fmt.Sprintf("…(+%d bytes)", len(s)-cut)
	}
	return s
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
