// Package retrievers holds eval.Retriever implementations (baselines + adapters).
package retrievers

import (
	"math"
	"sort"
	"strings"
	"github.com/dmmdea/meta-router/internal/catalog"
)

type BM25 struct {
	ids    []string
	docs   [][]string
	df     map[string]int
	avgLen float64
}

func tokenize(s string) []string { return strings.Fields(strings.ToLower(s)) }

func NewBM25(skills []catalog.Skill) *BM25 {
	b := &BM25{df: map[string]int{}}
	var total int
	for _, s := range skills {
		toks := tokenize(s.EmbedText())
		b.ids = append(b.ids, s.ID)
		b.docs = append(b.docs, toks)
		total += len(toks)
		seen := map[string]bool{}
		for _, t := range toks {
			if !seen[t] { b.df[t]++; seen[t] = true }
		}
	}
	if len(b.docs) > 0 { b.avgLen = float64(total) / float64(len(b.docs)) }
	return b
}

func (b *BM25) Name() string { return "bm25" }

func (b *BM25) Retrieve(prompt string, k int) ([]string, error) {
	const k1, bb = 1.2, 0.75
	n := float64(len(b.docs))
	q := tokenize(prompt)
	type sc struct{ id string; s float64 }
	scores := make([]sc, len(b.docs))
	for i, doc := range b.docs {
		tf := map[string]int{}
		for _, t := range doc { tf[t]++ }
		var s float64
		dl := float64(len(doc))
		for _, t := range q {
			if tf[t] == 0 { continue }
			idf := math.Log(1 + (n-float64(b.df[t])+0.5)/(float64(b.df[t])+0.5))
			num := float64(tf[t]) * (k1 + 1)
			den := float64(tf[t]) + k1*(1-bb+bb*dl/b.avgLen)
			s += idf * num / den
		}
		scores[i] = sc{b.ids[i], s}
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].s > scores[j].s })
	var out []string
	for i := 0; i < len(scores) && i < k; i++ {
		if scores[i].s <= 0 { break }
		out = append(out, scores[i].id)
	}
	return out, nil
}
