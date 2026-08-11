// Package catalog enumerates installed Claude Code skills into a normalized form.
package catalog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
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
// Kind selects the harvest shape for a root. The zero value ("", or the
// explicit "skills") is the original SKILL.md directory walk, so every
// pre-Kind roots.json keeps meaning exactly what it meant. W9 R9.1 added the
// flat kinds for ~/.claude/commands and ~/.claude/agents — 46 invocables that
// were structurally absent from the index. Adding a Kind here is a CAPABILITY;
// production index composition changes only when a roots.json entry actually
// names one of these roots, and that entry ships only behind W9's measured
// recall/precision gate.
// ID namespaces (review 2026-07-30): commands take "/<name>" — a "/" can never
// appear in a skill ID, so that space is collision-free. Agents take
// "agent:<name>", which RESERVES the pack name "agent": a plugin literally
// named "agent" would mint byte-identical IDs and DedupByID would silently
// shadow one side. No such plugin exists and pack names come from installer
// manifests, but the reservation is a contract, not an accident — ValidKind
// and this comment are where it is recorded.
const (
	KindSkills   = "skills"
	KindCommands = "commands" // flat *.md; invoked as /<basename>; frontmatter has no name
	KindAgents   = "agents"   // flat *.md; dispatched by frontmatter name via the Agent tool
)

// ValidKind reports whether k is a recognized root kind. The zero value is the
// SKILL.md walk. Anything else ("command" singular, "Agents") must be REFUSED
// loudly by loaders: HarvestRoots would silently fall through to the SKILL.md
// walk, find nothing under a flat dir, and index zero entries — a no-op the
// operator can only detect by noticing absence (review 2026-07-30, MINOR).
func ValidKind(k string) bool {
	switch k {
	case "", KindSkills, KindCommands, KindAgents:
		return true
	}
	return false
}

