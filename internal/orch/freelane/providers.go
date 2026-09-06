// Package freelane drives the FREE-FOREVER third-party providers vetted on
// 2026-07-23 and re-vetted on 2026-09-06 (private specs: free-lane-vetting,
// frontier-refresh-v5 §3): Groq, Cloudflare Workers AI, OpenRouter `:free`,
// NVIDIA NIM (operator override — NVIDIA's own ToS calls it a trial, so it is
// metered as a DEPLETABLE credit pool, not a renewing window) and the Gemini
// API free tier (allowlist-gated: free-tier prompts train Google models).
//
// Every provider speaks the OpenAI chat-completions surface, so one adapter
// serves all five; what differs per provider is DATA — base URL, metering
// unit, window shape, the prior limits, whether the vendor reports rate state
// in response headers — and that data lives in the registry below. The
// registry is also the EXCLUSION LIST by construction: a provider that is not
// here (Cerebras, GitHub Models, Together, Fireworks, DeepSeek — expiring
// credits or dead tiers; Mistral, Zhipu, SambaNova — unverified limits or
// training stance) cannot be dispatched no matter what a stale listicle says.
//
// Contract mirrors codexlane/copilotlane: every path returns a CLASSIFIED
// Outcome; the error return is reserved for config failures. There is no
// child process — the lane is an HTTP call — so childenv is not involved;
// the credential is a FILE the operator provisions (see token.go), never an
// ambient environment variable.
package freelane

// Lane names. They are ROUTER LANES (one per provider), not one "free" lane:
// admission, the health breaker, status and the quota hint all key on lane, and
// a Groq outage must never mask Cloudflare.
const (
	LaneGroq       = "groq"
	LaneCloudflare = "cloudflare"
	LaneOpenRouter = "openrouter"
	LaneNIM        = "nim"
	LaneGemini     = "gemini"
)

// Lanes is the ordered registry — the exclusion list by construction.
var Lanes = []string{LaneGroq, LaneCloudflare, LaneOpenRouter, LaneNIM, LaneGemini}

// Unit is what the daily/trial window counts. The ledger stores MILLI-units.
type Unit string

const (
	UnitRequests Unit = "requests" // groq, openrouter, gemini: requests per day
	UnitNeurons  Unit = "neurons"  // cloudflare: neurons per day (token-rate table)
	UnitCredits  Unit = "credits"  // nim: trial credits, one per request, no reset
)

// Window shapes the lane's metered window.
const (
	WindowDay   = "day"   // calendar day, resets 00:00 UTC (ledger.WinDay)
	WindowTrial = "trial" // depletable pool, never resets (ledger.WinTrial)
)

// Spec is one provider's dispatch and metering data.
type Spec struct {
	Lane    string
	BaseURL string // OpenAI-compatible root; "{account_id}" is substituted for cloudflare
	Unit    Unit
	Window  string
	// RPM is the per-minute request ceiling enforced LOCALLY by a sliding
	// window (the vendor's own 429 is the upstream truth; this keeps us from
	// asking for it). DailyCap is the per-window ceiling in Unit, a CONFIG
	// PRIOR (estimate-sourced in the ledger: throttle-only, never exhaust).
	RPM      int
	DailyCap int64
	// HeaderLimits: the vendor reports the daily window in x-ratelimit-*
	// response headers (Groq, documented 2026-09-06) — a provider-true
	// observation the ledger lands with poll authority.
	HeaderLimits bool
	// ModelSuffix, when set, is REQUIRED on every model id (openrouter ":free"
	// — a paid model on an account holding a balance would spend; R10).
	ModelSuffix string
	// ClassGated: dispatch is refused unless the task class is allowlisted
	// (gemini — operator 2026-07-23: free-tier data trains Google; not seated
	// until the non-sensitive allowlist gate exists).
	ClassGated bool
	// NeedsAccountID: the endpoint embeds the operator's account id (cloudflare).
	NeedsAccountID bool
}

// specs are the 2026-09-06 priors. Numbers here are DATA about vendor tiers,
// re-verified by the policy watch (doc hashes) and overridden per lane through
// orchcfg free_providers; they are not constants of nature.
var specs = map[string]Spec{
	LaneGroq: {Lane: LaneGroq, BaseURL: "https://api.groq.com/openai/v1",
		Unit: UnitRequests, Window: WindowDay, RPM: 30, DailyCap: 1000, HeaderLimits: true},
	LaneCloudflare: {Lane: LaneCloudflare, BaseURL: "https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1",
		Unit: UnitNeurons, Window: WindowDay, RPM: 30, DailyCap: 10000, NeedsAccountID: true},
	LaneOpenRouter: {Lane: LaneOpenRouter, BaseURL: "https://openrouter.ai/api/v1",
		Unit: UnitRequests, Window: WindowDay, RPM: 20, DailyCap: 50, ModelSuffix: ":free"},
	LaneNIM: {Lane: LaneNIM, BaseURL: "https://integrate.api.nvidia.com/v1",
		Unit: UnitCredits, Window: WindowTrial, RPM: 40, DailyCap: 1000},
	LaneGemini: {Lane: LaneGemini, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
		Unit: UnitRequests, Window: WindowDay, RPM: 15, DailyCap: 250, ClassGated: true},
}

// Provider returns the registry spec for lane.
func Provider(lane string) (Spec, bool) {
	s, ok := specs[lane]
	return s, ok
}

// IsLane reports whether lane is a free-provider lane.
func IsLane(lane string) bool {
	_, ok := specs[lane]
	return ok
}

// Limits is the operator's per-lane overlay (orchcfg free_providers). Zero
// values keep the registry prior; Off masks the lane.
type Limits struct {
	RPM      int
	DailyCap int64
	Off      bool
}

// Resolve returns the spec with the overlay applied.
func Resolve(lane string, o Limits) (Spec, bool) {
	s, ok := specs[lane]
	if !ok {
		return Spec{}, false
	}
	if o.RPM > 0 {
		s.RPM = o.RPM
	}
	if o.DailyCap > 0 {
		s.DailyCap = o.DailyCap
	}
	return s, true
}
