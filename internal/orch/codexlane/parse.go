package codexlane

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

type Usage struct {
	Input           int64 `json:"input_tokens"`
	CachedInput     int64 `json:"cached_input_tokens"`
	Output          int64 `json:"output_tokens"`
	ReasoningOutput int64 `json:"reasoning_output_tokens"`
}

// FreshInput is input minus cached input (cached is a subset of input —
// fixture-verified against the committed CLI-0.142.5 capture).
func (u Usage) FreshInput() int64 { return u.Input - u.CachedInput }

type Outcome struct {
	Class    string // "ok"|"rate_limit"|"error"|"incomplete"|"parse_error" (+ "spawn_error"/"config_error" at Run layer)
	Result   string // last agent_message text (or the failure message)
	ThreadID string
	Usage    Usage
	Turns    int
}

// event mirrors the LIVE captured JSONL spellings (a7cfe75, CLI 0.142.5).
// Unknown event types are SKIPPED (additive drift is advisory, RS8); an
// undecodable line is parse_error (fail loud, fixture-guarded).
type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage   *Usage `json:"usage"`
	Message string `json:"message"` // error events (SYNTHETIC shape until a live capture exists)
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"` // turn.failed carries {error:{message}} (S2R-5; SYNTHETIC fixture, labeled)
}

var rateLimitHints = []string{"rate limit", "rate-limit", "usage limit", "quota", "429", "too many requests"}

// classifyFailure maps a failure message to "rate_limit" when it names the
// quota class, else "error".
func classifyFailure(msg string) string {
	low := strings.ToLower(msg)
	for _, h := range rateLimitHints {
		if strings.Contains(low, h) {
			return "rate_limit"
		}
	}
	return "error"
}

func Parse(jsonl []byte) Outcome {
	o := Outcome{Class: "incomplete"} // no turn.completed seen ⇒ the turn never finished
	sc := bufio.NewScanner(bytes.NewReader(jsonl))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e event
		if json.Unmarshal(line, &e) != nil || e.Type == "" {
			return Outcome{Class: "parse_error", Result: string(line)}
		}
		switch e.Type {
		case "thread.started":
			o.ThreadID = e.ThreadID
		case "item.completed":
			if e.Item != nil && e.Item.Type == "agent_message" {
				o.Result = e.Item.Text
			}
		case "turn.completed":
			o.Turns++
			if e.Usage != nil {
				o.Usage = *e.Usage
			}
			o.Class = "ok"
		case "turn.failed":
			// S2R-5: a rate-limit can arrive as turn.failed — without this
			// case the lane re-dispatches into a dead, degraded window.
			msg := e.Message
			if e.Error != nil && e.Error.Message != "" {
				msg = e.Error.Message
			}
			o.Result = msg
			o.Class = classifyFailure(msg)
			return o // a failed turn terminates the stream
		case "error":
			o.Result = e.Message
			o.Class = classifyFailure(e.Message)
			return o // an error event terminates the turn
		}
	}
	return o
}
