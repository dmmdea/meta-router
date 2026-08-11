package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHarvest(t *testing.T) {
	root := filepath.Join("testdata", "skills")
	got, err := Harvest([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	// alpha + beta + nameless. The bad/ fixture proves skip-bad-skill
	// resilience: one malformed SKILL.md (no frontmatter fence at all) is
	// skipped, not fatal.
	if len(got) != 3 {
		t.Fatalf("want 3 skills, got %d: %+v", len(got), got)
	}
	for _, s := range got {
		if s.ID == "" || s.Name == "" {
			t.Fatalf("bad skill %+v", s)
		}
		// A root whose basename is "skills" is the user pack: IDs are the
		// bare invocable dir name (no prefix), Source is "skills".
		if s.Source != UserPack {
			t.Fatalf("legacy Harvest must infer pack from root basename: %+v", s)
		}
		if s.ID != s.Name {
			t.Fatalf("user-pack ID must equal the invocable dir name: ID=%q Name=%q", s.ID, s.Name)
		}
	}
}

// A SKILL.md whose frontmatter carries a description but NO name is a real,
// invocable skill — the Skill tool resolves it by DIRECTORY name. The harvester
// already knows this: three lines after parsing it overwrites s.Name with
// dirName unconditionally. So requiring `name:` only ever excluded valid
// skills while changing nothing about the result.
//
// Measured on the live machine 2026-08-11: this silently dropped all 11
// searchfit-seo skills, every one of which IS invocable
// (searchfit-seo:seo-audit et al appear in the live skill list).
func TestHarvestKeepsSkillsWithNoNameInFrontmatter(t *testing.T) {
	got, err := Harvest([]string{filepath.Join("testdata", "skills")})
	if err != nil {
		t.Fatal(err)
	}
	var found *Skill
	for i := range got {
		if got[i].Name == "nameless" {
			found = &got[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("a description-only skill must be harvested under its DIR name; got %+v", got)
	}
	if found.ID != "nameless" {
		t.Fatalf("its invocable ID is the dir name, got %q", found.ID)
	}
	if found.Description == "" {
		t.Fatal("its description must survive — that is the whole retrieval signal")
	}
}

// A marketplace cache dir can hold the WHOLE marketplace's skills while the
// INSTALLED plugin owns only some of them. Harvesting the dir under one pack
// name mints IDs for skills that plugin does not own — uninvocable — and those
// phantoms then win description-dedup against the correctly-attributed copies.
//
// Measured on the live machine 2026-08-11: the anthropic-agent-skills cache
// held 17 skills, but the installed `document-skills` plugin owns only 4
// (xlsx/docx/pptx/pdf). The index carried 13 phantom `document-skills:*` IDs,
// and the real `frontend-design:frontend-design` and `skill-creator:skill-creator`
// were suppressed by them. `.claude-plugin/marketplace.json` states ownership
// authoritatively, so use it.
func TestHarvestRootsHonorsMarketplaceOwnership(t *testing.T) {
	home := t.TempDir()
	install := filepath.Join(home, "cache", "mkt", "owner-plugin", "1.0.0")
	skills := filepath.Join(install, "skills")
	for _, n := range []string{"mine", "someone-elses"} {
		writeTestSkill(t, filepath.Join(skills, n), n)
	}
	writeMarketplace(t, install, `{"plugins":[
		{"name":"owner-plugin","skills":["./skills/mine"]},
		{"name":"other-plugin","skills":["./skills/someone-elses"]}]}`)

	got, err := HarvestRoots([]Root{{Path: skills, Pack: "owner-plugin"}})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["owner-plugin:mine"] {
		t.Fatalf("the pack's OWN skill must be harvested; got %v", ids)
	}
	if ids["owner-plugin:someone-elses"] {
		t.Fatalf("a skill owned by another plugin must NOT be minted under this pack — that ID cannot be invoked; got %v", ids)
	}
}

// Conservative by design: with no ownership data, harvest everything exactly as
// before. Most roots have no marketplace.json and must be unaffected.
func TestHarvestRootsUnfilteredWithoutMarketplaceData(t *testing.T) {
	home := t.TempDir()
	skills := filepath.Join(home, "plain", "skills")
	for _, n := range []string{"one", "two"} {
		writeTestSkill(t, filepath.Join(skills, n), n)
	}
	got, err := HarvestRoots([]Root{{Path: skills, Pack: "plain"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("no ownership data means no filtering; want 2, got %d (%+v)", len(got), got)
	}
}

// If the manifest never mentions THIS pack, we have no authority over it —
// harvest everything rather than silently emptying the root.
func TestHarvestRootsUnfilteredWhenPackNotDeclared(t *testing.T) {
	home := t.TempDir()
	install := filepath.Join(home, "cache", "mkt", "p", "1.0.0")
	skills := filepath.Join(install, "skills")
	writeTestSkill(t, filepath.Join(skills, "a"), "a")
	writeMarketplace(t, install, `{"plugins":[{"name":"somebody-else","skills":["./skills/zzz"]}]}`)

	got, err := HarvestRoots([]Root{{Path: skills, Pack: "not-declared"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("an undeclared pack must not be filtered to nothing; got %d", len(got))
	}
}

func writeTestSkill(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: test skill " + name + " with its own distinct description.\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMarketplace(t *testing.T, installPath, body string) {
	t.Helper()
	d := filepath.Join(installPath, ".claude-plugin")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "marketplace.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A file with no frontmatter fence at all is still malformed and must stay
// skipped, so relaxing the name rule does not turn every stray .md into a skill.
func TestHarvestStillSkipsFileWithNoFrontmatter(t *testing.T) {
	got, err := Harvest([]string{filepath.Join("testdata", "skills")})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range got {
		if s.Name == "bad" {
			t.Fatalf("a SKILL.md with no frontmatter fence must not be harvested: %+v", s)
		}
	}
}
