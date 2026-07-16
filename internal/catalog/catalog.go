// Package catalog enumerates installed Claude Code skills into a normalized form.
package catalog

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Skill struct {
	ID, Name, Description, WhenToUse, Source, Path string
}

// Root is one harvest root plus the pack it belongs to. Pack is "skills" for
// the user's top-level ~/.claude/skills dir and the plugin name for a plugin's
// skills dir (e.g. "superpowers"). The pack determines the invocable ID:
// user skills are invoked by bare directory name ("gstack-qa"), plugin skills
// by "<plugin>:<skill-dir>" ("superpowers:brainstorming") — matching exactly
// how the Skill tool invokes them.
type Root struct {
	Path string `json:"path"`
	Pack string `json:"pack"`
}

// UserPack is the pack name for the user's own skills; its IDs are unprefixed.
const UserPack = "skills"

// InvocableID returns the ID the Skill tool would use to invoke a skill named
// name (its directory basename) from the given pack.
func InvocableID(pack, name string) string {
	if pack == "" || pack == UserPack {
		return name
	}
	return pack + ":" + name
}

// ParseSkillMD reads YAML-frontmatter fields (name, description, when_to_use)
// from a SKILL.md. It is a small stdlib-only parser, but it DOES handle the two
// shapes real skills use: a long single-line scalar, and a block scalar
// (`description: >` folded / `description: |` literal) whose text lives on the
// following indented lines. ~69% of installed skills use a block-scalar
// description; a naive line parser would capture only ">" and embed those skills
// as name-only, which was the dominant cause of weak skill retrieval. Indented
// children of a non-target key (e.g. a `metadata:` block) are skipped, so they
// never leak into the description.
func ParseSkillMD(path string) (Skill, error) {
	f, err := os.Open(path)
	if err != nil {
		return Skill{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return Skill{}, fmt.Errorf("%s: no frontmatter fence", path)
	}

	// Collect the frontmatter body (between the fences).
	var lines []string
	closed := false
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			closed = true
			break
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		return Skill{}, err
	}
	if !closed {
		return Skill{}, fmt.Errorf("%s: unterminated frontmatter", path)
	}

	isTopLevel := func(l string) bool { return len(l) > 0 && l[0] != ' ' && l[0] != '\t' }
	blockIndicator := func(v string) bool {
		switch v {
		case ">", "|", ">-", "|-", ">+", "|+":
			return true
		}
		return false
	}

	s := Skill{Path: path}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if !isTopLevel(line) { // indented child of some key, or blank — not a new field
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		if key != "name" && key != "description" && key != "when_to_use" && key != "whenToUse" {
			continue // non-target key; its indented children are skipped by isTopLevel above
		}
		val := strings.TrimSpace(v)
		var parts []string
		if val != "" && !blockIndicator(val) {
			parts = append(parts, val)
		}
		// Gather any following indented lines (block-scalar body or a wrapped
		// plain scalar) until the next top-level key.
		for i+1 < len(lines) && !isTopLevel(lines[i+1]) {
			i++
			if t := strings.TrimSpace(lines[i]); t != "" {
				parts = append(parts, t)
			}
		}
		joined := strings.Trim(strings.Join(parts, " "), `"'`)
		switch key {
		case "name":
			if s.Name == "" {
				s.Name = joined
			}
		case "description":
			if s.Description == "" {
				s.Description = joined
			}
		case "when_to_use", "whenToUse":
			if s.WhenToUse == "" {
				s.WhenToUse = joined
			}
		}
	}
	if s.Name == "" {
		return Skill{}, fmt.Errorf("%s: frontmatter has no name", path)
	}
	return s, nil
}

// EmbedText is the canonical text embedded and indexed for a skill. Keep this
// the single source of truth — embedder, BM25, and the index hash all use it,
// so they must agree on exactly what text represents a skill.
func (s Skill) EmbedText() string {
	return s.Name + " " + s.Description + " " + s.WhenToUse
}

