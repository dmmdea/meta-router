package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/goldtask"
	"github.com/dmmdea/meta-router/internal/policyeval"
)

// The oracle row records the model PIN, not the model that answered, so a lane
// replayed without a pin writes evidence under a model nobody chose. That is
// what silently happened: -claude-model defaulted to claude-sonnet-5 and the A2
// weekly script never passed it, so 204 claude rows recorded Sonnet while the
// rank table dispatched claude-opus-4-8. These pin the defaults OUT.
// Making the pin MANDATORY is worthless if the pin is not part of the cell's
// identity. Review 2026-07-27 reproduced it: with a sonnet row already in the
// oracle, running with the corrected pin -claude-model claude-opus-4-8 printed
// "0 run now, 1 already recorded" and exited 0 — the operator does exactly what
// the new error demands, gets a green run, and the mislabelled row still stands.
// With -trials 2 an opus row lands BESIDE the sonnet row and the scorecard
// (which keys by task+lane) aggregates two models into one cell.
func TestRowKeySeparatesModels(t *testing.T) {
	sonnet := rowKey("RS-01", "claude", "claude-sonnet-5", "high", 1)
	opus := rowKey("RS-01", "claude", "claude-opus-4-8", "high", 1)
	if sonnet == opus {
		t.Fatal("a different model MUST produce a different resume key, or the mandatory pin is a no-op")
	}
	if sonnet != rowKey("RS-01", "claude", "claude-sonnet-5", "high", 1) {
		t.Fatal("same config must produce a stable key")
	}
	if sonnet == rowKey("RS-01", "claude", "claude-sonnet-5", "high", 2) {
		t.Fatal("trial must still separate keys")
	}
	if sonnet == rowKey("RS-01", "codex", "claude-sonnet-5", "high", 1) {
		t.Fatal("lane must still separate keys")
	}
}

// The regression the review actually reproduced, at the loadDone level: an
// existing sonnet observation must NOT mark the opus cell as already-recorded.
func TestLoadDoneDoesNotCreditADifferentModel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	row := `{"ts":"t","task":"RS-01","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}` + "\n"
	if err := os.WriteFile(p, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	done := loadDone(p).done
	if !done[rowKey("RS-01", "claude", "claude-sonnet-5", "high", 1)] {
		t.Fatal("the recorded sonnet cell must be marked done")
	}
	if done[rowKey("RS-01", "claude", "claude-opus-4-8", "high", 1)] {
		t.Fatal("a sonnet row must NOT mark the opus cell done — that is the reproduced no-op")
	}
}

// The gate validated strings.TrimSpace(pin) while the run passed the raw value
// through to both the oracle row and the -model dispatch arg, so a padded pin
// passed validation and was recorded as a distinct model string.
func TestPinsAreTrimmedNotJustValidated(t *testing.T) {
	got := normalizePins(map[string]policyeval.Config{
		"claude": {Lane: "claude", Model: "  claude-opus-4-8  ", Effort: "  xhigh  "},
		"codex":  {Lane: "codex", Model: "gpt-5.6-terra", Effort: "high"},
	})
	if got["claude"].Model != "claude-opus-4-8" {
		t.Fatalf("pin must be trimmed for USE, got %q", got["claude"].Model)
	}
	if got["claude"].Effort != "xhigh" {
		t.Fatalf("effort must be trimmed for USE too, got %q", got["claude"].Effort)
	}
	if got["codex"].Model != "gpt-5.6-terra" || got["codex"].Effort != "high" {
		t.Fatalf("an already-clean pin must be untouched, got %+v", got["codex"])
	}
}

// -lanes claude,claude would replay the same cell twice in one run and emit the
// duplicated flag name in the error message.
func TestLanesAreDeduped(t *testing.T) {
	got := parseLanes("claude, codex ,claude,,codex")
	if len(got) != 2 || got[0] != "claude" || got[1] != "codex" {
		t.Fatalf("parseLanes must dedupe preserving first-seen order, got %v", got)
	}
}

// The four unit tests above are NOT sufficient and this comment is the reason.
// Review 2026-07-27 mutation-tested three reverts at main()'s CALL SITES and
// all three compiled and left the whole repo suite green:
//
//	MUT-1  done[rowKey(t.ID, lane, laneModel[lane], trial)] -> rowKey(..., "", ...)
//	MUT-2  laneModel := normalizePins(map[...]) -> laneModel := map[...]
//	MUT-3  lanes := parseLanes(*lanesFlag)      -> strings.Split(*lanesFlag, ",")
//
// A binary built from MUT-1 re-dispatched all 224 already-recorded cells of the
// real oracle. These tests drive buildRunPlan, which is where those call sites
// now live, so each mutation fails at least one of them.
func planFixture(t *testing.T, lanesCSV string, pins map[string]policyeval.Config, done map[string]bool) runPlan {
	t.Helper()
	tasks := []goldtask.Task{{ID: "AC-01", Class: "agentic-coding"}}
	return buildRunPlan(tasks, lanesCSV, pins, 1, nil, nil, done)
}

// pin is the terse fixture form of a fully-specified lane pin.
func pin(model, effort string) policyeval.Config {
	return policyeval.Config{Model: model, Effort: effort}
}

