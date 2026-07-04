// Package roots discovers and persists the harvest root set: the user's
// ~/.claude/skills dir plus every installed plugin's skills dir. The resolved
// set is written to roots.json next to the index, so the SessionStart hook
// (`mr-index refresh`, no flags) always sees the full set without any flag
// changes in settings.json.
package roots

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// FileVersion is bumped on incompatible roots.json schema changes.
const FileVersion = 1

// File is the on-disk shape of roots.json.
type File struct {
	Version int            `json:"version"`
	Roots   []catalog.Root `json:"roots"`
}

// ConfigPathFor returns the roots.json path that belongs to a given index
// path — always the sibling file, so tests that point -out at a temp dir get
// a temp roots.json too, and the production index gets ~/.meta-router/roots.json.
func ConfigPathFor(indexPath string) string {
	return filepath.Join(filepath.Dir(indexPath), "roots.json")
}

// DefaultClaudeDir is ~/.claude.
func DefaultClaudeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

// Load reads roots.json. A missing file returns os.ErrNotExist (callers
// discover instead); a present-but-invalid file returns an error so callers
// can warn and rediscover rather than silently indexing nothing.
func Load(path string) ([]catalog.Root, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("roots: parse %s: %w", path, err)
	}
	out := make([]catalog.Root, 0, len(f.Roots))
	for _, r := range f.Roots {
		if r.Path == "" {
			continue
		}
		if r.Pack == "" {
			r.Pack = catalog.UserPack
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("roots: %s contains no usable roots", path)
	}
	return out, nil
}

