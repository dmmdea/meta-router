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
	"runtime"
	"sort"
	"strings"

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
// discover instead); a present-but-invalid file returns an error that callers
// MUST treat as fatal — never "warn and rediscover", because rediscovery ends
// in Save overwriting the operator's file, repaying a typo with the silent
// destruction of the edits that carried it (review 2026-07-30).
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
		// An unknown kind must be REFUSED here, not tolerated: HarvestRoots
		// would fall through to the SKILL.md walk, find nothing under a flat
		// dir, and index zero entries with no diagnostic anywhere — a silent
		// no-op the operator can only detect by noticing absence. Since
		// hand-editing roots.json is the flat-root enablement path, a typo
		// ("command", "Agents") is the EXPECTED failure mode (review
		// 2026-07-30, MINOR).
		if !catalog.ValidKind(r.Kind) {
			return nil, fmt.Errorf("roots: %s: unknown kind %q for %s (valid: %q, %q, %q, %q, or omit)",
				path, r.Kind, r.Path, catalog.KindSkills, catalog.KindCommands, catalog.KindAgents, catalog.KindTools)
		}
		// The "agent" and "tool" pack names are RESERVED namespaces (catalog.go
		// Kind contract): a SKILLS-class root with one of these pack names
		// mints "agent:<x>" / "tool:<x>" IDs via InvocableID(pack, name), and
		// the hook then renders those entries with the namespace's invocation
		// trailer — instructing the model to dispatch a subagent or load an
		// MCP tool that does not exist. The reservation was comment-only until
		// a review demonstrated the mint against the real harvester; this
		// enforces it with the same refuse-loudly disposition as an unknown
		// kind. Flat roots are exempt: their IDs never derive from Pack.
		if (r.Pack == "agent" || r.Pack == "tool") && (r.Kind == "" || r.Kind == catalog.KindSkills) {
			return nil, fmt.Errorf("roots: %s: pack %q for %s is a reserved ID namespace (agent:/tool: entries are minted only by kind %q/%q roots)",
				path, r.Pack, r.Path, catalog.KindAgents, catalog.KindTools)
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

// Stale partitions the roots whose path no longer exists on disk into the two
// ownership classes the build carry-over rule already uses.
//
// It exists because `refresh` reads roots.json and returns it verbatim — it
// never re-discovers and never checks that the paths still resolve. Plugin
// cache roots are VERSIONED (…/superpowers/6.1.1/skills), so a plugin update
// silently strands every skill in that pack: the root stops existing, harvest
// finds nothing under it, and the entries leave the index with no diagnostic.
// Measured in production 2026-08-10 — 7 of 13 roots dead, index 155 → 112
// entries, the entire superpowers pack gone. index.Refresh's removal guard
// cannot catch this: the decay arrives in steps below its 30% threshold
// (155→127 is 18%, 127→112 is 12%).
//
//   - discovery: PROVABLY absent (ErrNotExist) and a plain skills root under
//     claudeDir. Discovery owns that space, so a pack that vanished from it is
//     an uninstall and SHOULD drop.
//   - operator: provably absent, but a flat root (Discover can never emit a
//     Kind) or any root outside claudeDir. Only a human puts those in
//     roots.json, so they are reported for visibility and never dropped.
//   - unknown: Stat failed for a reason OTHER than non-existence — a deny ACE,
//     an unreachable UNC path, an AV handle, a path replaced by a file. These
//     must NEVER force rediscovery: rediscovery ends in Save, which would
//     permanently delete a root whose directory is merely unreadable right now.
//     Review reproduced exactly that — a deny ACE dropped a live 3-skill pack
//     and it did NOT come back after the ACE was removed. Absence of evidence
//     is not evidence of absence, so an unknown is reported and kept.
//
// Callers decide what to do; Stale itself only classifies.
func Stale(rs []catalog.Root, claudeDir string) (discovery, operator []catalog.Root, unknown []Unresolved) {
	for _, r := range rs {
		fi, err := os.Stat(r.Path)
		if err == nil && fi.IsDir() {
			continue // live
		}
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			// Exists but is not a directory, or Stat failed for an unknown
			// reason. Either way we do not KNOW the pack is gone.
			if err == nil {
				err = fmt.Errorf("not a directory")
			}
			unknown = append(unknown, Unresolved{Root: r, Err: err})
			continue
		}
		if isOperatorOwned(r, claudeDir) {
			operator = append(operator, r)
			continue
		}
		discovery = append(discovery, r)
	}
	return discovery, operator, unknown
}

// Unresolved is a root whose liveness could not be determined.
type Unresolved struct {
	Root catalog.Root
	Err  error
}

