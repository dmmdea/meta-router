package main

import (
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/quotasig"
)

func TestBuildParityPairsAndUnpaired(t *testing.T) {
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	rows := []quotasig.TraceRow{
		{TS: base, Lane: "claude", Window: "7d", UsedPct: 39, Origin: "drop"},
		{TS: base.Add(time.Minute), Lane: "claude", Window: "7d", UsedPct: 41, Origin: "oauth_poll"},
		{TS: base.Add(-time.Hour), Lane: "claude", Window: "7d", UsedPct: 10, Origin: "oauth_poll"}, // older poll must lose to the newer one
		{TS: base, Lane: "codex", Window: "7d", UsedPct: 46, Origin: "wham_poll"},                   // no drop → unpaired
		{TS: base.Add(-48 * time.Hour), Lane: "claude", Window: "5h", UsedPct: 5, Origin: "drop"},   // outside lookback
	}
	rep := buildParity(rows, base.Add(-24*time.Hour))
	if len(rep.Pairs) != 1 {
		t.Fatalf("want 1 pair, got %+v", rep.Pairs)
	}
	p := rep.Pairs[0]
	if p.Delta != 2 || p.PollPct != 41 || p.DropPct != 39 {
		t.Fatalf("delta must use LATEST rows (41-39=2), got %+v", p)
	}
	if rep.MaxAbs != 2 {
		t.Fatalf("max_abs_delta want 2, got %v", rep.MaxAbs)
	}
	if len(rep.Unpaired) != 1 || rep.Unpaired[0] != "codex|7d" {
		t.Fatalf("codex|7d must be unpaired, got %+v", rep.Unpaired)
	}
}
