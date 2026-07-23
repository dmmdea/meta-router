// Package policyeval is V3 of the slice-4 eval stack: the counterfactual
// policy evaluator over the V2 oracle replay table. Every policy π (a
// task→lane assignment) is valued by EXACT table lookup — the replay-oracle
// Direct Method (no propensities, no variance blow-up) — and compared to the
// always-Claude reference on the two axes the decision record fixes (Q1):
// quality ratio and Claude-window fraction. A cell with no recorded data is
// UNKNOWN: it never passes and is counted, never imputed.
package policyeval

import (
	"math"
	"math/rand"
	"sort"
)

// Table holds per-(task,lane) pass rates aggregated over trials.
type Table struct {
	cells map[string]map[string]*cell // task → lane → cell
}

type cell struct{ pass, n int }

func NewTable() *Table { return &Table{cells: map[string]map[string]*cell{}} }

// Add records one trial observation.
func (t *Table) Add(task, lane string, pass bool) {
	m, ok := t.cells[task]
	if !ok {
		m = map[string]*cell{}
		t.cells[task] = m
	}
	c, ok := m[lane]
	if !ok {
		c = &cell{}
		m[lane] = c
	}
	c.n++
	if pass {
		c.pass++
	}
}

// Rate returns the cell's pass rate and whether any data exists.
func (t *Table) Rate(task, lane string) (float64, bool) {
	if c, ok := t.cells[task][lane]; ok && c.n > 0 {
		return float64(c.pass) / float64(c.n), true
	}
	return 0, false
}

// Policy assigns a lane to a task ("" = abstain/unknown).
type Policy func(task string) string

// Fixed routes every task to one lane.
func Fixed(lane string) Policy { return func(string) string { return lane } }

// FromMap routes per the given assignment.
func FromMap(m map[string]string) Policy { return func(task string) string { return m[task] } }

// laneCost orders lanes by window cost for oracle tie-breaks: free first,
// Claude last (the guarded resource on the WF@Q axis). Unknown lanes cost MAX
// (never win a tie by accident), and ties at equal cost break on lane name so
// the order is TOTAL — map iteration must never decide a pick.
var laneCost = map[string]int{"local": 0, "glm": 1, "codex": 2, "claude": 3}

func laneCostOf(lane string) int {
	if c, ok := laneCost[lane]; ok {
		return c
	}
	return math.MaxInt32
}

// betterPick reports whether (rate,lane) beats the incumbent under the total
// order: higher rate; then cheaper lane; then lexical lane name.
func betterPick(rate float64, lane string, bestRate float64, bestLane string) bool {
	if rate != bestRate {
		return rate > bestRate
	}
	c, bc := laneCostOf(lane), laneCostOf(bestLane)
	if c != bc {
		return c < bc
	}
	return lane < bestLane
}

// OracleBest picks, per task, the CHEAPEST lane whose pass rate is maximal.
func OracleBest(t *Table) Policy {
	return func(task string) string {
		bestLane, bestRate := "", -1.0
		for lane, c := range t.cells[task] {
			if c.n == 0 {
				continue
			}
			if r := float64(c.pass) / float64(c.n); bestLane == "" || betterPick(r, lane, bestRate, bestLane) {
				bestLane, bestRate = lane, r
			}
		}
		return bestLane
	}
}

// ClassCoverage counts, per class and lane, the subset tasks with observed
// cells — surfaced next to the assignment so a hole-driven pick is VISIBLE
// (a lane that deferred a class's hard tasks wins only its survivors; the
// coverage numbers expose that).
type ClassCoverage map[string]map[string]int

