// Package orchcfg holds the orchestrator's operator config. RS7: the Claude
// billing-mode switch exists from day one — the June-15 billing split is
// paused, not dead, and shipped on <24h notice historically. Flipping to
// "credits" changes admission semantics (R10: the operator keeps usage-credits OFF,
// so credits mode HARD-STOPS the lane rather than converting quota exhaustion
// into dollar spend). Usage-credit balance has no machine-readable surface
// (fact-refresh gap #4) — manual entry field only.
package orchcfg

import (
	"encoding/json"
	"os"
)

const (
	BillingSubscription = "subscription"
	BillingCredits      = "credits"
)

type Config struct {
	ClaudeBillingMode     string  `json:"claude_billing_mode"`
	UsageCreditBalanceUSD float64 `json:"usage_credit_balance_usd"` // manual entry; no API surface exists
	OAuthUsagePoll        bool    `json:"oauth_usage_poll"`         // D3: ships OFF; flipping is the operator's explicit risk call

	// Lane tiers as config — §6b "upgrade is a number change". These are DATA
	// about the operator's current plans, never constants of nature.
	CodexUsagePoll         bool    `json:"codex_usage_poll"`         // wham/usage read; ships OFF (D3-class risk call)
	CodexPlus5hCredits     float64 `json:"codex_plus_5h_credits"`    // default 40 (Plus 5h band 15–80, fact refresh)
	CodexDegradationFactor float64 `json:"codex_degradation_factor"` // default 15 (10–20× observed, #28879)
	GLM5hPrompts           int64   `json:"glm_5h_prompts"`           // default 80; weekly = 5× (never 10×)

	// S2R-6 cadence hygiene (GLM ban fires on PATTERN, not volume): ships ON;
	// explicit false is the operator's off-switch. Interval ∈ [min, min+jitter].
	GLMPacing        bool  `json:"glm_pacing"`
	GLMPaceMinSec    int64 `json:"glm_pace_min_sec"`
	GLMPaceJitterSec int64 `json:"glm_pace_jitter_sec"`

	// Local lane binaries (S3R-1 two-door adapter). Both PATH-resolved, both
	// keyless (local is the free lane). LocalOffloadBin is the offload-harness
	// cascade door (offload_* Gemma cascade); LocalAgentBin is the local-agent
	// agent door. Overridable so a non-PATH install can point at an absolute path.
	LocalOffloadBin        string `json:"local_offload_bin"`        // default "offload-harness"
	LocalAgentBin          string `json:"local_agent_bin"`          // default "local-agent"
	StrategyMaxConcurrency int    `json:"strategy_max_concurrency"` // default 2 (§4.5 stampede guard)

	// Slice-4 E1 burn-rate downshift. Thresholds are CONFIG priors over the
	// exhaust-at-reset burn multiple (m=1 is on-pace for any window length);
	// zero values fall back to burnrate.Defaults(). The operator's brief-§5 Q3 answer
	// + real-trace calibration retune these without a code change (the
	// no-blog-lore-constants rule). BurnDownshiftOff is the kill-switch.
	BurnDownshiftOff bool    `json:"burn_downshift_off"`
	BurnFastX        float64 `json:"burn_fast_x"`
	BurnMedX         float64 `json:"burn_med_x"`
	BurnSlowX        float64 `json:"burn_slow_x"`
	// Slice-4 E2 spend-down (Q2): batch-only rank boost toward a window
	// measured under-utilized near its reset. All numerics are CONFIG priors;
	// zero/invalid values fall back to spenddown.Defaults() via Normalize (the
	// no-blog-lore-constants rule — real-trace calibration retunes these
	// without a code change). SpendDownOff is the kill-switch.
	SpendDownOff            bool    `json:"spend_down_off"`
	SpendDownFloorUnusedPct float64 `json:"spend_down_floor_unused_pct"` // default 30
	SpendDownHorizonMin     int64   `json:"spend_down_horizon_min"`      // default 90
	SpendDownRaisePct       float64 `json:"spend_down_raise_pct"`        // default 25
	SpendDownDropPct        float64 `json:"spend_down_drop_pct"`         // default 35
	SpendDownCooldownSec    int64   `json:"spend_down_cooldown_sec"`     // default 600
	SpendDownBufferMin      int64   `json:"spend_down_buffer_min"`       // default 10
	SpendDownMaxBoost       int     `json:"spend_down_max_boost"`        // default 2
	SpendDownAvgWindowMin   int64   `json:"spend_down_avg_window_min"`   // default 15
	// Slice-4 E6: provider-signal/trace staleness alarm horizon (hours).
	QuotaStaleHours int `json:"quota_stale_hours"` // default 48
}

func Defaults() Config {
	return Config{
		ClaudeBillingMode: BillingSubscription, OAuthUsagePoll: false,
		CodexUsagePoll: false, CodexPlus5hCredits: 40, CodexDegradationFactor: 15, GLM5hPrompts: 80,
		GLMPacing: true, GLMPaceMinSec: 20, GLMPaceJitterSec: 20,
		LocalOffloadBin: "offload-harness", LocalAgentBin: "local-agent", StrategyMaxConcurrency: 2,
		QuotaStaleHours: 48,
	}
}

// Load reads config from path; missing or corrupt files fail open to Defaults.
// An UNKNOWN billing mode is preserved verbatim, NOT normalized: garbled
// operator intent (a hand-edit typo like "Credits") must fail SAFE at the
// gate — silently coercing it to the permissive "subscription" would disable
// the R10 hard-stop the operator was trying to arm. An empty mode is the
// absent-field case and stays the default.
func Load(path string) Config {
	b, err := os.ReadFile(path)
	if err != nil {
		return Defaults()
	}
	c := Defaults()
	if err := json.Unmarshal(b, &c); err != nil {
		return Defaults()
	}
	if c.ClaudeBillingMode == "" {
		c.ClaudeBillingMode = BillingSubscription
	}
	// Zero-value backfill: a hand-edited partial config must not zero a lane
	// tier (unmarshal-into-Defaults covers ABSENT fields; this covers explicit
	// zeros, which are hand-edit damage, not intent).
	if c.CodexPlus5hCredits == 0 {
		c.CodexPlus5hCredits = 40
	}
	if c.CodexDegradationFactor == 0 {
		c.CodexDegradationFactor = 15
	}
	if c.GLM5hPrompts == 0 {
		c.GLM5hPrompts = 80
	}
	if c.GLMPaceMinSec == 0 {
		c.GLMPaceMinSec = 20
	}
	if c.GLMPaceJitterSec == 0 {
		c.GLMPaceJitterSec = 20
	}
	if c.LocalOffloadBin == "" {
		c.LocalOffloadBin = "offload-harness"
	}
	if c.LocalAgentBin == "" {
		c.LocalAgentBin = "local-agent"
	}
	if c.StrategyMaxConcurrency == 0 {
		c.StrategyMaxConcurrency = 2
	}
	if c.QuotaStaleHours == 0 {
		c.QuotaStaleHours = 48
	}
	return c
}
