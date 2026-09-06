package freelane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Usage is the vendor-reported token count.
type Usage struct {
	Prompt     int64 `json:"prompt_tokens"`
	Completion int64 `json:"completion_tokens"`
}

// RateInfo is the vendor's own account of the DAILY request window, read from
// Groq's documented headers (x-ratelimit-limit-requests = daily ceiling,
// x-ratelimit-remaining-requests, x-ratelimit-reset-requests = duration until
// the daily reset, e.g. "2m59.56s"). Present only when the limit header is.
// RetryAfter is set on a 429 from `retry-after` (seconds) or, failing that,
// the reset duration.
type RateInfo struct {
	Present           bool
	LimitRequests     int64
	RemainingRequests int64
	ResetRequests     time.Duration
	RetryAfter        time.Duration
}

// UsedPct is the window depletion the headers imply; -1 when unknown.
func (r RateInfo) UsedPct() float64 {
	if !r.Present || r.LimitRequests <= 0 {
		return -1
	}
	used := float64(r.LimitRequests-r.RemainingRequests) / float64(r.LimitRequests) * 100
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	return used
}

// Outcome classes:
//
//	ok               — 2xx with non-empty assistant content
//	empty_result     — 2xx, decodable, but no content (e.g. finish_reason "length" spent on reasoning)
//	parse_error      — 2xx body not the chat-completion shape
//	rate_limit       — 429 (upstream by construction; the caller's local limiter never reaches Parse)
//	auth_error       — 401/403 (a bad or revoked key: config, not quota)
//	payment_required — 402 (nim credits gone / openrouter balance — the depletable-pool signal)
//	too_large        — 413 (request over the per-minute token ceiling)
//	api_error        — any other non-2xx
//	spawn_error      — set by Run on transport failure (no HTTP status)
type Outcome struct {
	Class        string
	Result       string // assistant content, or the failure message
	Model        string // the model the vendor reports serving (openrouter :free may resolve a variant)
	FinishReason string
	Usage        Usage
	Rate         RateInfo
	HTTPStatus   int
}

type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// Parse classifies one HTTP exchange. Content is taken from
// choices[0].message.content ONLY — never reasoning_content: a reasoning
// model's thinking is not an answer, and scoring it as one would inflate the
// oracle (live NIM capture 2026-09-06 shows both fields populated).
func Parse(status int, h http.Header, body []byte) Outcome {
	o := Outcome{HTTPStatus: status, Rate: parseRate(h, status)}
	if status < 200 || status > 299 {
		msg := errorMessage(body)
		switch {
		case status == http.StatusTooManyRequests:
			o.Class = "rate_limit"
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			o.Class = "auth_error"
		case status == http.StatusPaymentRequired:
			o.Class = "payment_required"
		case status == http.StatusRequestEntityTooLarge:
			o.Class = "too_large"
		default:
			o.Class = "api_error"
		}
		o.Result = fmt.Sprintf("HTTP %d: %s", status, msg)
		return o
	}
	var r chatResponse
	if err := json.Unmarshal(body, &r); err != nil {
		o.Class = "parse_error"
		o.Result = "response is not a chat completion: " + err.Error()
		return o
	}
	o.Model = r.Model
	if r.Usage != nil {
		o.Usage = *r.Usage
	}
	if len(r.Choices) == 0 {
		o.Class = "empty_result"
		o.Result = "no choices in response"
		return o
	}
	o.FinishReason = r.Choices[0].FinishReason
	content := strings.TrimSpace(r.Choices[0].Message.Content)
	if content == "" {
		o.Class = "empty_result"
		o.Result = "empty assistant content (finish_reason=" + o.FinishReason + ")"
		return o
	}
	// Live NIM capture (2026-09-06, max_tokens 32): a reasoning model that ran
	// out of budget returned its THINKING as `content`, byte-identical to
	// `reasoning_content`, finish_reason "length". That is not an answer, and
	// scoring it as one would credit the lane with output it never produced.
	if rc := strings.TrimSpace(r.Choices[0].Message.ReasoningContent); rc != "" && rc == content {
		o.Class = "empty_result"
		o.Result = "reasoning echoed as content, no answer produced (finish_reason=" + o.FinishReason + "; raise free_max_tokens)"
		return o
	}
	o.Class = "ok"
	o.Result = content
	return o
}

// errorMessage pulls error.message from a vendor error body when present;
// otherwise a bounded slice of the raw body (never the whole thing: a 5xx
// from a proxy can be an HTML page).
func errorMessage(body []byte) string {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Error != nil && e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 240 {
		s = s[:240] + "…"
	}
	if s == "" {
		return "(empty body)"
	}
	return s
}

func parseRate(h http.Header, status int) RateInfo {
	var r RateInfo
	if h == nil {
		return r
	}
	if v := h.Get("x-ratelimit-limit-requests"); v != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			r.Present = true
			r.LimitRequests = n
		}
	}
	if r.Present {
		if n, err := strconv.ParseInt(strings.TrimSpace(h.Get("x-ratelimit-remaining-requests")), 10, 64); err == nil {
			r.RemainingRequests = n
		}
		if d, ok := ParseGroqDuration(h.Get("x-ratelimit-reset-requests")); ok {
			r.ResetRequests = d
		}
	}
	if status == http.StatusTooManyRequests {
		if v := strings.TrimSpace(h.Get("retry-after")); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				r.RetryAfter = time.Duration(f * float64(time.Second))
			} else if t, err := http.ParseTime(v); err == nil {
				if d := time.Until(t); d > 0 {
					r.RetryAfter = d
				}
			}
		}
		if r.RetryAfter == 0 && r.ResetRequests > 0 {
			r.RetryAfter = r.ResetRequests
		}
	}
	return r
}
