package main

import (
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
