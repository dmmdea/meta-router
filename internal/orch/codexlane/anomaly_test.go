package codexlane

import "testing"

// Fact-refresh gap #6: with no provider usage surface, "observed" is the
// provider's own veto — a rate-limit at predicted headroom IS the anomaly.
func TestBurnAnomalyFiresOnlyOnPredictedHeadroom(t *testing.T) {
	tests := []struct {
		predicted   float64
		rateLimited bool
		anomalous   bool
	}{
		{35, true, true},   // comfortable headroom + veto = vendor misfire class
		{85, true, false},  // real depletion, not a misfire
		{-1, true, false},  // no prediction, nothing to contradict
		{35, false, false}, // no veto, no anomaly
		{0, true, true},    // zero predicted burn + veto is the loudest misfire
		{70, true, false},  // boundary: >=70 predicted is no longer "headroom"
	}
	for _, tc := range tests {
		note, got := BurnAnomaly(tc.predicted, tc.rateLimited)
		if got != tc.anomalous {
			t.Fatalf("BurnAnomaly(%v,%v) = %v, want %v", tc.predicted, tc.rateLimited, got, tc.anomalous)
		}
		if got && note == "" {
			t.Fatalf("an anomaly must carry a usable note")
		}
	}
}
