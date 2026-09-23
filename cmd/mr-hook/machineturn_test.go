package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// Background task notifications reach UserPromptSubmit like typed prompts. Measured on one
// long session: the skill hint fired 25 times on notifications and 7 times on the operator's
// own prompts, and every one of those blocks stays in context for the rest of the session.
// The fixtures below keep the real layouts (tag order, the agent note, the monitor event);
// ids and paths are neutral.

// A background command finished: delivered as the next user turn.
const notifBackgroundCommand = "<task-notification>\n" +
	"<task-id>b75yye2lr</task-id>\n" +
	"<tool-use-id>toolu_01AAAAAAAAAAAAAAAAAAAAAAAA</tool-use-id>\n" +
	"<output-file>/tmp/claude/project/session/tasks/b75yye2lr.output</output-file>\n" +
	"<status>completed</status>\n" +
	"<summary>Background command \"QA test the web application\" completed (exit code 0)</summary>\n" +
	"</task-notification>"

// A subagent finished while the model was busy: Claude Code queues it (a queued_command with
// commandMode "task-notification") and the hook receives the queued text verbatim -- the
// usage log's prompt hash equals the sha256 of the transcript's queued prompt.
const notifQueuedAgent = "<task-notification>\n" +
	"<task-id>a254e70dc0ce1a2bb</task-id>\n" +
	"<tool-use-id>toolu_01BBBBBBBBBBBBBBBBBBBBBBBB</tool-use-id>\n" +
	"<output-file>/tmp/claude/project/session/tasks/a254e70dc0ce1a2bb.output</output-file>\n" +
	"<status>completed</status>\n" +
	"<summary>Agent \"QA test the web application\" finished</summary>\n" +
	"<note>A task-notification fires each time this agent stops with no live background children of its own. " +
	"The user can send it another message and resume it, so the same task-id may notify more than once.</note>\n" +
	"<result>All tests pass with a fresh run (no cache). The web application QA checks are green.</result>\n" +
	"</task-notification>"

// A Monitor event: no tool-use-id and no output file, an <event> body instead.
const notifMonitorEvent = "<task-notification>\n" +
	"<task-id>bx1monitor</task-id>\n" +
	"<summary>Monitor event: \"CI settles\"</summary>\n" +
	"<event>run 42 completed: success</event>\n" +
	"</task-notification>"

const humanPrompt = "QA test my running web application"

