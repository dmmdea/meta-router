package claudelane

import "encoding/json"

type ModelUse struct {
	InputTokens              int64   `json:"inputTokens"`
	OutputTokens             int64   `json:"outputTokens"`
	CacheCreationInputTokens int64   `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64   `json:"cacheReadInputTokens"`
	CostUSD                  float64 `json:"costUSD"`
}

type Outcome struct {
	Class          string // "ok"|"refusal"|"rate_limit"|"api_error"|"empty_result"|"parse_error"
	Result         string
	ModelUsage     map[string]ModelUse
	NotionalUSD    float64 // total_cost_usd — NEVER spend under subscription auth
	NumTurns       int
	DurationAPIMs  int64
	StopReason     string
	TerminalReason string
}

// TotalTokens sums fresh input + output across attributed models — the shadow
// -accounting quantity. Cache reads/creation are tracked separately in
// ModelUsage for calibration but excluded from the floor (provider windows
// meter them differently; learned capacity absorbs the difference).
func (o Outcome) TotalTokens() int64 {
	var n int64
	for _, u := range o.ModelUsage {
		n += u.InputTokens + u.OutputTokens
	}
	return n
}

// rawResult mirrors the LIVE captured fixture spellings (Task 2, CLI 2.1.199)
// — not documentation. api_error_status is JSON null on success (int zero
// value after unmarshal); modelUsage keys are the models that ACTUALLY
// answered (silent-fallback attribution).
type rawResult struct {
	Type           string              `json:"type"`
	Subtype        string              `json:"subtype"`
	IsError        bool                `json:"is_error"`
	StopReason     string              `json:"stop_reason"`
	TerminalReason string              `json:"terminal_reason"`
	APIErrorStatus int                 `json:"api_error_status"`
	Result         string              `json:"result"`
	TotalCostUSD   float64             `json:"total_cost_usd"`
	NumTurns       int                 `json:"num_turns"`
	DurationAPIMs  int64               `json:"duration_api_ms"`
	ModelUsage     map[string]ModelUse `json:"modelUsage"`
}

// Parse classifies a claude -p result. Field names verified against LIVE
// captured fixtures — if the CLI schema drifts, the fixture tests (and the
// probe --verify gate, RS8) catch it before the ledger mis-attributes.
func Parse(raw []byte) Outcome {
	var r rawResult
	if err := json.Unmarshal(raw, &r); err != nil || r.Type != "result" {
		return Outcome{Class: "parse_error", Result: string(raw)}
	}
	o := Outcome{
		Result: r.Result, ModelUsage: r.ModelUsage, NotionalUSD: r.TotalCostUSD,
		NumTurns: r.NumTurns, DurationAPIMs: r.DurationAPIMs,
		StopReason: r.StopReason, TerminalReason: r.TerminalReason,
	}
	switch {
	case r.StopReason == "refusal":
		o.Class = "refusal"
	case r.APIErrorStatus == 429:
		o.Class = "rate_limit"
	case r.IsError || r.APIErrorStatus >= 400:
		o.Class = "api_error"
	case r.Result == "" && o.TotalTokens() > 0:
		o.Class = "empty_result" // RS6: billed-but-empty, never "ok"
	default:
		o.Class = "ok"
	}
	return o
}
