package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

// W9 R9.1: ~/.claude/commands (flat *.md, frontmatter usually has NO name) and
// ~/.claude/agents (flat *.md, frontmatter name + description + tools) were
// structurally absent from the index — 46 invocables unreachable, not badly
// ranked. These roots are FLAT: no SKILL.md, no per-entry directory.

func writeFlat(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A command's invocable identity is its FILE name (invoked as /<basename>), the
// same philosophy as skills taking their directory name. Frontmatter has no
// name field, and the description commonly uses a block scalar — the shape
// whose mishandling once blinded 69% of skills. Both must work.
func TestHarvestCommandsRoot(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, "insights.md", "---\ndescription: >\n  Correction and learning\n  analytics report.\n---\nbody\n")
	writeFlat(t, dir, "hooks-doctor.md", "---\ndescription: Sanity-check the hook fleet.\n---\nbody\n")
	writeFlat(t, dir, "notes.txt", "not markdown, must be ignored")
	writeFlat(t, dir, "broken.md", "no frontmatter fence at all")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFlat(t, filepath.Join(dir, "subdir"), "nested.md", "---\ndescription: flat harvest must not descend\n---\n")

	got, err := HarvestRoots([]Root{{Path: dir, Pack: "commands", Kind: KindCommands}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 commands (bad + non-md + nested skipped), got %d: %+v", len(got), got)
	}
	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	ins, ok := byName["insights"]
	if !ok {
		t.Fatalf("insights missing: %+v", got)
	}
	if ins.ID != "/insights" {
		t.Fatalf("command ID must be the slash invocation, got %q", ins.ID)
	}
	if ins.Source != "commands" {
		t.Fatalf("Source = %q, want commands", ins.Source)
	}
	if ins.Description != "Correction and learning analytics report." {
		t.Fatalf("block-scalar description mangled: %q", ins.Description)
	}
}

// An agent's dispatch identity is its frontmatter name (the Agent tool's
// subagent_type), falling back to the file basename when absent. The tools
// list must not leak into the description.
func TestHarvestAgentsRoot(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, "context-engineer.md", "---\nname: context-engineer\ndescription: Read-only audit of context overhead.\ntools: Read, Glob, Grep\n---\nbody\n")
	writeFlat(t, dir, "oddfile.md", "---\nname: real-name\ndescription: frontmatter name wins over the basename.\n---\n")
	writeFlat(t, dir, "nameless.md", "---\ndescription: falls back to the basename.\n---\n")

	got, err := HarvestRoots([]Root{{Path: dir, Pack: "agents", Kind: KindAgents}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 agents, got %d: %+v", len(got), got)
	}
	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	ce, ok := byName["context-engineer"]
	if !ok {
		t.Fatalf("context-engineer missing: %+v", got)
	}
	if ce.ID != "agent:context-engineer" {
		t.Fatalf("agent ID = %q, want agent:context-engineer", ce.ID)
	}
	if ce.Description != "Read-only audit of context overhead." {
		t.Fatalf("description = %q (tools leaked?)", ce.Description)
	}
	if _, ok := byName["real-name"]; !ok {
		t.Fatal("frontmatter name must win over the basename for agents")
	}
	if _, ok := byName["nameless"]; !ok {
		t.Fatal("a name-less agent must fall back to its basename, not be skipped")
	}
}

// Mixed roots: a skills root (Kind zero-value) must behave exactly as before,
// alongside flat roots, in one HarvestRoots call — the composition mr-index
// will actually run once the roots.json entries exist.
func TestHarvestRootsMixedKinds(t *testing.T) {
	skills := t.TempDir()
	if err := os.MkdirAll(filepath.Join(skills, "myskill"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFlat(t, filepath.Join(skills, "myskill"), "SKILL.md", "---\nname: myskill\ndescription: a normal skill.\n---\n")
	cmds := t.TempDir()
	writeFlat(t, cmds, "route.md", "---\ndescription: a command that shares a name with nothing.\n---\n")

	got, err := HarvestRoots([]Root{
		{Path: skills, Pack: UserPack},
		{Path: cmds, Pack: "commands", Kind: KindCommands},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["myskill"] || !ids["/route"] {
		t.Fatalf("expected ids myskill and /route, got %v", ids)
	}
}

// EmbedText must stay non-degenerate for flat entries — name plus description,
// never name-only (the 69%-blind failure shape).
func TestFlatEntriesEmbedTextIsFull(t *testing.T) {
	dir := t.TempDir()
	writeFlat(t, dir, "dream-now.md", "---\ndescription: |\n  Force a manual dream consolidation\n  run now.\n---\n")
	got, err := HarvestRoots([]Root{{Path: dir, Pack: "commands", Kind: KindCommands}})
	if err != nil || len(got) != 1 {
		t.Fatalf("harvest: %v %d", err, len(got))
	}
	et := got[0].EmbedText()
	if et == "dream-now  " || len(et) < len("dream-now")+20 {
		t.Fatalf("embed text is degenerate (name-only): %q", et)
	}
}