// isOperatorOwned reports whether only a human could have put this root in
// roots.json — a flat root, or one outside the claudeDir tree.
//
// The containment test is boundary-safe (trailing separator on both sides, so
// the sibling "<claude>x" is correctly outside) and case-insensitive on
// Windows. The case-folding matters because the polarity is unsafe here: this
// predicate is shared with the build carry-over, where deciding "outside" wrongly
// merely PRESERVES a root (harmless), but here deciding "outside" wrongly means
// the decay is never repaired. Plugin paths reach roots.json verbatim from
// installed_plugins.json — a file written by another program — so nothing
// guarantees they share os.UserHomeDir's casing.
func isOperatorOwned(r catalog.Root, claudeDir string) bool {
	if r.Kind == catalog.KindCommands || r.Kind == catalog.KindAgents {
		return true
	}
	p := filepath.Clean(r.Path) + string(filepath.Separator)
	prefix := filepath.Clean(claudeDir) + string(filepath.Separator)
	if runtime.GOOS == "windows" {
		return !strings.HasPrefix(strings.ToLower(p), strings.ToLower(prefix))
	}
	return !strings.HasPrefix(p, prefix)
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
	plug := preferEnabled(manifestRoots(filepath.Join(claudeDir, "plugins", "installed_plugins.json")),
		userEnabledPlugins(claudeDir))
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
	var out []catalog.Root
	for _, kr := range manifestRoots(path) {
		out = append(out, kr.root)
	}
	return out
}

// keyedRoot is a manifest root plus the installed key it came from (name@marketplace).
type keyedRoot struct {
	key  string
	root catalog.Root
}

// preferEnabled resolves one plugin installed from several marketplaces. Every copy
// carries the same invocable ids and harvest keeps the first, so indexing all of them
// could keep the copy Claude Code does not load. When at least one copy's key is
// enabled in the user's settings.json, the other copies are dropped; otherwise every
// copy is kept and the hook filters disabled plugins at prompt time.
func preferEnabled(rs []keyedRoot, enabled map[string]bool) []catalog.Root {
	copies := map[string]int{}
	anyOn := map[string]bool{}
	for _, kr := range rs {
		copies[kr.root.Pack]++
		if enabled[kr.key] {
			anyOn[kr.root.Pack] = true
		}
	}
	var out []catalog.Root
	for _, kr := range rs {
		if copies[kr.root.Pack] > 1 && anyOn[kr.root.Pack] && !enabled[kr.key] {
			continue
		}
		out = append(out, kr.root)
	}
	return out
}

// userEnabledPlugins reads enabledPlugins from <claudeDir>/settings.json. Any read or
// parse error yields an empty map, which keeps every installed copy.
func userEnabledPlugins(claudeDir string) map[string]bool {
	var f struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	if raw, err := os.ReadFile(filepath.Join(claudeDir, "settings.json")); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	return f.EnabledPlugins
}

// Superseded returns the discovery-owned plugin roots that installed_plugins.json no
// longer lists for their pack. A plugin update leaves the previous version directory
// in the cache, so Stale still sees the old root as live while Claude Code loads the
// new version, and the index keeps serving skills that no longer load. A pack the
// manifest does not list, an operator-owned root, and a missing or unreadable manifest
// yield nothing: without positive evidence there is no rediscovery.
func Superseded(rs []catalog.Root, claudeDir string) []catalog.Root {
	active := map[string]map[string]bool{}
	for _, r := range fromManifest(filepath.Join(claudeDir, "plugins", "installed_plugins.json")) {
		if active[r.Pack] == nil {
			active[r.Pack] = map[string]bool{}
		}
		active[r.Pack][pathKey(r.Path)] = true
	}
	var out []catalog.Root
	for _, r := range rs {
		if r.Pack == catalog.UserPack || isOperatorOwned(r, claudeDir) {
			continue
		}
		if paths, ok := active[r.Pack]; ok && !paths[pathKey(r.Path)] {
			out = append(out, r)
		}
	}
	return out
}

// pathKey normalises a path for equality: cleaned, and case-folded on Windows, where
// the manifest and roots.json can spell the same directory differently.
func pathKey(p string) string {
	p = filepath.Clean(p)
	if runtime.GOOS == "windows" {
		p = strings.ToLower(p)
	}
	return p
}

// manifestRoots is fromManifest with each root's installed key kept.
func manifestRoots(path string) []keyedRoot {
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
	var out []keyedRoot
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
				out = append(out, keyedRoot{key: key, root: catalog.Root{Path: r, Pack: pack}})
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
