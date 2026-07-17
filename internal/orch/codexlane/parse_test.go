package codexlane

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/fixtures/codex/" + name)
	if err != nil {
		t.Fatalf("fixture %s missing (committed at a7cfe75): %v", name, err)
	}
	return b
}

func TestParseLiveFixture(t *testing.T) { // against the COMMITTED CLI-0.142.5 capture
	o := Parse(fixture(t, "exec-events.jsonl"))
	if o.Class != "ok" || o.Result != "ok" {
		t.Fatalf("live fixture must classify ok: %+v", o)
	}
	if o.Usage.Input != 18487 || o.Usage.CachedInput != 18176 || o.Usage.Output != 5 {
		t.Fatalf("usage must come from turn.completed verbatim: %+v", o.Usage)
	}
	if o.Usage.FreshInput() != 311 {
		t.Fatalf("fresh input = input - cached: %d", o.Usage.FreshInput())
	}
}

func TestParseErrorEvents(t *testing.T) {
	// SYNTHETIC shapes (no live error capture exists; fact-refresh event
	// vocabulary thread/turn/item/error). RS8 verify-codex catches real drift.
	o := Parse(fixture(t, "exec-events-error.jsonl"))
	if o.Class != "rate_limit" {
		t.Fatalf("usage-limit error text must classify rate_limit: %+v", o)
	}
}

// S2R-5: a codex rate-limit can arrive as turn.failed (documented real event;
// this fixture line is SYNTHETIC and labeled). Without this case the lane
// re-dispatches into a dead, degraded window instead of writing the RS5
// exhaustion observation.
func TestParseTurnFailedClassifiesRateLimit(t *testing.T) {
	o := Parse(fixture(t, "exec-events-turnfailed.jsonl"))
	if o.Class != "rate_limit" {
		t.Fatalf("turn.failed with a usage-limit message must classify rate_limit: %+v", o)
	}
	if o.Result == "" {
		t.Fatalf("the failure message must surface in Result: %+v", o)
	}
	// A non-rate-limit turn.failed is a plain error, not a quota signal.
	plain := []byte(`{"type":"thread.started","thread_id":"SYNTHETIC"}` + "\n" +
		`{"type":"turn.failed","error":{"message":"model exploded"}}` + "\n")
	if o := Parse(plain); o.Class != "error" || o.Result != "model exploded" {
		t.Fatalf("non-quota turn.failed must classify error: %+v", o)
	}
}

func TestParseIncompleteAndGarbage(t *testing.T) {
	if o := Parse([]byte(`{"type":"thread.started","thread_id":"x"}` + "\n")); o.Class != "incomplete" {
		t.Fatalf("no turn.completed must classify incomplete, got %q", o.Class)
	}
	if o := Parse([]byte("not json\n")); o.Class != "parse_error" {
		t.Fatalf("garbage must classify parse_error, got %q", o.Class)
	}
}
