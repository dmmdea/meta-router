// Package eval scores a Retriever against a labeled gold-set.
package eval

import (
	"github.com/dmmdea/meta-router/internal/goldset"
	"sort"
	"time"
)

type Retriever interface {
	Name() string
	Retrieve(prompt string, k int) ([]string, error)
}

type Metrics struct {
	Retriever       string
	N               int
	RecallAt        map[int]float64
	MRR             float64
	MedianLatencyMs float64
}

func Score(r Retriever, cases []goldset.Case, ks []int) (Metrics, error) {
	maxK := 0
	for _, k := range ks {
		if k > maxK {
			maxK = k
		}
	}
	m := Metrics{Retriever: r.Name(), N: len(cases), RecallAt: map[int]float64{}}
	hits := map[int]int{}
	var rr float64
	var lat []float64
	for _, c := range cases {
		t0 := time.Now()
		got, err := r.Retrieve(c.Prompt, maxK)
		if err != nil {
			return m, err
		}
		lat = append(lat, float64(time.Since(t0).Microseconds())/1000.0)
		rank := firstHitRank(got, c.Expect)
		if rank > 0 {
			rr += 1.0 / float64(rank)
		}
		for _, k := range ks {
			if rank > 0 && rank <= k {
				hits[k]++
			}
		}
	}
	if len(cases) > 0 {
		for _, k := range ks {
			m.RecallAt[k] = float64(hits[k]) / float64(len(cases))
		}
		m.MRR = rr / float64(len(cases))
	}
	m.MedianLatencyMs = median(lat)
	return m, nil
}

// firstHitRank returns the 1-based rank of the first retrieved id that is in
// expect, or 0 if none hit.
func firstHitRank(got, expect []string) int {
	want := map[string]bool{}
	for _, e := range expect {
		want[e] = true
	}
	for i, g := range got {
		if want[g] {
			return i + 1
		}
	}
	return 0
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	return s[len(s)/2]
}
