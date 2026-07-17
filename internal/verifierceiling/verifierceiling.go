// Package verifierceiling turns labeled verifier records (verdict + graded
// confidence) into the load-bearing "verifier ceiling" metrics for the slice-4
// eval stack (CC1, decision record §Q9): decisive accuracy, coverage, the
// defer-discounted effective ceiling, and the selective-risk curves AURC and
// AUGRC. A defer or error is a non-answer — never a pass — and is counted as a
// loss whenever accepted, so non-answers lower the effective ceiling exactly as
// Q9 requires. AUGRC (joint accept-and-wrong) is the decision metric; AURC is
// reported alongside it.
package verifierceiling

import (
	"fmt"
	"sort"

	vp "github.com/dmmdea/meta-router/internal/orch/strategy/verifierpilot"
)

// Q9 degeneracy floors: fewer than minDistinct distinct decisive confidence
// levels, or more than maxBucketMass of decisive mass at one value, collapses
// the AUGRC curve toward the forbidden binary-score degeneracy.
const (
	minDistinct   = 5
	maxBucketMass = 0.60
)

// Ceiling is the roll-up of a verifier-ceiling measurement over a record set.
type Ceiling struct {
	N        int `json:"n"`
	Decisive int `json:"decisive"` // pass|fail verdicts (the answers)
	Agree    int `json:"agree"`
	Disagree int `json:"disagree"`
	Deferred int `json:"deferred"`
	Errored  int `json:"errored"`

	DecisiveAccuracy float64 `json:"decisive_accuracy"` // agree / decisive (accuracy given an answer)
	Coverage         float64 `json:"coverage"`          // decisive / n
	EffectiveCeiling float64 `json:"effective_ceiling"` // agree / n (non-answers drag it down)

	AURC  float64 `json:"aurc"`  // area under risk-coverage (conditional risk); lower better
	AUGRC float64 `json:"augrc"` // area under generalized risk-coverage; lower better — the decision metric

	DistinctConfidence int     `json:"distinct_confidence"`
	MaxBucketMass      float64 `json:"max_bucket_mass"`
	Degenerate         bool    `json:"degenerate"`
	DegenerateReason   string  `json:"degenerate_reason,omitempty"`
}

type point struct {
	conf     float64
	answered bool
	wrong    bool // decisive AND disagreed, OR (for a non-answer) accepted = a loss
}

// Compute rolls records into a Ceiling. Order-independent.
func Compute(recs []vp.Record) Ceiling {
	var c Ceiling
	c.N = len(recs)
	pts := make([]point, 0, len(recs))
	confCount := map[float64]int{}
	for _, r := range recs {
		p := point{conf: r.Confidence}
		switch r.Verdict {
		case vp.VerdictPass, vp.VerdictFail:
			p.answered = true
			p.wrong = !r.Agree
			c.Decisive++
			if r.Agree {
				c.Agree++
			} else {
				c.Disagree++
			}
			confCount[r.Confidence]++
		case vp.VerdictDefer:
			c.Deferred++
			p.conf = 0 // a non-answer sorts to the bottom of coverage
		default: // errored / unknown
			c.Errored++
			p.conf = 0
		}
		pts = append(pts, p)
	}
	if c.Decisive > 0 {
		c.DecisiveAccuracy = float64(c.Agree) / float64(c.Decisive)
	}
	if c.N > 0 {
		c.Coverage = float64(c.Decisive) / float64(c.N)
		c.EffectiveCeiling = float64(c.Agree) / float64(c.N)
	}
	c.AURC, c.AUGRC = curves(pts, c.N)
	c.DistinctConfidence = len(confCount)
	c.MaxBucketMass = maxMass(confCount, c.Decisive)
	c.Degenerate, c.DegenerateReason = degenerate(c.DistinctConfidence, c.MaxBucketMass, c.Decisive)
	return c
}

// curves computes AURC (mean conditional risk over coverage) and AUGRC (mean
// generalized risk over coverage) with order-independent tie handling: within a
// block of equal confidence of size g containing w losses, the expected losses
// among the first s of the block is s*(w/g), so the curve never depends on the
// arbitrary order of tied records.
func curves(pts []point, n int) (aurc, augrc float64) {
	if n == 0 {
		return 0, 0
	}
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].conf > pts[j].conf })
	var sumCond, sumGen, cumWrong float64
	i, k := 0, 0
	for i < n {
		j := i + 1
		for j < n && pts[j].conf == pts[i].conf {
			j++
		}
		g := j - i
		wBlock := 0
		for t := i; t < j; t++ {
			if pts[t].wrong || !pts[t].answered { // a non-answer accepted is a loss
				wBlock++
			}
		}
		for s := 1; s <= g; s++ {
			k++
			w := cumWrong + float64(s)*float64(wBlock)/float64(g)
			sumCond += w / float64(k)
			sumGen += w / float64(n)
		}
		cumWrong += float64(wBlock)
		i = j
	}
	return sumCond / float64(n), sumGen / float64(n)
}

func maxMass(counts map[float64]int, decisive int) float64 {
	if decisive == 0 {
		return 0
	}
	max := 0
	for _, v := range counts {
		if v > max {
			max = v
		}
	}
	return float64(max) / float64(decisive)
}

func degenerate(distinct int, maxBucket float64, decisive int) (bool, string) {
	if decisive == 0 {
		return true, "no decisive answers — verifier deferred/errored on everything"
	}
	if distinct < minDistinct {
		return true, fmt.Sprintf("only %d distinct confidence levels (<%d) — AUGRC curve under-resolved", distinct, minDistinct)
	}
	if maxBucket > maxBucketMass {
		return true, fmt.Sprintf("%.0f%% of decisive mass at one confidence value (>%.0f%%) — curve collapses", maxBucket*100, maxBucketMass*100)
	}
	return false, ""
}