// Kills MUT-1: a cell already recorded under the SAME model must be skipped,
// and the same cell under a DIFFERENT model must still run.
func TestRunPlanResumeIsModelAware(t *testing.T) {
	done := map[string]bool{rowKey("AC-01", "claude", "claude-sonnet-5", "high", 1): true}

	same := planFixture(t, "claude", map[string]policyeval.Config{"claude": pin("claude-sonnet-5", "high")}, done)
	if len(same.Run) != 0 || same.Skipped != 1 {
		t.Fatalf("recorded cell must be skipped: run=%d skipped=%d", len(same.Run), same.Skipped)
	}

	other := planFixture(t, "claude", map[string]policyeval.Config{"claude": pin("claude-opus-4-8", "high")}, done)
	if len(other.Run) != 1 || other.Skipped != 0 {
		t.Fatalf("a DIFFERENT model must still run: run=%d skipped=%d", len(other.Run), other.Skipped)
	}
}

// The same resume regression, one axis over: a cell recorded at `high` must NOT
// mark the `xhigh` cell already-done, or the mandatory effort pin is the same
// no-op the mandatory model pin was.
func TestRunPlanResumeIsEffortAware(t *testing.T) {
	done := map[string]bool{rowKey("AC-01", "claude", "claude-opus-4-8", "high", 1): true}

	same := planFixture(t, "claude", map[string]policyeval.Config{"claude": pin("claude-opus-4-8", "high")}, done)
	if len(same.Run) != 0 || same.Skipped != 1 {
		t.Fatalf("recorded cell must be skipped: run=%d skipped=%d", len(same.Run), same.Skipped)
	}

	other := planFixture(t, "claude", map[string]policyeval.Config{"claude": pin("claude-opus-4-8", "xhigh")}, done)
	if len(other.Run) != 1 || other.Skipped != 0 {
		t.Fatalf("a DIFFERENT effort must still run: run=%d skipped=%d", len(other.Run), other.Skipped)
	}
}

// Kills MUT-2: the model that reaches the planned cell must be the TRIMMED pin,
// otherwise the padded value is what gets recorded and dispatched.
func TestRunPlanUsesTrimmedPin(t *testing.T) {
	p := planFixture(t, "claude", map[string]policyeval.Config{"claude": pin("  claude-opus-4-8  ", "  xhigh  ")}, nil)
	if len(p.Run) != 1 {
		t.Fatalf("expected one planned cell, got %d", len(p.Run))
	}
	if p.Run[0].Model() != "claude-opus-4-8" {
		t.Fatalf("planned cell must carry the trimmed pin, got %q", p.Run[0].Model())
	}
	if p.Run[0].Config.Effort != "xhigh" {
		t.Fatalf("planned cell must carry the trimmed effort, got %q", p.Run[0].Config.Effort)
	}
}

// Kills MUT-3: a duplicated lane must produce ONE cell, and a space-padded lane
// name must still resolve its pin (strings.Split leaves " codex" untrimmed, so
// laneCfg[" codex"] is the zero Config and the cell would be planned empty).
func TestRunPlanDedupesAndTrimsLanes(t *testing.T) {
	p := planFixture(t, "claude,claude, codex",
		map[string]policyeval.Config{"claude": pin("claude-opus-4-8", "xhigh"), "codex": pin("gpt-5.6-terra", "high")}, nil)
	if len(p.Run) != 2 {
		t.Fatalf("expected 2 cells (claude once, codex once), got %d: %+v", len(p.Run), p.Run)
	}
	for _, c := range p.Run {
		if c.Model() == "" {
			t.Fatalf("lane %q planned with an empty model — its pin was not resolved", c.Lane())
		}
		// A lane whose pin failed to resolve would normalize to the unrecorded
		// marker and look like a deliberate choice; it is not one here.
		if c.Config.Effort == policyeval.EffortUnrecorded {
			t.Fatalf("lane %q planned with an unresolved effort", c.Lane())
		}
	}
}

func TestRequireConfigPins(t *testing.T) {
	cases := []struct {
		name  string
		lanes []string
		cfgs  map[string]policyeval.Config
		want  []string
	}{
		{"all pinned", []string{"claude", "codex"},
			map[string]policyeval.Config{"claude": pin("claude-opus-5", "xhigh"), "codex": pin("gpt-5.6-terra", "high")}, nil},
		{"claude unpinned is caught", []string{"claude", "codex"},
			map[string]policyeval.Config{"claude": pin("", "xhigh"), "codex": pin("gpt-5.6-terra", "high")}, []string{"claude"}},
		{"whitespace is not a pin", []string{"claude"},
			map[string]policyeval.Config{"claude": pin("   ", "xhigh")}, []string{"claude"}},
		{"a missing effort is caught exactly like a missing model", []string{"claude"},
			map[string]policyeval.Config{"claude": pin("claude-opus-5", "")}, []string{"claude"}},
		{"the unrecorded marker IS an explicit choice", []string{"local"},
			map[string]policyeval.Config{"local": pin("qwythos", policyeval.EffortUnrecorded)}, nil},
		{"only replayed lanes are required", []string{"claude"},
			map[string]policyeval.Config{"claude": pin("claude-opus-5", "xhigh"), "glm": pin("", "")}, nil},
		{"order follows the lane list, not the map", []string{"local", "glm"},
			map[string]policyeval.Config{"local": pin("", ""), "glm": pin("", "")}, []string{"local", "glm"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := requireConfigPins(c.lanes, c.cfgs)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("requireConfigPins = %v, want %v", got, c.want)
			}
		})
	}
}