// DedupByID returns a new slice with duplicate ids removed, keeping the first
// occurrence (order preserved). HarvestRoots orders each root's skills so the
// top-level (invocable) copy comes first, so "keep first" keeps the right one.
func DedupByID(skills []Skill) []Skill {
	seen := make(map[string]bool, len(skills))
	out := make([]Skill, 0, len(skills))
	for _, s := range skills {
		if seen[s.ID] {
			continue
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	return out
}

// descKey is the dedup key for DedupByDescription: the description lowercased
// with whitespace runs collapsed, so cosmetic reflowing doesn't defeat the
// collapse. Empty descriptions return "" (never collapsed — two distinct
// name-only skills must both survive). The skill name is deliberately NOT part
// of the key: pack-internal twin copies carry the same description under a
// different (stripped, non-invocable) name, and collapsing on the
// name-independent description is exactly what removes them.
func descKey(s Skill) string {
	d := strings.Join(strings.Fields(strings.ToLower(s.Description)), " ")
	return d
}

// DedupByDescription collapses skills with identical (normalized)
// descriptions to the first occurrence. HarvestRoots sorts each root so
// top-level invocable copies precede nested pack-internal twins; therefore
// "first wins" keeps the copy a user (or the Skill tool) can actually invoke.
// Skills with empty descriptions are never collapsed.
func DedupByDescription(skills []Skill) []Skill {
	seen := make(map[string]bool, len(skills))
	out := make([]Skill, 0, len(skills))
	for _, s := range skills {
		k := descKey(s)
		if k != "" {
			if seen[k] {
				continue
			}
			seen[k] = true
		}
		out = append(out, s)
	}
	return out
}

// Dedup is the canonical hygiene pipeline every consumer (build, refresh,
// eval) must share: collapse description twins first, then exact ID
// collisions as a backstop.
func Dedup(skills []Skill) []Skill {
	return DedupByID(DedupByDescription(skills))
}

// SkipDirName reports whether an entire directory subtree must be excluded
// from harvest: hidden dirs (".agents", ".git", …) hold pack-internal copies
// never exposed to the Skill tool; temp_git_*/temp_subdir_* are the plugin
// installer's scratch clones; node_modules is vendored junk. Exported so root
// discovery applies the same exclusions when scanning the plugin cache.
func SkipDirName(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	if strings.HasPrefix(name, "temp_git_") || strings.HasPrefix(name, "temp_subdir_") {
		return true
	}
	return name == "node_modules"
}

// harvested carries per-file ordering facts used only for the in-root sort.
type harvested struct {
	skill     Skill
	depth     int  // path separators below the root (1 = root/<dir>/SKILL.md)
	nameMatch bool // frontmatter name == directory name
}

// Harvest walks plain root paths with the pack inferred from each root's
// basename (a root literally named "skills" behaves as the user pack).
// Kept for -skill-roots flag compatibility; new code should use HarvestRoots.
func Harvest(rootPaths []string) ([]Skill, error) {
	roots := make([]Root, len(rootPaths))
	for i, p := range rootPaths {
		roots[i] = Root{Path: p, Pack: filepath.Base(filepath.Clean(p))}
	}
	return HarvestRoots(roots)
}

// HarvestRoots walks each root for SKILL.md files and returns normalized
// skills under their INVOCABLE identity:
//   - Name  = the skill's directory basename (what the Skill tool accepts),
//   - Source = the pack name (root.Pack: "skills" or the plugin name),
//   - ID    = InvocableID(pack, name) — "gstack-qa" or "superpowers:brainstorming".
//
// Hidden/temp/node_modules subtrees are skipped entirely (see skipDirName).
// Within each root the results are ordered top-level-first (depth asc, then
// frontmatter-name==dirname, then path), so Dedup's "first wins" keeps the
// invocable copy of any twin. Unparseable skills are skipped (not fatal) so
// one bad skill can't blind the whole catalog.
func HarvestRoots(roots []Root) ([]Skill, error) {
	var out []Skill
	for _, root := range roots {
		cleanRoot := filepath.Clean(root.Path)
		var hs []harvested
		_ = filepath.WalkDir(cleanRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != cleanRoot && SkipDirName(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			if d.Name() != "SKILL.md" {
				return nil
			}
			s, perr := ParseSkillMD(p)
			if perr != nil {
				return nil // skip bad
			}
			dir := filepath.Dir(p)
			dirName := filepath.Base(dir)
			if dir == cleanRoot { // SKILL.md directly at the root
				dirName = filepath.Base(cleanRoot)
			}
			rel, rerr := filepath.Rel(cleanRoot, p)
			depth := 1
			if rerr == nil {
				depth = strings.Count(filepath.ToSlash(rel), "/")
			}
			nameMatch := s.Name == dirName
			s.Name = dirName
			s.Source = root.Pack
			s.ID = InvocableID(root.Pack, dirName)
			hs = append(hs, harvested{skill: s, depth: depth, nameMatch: nameMatch})
			return nil
		})
		// Top-level invocable copies first so downstream keep-first dedup
		// (by description or ID) always keeps the copy that can be invoked.
		// Among same-depth description twins (alias dirs like _gstack-command /
		// gstack / gstack-browse sharing one description), prefer the dir whose
		// name matches its frontmatter name, then the SHORTEST name — the
		// canonical alias — then path for determinism.
		sort.SliceStable(hs, func(i, j int) bool {
			if hs[i].depth != hs[j].depth {
				return hs[i].depth < hs[j].depth
			}
			if hs[i].nameMatch != hs[j].nameMatch {
				return hs[i].nameMatch
			}
			if len(hs[i].skill.Name) != len(hs[j].skill.Name) {
				return len(hs[i].skill.Name) < len(hs[j].skill.Name)
			}
			return hs[i].skill.Path < hs[j].skill.Path
		})
		for _, h := range hs {
			out = append(out, h.skill)
		}
	}
	return out, nil
}
