package fuses

import (
	"testing"
	"time"
)

func TestActiveFiltersExpired(t *testing.T) {
	fs := []Fuse{
		{Name: "fable-carveout", ExpiresAt: time.Date(2026, 7, 7, 0, 0, 0, 0, time.UTC)},
		{Name: "weekly-boost-50pct", ExpiresAt: time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC)},
	}
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	act := Active(fs, now)
	if len(act) != 1 || act[0].Name != "weekly-boost-50pct" {
		t.Fatalf("want only weekly-boost active at %v, got %+v", now, act)
	}
}

func TestSeedContainsAllFourFuses(t *testing.T) {
	names := map[string]bool{}
	for _, f := range Seed() {
		names[f.Name] = true
	}
	for _, want := range []string{"fable-carveout", "weekly-boost-50pct", "sonnet5-promo-price", "glm-offpeak-promo"} {
		if !names[want] {
			t.Fatalf("Seed() missing fuse %q", want)
		}
	}
}
