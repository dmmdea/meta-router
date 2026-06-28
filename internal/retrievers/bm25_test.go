package retrievers

import (
	"testing"
	"github.com/dmmdea/meta-router/internal/catalog"
)

func TestBM25RanksLexicalMatch(t *testing.T) {
	skills := []catalog.Skill{
		{ID: "gstack:gstack-qa", Name: "gstack-qa", Description: "QA test a web application and fix bugs"},
		{ID: "gsd:gsd-new-project", Name: "gsd-new-project", Description: "initialize a new project with a roadmap"},
	}
	r := NewBM25(skills)
	got, err := r.Retrieve("qa test my web app for bugs", 2)
	if err != nil { t.Fatal(err) }
	if len(got) == 0 || got[0] != "gstack:gstack-qa" {
		t.Fatalf("expected gstack-qa first, got %v", got)
	}
}
