package router

import "testing"

// The free-provider lane states are registered in the masked() denylist: an
// unregistered state name reads as SELECTABLE (the same trap
// TestMaskedRegistersExcluded pins for delegate-mode).
func TestMaskedRegistersFreeLaneStates(t *testing.T) {
	for _, st := range []string{"unconfigured", "gated", "off"} {
		if !masked(st) {
			t.Fatalf("masked(%q) = false: a lane in that state would still be selectable", st)
		}
	}
	for _, st := range []string{"open", "throttled", ""} {
		if masked(st) {
			t.Fatalf("masked(%q) = true: a live state must stay selectable", st)
		}
	}
}

// The residual tiebreak is a TOTAL order and the free providers sit after
// every subscription lane: at equal rank, paid-for capacity is spent first
// (R14), free capacity is the overflow.
func TestLanePriorityOrder(t *testing.T) {
	order := []string{"claude", "codex", "glm", "local", "copilot"}
	for i := 1; i < len(order); i++ {
		if lanePriority(order[i-1]) >= lanePriority(order[i]) {
			t.Fatalf("%s must outrank %s at parity", order[i-1], order[i])
		}
	}
	free := []string{"groq", "cloudflare", "openrouter", "nim", "gemini"}
	for _, f := range free {
		if lanePriority(f) <= lanePriority("copilot") {
			t.Fatalf("free lane %s must sit after copilot", f)
		}
		if lanePriority(f) != lanePriority("groq") {
			t.Fatalf("free lanes tie with each other: %s", f)
		}
		if lanePriority(f) >= lanePriority("never-heard-of-it") {
			t.Fatalf("an unknown lane must sort last, after %s", f)
		}
	}
}
