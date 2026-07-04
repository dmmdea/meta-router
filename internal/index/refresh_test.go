package index

import (
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
)

func TestRefresh_OnlyEmbedsChanged(t *testing.T) {
	// seed index: a (hash matches future harvest), b (will change)
	a := catalog.Skill{ID: "skills:a", Name: "a", Description: "alpha"}
	bOld := catalog.Skill{ID: "skills:b", Name: "b", Description: "beta-OLD"}
	idx := &Index{Entries: []Entry{
		{Skill: a, Vec: []float64{1, 1}, Hash: HashSkill(a)},
		{Skill: bOld, Vec: []float64{2, 2}, Hash: HashSkill(bOld)},
	}}

	// harvest seam returns: a (unchanged), b (changed desc), c (new); b's old is gone
	bNew := catalog.Skill{ID: "skills:b", Name: "b", Description: "beta-NEW"}
	c := catalog.Skill{ID: "skills:c", Name: "c", Description: "gamma"}
	harvestFn = func(roots []catalog.Root) ([]catalog.Skill, error) {
		return []catalog.Skill{a, bNew, c}, nil
	}
	var embedded []string
	embedFn = func(ep string, to time.Duration, in []string) ([][]float64, error) {
		embedded = append(embedded, in...)
		out := make([][]float64, len(in))
		for i := range in {
			out[i] = []float64{9, 9}
		}
		return out, nil
	}
	t.Cleanup(func() { harvestFn = nil; embedFn = nil }) // restore defaults

	added, updated, removed, err := idx.Refresh(nil, "ep", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || updated != 1 || removed != 0 {
		t.Fatalf("counts: added=%d updated=%d removed=%d", added, updated, removed)
	}
	if len(embedded) != 2 {
		t.Fatalf("re-embedded %d texts, want 2 (b changed + c new), not a", len(embedded))
	}
	if len(idx.Entries) != 3 {
		t.Fatalf("entries=%d want 3", len(idx.Entries))
	}
	// unchanged 'a' kept its cached vector
	for _, e := range idx.Entries {
		if e.Skill.ID == "skills:a" && e.Vec[0] != 1 {
			t.Fatalf("a's cached vector was clobbered: %v", e.Vec)
		}
	}
	// changed (b) and new (c) skills must carry the freshly-embedded vector {9,9}
	for _, e := range idx.Entries {
		if (e.Skill.ID == "skills:b" || e.Skill.ID == "skills:c") && (len(e.Vec) != 2 || e.Vec[0] != 9) {
			t.Fatalf("%s did not get the re-embedded vector: %v", e.Skill.ID, e.Vec)
		}
	}
}
