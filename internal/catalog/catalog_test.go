package catalog

import (
	"strings"
	"testing"
)

func TestParseSkillMD(t *testing.T) {
	s, err := ParseSkillMD("testdata/valid.md")
	if err != nil { t.Fatal(err) }
	if s.Name != "gstack-qa" { t.Fatalf("name=%q", s.Name) }
	if s.Description == "" || s.WhenToUse == "" { t.Fatalf("missing text: %+v", s) }
}

func TestParseSkillMD_NoFrontmatter(t *testing.T) {
	if _, err := ParseSkillMD("testdata/no-frontmatter.md"); err == nil {
		t.Fatal("expected error on missing frontmatter")
	}
}

func TestParseSkillMD_Minimal(t *testing.T) {
	s, err := ParseSkillMD("testdata/minimal.md") // name+description only
	if err != nil { t.Fatal(err) }
	if s.Name != "tiny" || s.WhenToUse != "" { t.Fatalf("%+v", s) }
}

func TestParseSkillMD_BlockFolded(t *testing.T) {
	s, err := ParseSkillMD("testdata/block-folded.md")
	if err != nil { t.Fatal(err) }
	if s.Name != "img-skill" { t.Fatalf("name=%q", s.Name) }
	// The full block-scalar description must be captured, not just ">".
	for _, want := range []string{"Image prompting", "Use when", "blog covers", "storyboards"} {
		if !strings.Contains(s.Description, want) {
			t.Fatalf("description missing %q; got %q", want, s.Description)
		}
	}
	if strings.TrimSpace(s.Description) == ">" {
		t.Fatal("description collapsed to the block indicator")
	}
	if s.WhenToUse != "when the user wants an image prompt" {
		t.Fatalf("when_to_use after block scalar not parsed: %q", s.WhenToUse)
	}
}

func TestParseSkillMD_BlockLiteral(t *testing.T) {
	s, err := ParseSkillMD("testdata/block-literal.md")
	if err != nil { t.Fatal(err) }
	for _, want := range []string{"Line one", "Line two", "alpha", "gamma"} {
		if !strings.Contains(s.Description, want) {
			t.Fatalf("literal block missing %q; got %q", want, s.Description)
		}
	}
}

func TestParseSkillMD_NestedMetadataNoLeak(t *testing.T) {
	s, err := ParseSkillMD("testdata/nested-metadata.md")
	if err != nil { t.Fatal(err) }
	if s.Name != "meta-skill" { t.Fatalf("name=%q", s.Name) }
	if !strings.Contains(s.Description, "fork and sync") {
		t.Fatalf("description after nested metadata not captured: %q", s.Description)
	}
	// metadata children must NOT leak into the description.
	if strings.Contains(s.Description, "0.1.0") || strings.Contains(s.Description, "MIT") {
		t.Fatalf("metadata leaked into description: %q", s.Description)
	}
}
