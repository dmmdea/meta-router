package codexlane

import "testing"

func TestCreditsMathAgainstLiveFixtureNumbers(t *testing.T) {
	u := Usage{Input: 18487, CachedInput: 18176, Output: 5} // the committed capture
	// fresh 311×125/1M + cached 18176×12.5/1M + out 5×750/1M = 0.269825 credits
	// (S2R-13 note: the plan text said 0.2698875 / milli 4048 — arithmetic
	// resolves by execution; computed reality is 0.269825 → ×1000×15 =
	// 4047.375 → 4047.)
	if got := Credits(u); got < 0.2698 || got > 0.2700 {
		t.Fatalf("credits: %v", got)
	}
	// ledger unit: millicredits × degradation factor 15 ⇒ 4047
	if got := CreditsMilli(u, 15); got != 4047 {
		t.Fatalf("millicredits ×15: %d", got)
	}
	if got := CreditsMilli(u, 1); got != 270 { // undegraded floor
		t.Fatalf("millicredits ×1: %d", got)
	}
	if got := CreditsMilli(u, 0); got != 270 { // zero/negative factor clamps to 1
		t.Fatalf("millicredits ×0 must clamp to undegraded: %d", got)
	}
}
