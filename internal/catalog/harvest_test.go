package catalog

import (
	"path/filepath"
	"strings"
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
		// Assert ID format: ID = source:name (spec requirement).
		if s.Source == "" {
			t.Fatalf("skill has empty Source: %+v", s)
		}
		if !strings.HasPrefix(s.ID, s.Source+":") {
			t.Fatalf("ID does not start with source: %q should start with %q", s.ID, s.Source+":")
		}
	}
}
