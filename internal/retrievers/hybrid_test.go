package retrievers

import (
	"github.com/dmmdea/meta-router/internal/catalog"
	"testing"
)

func TestHybridRanks(t *testing.T) {
	liveOrSkip(t)
	skills := []catalog.Skill{
		{ID: "skills:gstack-qa", Name: "gstack-qa", Description: "QA test a web application and fix bugs"},
		{ID: "skills:gsd-new-project", Name: "gsd-new-project", Description: "initialize a new project with a roadmap"},
	}
	r, err := NewHybrid(skills, "http://127.0.0.1:11436")
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Retrieve("qa test that my website works", 2) // mix of lexical (qa/test) + semantic (website)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0] != "skills:gstack-qa" {
		t.Fatalf("expected gstack-qa first, got %v", got)
	}
	if r.Name() != "hybrid-rrf" {
		t.Fatalf("name=%q", r.Name())
	}
}
