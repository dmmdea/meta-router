package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
}
