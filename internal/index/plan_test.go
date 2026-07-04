package index

import (
	"errors"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
)

func seedIndex() *Index {
	a := catalog.Skill{ID: "a", Name: "a", Description: "alpha"}
	b := catalog.Skill{ID: "b", Name: "b", Description: "beta"}
	c := catalog.Skill{ID: "c", Name: "c", Description: "gamma"}
	return &Index{Entries: []Entry{
		{Skill: a, Vec: []float64{1}, Hash: HashSkill(a)},
		{Skill: b, Vec: []float64{2}, Hash: HashSkill(b)},
		{Skill: c, Vec: []float64{3}, Hash: HashSkill(c)},
	}}
}

func TestPlanRefresh_ReportsRemovals(t *testing.T) {
	idx := seedIndex()
	cur := []catalog.Skill{{ID: "a", Name: "a", Description: "alpha"}}
	p := idx.PlanRefresh(cur)
	if p.Added != 0 || p.Updated != 0 {
		t.Fatalf("added=%d updated=%d, want 0/0", p.Added, p.Updated)
	}
	if len(p.RemovedIDs) != 2 {
		t.Fatalf("RemovedIDs=%v, want b and c", p.RemovedIDs)
	}
	// Plan must be pure: index untouched until ApplyRefresh.
	if len(idx.Entries) != 3 {
		t.Fatalf("PlanRefresh mutated the index: %d entries", len(idx.Entries))
	}
}

func TestRemovalExceeds(t *testing.T) {
	cases := []struct {
		before, removed int
		want            bool
	}{
		{150, 60, true},  // 40% > 30%
		{150, 45, false}, // exactly 30% is allowed
		{150, 46, true},
		{150, 0, false},
		{0, 10, false}, // empty index never triggers
		{3, 2, true},
	}
	for _, c := range cases {
		if got := RemovalExceeds(c.before, c.removed, 0.30); got != c.want {
			t.Fatalf("RemovalExceeds(%d,%d)=%v want %v", c.before, c.removed, got, c.want)
		}
	}
}

func TestApplyRefresh_EmbedFailureLeavesIndexUntouched(t *testing.T) {
	idx := seedIndex()
	embedFn = func(ep string, to time.Duration, in []string) ([][]float64, error) {
		return nil, errors.New("embedder down")
	}
	t.Cleanup(func() { embedFn = nil })
	cur := []catalog.Skill{
		{ID: "a", Name: "a", Description: "alpha"},
		{ID: "d", Name: "d", Description: "delta"}, // new → needs embed
	}
	p := idx.PlanRefresh(cur)
	if err := idx.ApplyRefresh(p, "ep", time.Second); err == nil {
		t.Fatal("expected embed error")
	}
	if len(idx.Entries) != 3 || idx.Entries[0].Skill.ID != "a" {
		t.Fatalf("failed apply must not mutate the index: %+v", idx.Entries)
	}
}
