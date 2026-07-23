package orchcfg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingFailsOpenToDefaults(t *testing.T) {
	c := Load(filepath.Join(t.TempDir(), "nope.json"))
	// W1 (Daniel 2026-07-23): usage polls default ON — the old D3 off-gate is
	// superseded; an explicit false in a hand-edited config is still honored.
	if c.ClaudeBillingMode != BillingSubscription || !c.OAuthUsagePoll {
		t.Fatalf("defaults wrong: %+v", c)
	}
}

func TestLoadCreditsMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"claude_billing_mode":"credits","usage_credit_balance_usd":12.5}`), 0o644)
	c := Load(p)
	if c.ClaudeBillingMode != BillingCredits || c.UsageCreditBalanceUSD != 12.5 {
		t.Fatalf("credits config mis-read: %+v", c)
	}
}

// Fail-safe: a garbled mode must be PRESERVED so the gate hard-stops on it —
// normalizing to "subscription" would silently disarm the R10 hard-stop.
func TestLoadUnknownModePreservedForFailSafeGate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"claude_billing_mode":"Credits"}`), 0o644)
	if c := Load(p); c.ClaudeBillingMode != "Credits" {
		t.Fatalf("unknown mode must be preserved verbatim: %+v", c)
	}
}

// Slice 2: lane tiers are config, not code (§6b "upgrade is a number change").
// A hand-edited partial config must not zero a tier (zero-value backfill).
func TestLaneTierDefaults(t *testing.T) {
	d := Defaults()
	if d.CodexPlus5hCredits != 40 || d.CodexDegradationFactor != 15 || d.GLM5hPrompts != 80 || !d.CodexUsagePoll {
		t.Fatalf("lane tier defaults wrong: %+v", d)
	}
	if d.PollMinIntervalMin != 5 || d.PaceRankOn {
		t.Fatalf("W1 defaults wrong (poll interval 5, pace rank OFF per B8): %+v", d)
	}
	// E6: Defaults() must set QuotaStaleHours (Load already backfills it; Defaults
	// was the only place the field was missing).
	if d.QuotaStaleHours != 48 {
		t.Fatalf("Defaults().QuotaStaleHours must be 48, got %d", d.QuotaStaleHours)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"claude_billing_mode":"subscription"}`), 0o644)
	c := Load(p)
	if c.CodexPlus5hCredits != 40 || c.CodexDegradationFactor != 15 || c.GLM5hPrompts != 80 {
		t.Fatalf("partial config must backfill tier defaults: %+v", c)
	}
	os.WriteFile(p, []byte(`{"codex_plus_5h_credits":0,"glm_5h_prompts":0}`), 0o644)
	if c := Load(p); c.CodexPlus5hCredits != 40 || c.GLM5hPrompts != 80 {
		t.Fatalf("explicit zeros are hand-edit damage, not intent — backfill: %+v", c)
	}
}

func TestLoadEmptyModeIsDefault(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(p, []byte(`{"usage_credit_balance_usd":1}`), 0o644)
	if c := Load(p); c.ClaudeBillingMode != BillingSubscription {
		t.Fatalf("absent mode is the default: %+v", c)
	}
}

// S2R-6 cadence hygiene gates: pacing ships ON with a ~20-40s interval; the
// numerics backfill on hand-edit zeros; the bool honors an explicit operator
// off-switch.
func TestGLMPacingDefaultsAndGate(t *testing.T) {
	d := Defaults()
	if !d.GLMPacing || d.GLMPaceMinSec != 20 || d.GLMPaceJitterSec != 20 {
		t.Fatalf("pacing must ship ON at 20s+20s jitter: %+v", d)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(`{"glm_pace_min_sec":0}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := Load(p); !c.GLMPacing || c.GLMPaceMinSec != 20 {
		t.Fatalf("hand-edit zero must backfill, absent bool stays ON: %+v", c)
	}
	if err := os.WriteFile(p, []byte(`{"glm_pacing":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := Load(p); c.GLMPacing {
		t.Fatalf("explicit operator off-switch must be honored: %+v", c)
	}
}
