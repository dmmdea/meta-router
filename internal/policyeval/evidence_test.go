package policyeval

import "testing"

// The predicate the whole eval stack shares. Every reader (scorecard, B15) and
// the writer's resume set must agree on this table, or a cell can be
// simultaneously "recorded" and "unmeasured" — the unfillable hole the shared
// definition exists to end.
func TestIsEvidence(t *testing.T) {
	cases := []struct {
		name       string
		dispatched bool
		class      string
		want       bool
	}{
		{"ok is evidence", true, "ok", true},
		{"dispatched-not-ok is evidence (ran, returned a verdict)", true, "dispatched-not-ok", true},
		{"never dispatched is a hole even when marked ok", false, "ok", false},
		{"deferred is a hole", false, "deferred", false},
		{"deferred stays a hole even if dispatched is set", true, "deferred", false},
		{"error is a hole", false, "error", false},
		{"spawn_error is a hole", true, "spawn_error", false},
		{"config_error is a hole", true, "config_error", false},
		{"parse_error is a hole", true, "parse_error", false},
		{"api_error is a hole", true, "api_error", false},
		{"verify_error is a hole: the verifier broke, the task was not measured", true, "verify_error", false},
		{"exit-4 is a hole", true, "exit-4", false},
		{"exit-1 is a hole", true, "exit-1", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsEvidence(c.dispatched, c.class, ""); got != c.want {
				t.Fatalf("IsEvidence(%v, %q, \"\") = %v, want %v", c.dispatched, c.class, got, c.want)
			}
		})
	}
}

// A quarantined row is a hole whatever its class says: the marker names the
// instrument fix that proved the verdict was the harness's (v0.40.4).
func TestIsEvidenceQuarantined(t *testing.T) {
	if IsEvidence(true, "ok", "v0.40.4") {
		t.Fatal("quarantined ok row must not be evidence")
	}
	if IsEvidence(true, "dispatched-not-ok", "v0.40.4") {
		t.Fatal("quarantined dispatched-not-ok row must not be evidence")
	}
	if !IsEvidence(true, "ok", "") {
		t.Fatal("an unquarantined ok row is evidence")
	}
}
