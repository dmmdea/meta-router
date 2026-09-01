package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/router"
)

var xnow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// delegate-mode (spec §3): --exclude claude masks the lane for THIS consult —
// never selected, state "excluded", surfaced in Masked with a reason.
func TestRouteExcludeMasksClaude(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, router.HardRepo, 0, xnow, spendDownReq{Exclude: []string{"claude"}})
	if d.Lane == "claude" {
		t.Fatalf("excluded claude was selected: %+v", d)
	}
	if st := d.QuotaState["claude"]; st != "excluded" {
		t.Fatalf("claude quota state = %q, want excluded", st)
	}
	for _, m := range d.Masked {
		if m.Lane == "claude" {
			return
		}
	}
	t.Fatalf("claude must appear in Masked with the exclusion reason: %+v", d.Masked)
}

// The exclusion is per-invocation: the next consult without it must not see
// "excluded" — nothing was persisted (spec §3: never config).
func TestRouteExcludeIsPerConsult(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	_ = buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, router.HardRepo, 0, xnow, spendDownReq{Exclude: []string{"claude"}})
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, router.HardRepo, 0, xnow, spendDownReq{})
	if st := d.QuotaState["claude"]; st == "excluded" {
		t.Fatalf("exclusion leaked into a later consult: %+v", d.QuotaState)
	}
}

// Excluding a lane the states map does not carry is a no-op, not a panic
// (validation of names happens at the flag layer, Task 2).
func TestRouteExcludeUnknownLaneIsNoop(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), nil, router.HardRepo, 0, xnow, spendDownReq{Exclude: []string{"nope"}})
	if d.Lane == "" {
		t.Fatalf("unknown exclusion must not relegate: %+v", d)
	}
}

// toolResultText mirrors the inline res.Content[0].Text extraction used
// throughout mcp_test.go.
func toolResultText(r toolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

func TestMCPRouteExclude(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	// NOTE: brief specified class inferred from --task alone, but with the
	// default config (glm_retired: true — GLM subscription cancelled
	// 2026-09-01) the heuristic "hard-repo" class only has claude+glm
	// ranked, so excluding claude masks everything and the consult defers
	// instead of falling back — see task-2-report.md "Deviations". Pinning
	// class explicitly to "many-tool-orchestration" (claude rank1, codex
	// rank2) keeps the test's actual intent: excluding claude must still
	// fall back to an admitted lane and show "claude": "excluded".
	r := toolRoute(json.RawMessage(`{"task":"refactor the parser across 12 files","class":"many-tool-orchestration","exclude":["claude"]}`))
	if r.IsError {
		t.Fatalf("unexpected error: %+v", r)
	}
	if txt := toolResultText(r); !strings.Contains(txt, `"claude": "excluded"`) {
		t.Fatalf("route JSON lacks the excluded state:\n%s", txt)
	}
	bad := toolRoute(json.RawMessage(`{"task":"x","exclude":["claud"]}`))
	if !bad.IsError || !strings.Contains(toolResultText(bad), "unknown lane") {
		t.Fatalf("typo must be a typed error: %+v", bad)
	}

	// MCP-level isolation: the exclusion above must not leak into a fresh
	// consult for the same task/class with no exclude.
	clean := toolRoute(json.RawMessage(`{"task":"refactor the parser across 12 files","class":"many-tool-orchestration"}`))
	if clean.IsError {
		t.Fatalf("unexpected error on the follow-up consult: %+v", clean)
	}
	if txt := toolResultText(clean); strings.Contains(txt, `"claude": "excluded"`) {
		t.Fatalf("exclusion leaked into a later MCP consult:\n%s", txt)
	}
}
