package locallane

import (
	"context"
	"testing"
)

// ── Agent door (local-agent) ──────────────────────────────────────────────

func TestMapStopReason(t *testing.T) {
	cases := map[string]string{"done": "ok", "budget": "incomplete", "error": "api_error", "": "parse_error"}
	for stop, want := range cases {
		if got := classFromStop(stop, "some output"); got != want {
			t.Errorf("stop %q → class %q, want %q", stop, got, want)
		}
	}
	// done but empty output is not a real answer (RS6-style): empty_result.
	if got := classFromStop("done", ""); got != "empty_result" {
		t.Errorf("done+empty → %q, want empty_result", got)
	}
}

func TestRunFailsOpenOnMissingBinary(t *testing.T) {
	// A binary that cannot spawn must classify, never error (fail-open, R8 cold endpoint).
	o, _, err := Run(context.Background(), "definitely-not-a-real-binary-xyz", "hi", ".", 5)
	if err != nil {
		t.Fatalf("missing binary must fail open, got err: %v", err)
	}
	if o.Class != "spawn_error" {
		t.Fatalf("missing binary → class %q, want spawn_error", o.Class)
	}
}

// ── Cascade door (offload-harness) ────────────────────────────────────────

// TestCascadeDeferMapsToDeferred is the S3R-1 contract: the harness's structured
// DEFER (Deferred:true) → a `deferred` relegation outcome — NEVER a false `ok`,
// NEVER an error. A deferred local node lets the DAG relegate/escalate to a cloud
// alternative (the whole point of the local lane's honest-defer contract).
func TestCascadeDeferMapsToDeferred(t *testing.T) {
	raw := []byte(`{"ok":false,"deferred":true,"reason":"input exceeds local window","meta":{}}`)
	o := parseCascade(raw)
	if o.Class != "deferred" {
		t.Fatalf("structured DEFER → class %q, want deferred (relegation, never ok/error)", o.Class)
	}
	if o.Result == "" {
		t.Fatalf("deferred outcome should carry the defer reason for the receipt, got empty")
	}
}

// TestCascadeOKMapsToOK: a successful cascade result (ok:true, not deferred) → ok,
// with the result payload carried as the content.
func TestCascadeOKMapsToOK(t *testing.T) {
	raw := []byte(`{"ok":true,"deferred":false,"result":{"summary":"a short gist","bullets":["x"]},"meta":{}}`)
	o := parseCascade(raw)
	if o.Class != "ok" {
		t.Fatalf("ok cascade result → class %q, want ok", o.Class)
	}
	if o.Result == "" {
		t.Fatalf("ok cascade must carry the result payload, got empty")
	}
}

// TestParseCascadeAgainstRealCaptures pins the adapter against ACTUAL captures
// from the live offload-harness binary (smoke 2026-07-07): a success carries no
// explicit `deferred` field and puts the payload under `result`; a real DEFER is
// {"ok":false,"deferred":true,"reason":...}. These are the ground-truth wire
// shapes the door contract must never drift from.
func TestParseCascadeAgainstRealCaptures(t *testing.T) {
	okReal := []byte(`{"ok":true,"result":{"bullets":["a"],"summary":"s"},"meta":{"tokens_in":95}}`)
	if o := parseCascade(okReal); o.Class != "ok" {
		t.Fatalf("real ok capture → %q, want ok", o.Class)
	}
	deferReal := []byte(`{"ok":false,"deferred":true,"reason":"input too small to offload","meta":{}}`)
	o := parseCascade(deferReal)
	if o.Class != "deferred" {
		t.Fatalf("real DEFER capture → %q, want deferred", o.Class)
	}
	if o.Result != "input too small to offload" {
		t.Fatalf("defer reason must carry through for the receipt: %q", o.Result)
	}
}

// TestCascadeUnparseableIsParseError: garbage stdout → parse_error, never ok.
func TestCascadeUnparseableIsParseError(t *testing.T) {
	if o := parseCascade([]byte("not json at all")); o.Class != "parse_error" {
		t.Fatalf("garbage → class %q, want parse_error", o.Class)
	}
	// An ok:false / not-deferred result (a genuine local failure) is not_ok, never ok.
	raw := []byte(`{"ok":false,"deferred":false,"reason":"tier error","meta":{}}`)
	if o := parseCascade(raw); o.Class == "ok" {
		t.Fatalf("ok:false non-deferred must not be ok, got %q", o.Class)
	}
}

func TestRunCascadeFailsOpenOnMissingBinary(t *testing.T) {
	o, _, err := RunCascade(context.Background(), "definitely-not-a-real-binary-xyz", "summarize", "hi there", "", 5)
	if err != nil {
		t.Fatalf("missing binary must fail open, got err: %v", err)
	}
	if o.Class != "spawn_error" {
		t.Fatalf("missing binary → class %q, want spawn_error", o.Class)
	}
}

// ── Door routing ──────────────────────────────────────────────────────────

// TestDoorRouting pins the S3R-1 routing rule: cascade classes/models take the
// cascade door; the agentic class/model takes the agent door; the ambiguous
// default is the cascade door (cheaper/faster).
func TestDoorRouting(t *testing.T) {
	cases := []struct {
		class, model string
		wantDoor     Door
		wantVerb     string // cascade verb (empty for the agent door)
	}{
		{"mechanical-text", "gemma4-cascade", doorCascade, "summarize"},
		{"doc-summarize", "qwythos", doorCascade, "summarize"},
		{"verify-gate", "qwythos", doorCascade, "triage"},
		{"hard-case-reclaim", "qwythos-think", doorAgent, ""},
		{"", "gemma4-cascade", doorCascade, "summarize"},                       // cascade model, no class
		{"", "qwythos-think", doorAgent, ""},                                   // agentic model, no class
		{"", "", doorCascade, "summarize"},                                     // ambiguous → cascade default
		{"some-unknown-class", "some-unknown-model", doorCascade, "summarize"}, // unknown → cascade default
	}
	for _, tc := range cases {
		d, verb := resolveDoor(tc.class, tc.model)
		if d != tc.wantDoor {
			t.Errorf("resolveDoor(%q,%q) door=%v, want %v", tc.class, tc.model, d, tc.wantDoor)
		}
		if verb != tc.wantVerb {
			t.Errorf("resolveDoor(%q,%q) verb=%q, want %q", tc.class, tc.model, verb, tc.wantVerb)
		}
	}
}
