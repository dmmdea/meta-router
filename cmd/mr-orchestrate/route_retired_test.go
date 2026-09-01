package main

import (
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/router"
)

// Default config retires glm: the router must MASK it (state "retired", never
// selected even when the rank table would pick it) and the copilot lane must
// appear in the quota-state map. Pinned because masked() is a denylist — a
// new state name silently reads as selectable unless listed.
func TestRouteDefaultMasksRetiredGLM(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	rnow := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	snap := []ledger.Bucket{
		{Lane: "claude", Window: "7d", UsedPct: 99, Source: "provider", ResetsAt: rnow.Add(3 * time.Hour)},
	}
	d := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.HardRepo, 0, rnow, spendDownReq{})
	if d.Lane == "glm" {
		t.Fatalf("retired glm was selected: %+v", d)
	}
	if st := d.QuotaState["glm"]; st != "retired" {
		t.Fatalf("glm quota state = %q, want retired", st)
	}
	if _, ok := d.QuotaState["copilot"]; !ok {
		t.Fatalf("copilot lane missing from quota state: %+v", d.QuotaState)
	}
	for _, m := range d.Masked {
		if m.Lane == "glm" {
			return // masked with a reason — the surfaced explanation exists
		}
	}
	t.Fatalf("glm must appear in Masked with its retirement reason: %+v", d.Masked)
}
