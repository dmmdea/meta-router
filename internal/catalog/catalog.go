// Package catalog enumerates installed Claude Code skills into a normalized form.
package catalog

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Skill struct {
	ID, Name, Description, WhenToUse, Source, Path string
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
	if err != nil { return Skill{}, err }
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
			if s.Name == "" { s.Name = joined }
		case "description":
			if s.Description == "" { s.Description = joined }
		case "when_to_use", "whenToUse":
			if s.WhenToUse == "" { s.WhenToUse = joined }
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
// occurrence (order preserved). Harvest returns ~700 rows with duplicate ids
// from gstack sub-pack nesting; callers dedup before indexing/eval.
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

// Harvest walks each root for */SKILL.md and returns normalized skills.
// Unparseable skills are skipped (not fatal) so one bad skill can't blind the
// whole catalog.
func Harvest(roots []string) ([]Skill, error) {
	var out []Skill
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "SKILL.md" {
				return nil
			}
			s, perr := ParseSkillMD(p)
			if perr != nil {
				return nil // skip bad
			}
			s.Source = filepath.Base(root)
			s.ID = s.Source + ":" + s.Name
			out = append(out, s)
			return nil
		})
	}
	return out, nil
}
