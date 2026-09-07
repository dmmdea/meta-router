package main

import (
	"encoding/json"
	"testing"
)

// A quarantined row is a hole for the scorecard too: the marker is read
// through the same predicate the replay's resume set uses. Mutation: reading
// ran() without the marker makes this red.
func TestScorecardExcludesQuarantined(t *testing.T) {
	line := `{"task":"AC-10","class":"agentic-coding","lane":"codex","model":"gpt-6-astra","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false,"note":"verify-fail: git apply: corrupt patch at x","quarantined":"v0.40.4","quarantine_reason":"r"}`
	var r oracleRow
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatal(err)
	}
	if r.Quarantined != "v0.40.4" {
		t.Fatalf("quarantined marker not decoded: %+v", r)
	}
	if r.ran() {
		t.Fatalf("a quarantined row must not be evidence: %+v", r)
	}
	r.Quarantined = ""
	if !r.ran() {
		t.Fatalf("the same row unquarantined is evidence: %+v", r)
	}
}
