package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// The skill hint suggested skills the model CANNOT invoke: ~/.claude/skills entries the
// operator hid with skillOverrides ("user-invocable-only" / "off"), and skills of
// installed-but-disabled plugins. Observed live 2026-09-21 on nearly every prompt of a
// session (gstack-review, gstack-ship, remember:remember,
// claude-md-management:claude-md-improver ...). A suggestion the model cannot act on is
// pure context cost.

func writeClaudeConfig(t *testing.T, settings string, installed []string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	if installed != nil {
		plugins := map[string]any{}
		for _, p := range installed {
			plugins[p] = []any{}
		}
		raw, _ := json.Marshal(map[string]any{"version": 2, "plugins": plugins})
		os.MkdirAll(filepath.Join(dir, "plugins"), 0o755)
		os.WriteFile(filepath.Join(dir, "plugins", "installed_plugins.json"), raw, 0o644)
	}
	return dir
}

func sk(id, source string) catalog.Skill { return catalog.Skill{ID: id, Name: id, Source: source} }

func TestHiddenUserSkillsAreNotInvocable(t *testing.T) {
	dir := writeClaudeConfig(t, `{"skillOverrides":{"gstack-review":"user-invocable-only","old-thing":"off","grill-me":"name-only"}}`, nil)
	v := loadSkillVisibility(dir)
	for id, want := range map[string]bool{
		"gstack-review": false, // hidden from the model, only /name works
		"old-thing":     false, // hidden entirely
		"grill-me":      true,  // listed without description: still model-invocable
		"clean-ship":    true,  // no override
	} {
		if got := v.modelInvocable(sk(id, "skills")); got != want {
			t.Errorf("%s: modelInvocable=%v want %v", id, got, want)
		}
	}
}

func TestDisabledInstalledPluginSkillsAreNotInvocable(t *testing.T) {
	dir := writeClaudeConfig(t,
		`{"enabledPlugins":{"superpowers@official":true,"claude-md-management@official":false}}`,
		[]string{"superpowers@official", "claude-md-management@official", "remember@official"})
	v := loadSkillVisibility(dir)
	for _, c := range []struct {
		id, src string
		want    bool
	}{
		{"superpowers:brainstorming", "superpowers", true},                         // enabled
		{"claude-md-management:claude-md-improver", "claude-md-management", false}, // explicitly false
		{"remember:remember", "remember", false},                                   // installed, never enabled
		{"anthropic-skills:docx", "anthropic-skills", true},                        // not a plugin (account sync): keep
	} {
		if got := v.modelInvocable(sk(c.id, c.src)); got != c.want {
			t.Errorf("%s: modelInvocable=%v want %v", c.id, got, c.want)
		}
	}
}

func TestAgentsToolsAndCommandsAreNeverFiltered(t *testing.T) {
	dir := writeClaudeConfig(t, `{"skillOverrides":{"x":"off"}}`, []string{"p@m"})
	v := loadSkillVisibility(dir)
	for _, id := range []string{"agent:context-engineer", "tool:offload-repo-recon", "/insights"} {
		if !v.modelInvocable(sk(id, "p")) {
			t.Errorf("%s must never be filtered: it is not a Skill-tool entry", id)
		}
	}
}

// Fail open: an unreadable or corrupt settings file filters NOTHING (the old behaviour).
func TestUnreadableSettingsFiltersNothing(t *testing.T) {
	for _, dir := range []string{t.TempDir(), writeClaudeConfig(t, `{not json`, nil)} {
		v := loadSkillVisibility(dir)
		if !v.modelInvocable(sk("gstack-review", "skills")) {
			t.Fatal("no readable settings: nothing may be filtered")
		}
	}
}

func TestFilterInvocableSplitsShownAndHidden(t *testing.T) {
	dir := writeClaudeConfig(t, `{"skillOverrides":{"gstack-review":"user-invocable-only"}}`, nil)
	byID := map[string]catalog.Skill{"gstack-review": sk("gstack-review", "skills"), "clean-ship": sk("clean-ship", "skills")}
	shown, hidden := filterInvocable(byID, []string{"gstack-review", "clean-ship"}, loadSkillVisibility(dir))
	if strings.Join(shown, ",") != "clean-ship" || strings.Join(hidden, ",") != "gstack-review" {
		t.Fatalf("shown=%v hidden=%v", shown, hidden)
	}
}

// Drives the BUILT binary against a fake llama-swap that surfaces the fixture skill
// gstack-qa. Hidden by skillOverrides it must vanish from the output AND from the
// usage log's Surfaced (which feeds the surfaced-vs-invoked outcome join); it is
// recorded in Hidden instead. Control arm: with no override it surfaces.
func TestHintOmitsUninvocableSkillE2E(t *testing.T) {
	bin := buildMRHook(t)
	var embedCap, rerankCap atomic.Value
	srv := templatedRankerServer(t, &embedCap, &rerankCap)
	prompt := "QA test my running web application"

	run := func(settings string) (string, string) {
		work := t.TempDir()
		idx := writeIndex(t, work, "embeddinggemma/tpl1", "tpl1")
		logPath := filepath.Join(work, "usage.jsonl")
		cmd := exec.Command(bin, "-index", idx, "-log", logPath, "-endpoint", srv.URL, "-ranker", "hybrid",
			"-quota-hint=false", "-timeout-ms", "5000")
		in, _ := json.Marshal(map[string]string{"prompt": prompt})
		cmd.Stdin = bytes.NewReader(in)
		cmd.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+writeClaudeConfig(t, settings, nil))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("mr-hook exited non-zero: %v", err)
		}
		return string(out), logPath
	}

	if out, _ := run(`{}`); !strings.Contains(out, "gstack-qa") {
		t.Fatalf("control arm: with no override gstack-qa must surface, got %q", out)
	}
	out, logPath := run(`{"skillOverrides":{"gstack-qa":"user-invocable-only"}}`)
	if strings.Contains(out, "gstack-qa") {
		t.Fatalf("a skill hidden from the model was still suggested: %q", out)
	}
	rec := readLastRecord(t, logPath)
	if len(rec.Surfaced) != 0 || strings.Join(rec.Hidden, ",") != "gstack-qa" {
		t.Fatalf("usage log: surfaced=%v hidden=%v (want [] / [gstack-qa])", rec.Surfaced, rec.Hidden)
	}
}
