package claudelane

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/fixtures/claude/" + name)
	if err != nil {
		t.Skipf("fixture %s not captured yet (run: mr-orchestrate probe)", name)
	}
	return b
}

func TestParseSuccessFixture(t *testing.T) {
	o := Parse(fixture(t, "result-sonnet.json"))
	if o.Class != "ok" {
		t.Fatalf("want ok, got %q", o.Class)
	}
	if len(o.ModelUsage) == 0 {
		t.Fatal("modelUsage must be attributed (silent-fallback detection depends on it)")
	}
	if o.NotionalUSD <= 0 {
		t.Fatal("total_cost_usd should parse as notional")
	}
	if o.TotalTokens() <= 0 {
		t.Fatal("token totals feed shadow accounting; must be nonzero on a real run")
	}
}

// The fable fixture proves per-model attribution: the run was pinned to
// claude-fable-5 and the modelUsage key must say which model ACTUALLY answered
// (the silent Opus-4.8 fallback case would show a different key).
func TestParseFableAttribution(t *testing.T) {
	o := Parse(fixture(t, "result-claude-fable-5.json"))
	if o.Class != "ok" {
		t.Fatalf("want ok, got %q", o.Class)
	}
	if _, hit := o.ModelUsage["claude-fable-5"]; !hit {
		t.Fatalf("expected claude-fable-5 attribution, got keys %v", keys(o.ModelUsage))
	}
}

func TestParseRefusal(t *testing.T) {
	// Inline minimal refusal shape (verified live 2026-07-05: HTTP-200 + stop_reason refusal).
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"refusal","result":"","modelUsage":{"claude-fable-5":{"inputTokens":10,"outputTokens":2,"costUSD":0.01}},"total_cost_usd":0.01,"num_turns":1}`)
	if o := Parse(raw); o.Class != "refusal" {
		t.Fatalf("refusal must be classified (exit codes will NOT catch it), got %q", o.Class)
	}
}

// RS6: ok-shaped result with empty result text + nonzero tokens is a real,
// billed-but-useless outcome class (claude-code #38623) — never counted "ok".
func TestParseEmptyResult(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"end_turn","result":"","modelUsage":{"claude-sonnet-5":{"inputTokens":900,"outputTokens":0,"costUSD":0.002}},"total_cost_usd":0.002,"num_turns":1}`)
	if o := Parse(raw); o.Class != "empty_result" {
		t.Fatalf("empty-but-billed must classify empty_result (RS6), got %q", o.Class)
	}
}

func TestParseRateLimit(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"","total_cost_usd":0,"num_turns":0}`)
	if o := Parse(raw); o.Class != "rate_limit" {
		t.Fatalf("429 must classify rate_limit, got %q", o.Class)
	}
}

func TestParseGarbage(t *testing.T) {
	if o := Parse([]byte("not json")); o.Class != "parse_error" {
		t.Fatalf("garbage must classify parse_error, got %q", o.Class)
	}
}

func keys(m map[string]ModelUse) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