// ClassBest derives a per-CLASS best-lane assignment from a task SUBSET — the
// B'2 cross-validation primitive: called with the TUNING tasks only, the
// returned map is a policy expressible on unseen tasks (unlike the per-task
// oracle, which cannot generalize). Per class it scores each lane by the MEAN
// of its per-task pass rates over the subset (the same unweighted-task-mean
// objective Evaluate reports) and picks the best under the total betterPick
// order. A class with no observed cell in any lane is ABSENT from the map:
// unknown, never imputed.
func ClassBest(t *Table, tasks []string, classOf map[string]string) (map[string]string, ClassCoverage) {
	type agg struct {
		sum float64 // sum of per-task rates (the EVAL objective: unweighted task mean)
		n   int     // tasks observed
	}
	acc := map[string]map[string]*agg{} // class → lane → agg
	for _, task := range tasks {
		cls := classOf[task]
		if cls == "" {
			continue
		}
		for lane, c := range t.cells[task] {
			if c.n == 0 {
				continue
			}
			m, ok := acc[cls]
			if !ok {
				m = map[string]*agg{}
				acc[cls] = m
			}
			a, ok := m[lane]
			if !ok {
				a = &agg{}
				m[lane] = a
			}
			// Mean of per-task rates, NOT pooled pass/n: Evaluate scores the
			// unweighted mean over tasks, and pooling would weight tasks by
			// trial count — optimizing a different objective than the one the
			// policy is scored on.
			a.sum += float64(c.pass) / float64(c.n)
			a.n++
		}
	}
	out := map[string]string{}
	cov := ClassCoverage{}
	for cls, m := range acc {
		bestLane, bestRate := "", -1.0
		cov[cls] = map[string]int{}
		for lane, a := range m {
			cov[cls][lane] = a.n
			if r := a.sum / float64(a.n); bestLane == "" || betterPick(r, lane, bestRate, bestLane) {
				bestLane, bestRate = lane, r
			}
		}
		out[cls] = bestLane
	}
	return out, cov
}

// ByClass routes each task through a class→lane assignment ("" when the
// task's class has no assignment — unknown, never imputed).
func ByClass(assign map[string]string, classOf map[string]string) Policy {
	return func(task string) string { return assign[classOf[task]] }
}

// Eval is a policy's value on the table.
type Eval struct {
	Passes         float64 `json:"passes"` // expected passes (sum of cell rates)
	PassRate       float64 `json:"pass_rate"`
	ClaudeFraction float64 `json:"claude_fraction"` // unit-cost model: claude-routed share
	Unknown        int     `json:"unknown_cells"`
	Assignment     map[string]string
	PerTask        map[string]float64
}

// Evaluate values π over tasks by table lookup.
func Evaluate(t *Table, tasks []string, p Policy) Eval {
	ev := Eval{Assignment: map[string]string{}, PerTask: map[string]float64{}}
	claude := 0
	for _, task := range tasks {
		lane := p(task)
		ev.Assignment[task] = lane
		if lane == "claude" {
			claude++
		}
		r, ok := t.Rate(task, lane)
		if !ok {
			ev.Unknown++
			ev.PerTask[task] = 0
			continue
		}
		ev.Passes += r
		ev.PerTask[task] = r
	}
	n := len(tasks)
	if n > 0 {
		ev.PassRate = ev.Passes / float64(n)
		ev.ClaudeFraction = float64(claude) / float64(n)
	}
	return ev
}

// Regret is the value gap between two evaluations (a - b, in pass-rate).
func Regret(a, b Eval) float64 { return b.PassRate - a.PassRate }

// FrontierPoint is one oracle operating point under a Claude budget.
type FrontierPoint struct {
	ClaudeBudget   int     `json:"claude_budget"`
	ClaudeFraction float64 `json:"claude_fraction"`
	Passes         float64 `json:"passes"`
	PassRate       float64 `json:"pass_rate"`
}

// Frontier sweeps the Claude budget 0..len(tasks): free passes come from the
// best non-Claude lane; tasks whose Claude gain is largest consume budget
// first. This is THE cost-quality curve WF@Q reads (Q1).
func Frontier(t *Table, tasks []string) []FrontierPoint {
	type gain struct{ free, withClaude float64 }
	gains := make([]gain, 0, len(tasks))
	for _, task := range tasks {
		g := gain{}
		for lane := range t.cells[task] {
			r, ok := t.Rate(task, lane)
			if !ok {
				continue
			}
			if lane == "claude" {
				if r > g.withClaude {
					g.withClaude = r
				}
			} else if r > g.free {
				g.free = r
			}
		}
		if g.withClaude < g.free {
			g.withClaude = g.free // claude never forced when worse
		}
		gains = append(gains, g)
	}
	deltas := make([]float64, len(gains))
	base := 0.0
	for i, g := range gains {
		base += g.free
		deltas[i] = g.withClaude - g.free
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(deltas)))
	n := len(tasks)
	pts := make([]FrontierPoint, 0, n+1)
	cum := base
	for b := 0; b <= n; b++ {
		if b > 0 {
			cum += deltas[b-1]
		}
		fp := FrontierPoint{ClaudeBudget: b, Passes: cum}
		if n > 0 {
			fp.ClaudeFraction = float64(b) / float64(n)
			fp.PassRate = cum / float64(n)
		}
		pts = append(pts, fp)
	}
	return pts
}

