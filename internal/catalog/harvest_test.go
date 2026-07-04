package catalog

import (
	"path/filepath"
	"testing"
)

func TestHarvest(t *testing.T) {
	root := filepath.Join("testdata", "skills")
	got, err := Harvest([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly 2 valid skills expected (alpha + beta).
	// The bad/ fixture proves skip-bad-skill resilience: one malformed SKILL.md is skipped, not fatal.
	if len(got) != 2 {
		t.Fatalf("want 2 skills, got %d: %+v", len(got), got)
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
