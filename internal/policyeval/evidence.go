package policyeval

import "strings"

// IsEvidence is THE definition of "this oracle cell was measured" — the one
// predicate shared by every reader and writer of the oracle: mr-goldreplay's
// resume set (loadDone), mr-scorecard's row admission (ran), and the B15
// canary (loadOracleModels). The list existed as three byte-identical copies
// and they drifted once already: loadDone admitted error rows the scorecard
// discarded, leaving cells simultaneously "already recorded" (resume never
// refilled them) and "not evidence" (scoring ignored them) — a hole that
// could never fill (review 2026-08-12). One definition ends the class.
//
// A cell is evidence iff the dispatch actually happened AND produced a
// verdict. Everything else is a HOLE (B6: unknown is counted, never imputed)
// — never measurement, always re-attemptable:
//   - not dispatched (spawn failed, admission refused before dispatch)
//   - "deferred" (admission closed / egress-denied; refilled when it reopens)
//   - "error", "spawn_error", "config_error", "parse_error", "api_error"
//   - "verify_error": the VERIFIER'S infrastructure failed (missing binary,
//     bad -repos path, a git failure inside goldverify) — as opposed to the
//     verifier running and failing the diff, which is a real measured
//     failure. Recording infrastructure faults as measured failures let a
//     missing goldverify binary score an entire replay as incompetent.
//   - any "exit-N" outcome
func IsEvidence(dispatched bool, outcomeClass string) bool {
	if !dispatched {
		return false
	}
	switch outcomeClass {
	case "deferred", "error", "spawn_error", "config_error", "parse_error", "api_error", "verify_error":
		return false
	}
	return !strings.HasPrefix(outcomeClass, "exit-")
}
