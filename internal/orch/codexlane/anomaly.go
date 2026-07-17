package codexlane

import "fmt"

// BurnAnomaly is the fact-refresh gap #6 detector: expected-vs-observed
// drawdown. With no provider usage surface on the exec stream, "observed" is
// the provider's own veto — a rate-limit arriving while the ledger predicted
// comfortable headroom IS the anomaly (vendor misfire class — resolved 6/29,
// recurred 7/4 #31125). S2R-3 corollary: the latch keys off REAL rate-limits
// at predicted headroom, never off modeled exhaustion.
//
// anomalous ⇔ gotRateLimited && 0 <= predictedUsedPct < 70. The caller latches
// codex-alert.json; the latch never auto-mutates capacity (the operator/
// fitting adjusts) — it makes the misfire VISIBLE.
func BurnAnomaly(predictedUsedPct float64, gotRateLimited bool) (note string, anomalous bool) {
	if !gotRateLimited || predictedUsedPct < 0 || predictedUsedPct >= 70 {
		return "", false
	}
	return fmt.Sprintf("codex burn anomaly: provider rate-limited while the ledger predicted only %.1f%% of the 5h window used — vendor misfire class (#28879/#31125) or the capacity estimate/degradation factor is off; inspect and `probe --ack-codex` to clear", predictedUsedPct), true
}
