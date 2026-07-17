package main

import (
	"strings"
	"testing"
)

const miniFixture = `{"type":"result","total_cost_usd":0.1,"modelUsage":{"claude-sonnet-5":{"inputTokens":1,"costUSD":0.1}},"usage":{"output_tokens":4,"iterations":[{"input_tokens":1}]}}`

func TestKeyPathsNormalizeModelUsage(t *testing.T) {
	keys, err := jsonKeys([]byte(miniFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type", "modelUsage.*.inputTokens", "usage.output_tokens", "usage.iterations.input_tokens"} {
		if !keys[want] {
			t.Fatalf("missing path %q in %v", want, keys)
		}
	}
	if keys["modelUsage.claude-sonnet-5.inputTokens"] {
		t.Fatal("model names are data, not schema — must normalize to *")
	}
}

func TestVerifySchemaDetectsBreakingRemoval(t *testing.T) {
	live := `{"type":"result","modelUsage":{"claude-opus-4-8":{"inputTokens":1,"costUSD":0.1}},"usage":{"output_tokens":4,"iterations":[{"input_tokens":1}]}}`
	report, breaking, err := verifySchema([]byte(miniFixture), []byte(live))
	if err != nil {
		t.Fatal(err)
	}
	if !breaking || !strings.Contains(report, "REMOVED (breaking): total_cost_usd") {
		t.Fatalf("dropped total_cost_usd must break the gate:\n%s", report)
	}
}

func TestVerifySchemaAdditiveIsAdvisory(t *testing.T) {
	live := miniFixture[:len(miniFixture)-1] + `,"new_field":1}`
	report, breaking, err := verifySchema([]byte(miniFixture), []byte(live))
	if err != nil {
		t.Fatal(err)
	}
	if breaking || !strings.Contains(report, "added (advisory): new_field") {
		t.Fatalf("additive drift must warn, not fail:\n%s", report)
	}
}

func TestVerifySchemaStable(t *testing.T) {
	// Different model name + values, same shape.
	live := `{"type":"result","total_cost_usd":9.9,"modelUsage":{"claude-opus-4-8":{"inputTokens":7,"costUSD":9.9}},"usage":{"output_tokens":1,"iterations":[{"input_tokens":7}]}}`
	report, breaking, err := verifySchema([]byte(miniFixture), []byte(live))
	if err != nil || breaking || !strings.Contains(report, "schema stable") {
		t.Fatalf("same shape must be stable (%v):\n%s", err, report)
	}
}

func TestStripHTMLIgnoresScriptChurn(t *testing.T) {
	a := `<html><head><script nonce="abc123">var x=1;</script><style>.a{}</style></head><body><h1>Usage policy</h1><p>Subscription covers claude -p.</p></body></html>`
	b := `<html><head><script nonce="zzz999">var y=2;</script></head><body><h1>Usage  policy</h1> <p>Subscription covers claude -p.</p></body></html>`
	if hashText(stripHTMLText([]byte(a))) != hashText(stripHTMLText([]byte(b))) {
		t.Fatal("script/nonce churn must not change the policy hash")
	}
	c := strings.Replace(a, "covers claude -p", "NO LONGER covers claude -p", 1)
	if hashText(stripHTMLText([]byte(a))) == hashText(stripHTMLText([]byte(c))) {
		t.Fatal("a real policy change MUST change the hash")
	}
}

// evalPolicy seed/alert/latch coverage lives in policywatch_test.go (the
// multi-vendor observed-struct API).
