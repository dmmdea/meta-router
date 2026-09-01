package copilotlane

import (
	"encoding/json"
	"testing"
)

// REGRESSION (2026-09-01, found by live E2E — a successful dispatch parsed as
// parse_error). The stream is POLYMORPHIC: `data.message` is a plain string
// on some events and an OBJECT on `model.message`. A single flat struct over
// all events fails json.Unmarshal on a perfectly good stream. The fixture now
// carries every event the real run emitted (a filtered fixture is what hid
// this), and this test pins the specific shape.
func TestParseSurvivesPolymorphicMessagePayload(t *testing.T) {
	jsonl := []byte(
		`{"type":"model.message","data":{"message":{"role":"assistant","content":"hi","refusal":null}}}` + "\n" +
			`{"type":"assistant.message","data":{"content":"hi","model":"claude-sonnet-5"}}` + "\n" +
			`{"type":"session.usage_checkpoint","data":{"totalPremiumRequests":1,"totalNanoAiu":42}}` + "\n" +
			`{"type":"result","data":{}}` + "\n")
	o := Parse(jsonl)
	if o.Class != "ok" {
		t.Fatalf("object-shaped data.message must not break the parse: Class=%q Result=%q", o.Class, o.Result)
	}
	if o.Usage.PremiumRequests != 1 || o.Result != "hi" {
		t.Fatalf("payload lost: %+v", o)
	}
}

// The fixture must contain model.message — the event whose shape caused the
// bug. Pinning this stops a future "tidy the fixture" from re-hiding it.
func TestFixtureContainsThePolymorphicEvent(t *testing.T) {
	found := false
	for _, line := range splitLines(fixture(t)) {
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(line, &e) == nil && e.Type == "model.message" {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture must keep model.message: a fixture filtered to the events the parser already handles cannot catch what it does not")
	}
}

// Unrelated vendor drift (an added field, a retyped field on an event we do
// not consume) must NOT fail a dispatch — RS8: additive drift is advisory.
func TestParseIgnoresDriftOnUnconsumedEvents(t *testing.T) {
	jsonl := []byte(
		`{"type":"some.future.event","data":{"anything":[1,2,3],"nested":{"deep":true}}}` + "\n" +
			`{"type":"assistant.message","data":{"content":"ok","model":"m"}}` + "\n" +
			`{"type":"result","data":{}}` + "\n")
	if o := Parse(jsonl); o.Class != "ok" {
		t.Fatalf("unknown-event drift broke the parse: %+v", o)
	}
}

func splitLines(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == '\n' {
			if i > start {
				out = append(out, b[start:i])
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, b[start:])
	}
	return out
}
