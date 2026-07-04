package catalog

import (
	"path/filepath"
	"strings"
	"testing"
)

// harvestHygiene runs the full production pipeline over the hygiene fixture,
// which mirrors the real ~/.claude/skills pathologies: a pack-internal twin at
// gstack/qa (byte-identical to gstack-qa), a hidden .agents stripped-name twin,
// an alias dir (_gstack-command, identical to gstack), temp_git_*/temp_subdir_*
// scratch clones, node_modules junk, and two description-less skills.
func harvestHygiene(t *testing.T) []Skill {
	t.Helper()
	root := filepath.Join("testdata", "hygiene", "skills")
	raw, err := HarvestRoots([]Root{{Path: root, Pack: UserPack}})
	if err != nil {
		t.Fatal(err)
	}
	return Dedup(raw)
}

func TestHarvestRoots_SkipsHiddenTempNodeModules(t *testing.T) {
	got := harvestHygiene(t)
	for _, s := range got {
		p := filepath.ToSlash(s.Path)
		for _, bad := range []string{"/.agents/", "/temp_git_", "/temp_subdir_", "/node_modules/"} {
			if strings.Contains(p, bad) {
				t.Fatalf("harvested a skill from a skipped subtree: %s", s.Path)
			}
		}
	}
}

func TestDedup_CollapsesDescriptionTwinsToInvocableCopy(t *testing.T) {
	got := harvestHygiene(t)
	byID := map[string]Skill{}
	for _, s := range got {
		if _, dup := byID[s.ID]; dup {
			t.Fatalf("duplicate ID after Dedup: %s", s.ID)
		}
		byID[s.ID] = s
	}

	wantIDs := []string{"gstack-qa", "gstack", "tiny-a", "tiny-b"}
	if len(got) != len(wantIDs) {
		var ids []string
		for _, s := range got {
			ids = append(ids, s.ID)
		}
		t.Fatalf("want %d skills %v, got %d: %v", len(wantIDs), wantIDs, len(got), ids)
	}
	for _, id := range wantIDs {
		if _, ok := byID[id]; !ok {
			t.Fatalf("missing expected skill %q; got %+v", id, got)
		}
	}

	// The surviving gstack-qa must be the TOP-LEVEL invocable copy, not the
	// nested gstack/qa twin.
	if p := filepath.ToSlash(byID["gstack-qa"].Path); !strings.HasSuffix(p, "skills/gstack-qa/SKILL.md") {
		t.Fatalf("gstack-qa survived from the wrong path: %s", p)
	}
	// gstack vs _gstack-command are same-depth twins; the dir whose name
	// matches the frontmatter name (gstack) must win.
	if p := filepath.ToSlash(byID["gstack"].Path); !strings.HasSuffix(p, "skills/gstack/SKILL.md") {
		t.Fatalf("gstack survived from the wrong path: %s", p)
	}
}

func TestDedup_NeverCollapsesEmptyDescriptions(t *testing.T) {
	got := harvestHygiene(t)
	seen := 0
	for _, s := range got {
		if s.ID == "tiny-a" || s.ID == "tiny-b" {
			seen++
		}
	}
	if seen != 2 {
		t.Fatalf("description-less skills were wrongly collapsed: got %d of 2", seen)
	}
}

func TestHarvestRoots_PluginPackNaming(t *testing.T) {
	root := filepath.Join("testdata", "hygiene", "plugcache", "superpowers", "skills")
	got, err := HarvestRoots([]Root{{Path: root, Pack: "superpowers"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 skill, got %d: %+v", len(got), got)
	}
	s := got[0]
	if s.ID != "superpowers:brainstorming" {
		t.Fatalf("plugin ID must be the invocable <plugin>:<skill> name, got %q", s.ID)
	}
	if s.Name != "brainstorming" || s.Source != "superpowers" {
		t.Fatalf("Name/Source wrong: %+v", s)
	}
}

func TestInvocableID(t *testing.T) {
	if got := InvocableID(UserPack, "gstack-qa"); got != "gstack-qa" {
		t.Fatalf("user skills are invoked by bare dir name, got %q", got)
	}
	if got := InvocableID("", "gstack-qa"); got != "gstack-qa" {
		t.Fatalf("empty pack must behave as user pack, got %q", got)
	}
	if got := InvocableID("superpowers", "brainstorming"); got != "superpowers:brainstorming" {
		t.Fatalf("plugin skills are invoked as plugin:skill, got %q", got)
	}
}