// RCI is the routing-collapse index: the share of tasks routed to the modal
// lane (Q7 — deterministic routers degenerate toward the strongest lane).
func RCI(assignment map[string]string) float64 {
	if len(assignment) == 0 {
		return 0
	}
	counts := map[string]int{}
	for _, lane := range assignment {
		counts[lane]++
	}
	max := 0
	for _, c := range counts {
		if c > max {
			max = c
		}
	}
	return float64(max) / float64(len(assignment))
}

// SignFlipP is the paired sign-flip permutation test (Q1): the two-sided
// p-value that the mean of deltas is 0. For n ≤ 20 it enumerates ALL 2^n sign
// patterns — an EXACT p, immune to seed luck (a Monte-Carlo estimate can dip
// under a threshold by chance and defeat small-n refusal guarantees). Larger n
// falls back to Monte-Carlo with the add-one estimator (hits+1)/(iters+1),
// which cannot underestimate to zero.
func SignFlipP(deltas []float64, iters int, seed int64) float64 {
	n := len(deltas)
	if n == 0 {
		return 1
	}
	obs := math.Abs(mean(deltas))
	if n <= 24 { // exact enumeration: ≤ 16,777,216 patterns — covers the B'2 heldout n=23, keeping split verdicts out of the Monte-Carlo seed-luck regime the V7 fix exists for
		total := 1 << uint(n)
		hits := 0
		for mask := 0; mask < total; mask++ {
			sum := 0.0
			for j, d := range deltas {
				if mask&(1<<uint(j)) != 0 {
					sum -= d
				} else {
					sum += d
				}
			}
			if math.Abs(sum/float64(n)) >= obs-1e-12 {
				hits++
			}
		}
		return float64(hits) / float64(total)
	}
	if iters < 1 {
		iters = 1
	}
	rng := rand.New(rand.NewSource(seed))
	hits := 0
	flipped := make([]float64, n)
	for i := 0; i < iters; i++ {
		for j, d := range deltas {
			if rng.Intn(2) == 0 {
				flipped[j] = -d
			} else {
				flipped[j] = d
			}
		}
		if math.Abs(mean(flipped)) >= obs-1e-12 {
			hits++
		}
	}
	return float64(hits+1) / float64(iters+1)
}

// BootstrapCI is the BCa bootstrap CI for the mean of xs (Q1's interval).
func BootstrapCI(xs []float64, conf float64, iters int, seed int64) (lo, hi float64) {
	n := len(xs)
	if n == 0 {
		return 0, 0
	}
	if iters < 2 {
		iters = 2 // a CI needs ≥2 resamples; guards a caller's zero/negative
	}
	obs := mean(xs)
	rng := rand.New(rand.NewSource(seed))
	boots := make([]float64, iters)
	sample := make([]float64, n)
	for i := 0; i < iters; i++ {
		for j := range sample {
			sample[j] = xs[rng.Intn(n)]
		}
		boots[i] = mean(sample)
	}
	sort.Float64s(boots)
	// bias correction
	below := 0
	for _, b := range boots {
		if b < obs {
			below++
		}
	}
	if below == 0 || below == iters { // degenerate (constant data): percentile CI
		return boots[0], boots[iters-1]
	}
	z0 := qnorm(float64(below) / float64(iters))
	// acceleration via jackknife
	jk := make([]float64, n)
	sum := obs * float64(n)
	for i := range xs {
		jk[i] = (sum - xs[i]) / float64(n-1)
	}
	jm := mean(jk)
	num, den := 0.0, 0.0
	for _, v := range jk {
		d := jm - v
		num += d * d * d
		den += d * d
	}
	a := 0.0
	if den > 0 {
		a = num / (6 * math.Pow(den, 1.5))
	}
	alpha := (1 - conf) / 2
	adj := func(q float64) float64 {
		z := qnorm(q)
		p := pnorm(z0 + (z0+z)/(1-a*(z0+z)))
		idx := int(p * float64(iters))
		if idx < 0 {
			idx = 0
		}
		if idx >= iters {
			idx = iters - 1
		}
		return boots[idx]
	}
	return adj(alpha), adj(1 - alpha)
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// pnorm is the standard normal CDF.
func pnorm(z float64) float64 { return 0.5 * math.Erfc(-z/math.Sqrt2) }

// qnorm is the standard normal quantile (Acklam's rational approximation).
func qnorm(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02,
		1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02,
		6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00,
		-2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00,
		3.754408661907416e+00}
	const plow = 0.02425
	if p < plow {
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	if p > 1-plow {
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
		(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}
