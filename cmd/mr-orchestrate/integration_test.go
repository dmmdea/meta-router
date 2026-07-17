package main

import (
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/router"
)

// TestRunRecordsRecommendationAndDeviation: given a Decision recommending glm
// and an explicit --lane claude, resolveLane records Deviated:true with reason
// operator_override (R11 is never blocked, only recorded).
func TestRunRecordsRecommendationAndDeviation(t *testing.T) {
	rec := router.Decision{Lane: "glm", Model: "glm-5.2", Effort: "high", Rule: "workhorse-coding#1:glm", Class: router.Workhorse}
	lane, model, effort, rf := resolveLane(rec, "claude", "claude-opus-4-8", "high", "")
	if lane != "claude" || model != "claude-opus-4-8" || effort != "high" {
		t.Fatalf("explicit lane/model/effort must win: %s/%s/%s", lane, model, effort)
	}
	if !rf.Deviated || rf.DeviationReason != "operator_override" {
		t.Fatalf("differing lane must record operator_override deviation: %+v", rf)
	}
	if rf.RecLane != "glm" || rf.RecModel != "glm-5.2" || rf.RecRule != "workhorse-coding#1:glm" {
		t.Fatalf("rec fields must be stamped from the Decision: %+v", rf)
	}
}

// An explicit --deviation reason overrides the default operator_override tag.
func TestRunDeviationReasonCustom(t *testing.T) {
	rec := router.Decision{Lane: "glm", Model: "glm-5.2", Rule: "r"}
	_, _, _, rf := resolveLane(rec, "claude", "claude-opus-4-8", "", "trying opus for quality")
	if !rf.Deviated || rf.DeviationReason != "trying opus for quality" {
		t.Fatalf("custom deviation reason must win: %+v", rf)
	}
}

// TestLaneAutoAdoptsRecommendation: under --lane auto the resolved lane/model/
// effort come from the recommendation; an explicit --model still beats the
// adopted model (R11). Deviated:false by construction when nothing overrides.
func TestLaneAutoAdoptsRecommendation(t *testing.T) {
	rec := router.Decision{Lane: "glm", Model: "glm-5.2", Effort: "high", Rule: "workhorse-coding#1:glm", Class: router.Workhorse}
	lane, model, effort, rf := resolveLane(rec, "auto", "", "", "")
	if lane != "glm" || model != "glm-5.2" || effort != "high" {
		t.Fatalf("auto must adopt the rec wholesale: %s/%s/%s", lane, model, effort)
	}
	if rf.Deviated {
		t.Fatalf("clean adoption is not a deviation: %+v", rf)
	}
	// Explicit --model beats the adopted model but stays lane=glm; adopting the
	// rec lane while pinning a different model is NOT an operator_override of the
	// lane recommendation, so Deviated stays false (the lane matches the rec).
	lane2, model2, _, rf2 := resolveLane(rec, "auto", "glm-4.7", "", "")
	if lane2 != "glm" || model2 != "glm-4.7" {
		t.Fatalf("explicit --model must beat the adopted model: %s/%s", lane2, model2)
	}
	if rf2.Deviated {
		t.Fatalf("same-lane model pin under auto is not a lane deviation: %+v", rf2)
	}
}

// S2R-4(b): under --lane auto, a recommendation resolving to lane local CANNOT
// dispatch (local goes through the local-offload MCP directly until slice 3).
// Auto-falls to the first DISPATCHABLE alternative (next non-local candidate)
// and stamps Deviated:true, DeviationReason "local-handoff".
func TestLaneAutoLocalFallsToAlternative(t *testing.T) {
	rec := router.Decision{
		Lane: "local", Model: "gemma4-cascade", Effort: "", Rule: "mechanical-text#1:local", Class: router.MechanicalText,
		Alternatives: []router.Entry{
			{Lane: "glm", Model: "glm-4.7", Effort: "low", Rank: 2},
		},
	}
	lane, model, effort, rf := resolveLane(rec, "auto", "", "", "")
	if lane != "glm" || model != "glm-4.7" || effort != "low" {
		t.Fatalf("auto→local must fall to the first dispatchable alternative: %s/%s/%s", lane, model, effort)
	}
	if !rf.Deviated || rf.DeviationReason != "local-handoff" {
		t.Fatalf("local auto-fall must stamp local-handoff (S2R-4b): %+v", rf)
	}
	// The rec fields still reflect the ORIGINAL recommendation (local) — the
	// receipt records what the oracle said, then the deviation away from it.
	if rf.RecLane != "local" {
		t.Fatalf("rec_lane must stay the original recommendation: %+v", rf)
	}
}

// S2R-4(b) edge: auto→local with NO dispatchable alternative. There is nothing
// to fall to; resolveLane surfaces the empty lane so the caller relegates
// rather than silently dispatching local.
func TestLaneAutoLocalNoAlternativeRelegates(t *testing.T) {
	rec := router.Decision{Lane: "local", Model: "qwythos", Rule: "verify-gate#1:local", Class: router.VerifyGate}
	lane, _, _, rf := resolveLane(rec, "auto", "", "", "")
	if lane != "" {
		t.Fatalf("auto→local with no alternative must yield empty lane (relegate): %s", lane)
	}
	if !rf.Deviated || rf.DeviationReason != "local-handoff" {
		t.Fatalf("still a local-handoff deviation: %+v", rf)
	}
}

// TestRecFailureFailsOpen: a nil/empty Decision (broken oracle) yields empty
// rec fields and the operator's explicit lane, dispatch proceeds — a broken
// oracle must NEVER block dispatch.
func TestRecFailureFailsOpen(t *testing.T) {
	lane, model, effort, rf := resolveLane(router.Decision{}, "claude", "claude-opus-4-8", "high", "")
	if lane != "claude" || model != "claude-opus-4-8" || effort != "high" {
		t.Fatalf("empty rec must not disturb explicit flags: %s/%s/%s", lane, model, effort)
	}
	if rf.RecLane != "" || rf.Deviated {
		t.Fatalf("empty rec → no rec fields, no deviation: %+v", rf)
	}
}

// --lane auto with an empty recommendation (broken oracle) must fail open: no
// crash, empty resolved lane so the caller falls back to its own default rather
// than dispatching a phantom.
func TestLaneAutoWithEmptyRecFailsOpen(t *testing.T) {
	lane, _, _, rf := resolveLane(router.Decision{}, "auto", "", "", "")
	if lane != "" {
		t.Fatalf("auto with no rec must yield empty lane, not a phantom: %s", lane)
	}
	if rf.Deviated {
		t.Fatalf("no rec, no deviation: %+v", rf)
	}
}
