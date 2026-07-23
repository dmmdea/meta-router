package policyzoo

import (
	"fmt"
	"sort"

	"github.com/dmmdea/meta-router/internal/policyeval"
)

// Task is a candidate's full view of one gold task: observables only.
// BaseLane is the shipped router's neutral-state pick (the production
// baseline every floor candidate modifies).
type Task struct {
	ID, Class, Prompt, BaseLane string
}

// Candidate is one configured policy in the zoo.
type Candidate struct {
	Family string
	Desc   string
	Route  func(t Task) string
}

// laneTier mirrors policyeval's cost order; floors bump UP this ladder only —
// a floor is a quality complement, never a demotion.
var laneTier = map[string]int{"local": 0, "glm": 1, "codex": 2, "claude": 3}

func floored(base, floor string) string {
	if laneTier[base] < laneTier[floor] {
		return floor
	}
	return base
}

// ComplexityFloorGrid: minimum-tier floor on structural prompt complexity —
// the same failure family B'1's mechanical-text gold-stakes floor fixed,
// generalized to a finer-than-class signal (OmniRoute complexityRouter
// lineage). Small grid (6 configs) to bound the winner's-curse surface the
// -split gate then tests anyway.
func ComplexityFloorGrid() []Candidate {
	var out []Candidate
	type cfg struct {
		T     int
		floor string
	}
	for _, c := range []cfg{{2, "codex"}, {3, "codex"}, {4, "codex"}, {3, "claude"}, {4, "claude"}, {6, "claude"}} {
		c := c
		out = append(out, Candidate{
			Family: "complexity-floor",
			Desc:   fmt.Sprintf("T=%d,floor=%s", c.T, c.floor),
			Route: func(t Task) string {
				if Extract(t.Prompt).Score() >= c.T {
					return floored(t.BaseLane, c.floor)
				}
				return t.BaseLane
			},
		})
	}
	return out
}

// CtxFloorGrid: context-size floor — long prompts get a minimum codex tier.
func CtxFloorGrid() []Candidate {
	var out []Candidate
	for _, L := range []int{1500, 3000, 6000} {
		L := L
		out = append(out, Candidate{
			Family: "ctx-floor",
			Desc:   fmt.Sprintf("L=%d,floor=codex", L),
			Route: func(t Task) string {
				if len(t.Prompt) >= L {
					return floored(t.BaseLane, "codex")
				}
				return t.BaseLane
			},
		})
	}
	return out
}

// AllFamilies is the zoo roster the scorecard iterates.
func AllFamilies() map[string][]Candidate {
	return map[string][]Candidate{
		"complexity-floor": ComplexityFloorGrid(),
		"ctx-floor":        CtxFloorGrid(),
	}
}

// PolicyOf adapts a Candidate to policyeval.Policy over known tasks.
func PolicyOf(c Candidate, byID map[string]Task) policyeval.Policy {
	return func(id string) string {
		t, ok := byID[id]
		if !ok {
			return "" // unknown task: abstain, never guess
		}
		return c.Route(t)
	}
}

// assignCost is the summed lane-tier cost of an assignment — the tie-break
// axis after quality (claude costs most; claude-fraction alone can't order
// glm vs codex).
func assignCost(assignment map[string]string) int {
	cost := 0
	for _, lane := range assignment {
		cost += laneTier[lane]
	}
	return cost
}

// SelectBest scores each candidate on the TUNING tasks by exact table lookup
// (policyeval.Evaluate's task-mean objective) and returns the winner. Total
// order: higher PassRate, then LOWER summed lane cost, then lexical Desc (the
// pre-sort) — map iteration can never decide the pick. This is the only place
// the zoo touches the oracle table before the heldout verdict.
func SelectBest(cands []Candidate, tb *policyeval.Table, tuning []Task) (Candidate, policyeval.Eval) {
	byID := map[string]Task{}
	ids := make([]string, 0, len(tuning))
	for _, t := range tuning {
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	sorted := append([]Candidate(nil), cands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Desc < sorted[j].Desc })
	var best Candidate
	var bestEv policyeval.Eval
	have := false
	for _, c := range sorted {
		ev := policyeval.Evaluate(tb, ids, PolicyOf(c, byID))
		if !have || ev.PassRate > bestEv.PassRate ||
			(ev.PassRate == bestEv.PassRate && assignCost(ev.Assignment) < assignCost(bestEv.Assignment)) {
			best, bestEv, have = c, ev, true
		}
	}
	return best, bestEv
}
