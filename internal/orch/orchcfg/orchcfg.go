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
	OAuthUsagePoll        bool    `json:"oauth_usage_poll"`         // W1: default ON (Daniel 2026-07-23, supersedes D3) — official-shape endpoint, own subscription token; explicit false still honored

	// Lane tiers as config — §6b "upgrade is a number change". These are DATA
	// about the operator's current plans, never constants of nature.
	CodexUsagePoll         bool    `json:"codex_usage_poll"`         // W1: default ON as BEST-EFFORT EVIDENCE (Daniel 2026-07-23; Q10 caveats encoded in quotapoll)
	CodexPlus5hCredits     float64 `json:"codex_plus_5h_credits"`    // default 40 (Plus 5h band 15–80, fact refresh)
	CodexDegradationFactor float64 `json:"codex_degradation_factor"` // default 15 (10–20× observed, #28879)
	GLM5hPrompts           int64   `json:"glm_5h_prompts"`           // default 80; weekly = 5× (never 10×)

	// Copilot lane (2026-09-01: GLM subscription cancelled, Copilot Pro
	// purchased — lane tiers are DATA about the operator's plans).
	// CopilotTokenUser names the gh-keyring account that owns the Copilot
	// subscription; the lane mints an OAuth token from it per dispatch
	// (`gh auth token --user <x>`). EMPTY IS A REFUSAL, not a fallback:
	// falling through to the ambient/active gh account would bill whichever
	// account the environment happens to carry — the cross-account hazard.
	CopilotTokenUser string `json:"copilot_token_user"`
	// CopilotMonthlyCredits is the plan's monthly AI-CREDIT allowance (Pro:
	// 1,500 = 1,000 base + 500 flex; one credit = $0.01). Unit verified live
	// 2026-09-06: `gh api copilot_internal/user` reports entitlement 1500 with
	// token_based_billing true, and the official billing report lists the
	// month as sku "Copilot AI Credits" (1499.37 used, net $0.00) while the
	// premium-request report is empty. A config GUESS (estimate-sourced cap)
	// only until the copilot usage poll lands the vendor's entitlement as the
	// measured cap. Resets the 1st, 00:00 UTC; overage is off at the account.
	CopilotMonthlyCredits int64 `json:"copilot_monthly_credits"`
	// CopilotMonthlyRequests is RETIRED (2026-09-06): the plan bills AI
	// credits, not premium requests, and the v0.35–v0.37 meter that used it
	// latched the lane "exhausted" at 300 credits of a 1,500-credit month.
	// Still parsed so an old config loads; never read. Delete it.
	CopilotMonthlyRequests int64 `json:"copilot_monthly_requests,omitempty"`
	// CopilotMaxAiCredits bounds ONE dispatch's spend through the CLI's own
	// `--max-ai-credits` (a soft cap: the next model call is refused once the
	// session crosses it; min 30 — `copilot help limits`, CLI 1.0.83). The
	// measured dispatch median is ~13 credits (1,501 credits / 116 executed
	// dispatches, 2026-09), so 60 leaves headroom for an agentic turn and
	// still stops a runaway one. <=0 omits the flag; 1..29 clamps to 30.
	CopilotMaxAiCredits int64 `json:"copilot_max_ai_credits"`
	// CopilotUsagePoll polls `copilot_internal/user` for the month's
	// entitlement, consumption and reset — provider truth for the month
	// window (default ON, the codex_usage_poll posture). The poll
	// authenticates with a token minted from copilot_token_user per poll,
	// never an ambient env token.
	CopilotUsagePoll bool `json:"copilot_usage_poll"`
	// CopilotReviewCredits is the AI-credit ESTIMATE metered for one Copilot
	// code review requested through `copilot-review` (GitHub prices a lite
	// review at $0.05–$1 and a balanced one at $0.25–$5, i.e. 5–500 credits;
	// 25 is a mid-lite guess). It keeps the month honest between polls; the
	// next poll replaces it with the vendor's figure. <=0 meters nothing.
	CopilotReviewCredits int64 `json:"copilot_review_credits"`
	// CopilotModel default is gpt-5.6-terra, the model whose per-dispatch
	// vendor figure measured lowest (1) on the 2026-09-01 checkpoints. It was
	// "auto" until 2026-09-05, when a 168-cell gold probe under auto was
	// served by gpt-5.6-luna on 78% of dispatches at roughly 3x the credits
	// per dispatch. `auto` is still a legitimate, explicit choice (config or
	// --model auto); it is no longer the silent default.
	CopilotModel string `json:"copilot_model"`

	// GLMRetired ships TRUE (subscription cancelled 2026-09-01): the glm
	// lane refuses dispatch with a typed reason. Explicit false re-enables
	// everything — retirement is config, not deleted code (R14: plans are
	// data; subscriptions come back).
	GLMRetired bool `json:"glm_retired"`

	// DelegateProposePct: when the claude lane's worst live statusline window
	// reaches this percentage and the session is not armed, mr-hook PROPOSES
	// /delegate-mode (spec 2026-09-01 §4). A proposal only — arming is always an
	// explicit operator act. < 0 disables. Default 70 is a starting point to
	// calibrate against real sessions, not a measured threshold.
	DelegateProposePct float64 `json:"delegate_propose_pct"`

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

	// Egress policy for THIRD-PARTY lanes (glm today; the approved free lanes
	// inherit it). Repo context is denied unless its repo is listed here —
	// deny-by-default, force-proof, multi-brand isolation.
	GLMAllowRepos          []string `json:"glm_allow_repos"`
	EgressPromptOnlyDenied bool     `json:"egress_prompt_only_denied"`

	PollMinIntervalMin int  `json:"poll_min_interval_min"` // W1: min minutes between usage polls (status-triggered); default 5
	PaceRankOn         bool `json:"pace_rank_on"`          // W1: slack tie-break in the router; default OFF (B8 — promotes only via a budget-state eval)

	// W6 resilience knobs.
	IncidentModeOn bool `json:"incident_mode_on"` // default OFF (B8 posture: routing-visible, promotes via eval); arms router incident mode (engages only when most lanes are pressured)
	ExclusionOff   bool `json:"exclusion_off"`    // kill-switch for the self-healing lane breaker; ships ON (resilience, not routing policy)
	// LocalMaxPerMin caps local-lane dispatches over a 60s SLIDING window.
	// 0 = absent field → default 20; negative = limiter OFF (explicit
	// operator intent, distinguishable from hand-edit zero damage).
	LocalMaxPerMin int `json:"local_max_per_min"`

	// CompactionOff (W5) disables the LOSSLESS embed-time dep-context
	// compaction in strategy DAGs. Ships ON: the transform is provably
	// round-trippable (DG-3 lossless-first; nothing lossy exists here).
	// Scope: compaction ONLY — the W5 re-lane context handoff is not
	// compression and has no switch.
	CompactionOff bool `json:"compaction_off"`
}

