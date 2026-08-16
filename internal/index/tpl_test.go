package index

// W9-P item 1: the template is part of the index identity, and the hash is
// over the TEMPLATED text — the two properties that make a template change
// impossible to mix into an existing index silently.

import (
	"os"
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

// The sidecar round-trips the templated identity AND the guard through the
// PURE bin path — the JSON is deleted before LoadFast, so a decode failure
// cannot silently pass via the JSON fallback (review 2026-08-16, G7).
func TestSidecarRoundTripsIdentity(t *testing.T) {
	dir := t.TempDir()
	idx := &Index{Model: "embeddinggemma/tpl1", TplGuard: "tpl1", Dim: 2, BuiltUnix: 42,
		Entries: []Entry{{Skill: catalog.Skill{ID: "a"}, Vec: []float64{1, 0}, Hash: "h"}}}
	p := dir + "/index.json"
	if err := idx.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil { // force the bin path
		t.Fatal(err)
	}
	got, err := LoadFast(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "embeddinggemma/tpl1" || got.TplGuard != "tpl1" {
		t.Fatalf("identity/guard lost through the sidecar: model=%q guard=%q", got.Model, got.TplGuard)
	}
	if _, err := got.SpecForIndex(); err != nil {
		t.Fatalf("a bin-loaded templated index must resolve: %v", err)
	}
}

// The TplGuard tripwire: a templated identity whose guard is missing (a
// pre-template binary's save strips the unknown field) or names another
// version must refuse — that is the ONLY detectable trace of an old
// mr-index re-embedding a templated index raw (review 2026-08-16, MAJOR).
func TestSpecForIndexGuardTripwire(t *testing.T) {
	stripped := &Index{Model: "embeddinggemma/tpl1"}
	if _, err := stripped.SpecForIndex(); err == nil || !strings.Contains(err.Error(), "template guard") {
		t.Fatalf("stripped guard must refuse naming the guard, got %v", err)
	}
	mismatched := &Index{Model: "embeddinggemma/tpl1", TplGuard: "tpl2"}
	if _, err := mismatched.SpecForIndex(); err == nil {
		t.Fatal("mismatched guard must refuse")
	}
	ok := &Index{Model: "embeddinggemma/tpl1", TplGuard: "tpl1"}
	if _, err := ok.SpecForIndex(); err != nil {
		t.Fatalf("matching guard must resolve: %v", err)
	}
	// Raw identities carry no guard and never consult it.
	raw := &Index{Model: "embeddinggemma"}
	if _, err := raw.SpecForIndex(); err != nil {
		t.Fatalf("raw identity must resolve without a guard: %v", err)
	}
}

// ApplyRefresh refuses a plan whose spec disagrees with the index's own
// identity — the future-caller foot-gun round 2 named: after the explicit
// spec parameter was removed, a wrong-spec plan would have written
// mixed-space vectors and mis-stamped the guard with no signal anywhere.
func TestApplyRefreshRefusesForeignSpecPlan(t *testing.T) {
	fakeEmbed(t)
	spec, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	a := catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}
	tplIdx, err := Build([]catalog.Skill{a}, "ep", time.Second, spec)
	if err != nil {
		t.Fatal(err)
	}
	b := catalog.Skill{ID: "skills:b", Name: "b", Description: "beta"}
	// Plan built under the RAW spec against the templated index → refuse.
	rawPlan := tplIdx.PlanRefresh([]catalog.Skill{a, b}, embedtpl.Raw("embeddinggemma"))
	if err := tplIdx.ApplyRefresh(rawPlan, "ep", time.Second); err == nil {
		t.Fatal("raw-spec plan against a templated index must refuse")
	}
	if tplIdx.TplGuard != "tpl1" || len(tplIdx.Entries) != 1 {
		t.Fatalf("refused apply must not touch the index: guard=%q entries=%d", tplIdx.TplGuard, len(tplIdx.Entries))
	}
	// And the mirror: a templated plan against a raw index → refuse.
	rawIdx, err := Build([]catalog.Skill{a}, "ep", time.Second, embedtpl.Raw("embeddinggemma"))
	if err != nil {
		t.Fatal(err)
	}
	tplPlan := rawIdx.PlanRefresh([]catalog.Skill{a, b}, spec)
	if err := rawIdx.ApplyRefresh(tplPlan, "ep", time.Second); err == nil {
		t.Fatal("templated plan against a raw index must refuse")
	}
	if rawIdx.TplGuard != "" {
		t.Fatalf("refused apply must not stamp the guard: %q", rawIdx.TplGuard)
	}
}

// Build stamps the guard; a refresh under the same spec re-stamps it.
func TestGuardStampedByBuildAndRefresh(t *testing.T) {
	fakeEmbed(t)
	spec, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	a := catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}
	idx, err := Build([]catalog.Skill{a}, "ep", time.Second, spec)
	if err != nil {
		t.Fatal(err)
	}
	if idx.TplGuard != "tpl1" {
		t.Fatalf("build must stamp the guard, got %q", idx.TplGuard)
	}
	harvestFn = func(roots []catalog.Root) ([]catalog.Skill, error) { return []catalog.Skill{a}, nil }
	t.Cleanup(func() { harvestFn = nil })
	if _, _, _, err := idx.Refresh(nil, "ep", time.Second); err != nil {
		t.Fatal(err)
	}
	if idx.TplGuard != "tpl1" {
		t.Fatalf("refresh must preserve the guard, got %q", idx.TplGuard)
	}
	raw, err := Build([]catalog.Skill{a}, "ep", time.Second, embedtpl.Raw("embeddinggemma"))
	if err != nil {
		t.Fatal(err)
	}
	if raw.TplGuard != "" {
		t.Fatalf("raw build must leave the guard empty, got %q", raw.TplGuard)
	}
}
