package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/goldtask"
	"github.com/dmmdea/meta-router/internal/policyeval"
)

// The free-provider lanes print the vendor's OpenAI chat-completion body
// verbatim; the decoder must recover the answer from choices[0].message.content
// and the served model from the body — compact or pretty-printed.
func TestDecodeAgentStreamOpenAIBody(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "free", "groq-chat-200.json"))
	if err != nil {
		t.Fatal(err)
	}
	text, served := decodeAgentStream(string(body))
	if text != "LANE PROBE OK" || served != "openai/gpt-oss-120b" {
		t.Fatalf("compact body: text=%q served=%q", text, served)
	}
	pretty := "{\n  \"model\": \"meta-llama/llama-3.3-70b-instruct:free\",\n  \"choices\": [\n    {\n      \"message\": {\"content\": \"diff --git a/x b/x\"}\n    }\n  ]\n}\n"
	text, served = decodeAgentStream(pretty)
	if text != "diff --git a/x b/x" || served != "meta-llama/llama-3.3-70b-instruct:free" {
		t.Fatalf("pretty body: text=%q served=%q", text, served)
	}
	// Other lanes' shapes are untouched.
	if text, _ := decodeAgentStream(`{"type":"result","result":"claude says hi"}`); text != "claude says hi" {
		t.Fatalf("claude result shape regressed: %q", text)
	}
}

// Free lanes dispatch WITHOUT -cwd (HTTP transport, no working directory); every
// other lane keeps it.
func TestLaneCWDArgOmitsFreeLanes(t *testing.T) {
	for _, lane := range []string{"groq", "cloudflare", "openrouter", "nim", "gemini"} {
		if got := laneCWDArg(lane, `C:\tmp\wt`); got != nil {
			t.Fatalf("%s must not carry -cwd: %v", lane, got)
		}
	}
	for _, lane := range []string{"claude", "codex", "glm", "copilot", "local"} {
		if got := laneCWDArg(lane, `C:\tmp\wt`); len(got) != 2 || got[0] != "-cwd" || got[1] != `C:\tmp\wt` {
			t.Fatalf("%s must carry -cwd: %v", lane, got)
		}
	}
	if got := laneCWDArg("claude", ""); got != nil {
		t.Fatalf("empty cwd → no arg: %v", got)
	}
}

// A free-lane row carries the served model note, like copilot.
func TestReplayOneFreeLaneRowCarriesServedModel(t *testing.T) {
	_, goldset, _ := intFixture(t, "")
	tasks, err := goldtask.Load(goldset)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("goldset: %v", err)
	}
	cfg := policyeval.Config{Lane: "nim", Model: "nvidia/nemotron-3-ultra-550b-a55b", Effort: policyeval.EffortUnrecorded}
	t.Setenv("GOLDREPLAY_FAKE_ORCH_STDOUT", `{"model":"nvidia/nemotron-3-ultra-550b-a55b","choices":[{"finish_reason":"stop","message":{"content":"answer x"}}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`+"\n")
	t.Setenv("GOLDREPLAY_FAKE_ORCH_EXIT", "0")
	row := replayOne(tasks[0], cfg, 1, os.Args[0], "", "", 30, 10, "")
	if row.OutcomeClass != "ok" || !row.Dispatched || !row.VerifierPass {
		t.Fatalf("fake free dispatch must read ok+pass: %+v", row)
	}
	if !strings.Contains(row.Note, "served=nvidia/nemotron-3-ultra-550b-a55b") {
		t.Fatalf("served model must land on the note: %+v", row)
	}
}

// The pin gate covers the free lanes: -lanes nim without -nim-model/-nim-effort
// refuses before any dispatch, naming the flags.
func TestIntegrationFreeLanePinsRequired(t *testing.T) {
	dir, goldset, oracle := intFixture(t, "")
	code, _, se := intRun(t, dir, "-goldset", goldset, "-out", oracle, "-lanes", "nim",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code == 0 || !strings.Contains(se, "-nim-model") || !strings.Contains(se, "-nim-effort") {
		t.Fatalf("unpinned free lane must refuse naming the flags: exit %d\n%s", code, se)
	}
	if _, err := os.Stat(oracle); !os.IsNotExist(err) {
		t.Fatal("a refused run must not create the oracle")
	}
	// Pinned: passes the gate and reaches dispatch (spawn error on the fake binary).
	code, so, se := intRun(t, dir, "-goldset", goldset, "-out", oracle, "-lanes", "groq",
		"-groq-model", "openai/gpt-oss-120b", "-groq-effort", "unrecorded",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 || !strings.Contains(so, "outcome=error") {
		t.Fatalf("pinned free lane must reach dispatch: exit %d\n%s\n%s", code, so, se)
	}
}
