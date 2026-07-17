package oracle

import (
	"strings"
	"testing"
)

// V0 (master brief slice-0, the fake-green killer): the null agent (empty
// output) and trivial agents (plausible-looking constant slop) must score
// EXACTLY 0 on every verifier. A verifier that passes any of them is broken —
// it would grade garbage as success and poison every eval number downstream
// (oracle replay, scorecard, promotion gate). NO eval number is trusted until
// this audit passes.
func TestAuditCatchesFakeGreenVerifier(t *testing.T) {
	fakeGreen := Verifier{ID: "fake-green", Class: "test", Verify: func(string) bool { return true }}
	faults := Audit([]Verifier{fakeGreen})
	if len(faults) != 1+len(TrivialOutputs()) {
		t.Fatalf("an always-pass verifier must fault on null + every trivial output, got %d faults", len(faults))
	}
	for _, f := range faults {
		if f.VerifierID != "fake-green" {
			t.Fatalf("fault must name the broken verifier, got %+v", f)
		}
	}
}

func TestAuditPassesRealVerifier(t *testing.T) {
	real := Verifier{ID: "real", Class: "test", Verify: func(out string) bool {
		return strings.Contains(out, "func BinarySearch(") && strings.Contains(out, "return -1")
	}}
	if faults := Audit([]Verifier{real}); len(faults) != 0 {
		t.Fatalf("a content-demanding verifier must not fault, got %v", faults)
	}
}

// The registry-wide gate: EVERY registered verifier (V1's gold-set verifiers
// register here as they land) is audited on every test run — "auto-rerun on
// verifier change" is literally the test suite. Registry is empty until V1
// (operator-gated gold-set harvest); this test guards the mechanism NOW and the
// corpus LATER without modification.
func TestNullAndTrivialAgentsScoreZeroOnEveryRegisteredVerifier(t *testing.T) {
	if faults := AuditRegistry(); len(faults) != 0 {
		for _, f := range faults {
			t.Errorf("FAKE-GREEN verifier %q passes degenerate output %q", f.VerifierID, f.Output)
		}
	}
}

func TestRegisterPanicsOnDuplicateID(t *testing.T) {
	// Isolation: this test writes "dup-test" into the process-global registry;
	// clean it up so it never leaks into AuditRegistry() or a later test run.
	defer delete(registry, "dup-test")
	defer func() {
		if recover() == nil {
			t.Fatalf("duplicate verifier ID must panic (a silent shadow would skew the audit)")
		}
	}()
	Register(Verifier{ID: "dup-test", Class: "test", Verify: func(string) bool { return false }})
	Register(Verifier{ID: "dup-test", Class: "test", Verify: func(string) bool { return false }})
}
