package strategy

import (
	"fmt"
	"sort"
)

// Templates are pure DATA over the IR: each named strategy is a function
// goal+class → IR that emits the 3-list DAG. NONE is an auto-selected default —
// they are all --strategy-invokable seams only; the equal-budget/AUGRC promotion
// gate is slice-4. Every emitted IR MUST pass Validate (dep-ordering, ≤5 steps,
// unique terminal sink, no same-lane fan-out).

// hardClasses are the task classes where a dedicated planning/thinking step earns
// its keep. On easy classes the thinker is GATED OUT (design brief §3,
// verdict-validated: dropping it IMPROVES quality on easy tasks). Names mirror
// router.Class values (kept as strings to avoid importing router into this pure
// package). Anything not listed here is treated as easy → 2-node worker→verifier.
var hardClasses = map[string]bool{
	"hard-repo":               true,
	"deep-reasoning":          true,
	"formal-math":             true,
	"competition-math":        true,
	"hard-case-reclaim":       true,
	"many-tool-orchestration": true,
	"terminal-bounded":        true,
}

// Solo is the 1-node DAG — the slice-2 default fast path expressed as a strategy.
// deps=[], the goal as the instruction, class passed through. It MUST produce a
// byte-identical dispatch to a plain `run` (Group G regression pin).
func Solo(goal, class string) IR {
	return IR{Goal: goal, Name: "solo", Steps: []Step{
		{ID: 0, Instruction: goal, Class: class, Role: "worker", Deps: []int{}},
	}}
}

// PlanWorkVerify is plan→work→verify. The thinker is present ONLY on hard classes
// (dropping it on easy classes improves quality); the verifier ALWAYS routes to
// the offload-harness triage door (LaneHint:"local", Class:"verify-gate" per
// S3R-1) — a DIFFERENT lane than the worker (tier-2 rule: judgment never the sole
// gate; the tier-1 receipt gate still governs).
//   - hard class: [thinker(deps=[]), worker(deps=[0]), verifier(deps=[1])]
//   - easy class: [worker(deps=[]), verifier(deps=[0])]  (thinker gated out)
func PlanWorkVerify(goal, class string) IR {
	worker := Step{Instruction: "Do the task: " + goal, Class: class, Role: "worker", LaneHint: ""}
	verifier := Step{Instruction: "Verify the preceding work satisfies: " + goal, Class: "verify-gate", Role: "verifier", LaneHint: "local"}
	if !hardClasses[class] {
		// gated: worker → verifier (thinker removed)
		worker.ID, worker.Deps = 0, []int{}
		verifier.ID, verifier.Deps = 1, []int{0}
		return IR{Goal: goal, Name: "plan-work-verify", Steps: []Step{worker, verifier}}
	}
	thinker := Step{ID: 0, Instruction: "Plan an approach for: " + goal, Class: class, Role: "thinker", Deps: []int{}}
	worker.ID, worker.Deps = 1, []int{0}
	verifier.ID, verifier.Deps = 2, []int{1}
	return IR{Goal: goal, Name: "plan-work-verify", Steps: []Step{thinker, worker, verifier}}
}

// Cascade (DATA seam, NOT a default): worker → verify. Escalate-on-fail is the
// executor's Group-C max_depth=1 re-lane (local→GLM→Codex→Claude Pareto order),
// not a static node — the template only sets up worker+verify and relies on the
// re-lane. The worker starts on the free local lane; the verifier on a different
// lane so the two never collide.
func Cascade(goal, class string) IR {
	return IR{Goal: goal, Name: "cascade", Steps: []Step{
		{ID: 0, Instruction: goal, Class: class, Role: "worker", LaneHint: "local", Deps: []int{}},
		{ID: 1, Instruction: "Verify: " + goal, Class: "verify-gate", Role: "verifier", LaneHint: "glm", Deps: []int{0}},
	}}
}

// FanOutJudge (DATA seam): N=2 parallel workers on DIFFERENT explicit lanes (so
// they pass the same-lane-fan-out Validate rule + the S3R-3a serialization) + a
// judge that is the terminal sink depending on both.
func FanOutJudge(goal, class string) IR {
	return IR{Goal: goal, Name: "fan-out-judge", Steps: []Step{
		{ID: 0, Instruction: goal, Class: class, Role: "worker", LaneHint: "claude", Deps: []int{}},
		{ID: 1, Instruction: goal, Class: class, Role: "worker", LaneHint: "codex", Deps: []int{}},
		{ID: 2, Instruction: "Judge the two candidate answers for: " + goal, Class: "verify-gate", Role: "verifier", LaneHint: "glm", Deps: []int{0, 1}},
	}}
}

// SingleCritique (DATA seam): worker → one critique step (terminal). NOT a debate.
func SingleCritique(goal, class string) IR {
	return IR{Goal: goal, Name: "single-critique", Steps: []Step{
		{ID: 0, Instruction: goal, Class: class, Role: "worker", Deps: []int{}},
		{ID: 1, Instruction: "Critique the preceding answer for: " + goal, Class: "verify-gate", Role: "verifier", LaneHint: "local", Deps: []int{0}},
	}}
}

// registry maps a template name → its pure builder. Single source of truth for
// Expand and TemplateNames.
var registry = map[string]func(goal, class string) IR{
	"solo":             Solo,
	"plan-work-verify": PlanWorkVerify,
	"cascade":          Cascade,
	"fan-out-judge":    FanOutJudge,
	"single-critique":  SingleCritique,
}

// TemplateNames returns the sorted list of invokable template names. There is NO
// auto-selected default (the promotion gate is slice-4); callers pass a name.
func TemplateNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Expand maps a template name → IR. An unknown name errors with the valid list.
func Expand(name, goal, class string) (IR, error) {
	build, ok := registry[name]
	if !ok {
		return IR{}, fmt.Errorf("unknown strategy template %q (have: %v)", name, TemplateNames())
	}
	return build(goal, class), nil
}
