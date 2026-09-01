package copilotlane

import (
	"os"
	"path/filepath"
	"testing"
)

// The fixture is a SCRUBBED live capture of a full END-TO-END dispatch
// (CLI 1.0.82, 2026-09-01, via mr-orchestrate --lane copilot --live): every
// event the real run emitted, in order, with ids/paths/prompts scrubbed and
// three huge payloads emptied. Real spellings, real usage figures. It is
// deliberately NOT filtered to the events the parser consumes — that
// filtering is exactly what hid the polymorphic-payload bug (see
// parse_regression_test.go).
func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "fixtures", "copilot", "result-auto-dispatch.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseLiveFixture(t *testing.T) {
	o := Parse(fixture(t))
	if o.Class != "ok" {
		t.Fatalf("Class = %q, want ok (result: %s)", o.Class, o.Result)
	}
	if o.Result != "LANE E2E OK" {
		t.Fatalf("Result = %q", o.Result)
	}
	// MEASURED, and the reason attribution can never be taken from the
	// request: `--model auto` resolves vendor-side PER DISPATCH — the same
	// flag produced claude-sonnet-5 on one live run and gpt-5.6-terra on the
	// next. The ledger/receipt must record what actually served the turn.
	if o.Model != "gpt-5.6-terra" {
		t.Fatalf("Model attribution = %q — must come from the assistant.message event of THIS capture", o.Model)
	}
	if o.Usage.PremiumRequests != 1 {
		t.Fatalf("PremiumRequests = %d, want the provider-true 1", o.Usage.PremiumRequests)
	}
	if o.Usage.NanoAiu <= 0 {
		t.Fatalf("NanoAiu = %d, want the captured positive figure", o.Usage.NanoAiu)
	}
	if o.Turns != 1 {
		t.Fatalf("Turns = %d", o.Turns)
	}
}

// A stream that ends without the terminal `result` event is incomplete —
// never a silent empty success (the empty-Outcome hazard).
func TestParseNoResultEventIsIncomplete(t *testing.T) {
	jsonl := []byte(`{"type":"assistant.message","data":{"content":"partial","model":"claude-sonnet-5"}}` + "\n")
	if o := Parse(jsonl); o.Class != "incomplete" {
		t.Fatalf("Class = %q, want incomplete", o.Class)
	}
}

// `result` without any assistant message is equally incomplete: a dispatch
// that "finished" but said nothing must not read as ok.
func TestParseResultWithoutMessageIsIncomplete(t *testing.T) {
	jsonl := []byte(`{"type":"result","data":{}}` + "\n")
	if o := Parse(jsonl); o.Class != "incomplete" {
		t.Fatalf("Class = %q, want incomplete", o.Class)
	}
}

func TestParseUndecodableLineIsParseError(t *testing.T) {
	if o := Parse([]byte("not json at all\n")); o.Class != "parse_error" {
		t.Fatalf("Class = %q, want parse_error", o.Class)
	}
}

// Failure classification: quota-shaped messages are rate_limit (window
// pressure the ledger records), auth-shaped are auth_error (config/keyring
// problem, retry useless), anything else is error. Shapes are SYNTHETIC until
// a live failure capture exists — labeled per the codexlane precedent.
func TestParseFailureClassification(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"You have exceeded your premium request quota", "rate_limit"},
		{"429 too many requests", "rate_limit"},
		{"Bad credentials: token is invalid", "auth_error"},
		{"model exploded", "error"},
	}
	for _, c := range cases {
		jsonl := []byte(`{"type":"session.error","data":{"error":"` + c.msg + `"}}` + "\n")
		if o := Parse(jsonl); o.Class != c.want {
			t.Fatalf("%q → %q, want %q", c.msg, o.Class, c.want)
		}
	}
}
