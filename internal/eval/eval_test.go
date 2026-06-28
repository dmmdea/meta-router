package eval

import (
	"testing"
	"github.com/dmmdea/meta-router/internal/goldset"
)

type stubRetriever struct{}
func (stubRetriever) Name() string { return "stub" }
func (stubRetriever) Retrieve(p string, k int) ([]string, error) {
	// always ranks the correct answer 2nd for the first case, misses the rest
	if p == "a" { return []string{"wrong", "right"}, nil }
	return []string{"wrong"}, nil
}

func TestScore(t *testing.T) {
	cases := []goldset.Case{{Prompt: "a", Expect: []string{"right"}}, {Prompt: "b", Expect: []string{"x"}}}
	m, err := Score(stubRetriever{}, cases, []int{1, 3})
	if err != nil { t.Fatal(err) }
	if m.RecallAt[1] != 0.0 { t.Fatalf("recall@1 should be 0 (right was ranked 2nd), got %v", m.RecallAt[1]) }
	if m.RecallAt[3] != 0.5 { t.Fatalf("recall@3 should be 0.5 (1 of 2 hit), got %v", m.RecallAt[3]) }
	if m.MRR <= 0 { t.Fatalf("MRR should reflect the rank-2 hit, got %v", m.MRR) }
}
