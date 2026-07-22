// Package promotion is V7 of the slice-4 eval stack: the equal-budget
// strategy-template promotion gate (decision record Q5). A template earns
// default status ONLY through the conjunctive current-SOTA rule on PAIRED
// template-vs-solo runs at matched token budget:
//
//	paired BCa bootstrap CI lower bound > 0   AND   sign-flip permutation p < 0.05
//
// Equal budget is THE confound control — single-agent matches multi-agent at
// matched spend (~80% of variance is token budget) — so budget-skewed pairs
// are excluded and counted, never silently kept, and parity is normalized by
// the SMALLER spend (max/min ≤ 1.25) so the bigger spender gains no slack.
// Repeated tasks collapse to ONE delta per task (pseudo-replication is a
// promotion-gaming vector: thirty reruns of one easy task are one observation,
// not thirty). At small n the gate refuses BY ARITHMETIC — the sign-flip p is
// EXACT (full 2^n enumeration) at these sizes, so five uniform wins yield
// p = 2/32 = 0.0625 ≥ .05 on every seed. Until promoted, templates remain
// manual --strategy seams — zero feature regression.
package promotion

import (
	"fmt"
	"sort"

	"github.com/dmmdea/meta-router/internal/policyeval"
)

// budgetTolerance: max/min token spend must be ≤ 1+budgetTolerance.
const budgetTolerance = 0.25

// PairedRun is one gold task executed BOTH ways at matched budget.
type PairedRun struct {
	Task           string  `json:"task"`
	Template       string  `json:"template"`
	TemplateScore  float64 `json:"template_score"` // verifier pass rate ∈ [0,1]
	SoloScore      float64 `json:"solo_score"`     // verifier pass rate ∈ [0,1]
	TemplateTokens int64   `json:"template_tokens"`
	SoloTokens     int64   `json:"solo_tokens"`
}

// Verdict is the gate's decision for one template.
type Verdict struct {
	Template       string  `json:"template"`
	N              int     `json:"pairs_total"`
	UsedN          int     `json:"tasks_used"` // DISTINCT tasks after collapse
	ExcludedBudget int     `json:"pairs_excluded_budget_skew"`
	Invalid        int     `json:"pairs_invalid"` // non-positive tokens or out-of-range scores
	MeanDelta      float64 `json:"mean_delta"`    // template − solo, per-task means
	CILo           float64 `json:"ci_lo"`
	CIHi           float64 `json:"ci_hi"`
	CIDegenerate   bool    `json:"ci_degenerate"` // all per-task deltas equal — CI is a point, p carries the decision
	P              float64 `json:"sign_flip_p"`
	Promote        bool    `json:"promote"`
	Reason         string  `json:"reason"`
}

// BudgetParity reports whether two token spends are within tolerance,
// normalized by the SMALLER spend (symmetric; non-positive never passes).
func BudgetParity(a, b int64) bool {
	if a <= 0 || b <= 0 {
		return false
	}
	hi, lo := a, b
	if b > a {
		hi, lo = b, a
	}
	return float64(hi-lo)/float64(lo) <= budgetTolerance
}

func validScores(r PairedRun) bool {
	return r.TemplateScore >= 0 && r.TemplateScore <= 1 && r.SoloScore >= 0 && r.SoloScore <= 1
}

// Gate applies the Q5 conjunctive rule to one template's paired runs.
func Gate(template string, runs []PairedRun, iters int, seed int64) Verdict {
	v := Verdict{Template: template, N: len(runs), P: 1}
	perTask := map[string][]float64{}
	for _, r := range runs {
		if r.TemplateTokens <= 0 || r.SoloTokens <= 0 || !validScores(r) {
			v.Invalid++
			continue
		}
		if !BudgetParity(r.TemplateTokens, r.SoloTokens) {
			v.ExcludedBudget++
			continue
		}
		perTask[r.Task] = append(perTask[r.Task], r.TemplateScore-r.SoloScore)
	}
	// One delta per DISTINCT task (mean over its repeats) — deterministic order.
	tasks := make([]string, 0, len(perTask))
	for t := range perTask {
		tasks = append(tasks, t)
	}
	sort.Strings(tasks)
	deltas := make([]float64, 0, len(tasks))
	for _, t := range tasks {
		ds := perTask[t]
		sum := 0.0
		for _, d := range ds {
			sum += d
		}
		deltas = append(deltas, sum/float64(len(ds)))
	}
	v.UsedN = len(deltas)
	if v.UsedN == 0 {
		v.Reason = "no qualifying equal-budget pairs — nothing to judge (templates stay manual seams)"
		return v
	}
	sum := 0.0
	minD, maxD := deltas[0], deltas[0]
	for _, d := range deltas {
		sum += d
		if d < minD {
			minD = d
		}
		if d > maxD {
			maxD = d
		}
	}
	v.MeanDelta = sum / float64(v.UsedN)
	v.CIDegenerate = minD == maxD
	v.CILo, v.CIHi = policyeval.BootstrapCI(deltas, 0.95, iters, seed)
	v.P = policyeval.SignFlipP(deltas, iters, seed+1) // decorrelated from the CI resampling stream

	degNote := ""
	if v.CIDegenerate {
		degNote = " [CI degenerate: all per-task deltas equal — the exact p carries the decision]"
	}
	switch {
	case v.CILo <= 0 && v.P >= 0.05:
		v.Reason = fmt.Sprintf("refused: CI floor %.3f ≤ 0 and p %.4f ≥ .05 at n=%d%s", v.CILo, v.P, v.UsedN, degNote)
	case v.CILo <= 0:
		v.Reason = fmt.Sprintf("refused: CI floor %.3f ≤ 0 at n=%d (p %.4f alone is not enough — conjunctive rule)%s", v.CILo, v.UsedN, v.P, degNote)
	case v.P >= 0.05:
		v.Reason = fmt.Sprintf("refused: p %.4f ≥ .05 at n=%d (CI floor %.3f alone is not enough — conjunctive rule)%s", v.P, v.UsedN, v.CILo, degNote)
	default:
		v.Promote = true
		v.Reason = fmt.Sprintf("PROMOTED: CI [%.3f, %.3f] floor > 0 and exact/MC p %.4f < .05 over %d distinct equal-budget tasks%s", v.CILo, v.CIHi, v.P, v.UsedN, degNote)
	}
	return v
}
