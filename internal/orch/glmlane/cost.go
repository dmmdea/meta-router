package glmlane

import (
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/fuses"
)

// Multiplier prices one GLM prompt in quota units (fact refresh §1/§2):
//
//	glm-4.7*                                     → always 1×
//	peak (14:00–18:00 UTC+8 == 06:00–10:00 UTC)  → 3× for glm-5.2/turbo
//	off-peak                                     → 1× while the glm-offpeak-promo
//	                                               fuse is active, else 2×
//
// The promo is a DATED fuse (config-with-expiry, never a constant): when it
// expires the off-peak price reverts to 2× with zero code change.
func Multiplier(model string, now time.Time, fzs []fuses.Fuse) int64 {
	if strings.HasPrefix(model, CheapModel) { // glm-4.7 and its variants
		return 1
	}
	if h := now.UTC().Hour(); h >= 6 && h < 10 { // peak window, UTC
		return 3
	}
	for _, f := range fuses.Active(fzs, now) {
		if f.Name == "glm-offpeak-promo" {
			return 1
		}
	}
	return 2
}
