package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	sonnet := rowKey("RS-01", "claude", "claude-sonnet-5", 1)
	opus := rowKey("RS-01", "claude", "claude-opus-4-8", 1)
	if sonnet == opus {
		t.Fatal("a different model MUST produce a different resume key, or the mandatory pin is a no-op")
	}
	if sonnet != rowKey("RS-01", "claude", "claude-sonnet-5", 1) {
		t.Fatal("same config must produce a stable key")
	}
	if sonnet == rowKey("RS-01", "claude", "claude-sonnet-5", 2) {
		t.Fatal("trial must still separate keys")
	}
	if sonnet == rowKey("RS-01", "codex", "claude-sonnet-5", 1) {
		t.Fatal("lane must still separate keys")
	}
}

// The regression the review actually reproduced, at the loadDone level: an
// existing sonnet observation must NOT mark the opus cell as already-recorded.
func TestLoadDoneDoesNotCreditADifferentModel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "oracle.jsonl")
	row := `{"ts":"t","task":"RS-01","class":"research","lane":"claude","model":"claude-sonnet-5","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}` + "\n"
	if err := os.WriteFile(p, []byte(row), 0o644); err != nil {
		t.Fatal(err)
	}
	done := loadDone(p)
	if !done[rowKey("RS-01", "claude", "claude-sonnet-5", 1)] {
		t.Fatal("the recorded sonnet cell must be marked done")
	}
	if done[rowKey("RS-01", "claude", "claude-opus-4-8", 1)] {
		t.Fatal("a sonnet row must NOT mark the opus cell done — that is the reproduced no-op")
	}
}

// The gate validated strings.TrimSpace(pin) while the run passed the raw value
// through to both the oracle row and the -model dispatch arg, so a padded pin
// passed validation and was recorded as a distinct model string.
func TestPinsAreTrimmedNotJustValidated(t *testing.T) {
	got := normalizePins(map[string]string{"claude": "  claude-opus-4-8  ", "codex": "gpt-5.6-terra"})
	if got["claude"] != "claude-opus-4-8" {
		t.Fatalf("pin must be trimmed for USE, got %q", got["claude"])
	}
	if got["codex"] != "gpt-5.6-terra" {
		t.Fatalf("an already-clean pin must be untouched, got %q", got["codex"])
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

func TestRequireModelPins(t *testing.T) {
	cases := []struct {
		name  string
		lanes []string
		model map[string]string
		want  []string
	}{
		{"all pinned", []string{"claude", "codex"},
			map[string]string{"claude": "claude-opus-5", "codex": "gpt-5.6-terra"}, nil},
		{"claude unpinned is caught", []string{"claude", "codex"},
			map[string]string{"claude": "", "codex": "gpt-5.6-terra"}, []string{"claude"}},
		{"whitespace is not a pin", []string{"claude"},
			map[string]string{"claude": "   "}, []string{"claude"}},
		{"only replayed lanes are required", []string{"claude"},
			map[string]string{"claude": "claude-opus-5", "glm": ""}, nil},
		{"order follows the lane list, not the map", []string{"local", "glm"},
			map[string]string{"local": "", "glm": ""}, []string{"local", "glm"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := requireModelPins(c.lanes, c.model)
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Fatalf("requireModelPins = %v, want %v", got, c.want)
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
	body := `{"ts":"t","task":"AC-04","class":"agentic-coding","lane":"local","model":"m","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"latency_ms":5}
not json — torn line survives
{"ts":"t","task":"RS-03","class":"research","lane":"claude","model":"m","trial":2,"dispatched":true,"outcome_class":"ok","verifier_pass":true,"latency_ms":9}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	done := loadDone(p)
	if !done[rowKey("AC-04", "local", "m", 1)] || !done[rowKey("RS-03", "claude", "m", 2)] {
		t.Fatalf("resume set wrong: %v", done)
	}
	// A deferred row is a hole — resume must NOT count it as done.
	deferredLine := `{"ts":"t","task":"EX-01","class":"extraction","lane":"glm","model":"m","trial":1,"dispatched":false,"outcome_class":"deferred","verifier_pass":false,"latency_ms":1}` + "\n"
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString(deferredLine)
	f.Close()
	if loadDone(p)[rowKey("EX-01", "glm", "m", 1)] {
		t.Fatal("deferred row wrongly counted as done — the window-reopen refill would no-op")
	}
	if done[rowKey("AC-04", "local", "m", 2)] || len(done) != 2 {
		t.Fatalf("resume set has phantom rows: %v", done)
	}
	if loadDone(filepath.Join(t.TempDir(), "absent.jsonl")) == nil {
		t.Fatal("missing file must return empty set, not nil")
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
