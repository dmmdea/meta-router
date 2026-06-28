package retrievers

import (
	"net"
	"testing"
	"time"
	"github.com/dmmdea/meta-router/internal/catalog"
)

func liveOrSkip(t *testing.T) {
	c, err := net.DialTimeout("tcp", "127.0.0.1:11436", 300*time.Millisecond)
	if err != nil { t.Skip("llama-swap :11436 not up; skipping embed integration") }
	c.Close()
}

func TestEmbedSemanticMatch(t *testing.T) {
	liveOrSkip(t)
	skills := []catalog.Skill{
		{ID: "gstack:gstack-qa", Name: "gstack-qa", Description: "QA test a web application and fix bugs"},
		{ID: "gsd:gsd-new-project", Name: "gsd-new-project", Description: "initialize a new project with a roadmap"},
	}
	r, err := NewEmbed(skills, "http://127.0.0.1:11436")
	if err != nil { t.Fatal(err) }
	got, err := r.Retrieve("check my website works end to end", 2) // semantic, not lexical
	if err != nil { t.Fatal(err) }
	if len(got) == 0 || got[0] != "gstack:gstack-qa" {
		t.Fatalf("expected gstack-qa first, got %v", got)
	}
}
