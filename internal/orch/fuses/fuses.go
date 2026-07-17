// Package fuses holds DATED capacity/pricing modifiers. The fact-refresh rule:
// these are config-with-expiry, never constants — three providers repriced in
// ~2 months. Expired fuses drop out silently; nothing downstream may hardcode
// a boosted capacity.
package fuses

import (
	"encoding/json"
	"os"
	"time"
)

type Fuse struct {
	Name      string    `json:"name"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note,omitempty"`
}

// Seed returns the four dated fuses from the 2026-07-05 fact refresh.
func Seed() []Fuse {
	return []Fuse{
		{Name: "fable-carveout", ExpiresAt: time.Date(2026, 7, 7, 7, 0, 0, 0, time.UTC), Note: "Fable 5 ≤50% weekly inclusion; after: NOT a runtime lane (R10)"},
		{Name: "weekly-boost-50pct", ExpiresAt: time.Date(2026, 7, 14, 1, 0, 0, 0, time.UTC), Note: "+50% Claude weekly; assume reversion after (~6PM PDT 7/13, secondary-sourced)"},
		{Name: "sonnet5-promo-price", ExpiresAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Note: "$2/$10 per MTok promo → $3/$15 (notional-cost display only)"},
		{Name: "glm-offpeak-promo", ExpiresAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Note: "GLM off-peak 1x promo → 2x; YEAR UNCONFIRMED, verify at renewal"},
	}
}

// Load reads fuses from path; a missing or corrupt file fails open to Seed().
func Load(path string) ([]Fuse, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Seed(), nil
	}
	var fs []Fuse
	if err := json.Unmarshal(b, &fs); err != nil {
		return Seed(), nil
	}
	return fs, nil
}

// Active returns the fuses that have not yet expired at now.
func Active(fs []Fuse, now time.Time) []Fuse {
	var out []Fuse
	for _, f := range fs {
		if now.Before(f.ExpiresAt) {
			out = append(out, f)
		}
	}
	return out
}
