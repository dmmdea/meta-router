package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// skillVisibility is the part of the operator's Claude Code settings that decides whether
// the MODEL can invoke a skill at all. The hint used to surface skills it could not:
// ~/.claude/skills entries hidden by skillOverrides, and skills of plugins that are
// installed but not enabled. A suggestion the model cannot act on is pure context cost.
type skillVisibility struct {
	ok        bool
	overrides map[string]string // skillOverrides: on | name-only | user-invocable-only | off
	installed map[string]bool   // plugin names in plugins/installed_plugins.json
	enabled   map[string]bool   // plugin names whose enabledPlugins entry is true
}

// claudeConfigDir honours CLAUDE_CONFIG_DIR, as Claude Code does.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// settingsFields are the two keys this filter needs, from any one scope's file.
type settingsFields struct {
	SkillOverrides map[string]string `json:"skillOverrides"`
	EnabledPlugins map[string]bool   `json:"enabledPlugins"`
}

// readScope reads one settings file. present=false for a missing file (a scope that is
// simply not configured); ok=false for a file that exists but cannot be read or parsed.
func readScope(path string) (f settingsFields, present, ok bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, false, true
		}
		return f, true, false
	}
	if json.Unmarshal(raw, &f) != nil {
		return f, true, false
	}
	return f, true, true
}

// loadSkillVisibility merges the scopes Claude Code merges, in its precedence order
// user < project < local (later wins per key): userDir/settings.json, then the
// session's project <projectDir>/.claude/settings.json and settings.local.json.
// Reading only the user file was wrong both ways (review of PR #70): a plugin a PROJECT
// enables was hidden, and a skill a project-LOCAL file turns off was still suggested.
// Managed/policy settings are not read; they are rare and only ever restrict.
//
// Fail-open: an unreadable or corrupt user file, or a corrupt project/local file, makes
// the merged view unknowable -- the zero value filters NOTHING (the old behaviour).
func loadSkillVisibility(userDir, projectDir string) skillVisibility {
	if userDir == "" {
		return skillVisibility{}
	}
	paths := []string{filepath.Join(userDir, "settings.json")}
	if projectDir != "" {
		paths = append(paths,
			filepath.Join(projectDir, ".claude", "settings.json"),
			filepath.Join(projectDir, ".claude", "settings.local.json"))
	}
	v := skillVisibility{ok: true, overrides: map[string]string{}, installed: map[string]bool{}, enabled: map[string]bool{}}
	plugins := map[string]bool{} // plugin name -> enabled, last scope wins
	for i, p := range paths {
		f, present, ok := readScope(p)
		if !ok || (i == 0 && !present) {
			return skillVisibility{} // user file must exist; any corrupt scope -> filter nothing
		}
		for name, val := range f.SkillOverrides {
			v.overrides[name] = val
		}
		for key, on := range f.EnabledPlugins {
			name, _, _ := strings.Cut(key, "@")
			plugins[name] = on
		}
	}
	for name, on := range plugins {
		if on {
			v.enabled[name] = true
		}
	}
	if ip, err := os.ReadFile(filepath.Join(userDir, "plugins", "installed_plugins.json")); err == nil {
		var inst struct {
			Plugins map[string]json.RawMessage `json:"plugins"`
		}
		if json.Unmarshal(ip, &inst) == nil {
			for key := range inst.Plugins {
				name, _, _ := strings.Cut(key, "@")
				v.installed[name] = true
			}
		}
	}
	return v
}

// modelInvocable reports whether the model can invoke s through the Skill tool.
//   - agents, tool pointers and slash commands are not Skill-tool entries: never filtered;
//   - a user skill (pack "skills") is hidden by skillOverrides "off" / "user-invocable-only"
//     ("name-only" still lists it, so it stays invocable);
//   - a plugin skill is hidden only when its pack is an INSTALLED plugin that is not
//     enabled. A pack that is not a plugin at all (e.g. skills synced from the account)
//     is kept -- absence from enabledPlugins alone proves nothing.
func (v skillVisibility) modelInvocable(s catalog.Skill) bool {
	if !v.ok {
		return true
	}
	if strings.HasPrefix(s.ID, "agent:") || strings.HasPrefix(s.ID, "tool:") || strings.HasPrefix(s.ID, "/") {
		return true
	}
	if s.Source == "skills" {
		switch v.overrides[s.ID] {
		case "off", "user-invocable-only":
			return false
		}
		return true
	}
	if v.installed[s.Source] && !v.enabled[s.Source] {
		return false
	}
	return true
}

// filterInvocable splits ranked ids into those the model can act on and those it cannot.
// An id missing from byID is kept: formatContext already skips it, and dropping it here
// would change nothing but the log.
func filterInvocable(byID map[string]catalog.Skill, ids []string, v skillVisibility) (shown, hidden []string) {
	for _, id := range ids {
		if s, ok := byID[id]; ok && !v.modelInvocable(s) {
			hidden = append(hidden, id)
			continue
		}
		shown = append(shown, id)
	}
	return shown, hidden
}
