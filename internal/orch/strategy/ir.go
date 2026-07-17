// Package strategy is the slice-3 orchestration layer: a 3-list DAG IR
// (Step/IR), its validator, and the per-dispatch durable state store. The IR
// is CONFIG, never LLM-emitted (the prompted-Conductor planner is slice-4);
// the executor (Group C/D) drives each node through the inherited doRun core.
package strategy

import "fmt"

const MaxSteps = 5 // Conductor trains to 5 (design brief §2.1)

type Step struct {
	ID          int    `json:"id"`
	Instruction string `json:"instruction"`
	Class       string `json:"class,omitempty"`
	LaneHint    string `json:"lane_hint,omitempty"`
	ModelHint   string `json:"model_hint,omitempty"`
	EffortHint  string `json:"effort_hint,omitempty"`
	Role        string `json:"role,omitempty"`
	Deps        []int  `json:"deps"`
}

type IR struct {
	Goal  string `json:"goal"`
	Name  string `json:"strategy,omitempty"`
	Steps []Step `json:"steps"`
}

// Validate REJECTS (never truncates) a malformed IR: empty, over-cap, duplicate
// id, out-of-range/self/forward dep, same-lane parallel fan-out, and — S3R-2 —
// any non-terminal orphan sink.
//
// The dep-ordering rule (every dep references a step LISTED EARLIER by id) makes
// the graph provably acyclic without a separate cycle walk and is the executor's
// precondition. It also makes the LAST-listed step a sink by construction:
// nothing may depend on it. S3R-2 (terminal-sink honesty) closes the remaining
// gap — finalize reports the terminal step as the answer, so any OTHER step that
// no later step depends on (a "dangling branch") would have its result silently
// dropped. The rule is therefore: the last-listed step must be the UNIQUE sink;
// exactly one step may have zero dependents and it must be Steps[len-1]. This
// forces the terminal step to (transitively) collect every leaf branch, so no
// orphan result vanishes. A linear chain (one sink, the last) and the
// fan-out-judge shape (two leaves both feeding the terminal judge) both pass; an
// IR whose terminal depends on only one of two parallel leaves is rejected.
func Validate(ir IR) error {
	n := len(ir.Steps)
	if n == 0 {
		return fmt.Errorf("strategy IR has no steps")
	}
	if n > MaxSteps {
		return fmt.Errorf("strategy IR has %d steps, over the %d cap (rejected, never truncated)", n, MaxSteps)
	}
	pos := make(map[int]int, n) // id -> index
	for i, s := range ir.Steps {
		if _, dup := pos[s.ID]; dup {
			return fmt.Errorf("duplicate step id %d", s.ID)
		}
		pos[s.ID] = i
	}
	for i, s := range ir.Steps {
		for _, d := range s.Deps {
			j, ok := pos[d]
			if !ok {
				return fmt.Errorf("step %d depends on unknown id %d", s.ID, d)
			}
			if d == s.ID {
				return fmt.Errorf("step %d depends on itself", s.ID)
			}
			if j >= i {
				return fmt.Errorf("step %d depends on id %d which is not listed earlier (forward/cyclic dep)", s.ID, d)
			}
		}
	}
	// Same-lane fan-out: any two steps that share the SAME dep-set AND the same
	// explicit lane hint would run in parallel on one lane — rejected (§4.4: GLM
	// serialization + template-4's different-lanes rule). Empty lane hint = router
	// decides, so it is exempt (the executor serializes lane collisions at runtime).
	for i := 0; i < n; i++ {
		for k := i + 1; k < n; k++ {
			a, b := ir.Steps[i], ir.Steps[k]
			if a.LaneHint != "" && a.LaneHint == b.LaneHint && sameDeps(a.Deps, b.Deps) {
				return fmt.Errorf("steps %d and %d are a same-lane (%s) parallel fan-out — split lanes or serialize", a.ID, b.ID, a.LaneHint)
			}
		}
	}
	// S3R-2 terminal-sink honesty: a "sink" is a step with no dependents. The
	// last-listed step must be the UNIQUE sink; any earlier step that nothing
	// depends on is a dangling branch whose result finalize would silently drop.
	hasDependent := make([]bool, n)
	for _, s := range ir.Steps {
		for _, d := range s.Deps {
			hasDependent[pos[d]] = true // pos[d] is guaranteed valid by the dep check above
		}
	}
	for i := 0; i < n; i++ {
		if !hasDependent[i] && i != n-1 {
			return fmt.Errorf("step %d is a non-terminal orphan sink (nothing depends on it) — finalize would silently drop its result; the terminal step must transitively collect every branch (S3R-2)", ir.Steps[i].ID)
		}
	}
	return nil
}

func sameDeps(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[int]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}