// The error must tell the operator which flags to pass, not just what is wrong.
func TestPinFlagsForIsActionable(t *testing.T) {
	got := pinFlagsFor([]string{"claude", "glm"})
	for _, want := range []string{"-claude-model", "-glm-model"} {
		if !strings.Contains(got, want) {
			t.Fatalf("pinFlagsFor = %q, must name %s", got, want)
		}
	}
}

// containsPair reports whether args carries `flag value` as adjacent elements.
func containsPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// An effort we RECORD but never SEND is worse than no effort at all: the row
// asserts a configuration that never ran — the exact mislabelled-oracle defect
// this change exists to fix, laundered through a row claiming otherwise.
// replayOne built its args with no -effort; mr-orchestrate run accepts one, and
// the lane arg builders emit it only when non-empty.
func TestReplayArgsCarryEffortWhenPinned(t *testing.T) {
	args := replayArgs("QE-09", policyeval.Config{
		Lane: "claude", Model: "claude-opus-5", Effort: "xhigh"}, "desc", 10)
	if !containsPair(args, "-effort", "xhigh") {
		t.Fatalf("a pinned effort must reach the dispatch, got %v", args)
	}
	if !containsPair(args, "-model", "claude-opus-5") {
		t.Fatalf("model must still be passed, got %v", args)
	}
	if !containsPair(args, "-lane", "claude") {
		t.Fatalf("lane must still be passed, got %v", args)
	}
}

// EffortUnrecorded is a marker, not an effort — it must NOT be sent, or the
// orchestrator forwards the literal string "unrecorded" to the CLI.
func TestReplayArgsOmitUnrecordedEffort(t *testing.T) {
	args := replayArgs("QE-09", policyeval.Config{
		Lane: "local", Model: "gemma4-cascade", Effort: policyeval.EffortUnrecorded}, "desc", 10)
	for _, a := range args {
		if a == policyeval.EffortUnrecorded {
			t.Fatal("the unrecorded marker must never be dispatched as an effort value")
		}
	}
	if containsPair(args, "-effort", "") {
		t.Fatal("an empty -effort must not be dispatched either")
	}
}

func TestRowKeySeparatesEfforts(t *testing.T) {
	if rowKey("QE-09", "claude", "claude-opus-5", "high", 1) ==
		rowKey("QE-09", "claude", "claude-opus-5", "xhigh", 1) {
		t.Fatal("a different effort must produce a different resume key")
	}
	// The legacy marker is a real value, so it separates too: an unrecorded-effort
	// row must never mark a pinned-effort cell already-done.
	if rowKey("QE-09", "claude", "claude-opus-5", policyeval.EffortUnrecorded, 1) ==
		rowKey("QE-09", "claude", "claude-opus-5", "high", 1) {
		t.Fatal("unrecorded evidence must not satisfy a cell naming a real effort")
	}
}

