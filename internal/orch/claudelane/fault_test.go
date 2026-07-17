package claudelane

import (
	"context"
	"testing"
)

// Every fault ends in a CLASSIFIED outcome — never a panic, never an
// unclassified error (plan Task 9; relegation never loss).

func TestFaultMissingBinaryIsSpawnError(t *testing.T) {
	t.Setenv("PATH", "")    // no claude anywhere
	t.Setenv("PATHEXT", "") // windows: kill .exe resolution too
	o, _, err := Run(context.Background(), RunReq{Prompt: "x", Model: "sonnet", TimeoutSec: 5})
	if err != nil {
		t.Fatalf("spawn failure must classify, not error: %v", err)
	}
	if o.Class != "spawn_error" {
		t.Fatalf("want spawn_error, got %q (%s)", o.Class, o.Result)
	}
}

func TestFaultMalformedStdout(t *testing.T) {
	// Truncated JSON — the parse layer must classify, whatever the transport did.
	if o := Parse([]byte(`{"type":"result","subtype":"success","is_er`)); o.Class != "parse_error" {
		t.Fatalf("truncated JSON must classify parse_error, got %q", o.Class)
	}
}

func TestFaultRefusalIsRelaneHookPoint(t *testing.T) {
	raw := []byte(`{"type":"result","subtype":"success","is_error":false,"stop_reason":"refusal","result":"","modelUsage":{"claude-fable-5":{"inputTokens":10,"outputTokens":2,"costUSD":0.01}},"total_cost_usd":0.01,"num_turns":1}`)
	o := Parse(raw)
	if o.Class != "refusal" {
		t.Fatalf("refusal is the slice-2 re-lane key (R10 fallback hook point), got %q", o.Class)
	}
}

func TestFaultConfigErrorNeverSpawns(t *testing.T) {
	o, _, err := Run(context.Background(), RunReq{Prompt: "x"}) // model missing
	if err == nil || o.Class != "config_error" {
		t.Fatalf("arg-building failure must be config_error + error return: %q, %v", o.Class, err)
	}
}
