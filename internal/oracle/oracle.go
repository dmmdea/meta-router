// Package oracle is the slice-4 eval substrate: the task/lane oracle's verifier
// registry and the V0 null/trivial-agent audit gate (master brief slice-0, the
// fake-green killer). V1's gold-set verifiers Register here as the D6 harvest
// lands; V2's all-lanes replay consumes Registry(). The audit's contract: a
// verifier that passes DEGENERATE output (empty, or plausible-looking constant
// slop) is broken and poisons every downstream eval number — so the audit runs
// as a plain test on every gate ("auto-rerun on verifier change" = the suite).
package oracle

import (
	"fmt"
	"sort"
)

// Verifier is one gold-set task's programmatic tier-1 gate: execution-receipt
// style (does the output contain the demanded, checkable content), never an
// LLM judgment (the tier-2 critic is measured separately, verifierpilot).
type Verifier struct {
	ID     string
	Class  string
	Verify func(output string) bool
}

var registry = map[string]Verifier{}

// Register adds a verifier; duplicate IDs panic (a silent shadow would let a
// broken verifier hide behind a working one in the audit).
func Register(v Verifier) {
	if _, dup := registry[v.ID]; dup {
		panic(fmt.Sprintf("oracle: duplicate verifier ID %q", v.ID))
	}
	registry[v.ID] = v
}

// Registry returns the registered verifiers sorted by ID (deterministic audits).
func Registry() []Verifier {
	out := make([]Verifier, 0, len(registry))
	for _, v := range registry {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// NullOutput is the null agent: does nothing, returns nothing.
func NullOutput() string { return "" }

// TrivialOutputs are the trivial agent's plausible-looking constant slop —
// outputs a verifier must NEVER accept. Extend freely; every addition makes the
// audit stricter for all present and future verifiers.
func TrivialOutputs() []string {
	return []string{
		"Done.",
		"The task has been completed successfully.",
		"I have carefully analyzed the task and everything looks correct. All tests pass.",
		"```go\n// TODO: implement\n```",
		"LGTM — no issues found.",
	}
}

// Fault is one audit failure: a verifier that accepted degenerate output.
type Fault struct {
	VerifierID string
	Output     string
}

// Audit runs the V0 gate over the given verifiers: null + every trivial output
// must all FAIL every verifier. Exported with an explicit slice so tests can
// audit fixtures without touching the process-global registry.
func Audit(vs []Verifier) []Fault {
	outs := append([]string{NullOutput()}, TrivialOutputs()...)
	var faults []Fault
	for _, v := range vs {
		for _, o := range outs {
			if v.Verify(o) {
				faults = append(faults, Fault{VerifierID: v.ID, Output: o})
			}
		}
	}
	return faults
}

// AuditRegistry audits every registered verifier — the gate the test suite runs.
func AuditRegistry() []Fault { return Audit(Registry()) }
