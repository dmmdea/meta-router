package codexlane

import "math"

// GPT-5.5 credit rates per 1M tokens (fact refresh: "credits verified exactly"):
// 125 fresh-input / 12.5 cached-input / 750 output. These are DATA about the
// current rate card, not constants of nature — the burn-anomaly detector
// (anomaly.go) exists precisely because vendors misfire (#28879, #31125).
const (
	creditsPerMFresh  = 125.0
	creditsPerMCached = 12.5
	creditsPerMOut    = 750.0
)

func Credits(u Usage) float64 {
	return float64(u.FreshInput())*creditsPerMFresh/1e6 +
		float64(u.CachedInput)*creditsPerMCached/1e6 +
		float64(u.Output)*creditsPerMOut/1e6
}

// CreditsMilli is the codex-lane ledger unit: millicredits scaled by the Plus
// degradation factor (10–20× cost-per-token since 2026-06-16, #28879 — config,
// default 15). The scaled number is what depletes the learned 5h window, so
// the router sees the lane as the tiny surgical resource it really is.
func CreditsMilli(u Usage, degradationFactor float64) int64 {
	if degradationFactor <= 0 {
		degradationFactor = 1
	}
	return int64(math.Round(Credits(u) * 1000 * degradationFactor))
}