func TestIsMachineTurnShapes(t *testing.T) {
	cases := []struct {
		name   string
		prompt string
		want   bool
	}{
		{"background command notification", notifBackgroundCommand, true},
		{"queued agent notification", notifQueuedAgent, true},
		{"monitor event notification", notifMonitorEvent, true},
		{"leading whitespace before the tag", "\n \t\r\n" + notifBackgroundCommand, true},
		{"human prompt", humanPrompt, false},
		{"human prompt quoting a notification", "why did this fire twice? " + notifBackgroundCommand, false},
		{"truncated tag", "<task-notif", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := isMachineTurn(c.prompt); got != c.want {
			t.Errorf("%s: isMachineTurn = %v, want %v", c.name, got, c.want)
		}
	}
}

// countingEmbedServer answers like llama-swap with the vector [1,0] for ANY input, so every
// prompt scores cosine 1.0 against the fixture skill: silence can only come from the guard.
func countingEmbedServer(t *testing.T, embeds *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"embeddinggemma"}]}`))
		case "/v1/embeddings":
			atomic.AddInt32(embeds, 1)
			w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runHookInput runs the built hook on a full payload with extra environment and returns stdout.
func runHookInput(t *testing.T, bin string, payload map[string]string, env []string, args ...string) string {
	t.Helper()
	// bin is the binary this test just built into t.TempDir(); nothing outside the test reaches it.
	cmd := exec.Command(bin, args...) // nosemgrep: go.lang.security.audit.dangerous-exec-command.dangerous-exec-command
	in, _ := json.Marshal(payload)
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("mr-hook exited non-zero: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
}

// hintArm runs one prompt against a fresh index and a counting embedder, with the quota banner
// off and an empty orchestrator state dir, so the output is the skill hint or nothing.
func hintArm(t *testing.T, bin, prompt string) (out string, embeds int32, logPath string) {
	t.Helper()
	srv := countingEmbedServer(t, &embeds)
	work := t.TempDir()
	idx := writeIndex(t, work, "embeddinggemma/tpl1", "tpl1")
	logPath = filepath.Join(work, "usage.jsonl")
	out = runHookInput(t, bin, map[string]string{"prompt": prompt},
		[]string{"MR_ORCH_STATE=" + t.TempDir()},
		"-index", idx, "-log", logPath, "-endpoint", srv.URL, "-min-cosine", "0.40",
		"-quota-hint=false", "-timeout-ms", "5000")
	return out, atomic.LoadInt32(&embeds), logPath
}

func assertSilentMachineTurn(t *testing.T, bin, prompt string) {
	t.Helper()
	out, embeds, logPath := hintArm(t, bin, prompt)
	if out != "" {
		t.Fatalf("a task notification is not a request: the hook must inject nothing, got %q", out)
	}
	if embeds != 0 {
		t.Fatalf("the notification text reached the embedder %d time(s); a machine turn runs no retrieval", embeds)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "machine-turn" || len(rec.Surfaced) != 0 || len(rec.Cands) != 0 {
		t.Fatalf("usage row: mode=%q surfaced=%v cands=%v (want machine-turn, none, none)", rec.Mode, rec.Surfaced, rec.Cands)
	}
}

func TestHintSilentOnTaskNotificationE2E(t *testing.T) {
	bin := buildMRHook(t)
	assertSilentMachineTurn(t, bin, notifBackgroundCommand)
	assertSilentMachineTurn(t, bin, notifMonitorEvent)
}

func TestHintSilentOnQueuedNotificationE2E(t *testing.T) {
	bin := buildMRHook(t)
	assertSilentMachineTurn(t, bin, notifQueuedAgent)
	// The same payload with leading whitespace is still the same machine turn.
	assertSilentMachineTurn(t, bin, "\n  "+notifQueuedAgent)
}

// Control arm: the identical setup still surfaces the hint for the operator's own prompt, so the
// silence above comes from the turn type and not from a broken fixture.
func TestHintStillShownOnHumanPromptE2E(t *testing.T) {
	bin := buildMRHook(t)
	out, embeds, logPath := hintArm(t, bin, humanPrompt)
	if !strings.Contains(out, "relevant installed skills") || !strings.Contains(out, "gstack-qa") {
		t.Fatalf("a human prompt must still get the skill hint, got %q", out)
	}
	if embeds == 0 {
		t.Fatal("a human prompt must still be embedded")
	}
	if rec := readLastRecord(t, logPath); rec.Mode != "embed" || strings.Join(rec.Surfaced, ",") != "gstack-qa" {
		t.Fatalf("usage row: mode=%q surfaced=%v", rec.Mode, rec.Surfaced)
	}
}

// snapshotDir maps every file under dir to its bytes: the hook reads orchestrator state and
// must never write it.
func snapshotDir(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		snap[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Account protection is never gated by turn type: with the GLM hard-stop latched, a
// notification turn in an ESTABLISHED session (so the first-prompt rule cannot be what shows
// it) still carries the banner, while the skill hint stays silent.
func TestGLMLatchBannerOnNotificationTurnE2E(t *testing.T) {
	bin := buildMRHook(t)
	var embeds int32
	srv := countingEmbedServer(t, &embeds)
	work := t.TempDir()
	stateDir := filepath.Join(work, "orchstate")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "glm-alert.json"), []byte(`{"note":"test latch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	established := filepath.Join(work, "established.jsonl")
	if err := os.WriteFile(established, []byte(`{"type":"user"}`+"\n"+`{"type":"assistant","message":{}}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx := writeIndex(t, work, "embeddinggemma/tpl1", "tpl1")
	logPath := filepath.Join(work, "usage.jsonl")
	before := snapshotDir(t, stateDir)

	out := runHookInput(t, bin, map[string]string{"prompt": notifQueuedAgent, "transcript_path": established},
		[]string{"MR_ORCH_STATE=" + stateDir, "CLAUDE_STATE_DIR=" + filepath.Join(work, "claudestate")},
		"-index", idx, "-log", logPath, "-endpoint", srv.URL, "-min-cosine", "0.40", "-timeout-ms", "5000")

	if !strings.Contains(out, "glm HARD-STOP(1313)") {
		t.Fatalf("the GLM hard-stop banner must show on a notification turn, got %q", out)
	}
	if strings.Contains(out, "relevant installed skills") || strings.Contains(out, "gstack-qa") {
		t.Fatalf("the skill hint must stay silent on a notification turn: %q", out)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "machine-turn" || !rec.QuotaHint {
		t.Fatalf("usage row: mode=%q quotaHint=%v (want machine-turn, true)", rec.Mode, rec.QuotaHint)
	}
	after := snapshotDir(t, stateDir)
	if strings.Join(keys(before), "|") != strings.Join(keys(after), "|") {
		t.Fatalf("mr-hook wrote orchestrator state: before %v, after %v", keys(before), keys(after))
	}
	for p, b := range before {
		if after[p] != b {
			t.Fatalf("mr-hook rewrote %s", p)
		}
	}
	if _, err := os.Stat(filepath.Join(work, "claudestate")); !os.IsNotExist(err) {
		t.Fatalf("mr-hook created Claude-side state: %v", err)
	}
}