type Root struct {
	Path string `json:"path"`
	Pack string `json:"pack"`
	Kind string `json:"kind,omitempty"`
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
// parseFrontmatterMD parses the frontmatter without requiring a `name:` field.
//
// There is deliberately NO name-requiring variant. One existed (ParseSkillMD)
// and was used by the skills walk, where it silently dropped every skill whose
// frontmatter omitted `name:` — 11 live, all of them invocable — while the walk
// overwrote s.Name with the directory name immediately afterwards, so the rule
// never changed a result. Both the Skill tool and the flat harvesters identify
// a skill by its directory or filename; nothing should reintroduce that check.
func parseFrontmatterMD(path string) (Skill, error) {
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

// marketplaceFile is the per-plugin ownership manifest a marketplace ships at
// <installPath>/.claude-plugin/marketplace.json. Only the fields we need.
type marketplaceFile struct {
	Plugins []struct {
		Name   string      `json:"name"`
		Skills skillsField `json:"skills"`
	} `json:"plugins"`
}

// skillsField accepts BOTH shapes seen in the wild: a list of paths, and a bare
// string (the installed huggingface-skills manifest uses `"skills": "./"`).
// Decoding it as []string makes json.Unmarshal fail for the WHOLE document, so
// ONE sibling plugin's schema choice would silently switch ownership filtering
// off for every pack in that marketplace — re-admitting the phantom IDs with no
// signal anywhere.
type skillsField []string

func (s *skillsField) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err == nil {
		*s = list
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = []string{one}
		return nil
	}
	*s = nil // null, number, object: unknown shape, treated as "no declaration"
	return nil
}

// ownedSkillDirs returns the set of skill directory names that `pack` actually
// owns, or nil when there is no authoritative data (meaning: do not filter).
//
// A marketplace cache dir routinely holds the WHOLE marketplace's skills while
// the installed plugin owns only a subset. Harvesting the dir under one pack
// name mints IDs the Skill tool cannot invoke — and because those phantoms are
// harvested under an alphabetically-earlier pack, they win description-dedup
// and SUPPRESS the correctly-attributed copies. Measured 2026-08-11: 13 phantom
// `document-skills:*` IDs, with the real `frontend-design:frontend-design` and
// `skill-creator:skill-creator` lost to them.
//
// Deliberately conservative — it filters ONLY when the manifest exists, parses,
// and names this pack. No manifest, unreadable manifest, or a manifest that
// never mentions this pack all mean "no authority", so nothing is filtered and
// behavior is exactly as before.
func ownedSkillDirs(skillsRoot, pack string) map[string]bool {
	// The manifest sits beside the skills dir, at the plugin's install root.
	data, err := os.ReadFile(filepath.Join(filepath.Dir(skillsRoot), ".claude-plugin", "marketplace.json"))
	if err != nil {
		return nil
	}
	var mf marketplaceFile
	if json.Unmarshal(data, &mf) != nil {
		return nil
	}
	owned := make(map[string]bool)
	declared := false
	for _, pl := range mf.Plugins {
		if pl.Name != pack {
			continue
		}
		declared = true
		for _, s := range pl.Skills {
			if b := path.Base(strings.TrimSuffix(strings.ReplaceAll(s, "\\", "/"), "/")); b != "" && b != "." {
				owned[b] = true
			}
		}
	}
	if !declared || len(owned) == 0 {
		return nil // no authority over this pack — harvest everything
	}
	// Authority must RESOLVE. A declaration whose entries match no directory
	// under this root describes a different layout than the cache holds — e.g.
	// "./skills" (the dir itself) or a path to the SKILL.md rather than its
	// dir. Treating that as authority filters out every skill and silently
	// empties the pack, which is the permanent-silent-loss failure this whole
	// area exists to prevent (and 14 of 199 sits under the 30% removal guard,
	// so nothing downstream would object). If nothing resolves, we have no
	// usable authority: harvest everything, exactly as before.
	for name := range owned {
		if fi, err := os.Stat(filepath.Join(skillsRoot, name)); err == nil && fi.IsDir() {
			return owned
		}
	}
	return nil
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
	// Skills-class roots are processed BEFORE flat roots regardless of file
	// order: DedupByDescription keeps the first occurrence, and a command that
	// 1:1-wraps a skill carries a byte-identical description — whichever came
	// first would silently delete the other from the index. The skill must win
	// (it is the Skill-tool-invocable canonical), and that must be a rule, not
	// an artifact of roots.json ordering (review 2026-07-30, MINOR). Relative
	// order within each class is preserved.
	ordered := make([]Root, 0, len(roots))
	for _, r := range roots {
		if r.Kind != KindCommands && r.Kind != KindAgents {
			ordered = append(ordered, r)
		}
	}
	for _, r := range roots {
		if r.Kind == KindCommands || r.Kind == KindAgents {
			ordered = append(ordered, r)
		}
	}
	var out []Skill
	for _, root := range ordered {
		if root.Kind == KindCommands || root.Kind == KindAgents {
			out = append(out, harvestFlat(root)...)
			continue
		}
		cleanRoot := filepath.Clean(root.Path)
		owned := ownedSkillDirs(cleanRoot, root.Pack)
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
			// Ownership filter: skip a skill this pack does not own.
			if owned != nil {
				dn := filepath.Base(filepath.Dir(p))
				if !owned[dn] {
					return nil
				}
			}
			// parseFrontmatterMD, NOT ParseSkillMD: a `name:` field is not
			// required for a skill to be real or invocable. The Skill tool
			// resolves a skill by its DIRECTORY, and this walk agrees — it
			// overwrites s.Name with dirName a few lines below regardless. So
			// the name requirement changed nothing about the result and only
			// ever excluded valid skills: measured 2026-08-11, it silently
			// dropped all 11 searchfit-seo skills, every one of them invocable.
			// A file with no frontmatter fence at all still errors here and is
			// still skipped.
			s, perr := parseFrontmatterMD(p)
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

// harvestFlat reads a FLAT root of *.md files (commands/, agents/). It does
// not descend into subdirectories — neither surface nests — and skips
// non-markdown, hidden files, and unparseable frontmatter (skip-not-fatal, the
// same tolerance the SKILL.md walk has, so one bad file can't blind the rest).
//
// Identity rules differ per kind and both are the INVOCABLE identity, matching
// the skills philosophy (a skill's name is its directory because that is what
// the Skill tool accepts):
//   - commands: Name = file basename (invoked as /<basename>; frontmatter has
//     no name field), ID = "/" + name.
//   - agents:   Name = frontmatter name (the Agent tool's subagent_type),
//     falling back to the basename; ID = "agent:" + name.
//
// Results are name-sorted for determinism (ReadDir order is already sorted,
// but the frontmatter-name override for agents can reorder).
func harvestFlat(root Root) []Skill {
	cleanRoot := filepath.Clean(root.Path)
	des, err := os.ReadDir(cleanRoot)
	if err != nil {
		return nil
	}
	type flatEntry struct {
		skill     Skill
		nameMatch bool // identity == file basename (see ordering rule below)
	}
	var hs []flatEntry
	for _, de := range des {
		// EqualFold: Windows filesystems are case-insensitive, so FOO.MD is a
		// valid command file the exact-case check silently dropped (review
		// 2026-07-30, NIT).
		if de.IsDir() || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		if len(de.Name()) < 3 || !strings.EqualFold(de.Name()[len(de.Name())-3:], ".md") {
			continue
		}
		p := filepath.Join(cleanRoot, de.Name())
		s, perr := parseFrontmatterMD(p)
		if perr != nil {
			continue // skip bad
		}
		// An empty description is the degenerate name-only embed — the exact
		// failure shape that once blinded 69% of skills. Indexing it would let
		// it participate in cosine ranking as pure noise, and
		// DedupByDescription deliberately never collapses empties. Skip it,
		// the same disposition as a file with no frontmatter at all (review
		// 2026-07-30, MINOR).
		if strings.TrimSpace(s.Description) == "" {
			continue
		}
		base := de.Name()[:len(de.Name())-3] // strip ".md" in whatever case it arrived
		switch root.Kind {
		case KindCommands:
			s.Name = base
			s.ID = "/" + base
		case KindAgents:
			if s.Name == "" {
				s.Name = base
			}
			s.ID = "agent:" + s.Name
		}
		s.Source = root.Pack
		hs = append(hs, flatEntry{skill: s, nameMatch: s.Name == base})
	}
	// Ordering contract (load-bearing for two reasons): downstream DedupByID
	// keeps the FIRST occurrence, and two agent files can mint the same ID
	// (frontmatter `name: victim` in stealer.md vs victim.md's basename
	// fallback). sort.Slice is UNSTABLE, so equal keys made the survivor a Go
	// version accident (review 2026-07-30, MINOR). Rule: basename-matching
	// identity first (the file that IS what it claims), then name, then path —
	// SliceStable so the winner is a contract, not pdqsort's mood.
	sort.SliceStable(hs, func(i, j int) bool {
		if hs[i].nameMatch != hs[j].nameMatch {
			return hs[i].nameMatch
		}
		if hs[i].skill.Name != hs[j].skill.Name {
			return hs[i].skill.Name < hs[j].skill.Name
		}
		return hs[i].skill.Path < hs[j].skill.Path
	})
	out := make([]Skill, 0, len(hs))
	for _, h := range hs {
		out = append(out, h.skill)
	}
	return out
}
