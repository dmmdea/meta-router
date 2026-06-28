package index

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	idx := &Index{
		Model: "embeddinggemma", Dim: 2, BuiltUnix: 123,
		Entries: []Entry{
			{Skill: catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}, Vec: []float64{1, 0}, Hash: "h1"},
			{Skill: catalog.Skill{ID: "skills:b", Name: "b", Description: "beta"}, Vec: []float64{0, 1}, Hash: "h2"},
		},
	}
	p := filepath.Join(t.TempDir(), "index.json")
	if err := idx.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 2 || got.Entries[1].Skill.ID != "skills:b" || got.Entries[0].Vec[0] != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	sk, vs := got.Skills(), got.Vectors()
	if len(sk) != 2 || len(vs) != 2 || sk[0].ID != "skills:a" || vs[0][0] != 1 {
		t.Fatalf("accessors misaligned: %v / %v", sk, vs)
	}
}

func TestHashSkill_ChangesWithText(t *testing.T) {
	a := HashSkill(catalog.Skill{Name: "x", Description: "one"})
	b := HashSkill(catalog.Skill{Name: "x", Description: "two"})
	if a == b || a == "" {
		t.Fatalf("hash did not change with text: %q %q", a, b)
	}
}

func TestBuild_Live(t *testing.T) {
	const ep = "http://127.0.0.1:11436"
	if !live(ep) {
		t.Skip("embedder down")
	}
	idx, err := Build([]catalog.Skill{
		{ID: "skills:a", Name: "qa", Description: "QA a web app"},
		{ID: "skills:b", Name: "init", Description: "start a new project"},
	}, ep, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Entries) != 2 || idx.Dim == 0 || len(idx.Entries[0].Vec) != idx.Dim {
		t.Fatalf("bad index: dim=%d entries=%d", idx.Dim, len(idx.Entries))
	}
	if idx.Entries[0].Hash == "" {
		t.Fatal("entry hash is empty")
	}
}

func TestBuild_EmptySkills(t *testing.T) {
	idx, err := Build(nil, "http://127.0.0.1:11436", time.Second)
	if err != nil {
		t.Fatalf("empty build should not error: %v", err)
	}
	if len(idx.Entries) != 0 || idx.Dim != 0 {
		t.Fatalf("expected empty index, got %d entries dim %d", len(idx.Entries), idx.Dim)
	}
}

func live(ep string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(ep + "/v1/models")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
