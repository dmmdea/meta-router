package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The GLM retirement gate (subscription cancelled 2026-09-01). Behavioural,
// like the egress tests: assert on the dispatcher's OUTPUT and exit code.

func glmRetiredRun(t *testing.T, cfg map[string]any, force bool) (map[string]any, int, string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)
	if cfg != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "config.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	code, err := runGLMLane(&out, "hello", "glm-5.2", "", "", 30, nil,
		false /*live*/, force, "test", "retirement behaviour", recFields{}, strategyFields{})
	if err != nil {
		t.Fatalf("runGLMLane: %v\n%s", err, out.String())
	}
	var got map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("dispatcher output is not JSON (%v):\n%s", jerr, out.String())
	}
	return got, code, out.String()
}

// Default config (no file at all) → the lane is retired: typed refusal,
// deferred exit, and the reason names the successor lane and the re-enable
// switch — an operator hitting this at 2am must not need the source to know
// what happened or how to undo it.
func TestGLMLaneRetiredByDefault(t *testing.T) {
	got, code, raw := glmRetiredRun(t, nil, false)
	if code != exitDeferred {
		t.Fatalf("exit = %d, want exitDeferred (%d): %s", code, exitDeferred, raw)
	}
	if r, _ := got["lane_retired"].(bool); !r {
		t.Fatalf("output must carry lane_retired: %s", raw)
	}
	reason, _ := got["reason"].(string)
	for _, must := range []string{"copilot", "glm_retired"} {
		if !strings.Contains(reason, must) {
			t.Fatalf("reason must name %q: %s", must, reason)
		}
	}
}

// --force must NOT bypass retirement: a cancelled plan is not a quota
// judgement (the same force-proof class as the R10 billing hard-stop).
func TestGLMRetirementIsForceProof(t *testing.T) {
	got, code, raw := glmRetiredRun(t, nil, true)
	if code != exitDeferred {
		t.Fatalf("--force bypassed retirement: exit %d, %s", code, raw)
	}
	if r, _ := got["lane_retired"].(bool); !r {
		t.Fatalf("--force bypassed retirement: %s", raw)
	}
}

// Explicit glm_retired:false re-enables the lane wholesale — the dispatch
// proceeds past the gate to the normal path (dry-run reaches the egress
// decision, whose output shape has effective_cwd/egress fields, not
// lane_retired).
func TestGLMRetirementExplicitFalseReenables(t *testing.T) {
	got, _, raw := glmRetiredRun(t, map[string]any{"glm_retired": false}, false)
	if r, _ := got["lane_retired"].(bool); r {
		t.Fatalf("glm_retired:false must re-enable the lane: %s", raw)
	}
}