// Save writes roots.json atomically (tmp + rename).
func Save(path string, rs []catalog.Root) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(File{Version: FileVersion, Roots: rs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Discover resolves the full root set under a Claude config dir:
//   - <claudeDir>/skills as the user pack, and
//   - each installed plugin's skills dir, named by its plugin (pack) name.
//
// Plugin roots come from plugins/installed_plugins.json (the installer's own
// record of the ACTIVE version per plugin — the cache also holds stale
// versions and temp_git_* scratch clones we must not index). If the manifest
// is missing or yields nothing, fall back to scanning plugins/cache directly.
// Discovery never fails hard: the worst case is just the user root or an
// empty set.
func Discover(claudeDir string) []catalog.Root {
	var out []catalog.Root
	userSkills := filepath.Join(claudeDir, "skills")
	if isDir(userSkills) {
		out = append(out, catalog.Root{Path: userSkills, Pack: catalog.UserPack})
	}
	plug := fromManifest(filepath.Join(claudeDir, "plugins", "installed_plugins.json"))
	if len(plug) == 0 {
		plug = scanCache(filepath.Join(claudeDir, "plugins", "cache"))
	}
	sort.Slice(plug, func(i, j int) bool {
		if plug[i].Pack != plug[j].Pack {
			return plug[i].Pack < plug[j].Pack
		}
		return plug[i].Path < plug[j].Path
	})
	seen := map[string]bool{}
	for _, r := range out {
		seen[filepath.Clean(r.Path)] = true
	}
	for _, r := range plug {
		p := filepath.Clean(r.Path)
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, r)
	}
	return out
}

// manifestEntry mirrors the fields we need from installed_plugins.json.
type manifestEntry struct {
	InstallPath string `json:"installPath"`
}

// fromManifest reads the plugin installer's manifest:
//
//	{"version":2,"plugins":{"<plugin>@<marketplace>":[{"installPath":"..."}]}}
//
// Each key's first entry with a resolvable skills root wins. Plugins without
// any SKILL.md (MCP-only plugins like context7/github) are skipped.
func fromManifest(path string) []catalog.Root {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var m struct {
		Plugins map[string][]manifestEntry `json:"plugins"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	var out []catalog.Root
	for key, entries := range m.Plugins {
		pack := key
		for i := 0; i < len(key); i++ {
			if key[i] == '@' {
				pack = key[:i]
				break
			}
		}
		if pack == "" {
			continue
		}
		for _, e := range entries {
			if e.InstallPath == "" {
				continue
			}
			if r := skillRootUnder(e.InstallPath); r != "" {
				out = append(out, catalog.Root{Path: r, Pack: pack})
				break
			}
		}
	}
	return out
}

// scanCache is the manifest-less fallback. It handles both cache layouts:
//
//	cache/<marketplace>/<plugin>/<version>/skills/...   (marketplace layout)
//	cache/<plugin>/skills/...                           (direct layout)
//
// For versioned plugins the newest version dir (mtime) that contains skills
// wins. temp_git_*/temp_subdir_* and hidden dirs are skipped at every level.
func scanCache(cacheDir string) []catalog.Root {
	tops, err := os.ReadDir(cacheDir)
	if err != nil {
		return nil
	}
	var out []catalog.Root
	for _, top := range tops {
		if !top.IsDir() || catalog.SkipDirName(top.Name()) {
			continue
		}
		topPath := filepath.Join(cacheDir, top.Name())
		// Direct-plugin layout only counts with an immediate skills/ dir —
		// a marketplace dir also has SKILL.md files somewhere beneath it,
		// so the broader skillRootUnder check would misclassify it.
		if sk := filepath.Join(topPath, "skills"); isDir(sk) && hasSkillMD(sk) {
			out = append(out, catalog.Root{Path: sk, Pack: top.Name()})
			continue
		}
		subs, err := os.ReadDir(topPath)
		if err != nil {
			continue
		}
		for _, plug := range subs {
			if !plug.IsDir() || catalog.SkipDirName(plug.Name()) {
				continue
			}
			plugPath := filepath.Join(topPath, plug.Name())
			// Unversioned plugin with an immediate skills/ dir.
			r := ""
			if sk := filepath.Join(plugPath, "skills"); isDir(sk) && hasSkillMD(sk) {
				r = sk
			}
			// Versioned plugin: newest version dir that contains skills.
			// This must run before any broad SKILL.md fallback, or the
			// plugin dir itself would swallow all its version dirs.
			if r == "" {
				r = newestVersionRoot(plugPath)
			}
			// Last resort: the plugin dir IS the skills tree.
			if r == "" && hasSkillMD(plugPath) {
				r = plugPath
			}
			if r != "" {
				out = append(out, catalog.Root{Path: r, Pack: plug.Name()})
			}
		}
	}
	return out
}

// newestVersionRoot picks the most recently modified version dir under a
// plugin dir that actually contains skills.
func newestVersionRoot(plugPath string) string {
	vers, err := os.ReadDir(plugPath)
	if err != nil {
		return ""
	}
	type cand struct {
		root string
		mod  int64
	}
	var best *cand
	for _, v := range vers {
		if !v.IsDir() || catalog.SkipDirName(v.Name()) {
			continue
		}
		vPath := filepath.Join(plugPath, v.Name())
		r := skillRootUnder(vPath)
		if r == "" {
			continue
		}
		info, err := v.Info()
		if err != nil {
			continue
		}
		mod := info.ModTime().UnixNano()
		if best == nil || mod > best.mod {
			best = &cand{root: r, mod: mod}
		}
	}
	if best == nil {
		return ""
	}
	return best.root
}

// skillRootUnder returns the harvest root for an installed plugin dir:
// prefer <dir>/skills when it holds SKILL.md files, else <dir> itself when a
// SKILL.md tree lives elsewhere under it, else "" (no skills to index).
func skillRootUnder(dir string) string {
	sk := filepath.Join(dir, "skills")
	if isDir(sk) && hasSkillMD(sk) {
		return sk
	}
	if isDir(dir) && hasSkillMD(dir) {
		return dir
	}
	return ""
}

var errFound = errors.New("found")

// hasSkillMD reports whether any SKILL.md exists under dir, honoring the
// same subtree exclusions as harvest.
func hasSkillMD(dir string) bool {
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && catalog.SkipDirName(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == "SKILL.md" {
			return errFound
		}
		return nil
	})
	return errors.Is(err, errFound)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
