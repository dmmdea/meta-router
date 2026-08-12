package policyeval

import "testing"

func TestConfigKeyRoundTrips(t *testing.T) {
	c := Config{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh"}
	got, err := ParseConfig(c.Key())
	if err != nil || got != c {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, c)
	}
}

// seed.go carries entries with Effort:"" (MechanicalText local/gemma4-cascade,
// DocSummarize local/qwythos, VerifyGate local/qwythos, HardCaseReclaim
// local/qwythos-think). Normalizing blank effort on the ORACLE side ONLY would
// leave those seed keys ending "...|" — permanently uncoverable, and rejected
// by ParseConfig. Normalization must be SYMMETRIC and the value must round-trip.
func TestBlankEffortNormalizesAndRoundTrips(t *testing.T) {
	c := Config{Lane: "local", Model: "qwythos", Effort: NormalizeEffort("")}
	if c.Effort != EffortUnrecorded {
		t.Fatalf("blank effort must normalize to %q, got %q", EffortUnrecorded, c.Effort)
	}
	if _, err := ParseConfig(c.Key()); err != nil {
		t.Fatalf("a normalized blank-effort key must parse, got %v", err)
	}
	if NormalizeEffort("  ") != EffortUnrecorded || NormalizeEffort("high") != "high" {
		t.Fatal("NormalizeEffort must trim-and-default, and leave a real effort alone")
	}
	if NormalizeEffort("  high  ") != "high" {
		t.Fatal("NormalizeEffort must trim a padded effort for USE, not just for the blank test")
	}
}

// EffortUnrecorded is a REAL config value, never a wildcard: legacy evidence
// says nothing about how a pinned effort performs (B6 — unknown cells are
// counted, never imputed).
func TestUnrecordedEffortIsItsOwnConfig(t *testing.T) {
	legacy := Config{Lane: "claude", Model: "claude-sonnet-5", Effort: EffortUnrecorded}
	pinned := Config{Lane: "claude", Model: "claude-sonnet-5", Effort: "high"}
	if legacy.Key() == pinned.Key() {
		t.Fatal("unrecorded-effort evidence must not share a key with pinned-effort evidence")
	}
}

// ClaudeFraction is computed with `lane == "claude"`. Once policies return a
// Config that comparison must go through IsLane, or the fraction silently
// becomes 0 for every policy — which makes the non-inferiority verdict
// (RatioCILo >= 1-margin && ClaudeFraction < 1) unconditionally true.
func TestIsLane(t *testing.T) {
	c := Config{Lane: "claude", Model: "claude-opus-5", Effort: "xhigh"}
	if !c.IsLane("claude") || c.IsLane("codex") {
		t.Fatal("IsLane must compare the lane field, not the key")
	}
	if (Config{}).IsLane("claude") {
		t.Fatal("the zero Config (abstain) is not any lane")
	}
}

func TestParseConfigRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "claude", "claude|model", "a|b|c|d", "claude||high", "|m|high"} {
		if _, err := ParseConfig(s); err == nil {
			t.Fatalf("ParseConfig(%q) must error", s)
		}
	}
}
