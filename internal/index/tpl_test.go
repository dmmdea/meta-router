package index

// W9-P item 1: the template is part of the index identity, and the hash is
// over the TEMPLATED text — the two properties that make a template change
// impossible to mix into an existing index silently.

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/embedtpl"
)

// fakeEmbed installs an embedFn seam that records inputs and returns 2-dim
// vectors.
func fakeEmbed(t *testing.T) *[][]string {
	t.Helper()
	var calls [][]string
	embedFn = func(ep string, to time.Duration, model string, in []string) ([][]float64, error) {
		calls = append(calls, in)
		out := make([][]float64, len(in))
		for i := range in {
			out[i] = []float64{1, float64(i)}
		}
		return out, nil
	}
	t.Cleanup(func() { embedFn = nil })
	return &calls
}

func TestBuildRecordsTemplatedIdentityAndHash(t *testing.T) {
	calls := fakeEmbed(t)
	spec, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	s := catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}
	idx, err := Build([]catalog.Skill{s}, "ep", time.Second, spec)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Model != "embeddinggemma/tpl1" {
		t.Fatalf("identity: %q", idx.Model)
	}
	// The embedded text is the TEMPLATED doc…
	if got, want := (*calls)[0][0], "title: none | text: "+s.EmbedText(); got != want {
		t.Fatalf("embedded text:\n got %q\nwant %q", got, want)
	}
	// …and the hash covers it: templated hash ≠ raw hash, so flipping the
	// template on can never silently reuse raw-text vectors.
	if idx.Entries[0].Hash == HashSkill(s) {
		t.Fatal("templated hash must differ from the raw hash")
	}
	if idx.Entries[0].Hash != HashSkillSpec(s, spec) {
		t.Fatal("entry hash must be the spec-aware hash")
	}
}

func TestBuildRawKeepsLegacyIdentityAndHash(t *testing.T) {
	calls := fakeEmbed(t)
	s := catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}
	idx, err := Build([]catalog.Skill{s}, "ep", time.Second, embedtpl.Raw("embeddinggemma"))
	if err != nil {
		t.Fatal(err)
	}
	// Byte-identical to the pre-registry behavior: bare model identity, raw
	// text embedded, raw hash — an in-place upgrade re-embeds NOTHING.
	if idx.Model != "embeddinggemma" {
		t.Fatalf("identity: %q", idx.Model)
	}
	if got := (*calls)[0][0]; got != s.EmbedText() {
		t.Fatalf("raw build must embed the raw text, got %q", got)
	}
	if idx.Entries[0].Hash != HashSkill(s) {
		t.Fatal("raw build hash must equal the legacy HashSkill")
	}
}

// A refresh derives its spec from the index's own identity: a templated index
// refreshes templated (unchanged skills keep vectors; new ones embed through
// the doc template).
func TestRefreshPreservesTemplatedIdentity(t *testing.T) {
	calls := fakeEmbed(t)
	spec, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	a := catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}
	idx, err := Build([]catalog.Skill{a}, "ep", time.Second, spec)
	if err != nil {
		t.Fatal(err)
	}
	b := catalog.Skill{ID: "skills:b", Name: "b", Description: "beta"}
	harvestFn = func(roots []catalog.Root) ([]catalog.Skill, error) {
		return []catalog.Skill{a, b}, nil
	}
	t.Cleanup(func() { harvestFn = nil })
	added, updated, removed, err := idx.Refresh(nil, "ep", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || updated != 0 || removed != 0 {
		t.Fatalf("counts: +%d ~%d -%d", added, updated, removed)
	}
	if idx.Model != "embeddinggemma/tpl1" {
		t.Fatalf("refresh must preserve the identity, got %q", idx.Model)
	}
	// Only b was embedded, and through the doc template.
	last := (*calls)[len(*calls)-1]
	if len(last) != 1 || last[0] != spec.ApplyDoc(b.EmbedText()) {
		t.Fatalf("refresh embed input: %q", last)
	}
}

// An index recording a template this binary does not know must refuse to
// refresh — before any harvest or embed — and leave the index untouched.
func TestRefreshUnknownTemplateRefuses(t *testing.T) {
	fakeEmbed(t)
	harvested := false
	harvestFn = func(roots []catalog.Root) ([]catalog.Skill, error) {
		harvested = true
		return nil, nil
	}
	t.Cleanup(func() { harvestFn = nil })
	idx := &Index{Model: "embeddinggemma/tpl9", Entries: []Entry{{Skill: catalog.Skill{ID: "a"}, Vec: []float64{1}, Hash: "h"}}}
	_, _, _, err := idx.Refresh(nil, "ep", time.Second)
	if err == nil {
		t.Fatal("unknown template version must refuse")
	}
	if !strings.Contains(err.Error(), "tpl9") {
		t.Fatalf("error must name the unknown template: %v", err)
	}
	if harvested {
		t.Fatal("refusal must happen before any harvest")
	}
	if len(idx.Entries) != 1 || idx.Entries[0].Hash != "h" {
		t.Fatalf("refused refresh must not mutate the index: %+v", idx.Entries)
	}
}

// The sidecar round-trips the templated identity (Model is the identity
// carrier; a sidecar that dropped it would resurrect the silent mix).
func TestSidecarRoundTripsIdentity(t *testing.T) {
	dir := t.TempDir()
	idx := &Index{Model: "embeddinggemma/tpl1", Dim: 2, BuiltUnix: 42,
		Entries: []Entry{{Skill: catalog.Skill{ID: "a"}, Vec: []float64{1, 0}, Hash: "h"}}}
	p := dir + "/index.json"
	if err := idx.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFast(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "embeddinggemma/tpl1" {
		t.Fatalf("identity lost through save/load: %q", got.Model)
	}
}
