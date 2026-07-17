package glmlane

import (
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/fuses"
)

func promoActive() []fuses.Fuse {
	return []fuses.Fuse{{Name: "glm-offpeak-promo", ExpiresAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}}
}

// Multiplier prices one GLM prompt in quota units (fact refresh §1/§2):
// glm-4.7* always 1×; peak (14:00–18:00 UTC+8 == 06:00–10:00 UTC) 3× for the
// 5.2 tier; off-peak 1× while the glm-offpeak-promo fuse is active, else 2×.
func TestMultiplier(t *testing.T) {
	day := func(h, m int) time.Time { return time.Date(2026, 7, 6, h, m, 0, 0, time.UTC) }
	tests := []struct {
		name  string
		model string
		now   time.Time
		fzs   []fuses.Fuse
		want  int64
	}{
		{"glm-5.2 peak", "glm-5.2", day(7, 0), promoActive(), 3},
		{"glm-5.2 off-peak promo active", "glm-5.2", day(12, 0), promoActive(), 1},
		{"glm-5.2 off-peak promo expired", "glm-5.2", day(12, 0), nil, 2},
		{"glm-4.7 always 1x even at peak", "glm-4.7", day(7, 0), nil, 1},
		{"glm-4.7-turbo-class sibling stays 1x", "glm-4.7-air", day(7, 0), nil, 1},
		{"boundary 05:59 off-peak", "glm-5.2", day(5, 59), promoActive(), 1},
		{"boundary 06:00 peak", "glm-5.2", day(6, 0), promoActive(), 3},
		{"boundary 09:59 peak", "glm-5.2", day(9, 59), promoActive(), 3},
		{"boundary 10:00 off-peak", "glm-5.2", day(10, 0), promoActive(), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Multiplier(tc.model, tc.now, tc.fzs); got != tc.want {
				t.Fatalf("Multiplier(%q, %s) = %d, want %d", tc.model, tc.now, got, tc.want)
			}
		})
	}
}
