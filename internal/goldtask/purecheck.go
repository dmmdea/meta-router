// The pure verifier engine: one regex-key check covers V-FACTS, V-FINDINGS,
// and sampled V-LABELS (a label pair is a line-scoped key), and doubles as the
// exec kinds' static PreGate — which is what lets the V0 null/trivial audit
// cover ALL 56 tasks inside the normal test suite, execution kinds included.
package goldtask

import (
	"fmt"
	"regexp"

	"github.com/dmmdea/meta-router/internal/oracle"
)

// pureKeys returns the key set PureCheck evaluates for a spec: Keys for the
// pure kind, PreGate (all-required) for exec kinds.
func pureKeys(spec VerifySpec) ([]Key, int) {
	if spec.Kind == "pure" {
		min := spec.MinKeys
		if min <= 0 || min > len(spec.Keys) {
			min = len(spec.Keys)
		}
		return spec.Keys, min
	}
	return spec.PreGate, len(spec.PreGate)
}

// PureCheck evaluates a spec's pure face against a candidate output.
// Empty output never passes; a Forbidden match always fails; otherwise pass
// iff at least the effective minimum of keys match. Patterns are RE2,
// case-insensitive, with . matching newlines ((?is)).
func PureCheck(spec VerifySpec, output string) bool {
	if output == "" {
		return false
	}
	keys, min := pureKeys(spec)
	if len(keys) == 0 {
		return false // a verifier with no checkable face never passes (V0 stance)
	}
	for _, f := range spec.Forbidden {
		re, err := regexp.Compile("(?is)" + f.Pattern)
		if err != nil || re.MatchString(output) {
			return false // uncompilable forbidden = fail closed
		}
	}
	matched := 0
	for _, k := range keys {
		re, err := regexp.Compile("(?is)" + k.Pattern)
		if err != nil {
			return false // uncompilable key = fail closed (CompileErr surfaces it)
		}
		if re.MatchString(output) {
			matched++
		}
	}
	return matched >= min
}

// CompileErr reports the first non-compiling pattern in the spec (keys,
// forbidden, and pre-gate). The gold-set test runs it over every record so a
// typo'd pattern is a test failure, never a silent always-fail verifier.
func CompileErr(spec VerifySpec) error {
	groups := [][]Key{spec.Keys, spec.Forbidden, spec.PreGate}
	names := []string{"key", "forbidden", "pre_gate"}
	for gi, g := range groups {
		for _, k := range g {
			if _, err := regexp.Compile("(?is)" + k.Pattern); err != nil {
				return fmt.Errorf("%s %q: %v", names[gi], k.Name, err)
			}
		}
	}
	return nil
}

// RegisterAll adapts every task's pure face into an oracle.Verifier slice for
// the V0 audit. It does NOT touch oracle's process-global registry — the audit
// runs on the explicit slice, keeping tests hermetic.
func RegisterAll(ts []Task) []oracle.Verifier {
	out := make([]oracle.Verifier, 0, len(ts))
	for _, t := range ts {
		spec := t.Verify // capture per-iteration copy for the closure
		out = append(out, oracle.Verifier{
			ID:     t.ID,
			Class:  t.Class,
			Verify: func(output string) bool { return PureCheck(spec, output) },
		})
	}
	return out
}