// Legacy rows carry no `effort` key at all. loadDone must normalize the blank
// the SAME way the config side does, or every legacy row lands under a key
// ending "...|" that no planned cell can ever match — and the resume set
// silently stops working for the whole 825-row table.
func TestLoadDoneNormalizesBlankEffort(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	legacy := `{"ts":"t","task":"AC-10","class":"agentic-coding","lane":"local","model":"gemma4-cascade","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":3804}` + "\n"
	if err := os.WriteFile(p, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	done := loadDone(p).done
	if !done[rowKey("AC-10", "local", "gemma4-cascade", policyeval.EffortUnrecorded, 1)] {
		t.Fatalf("a legacy row must resume under the normalized unrecorded effort: %v", done)
	}
	if done[rowKey("AC-10", "local", "gemma4-cascade", "low", 1)] {
		t.Fatal("a legacy row must NOT mark a pinned-effort cell done")
	}
}

// Both fields are required, for the same reason: the row records the PIN, so an
// unpassed flag writes evidence under a configuration nobody chose.
func TestRequireConfigPinsCoversEffort(t *testing.T) {
	cfgs := map[string]policyeval.Config{
		"claude": {Model: "claude-opus-4-8", Effort: "xhigh"},
		"codex":  {Model: "gpt-5.6-terra", Effort: ""},
		"glm":    {Model: "", Effort: "high"},
	}
	got := requireConfigPins([]string{"claude", "codex", "glm"}, cfgs)
	if strings.Join(got, ",") != "codex,glm" {
		t.Fatalf("requireConfigPins = %v, want [codex glm] (missing effort, missing model)", got)
	}
	if pf := pinFlagsFor([]string{"codex"}); !strings.Contains(pf, "-codex-effort") {
		t.Fatalf("the error must name the effort flag too, got %q", pf)
	}
}

func TestExtractDiff(t *testing.T) {
	cases := []struct{ name, in, wantPrefix string }{
		{"clean diff", "diff --git a/x.go b/x.go\n@@ -1 +1 @@\n-a\n+b\n", "diff --git"},
		{"prose then diff", "Here is my change:\n\ndiff --git a/x.go b/x.go\n@@ -1 +1 @@\n-a\n+b\n", "diff --git"},
		{"minimal headers", "some text\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b\n", "--- a/x.go"},
		{"no diff", "I have completed the task successfully.", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		got := extractDiff(c.in)
		if c.wantPrefix == "" {
			if got != "" {
				t.Errorf("%s: want empty, got %q", c.name, got[:min(30, len(got))])
			}
		} else if len(got) < len(c.wantPrefix) || got[:len(c.wantPrefix)] != c.wantPrefix {
			t.Errorf("%s: got %q", c.name, got[:min(30, len(got))])
		}
	}
}

func TestLoadDoneResume(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	body := `{"ts":"t","task":"AC-04","class":"agentic-coding","lane":"local","model":"m","effort":"e","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":5}
not json — torn line survives
{"ts":"t","task":"RS-03","class":"research","lane":"claude","model":"m","effort":"e","trial":2,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	done := loadDone(p).done
	if !done[rowKey("AC-04", "local", "m", "e", 1)] || !done[rowKey("RS-03", "claude", "m", "e", 2)] {
		t.Fatalf("resume set wrong: %v", done)
	}
	// A deferred row is a hole — resume must NOT count it as done.
	deferredLine := `{"ts":"t","task":"EX-01","class":"extraction","lane":"glm","model":"m","effort":"e","trial":1,"dispatched":false,"outcome_class":"deferred","verifier_pass":false,"latency_ms":1}` + "\n"
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(deferredLine)
	f.Close()
	if d := loadDone(p).done; d[rowKey("EX-01", "glm", "m", "e", 1)] {
		t.Fatal("deferred row wrongly counted as done — the window-reopen refill would no-op")
	}
	if done[rowKey("AC-04", "local", "m", "e", 2)] || len(done) != 2 {
		t.Fatalf("resume set has phantom rows: %v", done)
	}
	if rs := loadDone(filepath.Join(t.TempDir(), "absent.jsonl")); rs.done == nil || rs.effortsByCell == nil || rs.modelsByIdent == nil {
		t.Fatal("missing file must return empty sets, not nil")
	}
}

func TestDecodeAgentText(t *testing.T) {
	codexEvents := `{"type":"thread.started","thread_id":"x"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"Here is the fix:\n\ndiff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b"}}
{"type":"turn.completed","usage":{"input_tokens":1}}`
	got := decodeAgentText(codexEvents)
	if !strings.Contains(got, "diff --git a/x.go") || strings.Contains(got, `\n`) {
		t.Fatalf("codex decode wrong: %q", got)
	}
	if d := extractDiff(got); !strings.HasPrefix(d, "diff --git a/x.go") {
		t.Fatalf("extract from decoded failed: %q", d)
	}

	claudeResult := `{"type":"result","subtype":"success","result":"done:\ndiff --git a/y.go b/y.go\n@@ -1 +1 @@\n-c\n+d","num_turns":1}`
	if got := decodeAgentText(claudeResult); !strings.Contains(got, "diff --git a/y.go") {
		t.Fatalf("claude result decode wrong: %q", got)
	}

	if got := decodeAgentText("plain prose, no json"); got != "" {
		t.Fatalf("non-json must decode empty, got %q", got)
	}
}

func TestTruncateDiff(t *testing.T) {
	withProse := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n-a\n+b\n context\n\nThis change fixes the bug by..."
	got := truncateDiff(withProse)
	if strings.Contains(got, "This change") {
		t.Fatalf("prose not cut: %q", got)
	}
	if !strings.HasSuffix(got, " context") {
		t.Fatalf("diff body truncated too early: %q", got)
	}
	if truncateDiff("") != "" {
		t.Fatal("empty must stay empty")
	}
	clean := "diff --git a/x.go b/x.go\n--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-a\n+b"
	if truncateDiff(clean) != clean {
		t.Fatalf("clean diff altered: %q", truncateDiff(clean))
	}
}

func TestRouterClass(t *testing.T) {
	for in, want := range map[string]string{
		"agentic-coding": "workhorse-coding", "quick-edit": "workhorse-coding",
		"research": "deep-reasoning", "extraction": "mechanical-text",
		"review": "verify-gate", "unknown": "",
	} {
		if got := routerClass(in); got != want {
			t.Errorf("routerClass(%s)=%q want %q", in, got, want)
		}
	}
}

// ── effort-drift guard (review 2026-08-12, round 3) ─────────────────────────
//
// The previous guard keyed on the AGGREGATE plan.Skipped == 0, which was wrong
// in both directions: it refused every legitimate first measurement of new
// cells against a populated oracle, and it was silent on PARTIAL drift (pin
// one lane's real effort while another lane still matches). Drift is per-cell:
// a planned cell whose (task,lane,model,trial) identity is already recorded
// under some OTHER effort.

// driftFixture builds a plan + effort index from terse cell descriptions.
func driftCell(task, lane, model, effort string, trial int) plannedCell {
	return plannedCell{Task: task, Trial: trial,
		Config: policyeval.Config{Lane: lane, Model: model, Effort: effort}}
}

func TestDetectEffortDrift_LegacyRepinDrifts(t *testing.T) {
	// The 476-cell incident shape: rows recorded pre-effort-capture resume as
	// "unrecorded"; the operator pins a real effort; every cell re-keys.
	effs := map[string]map[string]bool{
		cellKey("T-01", "claude", "m", 1): {policyeval.EffortUnrecorded: true},
		cellKey("T-02", "claude", "m", 1): {policyeval.EffortUnrecorded: true},
	}
	run := []plannedCell{
		driftCell("T-01", "claude", "m", "high", 1),
		driftCell("T-02", "claude", "m", "high", 1),
	}
	d := detectDrift(run, resumeState{effortsByCell: effs})
	if len(d.effortDrift) != 2 {
		t.Fatalf("both re-keyed cells must drift, got %d", len(d.effortDrift))
	}
	if len(d.recordedEfforts) != 1 || d.recordedEfforts[0] != policyeval.EffortUnrecorded {
		t.Fatalf("the recorded efforts must be named for the error message, got %v", d.recordedEfforts)
	}
}

func TestDetectDrift_NewCellsDoNotDrift(t *testing.T) {
	rs := resumeState{
		effortsByCell: map[string]map[string]bool{
			cellKey("T-01", "local", "m", 1): {policyeval.EffortUnrecorded: true},
		},
		modelsByIdent: map[string]map[string]bool{
			identKey("T-01", "local", 1): {"m": true},
		},
	}
	for name, run := range map[string][]plannedCell{
		"new lane":  {driftCell("T-01", "glm", "glm-5.2", policyeval.EffortUnrecorded, 1)},
		"new task":  {driftCell("T-99", "local", "m", policyeval.EffortUnrecorded, 1)},
		"new trial": {driftCell("T-01", "local", "m", policyeval.EffortUnrecorded, 2)},
	} {
		if d := detectDrift(run, rs); d.any() {
			t.Fatalf("%s: a cell with no recorded (task,lane,trial) counterpart is a first measurement, not drift: %+v", name, d)
		}
	}
}

// A planned cell whose (task,lane,trial) is recorded only under OTHER models
// is MODEL drift: a pin typo re-dispatches (and mislabels) the whole table if
// nothing refuses, and a deliberate new-model measurement is the -re-measure
// override, not a silent pass — v0.31.0's aggregate guard refused this shape
// and dropping it without a replacement was the round-4 MAJOR.
func TestDetectDrift_ModelRekeyDrifts(t *testing.T) {
	rs := resumeState{
		effortsByCell: map[string]map[string]bool{
			cellKey("T-01", "claude", "claude-sonnet-5", 1): {policyeval.EffortUnrecorded: true},
		},
		modelsByIdent: map[string]map[string]bool{
			identKey("T-01", "claude", 1): {"claude-sonnet-5": true},
		},
	}
	// The typo shape: claude-sonnet-55 was never recorded at any effort, so
	// the effort tier is blind to it — the model tier must catch it.
	d := detectDrift([]plannedCell{driftCell("T-01", "claude", "claude-sonnet-55", policyeval.EffortUnrecorded, 1)}, rs)
	if len(d.modelDrift) != 1 || len(d.effortDrift) != 0 {
		t.Fatalf("a model re-key must be caught by the model tier: %+v", d)
	}
	if len(d.recordedModels) != 1 || d.recordedModels[0] != "claude-sonnet-5" {
		t.Fatalf("the recorded models must be named for the error message, got %v", d.recordedModels)
	}
}

// A cell that drifts on effort is reported ONCE, by the effort tier — never
// double-counted into both.
//
// From a loadDone-built state the two tiers are mutually exclusive by
// construction: a non-empty effortsByCell entry for the PLANNED model implies
// loadDone also indexed that model, which silences the model tier. The
// `continue` is therefore defensive, not load-bearing — so this test builds
// the INCONSISTENT state loadDone cannot produce (effort recorded for the
// planned model, the model index missing it) and pins that the cell is still
// counted exactly once, in the effort tier. Without the fixture the test named
// a precedence it could not reach (review round 4).
func TestDetectDrift_EffortTierTakesPrecedence(t *testing.T) {
	inconsistent := resumeState{
		effortsByCell: map[string]map[string]bool{
			cellKey("T-01", "claude", "m", 1): {policyeval.EffortUnrecorded: true},
		},
		modelsByIdent: map[string]map[string]bool{
			identKey("T-01", "claude", 1): {"other-model": true}, // does NOT contain "m"
		},
	}
	d := detectDrift([]plannedCell{driftCell("T-01", "claude", "m", "high", 1)}, inconsistent)
	if len(d.effortDrift) != 1 || len(d.modelDrift) != 0 {
		t.Fatalf("a cell satisfying BOTH tiers must be reported once, by the effort tier: %+v", d)
	}

	// And the ordinary consistent case still reports effort drift alone.
	consistent := resumeState{
		effortsByCell: map[string]map[string]bool{
			cellKey("T-01", "claude", "m", 1): {policyeval.EffortUnrecorded: true},
		},
		modelsByIdent: map[string]map[string]bool{
			identKey("T-01", "claude", 1): {"m": true},
		},
	}
	d = detectDrift([]plannedCell{driftCell("T-01", "claude", "m", "high", 1)}, consistent)
	if len(d.effortDrift) != 1 || len(d.modelDrift) != 0 {
		t.Fatalf("same-model different-effort is effort drift, once: %+v", d)
	}
}

// The recorded-value lists reach an operator-facing refusal, so their order is
// part of the contract: map iteration must never decide it. Removing either
// sort.Strings left the whole suite green (review round 4).
func TestDetectDriftRecordedListsAreSorted(t *testing.T) {
	rs := resumeState{
		effortsByCell: map[string]map[string]bool{
			cellKey("T-01", "claude", "m", 1): {"xhigh": true, "high": true, "low": true, "medium": true},
		},
		modelsByIdent: map[string]map[string]bool{
			identKey("T-02", "claude", 1): {"zeta": true, "alpha": true, "mu": true, "beta": true},
		},
	}
	run := []plannedCell{
		driftCell("T-01", "claude", "m", policyeval.EffortUnrecorded, 1),
		driftCell("T-02", "claude", "omega", policyeval.EffortUnrecorded, 1),
	}
	for i := 0; i < 40; i++ {
		d := detectDrift(run, rs)
		if strings.Join(d.recordedEfforts, ",") != "high,low,medium,xhigh" {
			t.Fatalf("efforts not sorted: %v", d.recordedEfforts)
		}
		if strings.Join(d.recordedModels, ",") != "alpha,beta,mu,zeta" {
			t.Fatalf("models not sorted: %v", d.recordedModels)
		}
	}
}

// A recorded row with NO model indexes under the named sentinel, so the
// refusal says what the rows carry instead of naming nothing — and it points
// at the only escape, since no pin can match an empty recorded model.
func TestLoadDoneNamesAModellessRow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	body := `{"task":"T-01","lane":"local","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := loadDone(p)
	// Indexed RAW (unmatchable, since an empty pin is rejected); the sentinel
	// is a display form applied at the formatting boundary only — see
	// TestSentinelIsNotPinnable for why that distinction is load-bearing.
	if !rs.modelsByIdent[identKey("T-01", "local", 1)][""] {
		t.Fatalf("a model-less row must index under the raw empty model: %v", rs.modelsByIdent)
	}
	d := detectDrift([]plannedCell{driftCell("T-01", "local", "gemma4-cascade", policyeval.EffortUnrecorded, 1)}, rs)
	if len(d.modelDrift) != 1 {
		t.Fatalf("the model-less row must still trip the tier: %+v", d)
	}
	if len(d.recordedModels) != 1 || d.recordedModels[0] != modelUnrecorded {
		t.Fatalf("the refusal must NAME the condition, got %v", d.recordedModels)
	}
	if !d.blankRecordedModel {
		t.Fatal("the blank-model case must be flagged so the refusal can drop its 'never measured at' claim")
	}
}

// loadDone populates the model index from evidence rows only, TRIMMING lane
// and model exactly as the planned side does — a padded recorded field
// otherwise blinds both drift tiers on the seam with money attached.
func TestLoadDoneModelIndexAndTrim(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	body := `{"task":"T-01","lane":"claude ","model":" claude-sonnet-5","effort":"","trial":1,"dispatched":true,"outcome_class":"ok"}
{"task":"T-02","lane":"claude","model":"claude-sonnet-5","trial":1,"dispatched":false,"outcome_class":"error"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := loadDone(p)
	if !rs.done[rowKey("T-01", "claude", "claude-sonnet-5", policyeval.EffortUnrecorded, 1)] {
		t.Fatalf("padded lane/model must resume under the TRIMMED key: %v", rs.done)
	}
	if !rs.modelsByIdent[identKey("T-01", "claude", 1)]["claude-sonnet-5"] {
		t.Fatalf("the model index must hold the trimmed model: %v", rs.modelsByIdent)
	}
	if rs.modelsByIdent[identKey("T-02", "claude", 1)] != nil {
		t.Fatal("a hole (error row) must not enter the model index")
	}
	// And the guard actually fires off the trimmed index: an effort re-key of
	// the padded row must be caught.
	d := detectDrift([]plannedCell{driftCell("T-01", "claude", "claude-sonnet-5", "high", 1)}, rs)
	if len(d.effortDrift) != 1 {
		t.Fatalf("padded recorded row must still trip the effort tier: %+v", d)
	}
}

func TestDetectEffortDrift_PartialDriftIsCaught(t *testing.T) {
	// The case the aggregate tell was blind to: local's keys still match (its
	// cells were skipped upstream and are NOT in run), claude's are re-keyed.
	effs := map[string]map[string]bool{
		cellKey("T-01", "local", "lm", 1):  {policyeval.EffortUnrecorded: true},
		cellKey("T-01", "claude", "cm", 1): {policyeval.EffortUnrecorded: true},
	}
	run := []plannedCell{ // local skipped upstream; only claude planned
		driftCell("T-01", "claude", "cm", "high", 1),
	}
	d := detectDrift(run, resumeState{effortsByCell: effs})
	if len(d.effortDrift) != 1 || d.effortDrift[0].Config.Lane != "claude" {
		t.Fatalf("the drifted lane must be caught even when another lane resumes cleanly: %+v", d)
	}
}

func TestDetectEffortDrift_MixedNewAndDrifted(t *testing.T) {
	effs := map[string]map[string]bool{
		cellKey("T-01", "claude", "m", 1): {policyeval.EffortUnrecorded: true},
	}
	run := []plannedCell{
		driftCell("T-01", "claude", "m", "high", 1), // drift
		driftCell("T-02", "claude", "m", "high", 1), // new task: fine
	}
	d := detectDrift(run, resumeState{effortsByCell: effs})
	if len(d.effortDrift) != 1 || d.effortDrift[0].Task != "T-01" {
		t.Fatalf("only the re-keyed cell drifts, got %+v", d)
	}
}

// loadDone's second return feeds the drift detector: evidence rows indexed by
// effort-less identity, holes excluded (re-dispatching a hole was always going
// to happen — not a re-spend the guard needs to refuse).
func TestLoadDoneEffortIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	body := `{"task":"T-01","lane":"claude","model":"m","trial":1,"dispatched":true,"outcome_class":"ok"}
{"task":"T-01","lane":"claude","model":"m","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok"}
{"task":"T-02","lane":"claude","model":"m","trial":1,"dispatched":false,"outcome_class":"error"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	effs := loadDone(p).effortsByCell
	got := effs[cellKey("T-01", "claude", "m", 1)]
	if !got[policyeval.EffortUnrecorded] || !got["high"] || len(got) != 2 {
		t.Fatalf("both recorded efforts must be indexed under the cell identity, got %v", got)
	}
	if effs[cellKey("T-02", "claude", "m", 1)] != nil {
		t.Fatal("a hole (error row) must not enter the drift index — re-attempting it is not a re-spend")
	}
}

// The evidence definition is policyeval.IsEvidence — error/exit-N/verify_error
// rows are HOLES on the resume side exactly as they are on the scoring side,
// or a cell is simultaneously "already recorded" and "not evidence" forever.
func TestLoadDoneExcludesNonEvidenceRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	body := `{"task":"T-01","lane":"local","model":"m","effort":"e","trial":1,"dispatched":false,"outcome_class":"error"}
{"task":"T-02","lane":"local","model":"m","effort":"e","trial":1,"dispatched":true,"outcome_class":"exit-4"}
{"task":"T-03","lane":"local","model":"m","effort":"e","trial":1,"dispatched":true,"outcome_class":"verify_error"}
{"task":"T-04","lane":"local","model":"m","effort":"e","trial":1,"dispatched":true,"outcome_class":"ok"}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	done := loadDone(p).done
	for _, task := range []string{"T-01", "T-02", "T-03"} {
		if done[rowKey(task, "local", "m", "e", 1)] {
			t.Fatalf("%s is a hole and must be re-attemptable, not recorded", task)
		}
	}
	if !done[rowKey("T-04", "local", "m", "e", 1)] {
		t.Fatal("the ok row is evidence and must be recorded")
	}
}

// ── verify seam classification (review 2026-08-12, round 4) ─────────────────

// TestHelperProcess is not a test: it lets the tests below manufacture REAL
// *exec.ExitError values with chosen exit codes (the standard os/exec trick).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT_CODE"))
	os.Exit(code)
}

func realExitError(t *testing.T, code int) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", fmt.Sprintf("HELPER_EXIT_CODE=%d", code))
	err := cmd.Run()
	if err == nil {
		t.Fatalf("helper exited 0, wanted %d", code)
	}
	return err
}

// The three verify outcomes are not interchangeable: exit 0 is a measured
// pass, exit 1 a measured failure, and ANYTHING else is the verifier's own
// infrastructure breaking — a hole (verify_error), never a measured failure.
// Before this classification a missing goldverify binary scored an entire
// replay as incompetent.
func TestApplyVerifyOutcome(t *testing.T) {
	base := func() Row { return Row{Task: "T-01", Dispatched: true, OutcomeClass: "ok"} }

	r := base()
	applyVerifyOutcome(&r, []byte(`{"pass":true}`), nil)
	if !r.VerifierPass || r.OutcomeClass != "ok" {
		t.Fatalf("exit 0 must be a measured pass: %+v", r)
	}

	r = base()
	applyVerifyOutcome(&r, []byte(`{"detail":"held-out test failed"}`), realExitError(t, 1))
	if r.VerifierPass || r.OutcomeClass != "ok" || !strings.Contains(r.Note, "verify-fail") {
		t.Fatalf("exit 1 must be a measured failure with WHY in the note: %+v", r)
	}
	if !policyeval.IsEvidence(r.Dispatched, r.OutcomeClass) {
		t.Fatal("a measured verify failure IS evidence")
	}

	r = base()
	applyVerifyOutcome(&r, []byte("git: bad -repos path"), realExitError(t, 2))
	if r.OutcomeClass != "verify_error" {
		t.Fatalf("an unexpected verifier exit must be verify_error, got %+v", r)
	}
	if policyeval.IsEvidence(r.Dispatched, r.OutcomeClass) {
		t.Fatal("verifier infrastructure failure must be a HOLE, not evidence")
	}

	r = base()
	spawnErr := exec.Command(filepath.Join(t.TempDir(), "no-such-goldverify.exe")).Run()
	if spawnErr == nil {
		t.Fatal("expected a spawn error")
	}
	applyVerifyOutcome(&r, nil, spawnErr)
	if r.OutcomeClass != "verify_error" || !strings.Contains(r.Note, "goldverify:") {
		t.Fatalf("a missing verifier binary must be verify_error: %+v", r)
	}
}

// looksLikeOracle's majority-identity refusal threshold: a file where more
// than half the rows carry no task+lane is refused outright (a mistyped path
// pointed at dispatch.jsonl), while a minority passes with the VERBATIM
// warning. The threshold itself had no test (round-4 finding).
func TestLooksLikeOracleRefusalThreshold(t *testing.T) {
	oracleRow := `{"task":"T-01","lane":"local","model":"m","trial":1,"dispatched":true,"outcome_class":"ok"}`
	foreign := `{"ts":"t","tokens":1,"usd":0.1}`

	if err := looksLikeOracle(fixture(oracleRow, foreign, foreign)); err == nil {
		t.Fatal("2 of 3 identity-less rows must refuse the rewrite")
	}
	if err := looksLikeOracle(fixture(oracleRow, oracleRow, foreign)); err != nil {
		t.Fatalf("1 of 3 identity-less rows passes with a warning, got refusal: %v", err)
	}
	if err := looksLikeOracle([]byte("\n\n")); err == nil {
		t.Fatal("a file with no JSON rows must be refused")
	}
}

// The two drift tiers say DIFFERENT things because they are different facts,
// and the blank-model case says a third. Reverting any of the three to the
// generic "re-spend" sentence left the suite green (review round 5), so the
// message text is pinned here — it is the whole output of a guard whose only
// product is advice on a tool with money attached.
func TestDriftMessagesAreTierAccurate(t *testing.T) {
	effortOnly := resumeState{
		effortsByCell: map[string]map[string]bool{
			cellKey("T-01", "claude", "m", 1): {policyeval.EffortUnrecorded: true},
		},
		modelsByIdent: map[string]map[string]bool{identKey("T-01", "claude", 1): {"m": true}},
	}
	d := detectDrift([]plannedCell{driftCell("T-01", "claude", "m", "high", 1)}, effortOnly)
	msg := driftRefusal(d)
	if !strings.Contains(msg, "RE-SPENDS") {
		t.Fatalf("the effort tier DOES re-spend and must say so: %s", msg)
	}

	modelOnly := resumeState{
		effortsByCell: map[string]map[string]bool{},
		modelsByIdent: map[string]map[string]bool{identKey("T-01", "claude", 1): {"other": true}},
	}
	d = detectDrift([]plannedCell{driftCell("T-01", "claude", "m", "high", 1)}, modelOnly)
	msg = driftRefusal(d)
	if strings.Contains(msg, "RE-SPENDS") {
		t.Fatalf("a model-tier cell was never recorded at this model — it ADDS a measurement, it does not re-spend: %s", msg)
	}
	if !strings.Contains(msg, "ADDS a first measurement") {
		t.Fatalf("the model tier must say what it actually does: %s", msg)
	}

	// The blank-model branch: an unlabeled row does NOT say which model
	// produced it, so "never measured at this model" would be a lie and
	// -re-measure may re-buy recorded work.
	blank := resumeState{
		effortsByCell: map[string]map[string]bool{},
		modelsByIdent: map[string]map[string]bool{identKey("T-01", "local", 1): {"": true}},
	}
	d = detectDrift([]plannedCell{driftCell("T-01", "local", "gemma4-cascade", policyeval.EffortUnrecorded, 1)}, blank)
	msg = driftRefusal(d)
	if strings.Contains(msg, "never measured at") {
		t.Fatalf("an unlabeled row may BE a measurement at the pin; the message must not claim otherwise: %s", msg)
	}
	if !strings.Contains(msg, "could re-buy") || !strings.Contains(msg, modelUnrecorded) {
		t.Fatalf("the blank-model branch must name the condition and the re-buy risk: %s", msg)
	}
}

// The sentinel is a DISPLAY string, never an index key. Indexing it alongside
// real pins made it selectable: pinning it literally matched a model-less row,
// silenced BOTH drift tiers, and appended evidence under a fabricated model —
// a message fix that opened a hole in the money guard (review round 5).
func TestSentinelIsNotPinnable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	body := `{"task":"T-01","lane":"local","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rs := loadDone(p)
	if !rs.modelsByIdent[identKey("T-01", "local", 1)][""] {
		t.Fatalf("a blank recorded model must index under the RAW empty string: %v", rs.modelsByIdent)
	}
	if rs.modelsByIdent[identKey("T-01", "local", 1)][modelUnrecorded] {
		t.Fatal("the display sentinel must NOT be in the matching namespace — pinning it would disarm the guard")
	}
	// Pinning the sentinel string must still trip the guard.
	d := detectDrift([]plannedCell{driftCell("T-01", "local", modelUnrecorded, policyeval.EffortUnrecorded, 1)}, rs)
	if len(d.modelDrift) != 1 {
		t.Fatalf("pinning the sentinel must NOT match a blank recorded model: %+v", d)
	}
	// ...and the message still names the condition via the display form.
	if len(d.recordedModels) != 1 || d.recordedModels[0] != modelUnrecorded {
		t.Fatalf("the refusal must display the blank as the sentinel: %v", d.recordedModels)
	}
}
