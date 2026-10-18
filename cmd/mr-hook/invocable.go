package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// skillVisibility is the part of the operator's Claude Code settings that decides whether
// the MODEL can invoke a skill at all. The hint used to surface skills it could not:
// ~/.claude/skills entries hidden by skillOverrides, and skills of plugins that are
// installed but not enabled. A suggestion the model cannot act on is pure context cost.
type skillVisibility struct {
	ok        bool
	overrides map[string]string          // skillOverrides: on | name-only | user-invocable-only | off
	installed map[string]bool            // plugin names in plugins/installed_plugins.json
	enabled   map[string]bool            // plugin names with at least one enabledPlugins key set true
	installs  map[string][]pluginInstall // plugin name -> each installed copy and whether its own key is enabled
	// unloadable holds ranked candidates whose SKILL.md is gone or sets
	// disable-model-invocation. It is filled per prompt by unloadableSkills, not from settings.
	unloadable map[string]bool
}

// pluginInstall is one installed copy of a plugin. The same plugin name can be installed
// from two marketplaces (name@a, name@b); Claude Code loads only the copy whose own key
// is enabled.
type pluginInstall struct {
	dir string // installPath as pathKey, with a trailing separator
	on  bool
}

// pathKey normalises a path for prefix comparison: cleaned, and case-folded on Windows,
// where the plugin manifest and the index can spell the same directory differently.
func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
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
	v := skillVisibility{ok: true, overrides: map[string]string{}, installed: map[string]bool{},
		enabled: map[string]bool{}, installs: map[string][]pluginInstall{}}
	keys := map[string]bool{} // enabledPlugins key (name@marketplace) -> enabled, last scope wins per key
	for i, p := range paths {
		f, present, ok := readScope(p)
		if !ok || (i == 0 && !present) {
			return skillVisibility{} // user file must exist; any corrupt scope -> filter nothing
		}
		for name, val := range f.SkillOverrides {
			v.overrides[name] = val
		}
		for key, on := range f.EnabledPlugins {
			keys[key] = on
		}
	}
	// A plugin name is enabled when ANY of its keys is. Merging by bare name made the
	// answer depend on map order whenever one name had a true and a false key (the same
	// plugin from two marketplaces), so its skills were hidden on some prompts only.
	for key, on := range keys {
		if on {
			name, _, _ := strings.Cut(key, "@")
			v.enabled[name] = true
		}
	}
	if ip, err := os.ReadFile(filepath.Join(userDir, "plugins", "installed_plugins.json")); err == nil {
		var inst struct {
			Plugins map[string]json.RawMessage `json:"plugins"`
		}
		if json.Unmarshal(ip, &inst) == nil {
			for key, raw := range inst.Plugins {
				name, _, _ := strings.Cut(key, "@")
				v.installed[name] = true
				var entries []struct {
					InstallPath string `json:"installPath"`
				}
				if json.Unmarshal(raw, &entries) != nil {
					continue
				}
				for _, e := range entries {
					if e.InstallPath != "" {
						v.installs[name] = append(v.installs[name],
							pluginInstall{dir: pathKey(e.InstallPath) + string(filepath.Separator), on: keys[key]})
					}
				}
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
//     is kept -- absence from enabledPlugins alone proves nothing. When the skill's path
//     and the installed copies are known, the copy the path sits in decides, and a path
//     under no installed copy is a stale version directory (an update leaves the old
//     version in the cache), so it is hidden;
//   - a ranked candidate whose SKILL.md is gone or sets disable-model-invocation is
//     hidden whatever the settings say.
func (v skillVisibility) modelInvocable(s catalog.Skill) bool {
	if strings.HasPrefix(s.ID, "agent:") || strings.HasPrefix(s.ID, "tool:") || strings.HasPrefix(s.ID, "/") {
		return true
	}
	if v.unloadable[s.ID] {
		return false
	}
	if !v.ok {
		return true
	}
	if s.Source == "skills" {
		switch v.overrides[s.ID] {
		case "off", "user-invocable-only":
			return false
		}
		return true
	}
	if !v.installed[s.Source] {
		return true
	}
	if copies := v.installs[s.Source]; len(copies) > 0 && s.Path != "" {
		p := pathKey(s.Path)
		for _, c := range copies {
			if strings.HasPrefix(p, c.dir) {
				return c.on
			}
		}
		return false
	}
	return v.enabled[s.Source]
}

// unloadableSkills returns the ranked candidates the model cannot load right now: the
// skill's SKILL.md no longer exists (an index saved before an uninstall), or its
// frontmatter sets disable-model-invocation, which keeps the skill out of the model's
// listing. Only the few ranked ids are read, inside the hook's deadline window. A read
// that fails for any other reason proves nothing, so the skill is kept.
func unloadableSkills(byID map[string]catalog.Skill, ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		s, ok := byID[id]
		if !ok || !strings.EqualFold(filepath.Base(s.Path), "SKILL.md") {
			continue // agents, tools, commands and entries without a path carry no frontmatter
		}
		head, err := readHead(s.Path, 8192)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				out[id] = true
			}
			continue
		}
		if disablesModelInvocation(head) {
			out[id] = true
		}
	}
	return out
}

// readHead returns up to n bytes from the start of a file.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	m, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf[:m], nil
}

// disablesModelInvocation reports whether a SKILL.md frontmatter block sets the
// top-level key disable-model-invocation to true (quotes and case tolerated).
func disablesModelInvocation(head []byte) bool {
	sc := bufio.NewScanner(bytes.NewReader(head))
	first := true
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if first {
			first = false
			if strings.TrimSpace(strings.TrimPrefix(line, "\ufeff")) != "---" {
				return false
			}
			continue
		}
		if strings.TrimSpace(line) == "---" {
			return false
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok || strings.TrimRight(key, " \t") != "disable-model-invocation" {
			continue
		}
		return strings.EqualFold(strings.Trim(strings.TrimSpace(val), `"'`), "true")
	}
	return false
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
