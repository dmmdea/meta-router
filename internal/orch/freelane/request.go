package freelane

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EffortUnrecorded mirrors policyeval.EffortUnrecorded without importing it:
// the replay passes this marker for "no effort dial", and the lane must not
// forward the literal string to a vendor.
const EffortUnrecorded = "unrecorded"

// DefaultMaxTokens bounds the completion. Reasoning models spend output
// tokens on thinking first: the live NIM probe (2026-09-06) at max_tokens 32
// returned finish_reason "length" with the reasoning in `content` and no
// answer. 4096 leaves room for a full answer on a text-dispatch task.
const DefaultMaxTokens = 4096

// RunReq is one free-lane dispatch.
type RunReq struct {
	Lane, Model, Prompt string
	// Effort is forwarded as `reasoning_effort` only when set and not the
	// unrecorded marker (the OpenAI-compatible knob gpt-oss honours on groq /
	// openrouter / nim). Empty = the vendor default ran.
	Effort string
	// Token is the provider API key the caller loaded from the credential
	// file. Required: an empty token would let the vendor answer 401 on a
	// request that already left the machine.
	Token string
	// AccountID is required for cloudflare (endpoint embeds it).
	AccountID  string
	MaxTokens  int
	TimeoutSec int
	// BaseURL overrides the registry root — TEST INJECTION ONLY (httptest);
	// production leaves it empty and the https rule in Run applies.
	BaseURL string
}

// Endpoint resolves the chat-completions URL for spec.
func Endpoint(spec Spec, accountID string) (string, error) {
	base := spec.BaseURL
	if spec.NeedsAccountID {
		if strings.TrimSpace(accountID) == "" {
			return "", fmt.Errorf("free lane %s: free_cloudflare_account_id is required (the Workers AI endpoint embeds the account; keep that account on the Free plan — R10)", spec.Lane)
		}
		base = strings.ReplaceAll(base, "{account_id}", accountID)
	}
	return strings.TrimRight(base, "/") + "/chat/completions", nil
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatBody struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	MaxTokens       int           `json:"max_tokens"`
	Stream          bool          `json:"stream"`
	ReasoningEffort string        `json:"reasoning_effort,omitempty"`
}

// BuildBody validates the request and renders the OpenAI chat body. The token
// is validated for presence but NEVER serialized: it rides in the
// Authorization header only.
func BuildBody(r RunReq) ([]byte, error) {
	spec, ok := Provider(r.Lane)
	if !ok {
		return nil, fmt.Errorf("unknown free lane %q (registry: %s)", r.Lane, strings.Join(Lanes, ", "))
	}
	if strings.TrimSpace(r.Model) == "" {
		return nil, fmt.Errorf("model is required: pin --model on every %s dispatch (the oracle row records the pin)", r.Lane)
	}
	if spec.ModelSuffix != "" && !strings.HasSuffix(r.Model, spec.ModelSuffix) {
		return nil, fmt.Errorf("free lane %s refuses model %q: only models ending in %q are free; a paid model on an account holding a balance would SPEND (R10: zero spend by construction, force-proof)", r.Lane, r.Model, spec.ModelSuffix)
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if strings.TrimSpace(r.Token) == "" {
		return nil, fmt.Errorf("token is required: load it from %s (never from the environment)", TokenPath("<state>", r.Lane))
	}
	max := r.MaxTokens
	if max <= 0 {
		max = DefaultMaxTokens
	}
	b := chatBody{Model: r.Model, Messages: []chatMessage{{Role: "user", Content: r.Prompt}}, MaxTokens: max}
	if e := strings.TrimSpace(r.Effort); e != "" && e != EffortUnrecorded {
		b.ReasoningEffort = e
	}
	return json.Marshal(b)
}