// CopilotDefaultModel is the lane's default pin: the lowest measured
// per-dispatch credit figure. See the CopilotModel field comment for why
// `auto` lost the default.
const CopilotDefaultModel = "gpt-5.6-terra"

// CopilotMinAiCredits is the CLI's floor for --max-ai-credits (`copilot help
// limits`, 1.0.83): a smaller configured cap is clamped up to it, never sent.
const CopilotMinAiCredits int64 = 30

func Defaults() Config {
	return Config{
		ClaudeBillingMode: BillingSubscription, OAuthUsagePoll: true,
		CodexUsagePoll: true, CodexPlus5hCredits: 40, CodexDegradationFactor: 15, GLM5hPrompts: 80,
		GLMPacing: true, GLMPaceMinSec: 20, GLMPaceJitterSec: 20,
		CopilotMonthlyCredits: 1500, CopilotMaxAiCredits: 60, CopilotUsagePoll: true, CopilotReviewCredits: 25,
		CopilotModel: CopilotDefaultModel, GLMRetired: true,
		LocalOffloadBin: "offload-harness", LocalAgentBin: "local-agent", StrategyMaxConcurrency: 2,
		QuotaStaleHours: 48, PollMinIntervalMin: 5, LocalMaxPerMin: 20,
		DelegateProposePct: 70,
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
	if c.PollMinIntervalMin == 0 {
		c.PollMinIntervalMin = 5
	}
	if c.GLM5hPrompts == 0 {
		c.GLM5hPrompts = 80
	}
	if c.CopilotMonthlyCredits == 0 {
		c.CopilotMonthlyCredits = 1500
	}
	if c.CopilotMaxAiCredits > 0 && c.CopilotMaxAiCredits < CopilotMinAiCredits {
		c.CopilotMaxAiCredits = CopilotMinAiCredits
	}
	if c.CopilotModel == "" {
		c.CopilotModel = CopilotDefaultModel
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
	if c.LocalMaxPerMin == 0 {
		c.LocalMaxPerMin = 20 // absent field / hand-edit zero → default; negative = explicitly off
	}
	if c.DelegateProposePct == 0 {
		c.DelegateProposePct = 70 // absent field / hand-edit zero → default; negative = explicitly off
	}
	return c
}
