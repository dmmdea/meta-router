package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func runExcluded(t *testing.T, lane string, force bool) (map[string]any, int) {
	t.Helper()
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	var out bytes.Buffer
	code, err := doRun(runOpts{Prompt: "hello", Lane: lane, Model: "claude-sonnet-5", Live: false, Force: force,
		Origin: "test", Desc: "exclusion behaviour", Exclude: []string{"claude"}}, &out)
	if err != nil {
		t.Fatalf("doRun: %v\n%s", err, out.String())
	}
	var got map[string]any
	if jerr := json.Unmarshal(out.Bytes(), &got); jerr != nil {
		t.Fatalf("output is not JSON (%v):\n%s", jerr, out.String())
	}
	return got, code
}

// Bare `run` defaults to the claude lane (run.go flag). With --exclude claude
// that must be a typed refusal — this is the check that keeps an armed
// session from spending Claude through the orchestrator (spec §2, R2).
func TestRunExcludedLaneRefused(t *testing.T) {
	got, code := runExcluded(t, "claude", false)
	if code != exitDeferred || got["lane_excluded"] != true || got["lane"] != "claude" {
		t.Fatalf("want lane_excluded exit %d, got code=%d out=%v", exitDeferred, code, got)
	}
	if !strings.Contains(got["reason"].(string), "--exclude") {
		t.Fatalf("reason must name the flag: %v", got["reason"])
	}
}

// An exclusion is the caller's own statement of intent, not a quota judgement
// --force may override: force-proof, like retirement.
func TestRunExcludedLaneForceProof(t *testing.T) {
	if got, code := runExcluded(t, "claude", true); code != exitDeferred || got["lane_excluded"] != true {
		t.Fatalf("--force overrode an exclusion: code=%d out=%v", code, got)
	}
}

// --lane auto --exclude claude resolves AWAY from claude (the exclusion reaches
// the internal recommendation), so the dry-run never mentions the claude lane.
func TestRunAutoHonoursExclude(t *testing.T) {
	got, _ := runExcluded(t, "auto", false)
	if got["lane_excluded"] == true {
		t.Fatalf("auto must resolve away from the excluded lane, not trip the refusal: %v", got)
	}
	if got["lane"] == "claude" {
		t.Fatalf("auto resolved to the excluded lane: %v", got)
	}
}
