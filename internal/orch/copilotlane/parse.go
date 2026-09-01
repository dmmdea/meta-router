package copilotlane

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// Usage is the provider-true consumption a dispatch reports about itself.
// session.usage_checkpoint (non-ephemeral, live-captured CLI 1.0.82,
// 2026-09-01) carries the session's cumulative premium-request count and
// nano-AI-units — SELF-METERED BY THE VENDOR, so the ledger records real
// consumption rather than a doc-lore multiplier estimate. GitHub's billing
// is mid-transition (premium requests vs "AI credits"); both figures are
// kept so whichever regime wins remains reconstructable from dispatch logs.
type Usage struct {
	PremiumRequests int64 `json:"premium_requests"`
	NanoAiu         int64 `json:"nano_aiu"`
}

type Outcome struct {
	Class string // "ok"|"rate_limit"|"auth_error"|"error"|"incomplete"|"parse_error" (+ "spawn_error"/"config_error" at Run layer)
	Result string // last assistant.message content (or the failure message)
	Model  string // the model that actually served the turn (auto resolves vendor-side; attribution comes from the event, never the request)
	Usage  Usage
	Turns  int
}

// envelope is the ONLY shape every line must satisfy: a type plus an opaque
// payload. Payloads are decoded per event type, deliberately, because the
// stream is POLYMORPHIC — `data.message` is a string on some events and an
// object on `model.message`. A single flat struct over all events therefore
// fails `json.Unmarshal` on a perfectly good stream: that exact bug turned a
// successful live dispatch into `parse_error` (caught 2026-09-01 by an
// end-to-end run, invisible to a fixture that had been hand-filtered to the
// events the parser consumed). Decode-per-type is also the RS8 posture:
// additive vendor drift on events we ignore cannot break a dispatch.
type envelope struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type assistantMessage struct {
	Content string `json:"content"`
	Model   string `json:"model"`
}

type usageCheckpoint struct {
	TotalPremiumRequests int64 `json:"totalPremiumRequests"`
	TotalNanoAiu         int64 `json:"totalNanoAiu"`
}

var rateLimitHints = []string{"rate limit", "rate-limit", "usage limit", "quota", "429", "too many requests", "premium request"}
var authHints = []string{"unauthorized", "401", "not authenticated", "authentication", "login", "token is invalid", "bad credentials"}

// classifyFailure maps a failure message to its typed class. Auth is
// distinguished from rate_limit because their remedies differ: an auth
// failure means the minted token is wrong (config/keyring problem, retry is
// useless), while rate_limit is window pressure the ledger must record.
func classifyFailure(msg string) string {
	low := strings.ToLower(msg)
	for _, h := range authHints {
		if strings.Contains(low, h) {
			return "auth_error"
		}
	}
	for _, h := range rateLimitHints {
		if strings.Contains(low, h) {
			return "rate_limit"
		}
	}
	return "error"
}

// flexText pulls human text out of a field that may be a bare string OR an
// object carrying it under a conventional key. Error payloads across this
// CLI's events use both shapes; a parser that assumes one silently drops the
// diagnosis of a failed dispatch.
func flexText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, k := range []string{"message", "content", "error", "detail", "reason"} {
		if v, ok := obj[k]; ok {
			if t := flexText(v); t != "" {
				return t
			}
		}
	}
	return ""
}

// Parse walks the JSONL stream. The dispatch is "ok" only when a terminal
// `result` event was seen AND an assistant message exists — a stream that
// ends mid-turn is "incomplete", never a silent empty success.
func Parse(jsonl []byte) Outcome {
	o := Outcome{Class: "incomplete"}
	sawResult := false
	var failMsg string
	sc := bufio.NewScanner(bytes.NewReader(jsonl))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e envelope
		// Only a genuinely undecodable line (or one with no type) is a parse
		// error — the fail-loud case a fixture guards. Payload shape is NOT
		// part of that contract; see the envelope comment.
		if json.Unmarshal(line, &e) != nil || e.Type == "" {
			return Outcome{Class: "parse_error", Result: "undecodable JSONL line: " + truncate(string(line), 200)}
		}
		switch e.Type {
		case "assistant.message":
			var m assistantMessage
			if json.Unmarshal(e.Data, &m) == nil {
				o.Result = m.Content
				o.Model = m.Model
				o.Turns++
			}
		case "session.usage_checkpoint":
			// Cumulative for the session; process-per-turn makes it per-dispatch.
			var u usageCheckpoint
			if json.Unmarshal(e.Data, &u) == nil {
				o.Usage.PremiumRequests = u.TotalPremiumRequests
				o.Usage.NanoAiu = u.TotalNanoAiu
			}
		case "result":
			sawResult = true
		case "session.error", "error", "model.error":
			var obj map[string]json.RawMessage
			if json.Unmarshal(e.Data, &obj) == nil {
				for _, k := range []string{"error", "message", "content"} {
					if t := flexText(obj[k]); t != "" {
						failMsg = t
						break
					}
				}
			}
			if failMsg == "" {
				failMsg = "unspecified " + e.Type
			}
		}
	}
	if failMsg != "" {
		return Outcome{Class: classifyFailure(failMsg), Result: failMsg, Model: o.Model, Usage: o.Usage, Turns: o.Turns}
	}
	if sawResult && o.Result != "" {
		o.Class = "ok"
	}
	return o
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
