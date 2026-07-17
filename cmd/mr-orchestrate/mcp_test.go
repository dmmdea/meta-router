package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// nonEmptyLines splits s on newlines and drops blank lines — the JSON-RPC
// transport is newline-delimited and notifications emit no response line.
func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if strings.TrimSpace(ln) != "" {
			out = append(out, ln)
		}
	}
	return out
}

// assertJSONPath walks a dotted path (a.b.c) through a JSON object line and
// asserts the leaf equals want (string comparison via fmt).
func assertJSONPath(t *testing.T, line, path, want string) {
	t.Helper()
	var m any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("path %q: line is not JSON: %v\n%s", path, err, line)
	}
	cur := m
	for _, key := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %q: %q is not an object in\n%s", path, key, line)
		}
		cur, ok = obj[key]
		if !ok {
			t.Fatalf("path %q: key %q missing in\n%s", path, key, line)
		}
	}
	got, ok := cur.(string)
	if !ok || got != want {
		t.Fatalf("path %q = %v, want %q\n%s", path, cur, want, line)
	}
}

// TestServeMCPTranscript is the golden-transcript wire-format pin: a full
// initialize → notifications/initialized → tools/list → two tools/call →
// unknown-method sequence over newline-delimited JSON-RPC 2.0. It asserts the
// notification produces no response, the protocol echo, the four tool schemas,
// the hard-repo route recommendation, the published strategy_dispatch stub, and
// the -32601 for an unknown method.
func TestServeMCPTranscript(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir()) // empty state: fail-open, everything routable
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"route","arguments":{"task":"refactor the ledger","class":"hard-repo"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"strategy_dispatch","arguments":{"goal":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"nope"}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 5 { // 6 inputs, 1 notification ⇒ 5 responses
		t.Fatalf("want 5 responses, got %d:\n%s", len(lines), out.String())
	}
	assertJSONPath(t, lines[0], "result.protocolVersion", "2025-06-18")
	assertJSONPath(t, lines[0], "result.serverInfo.name", "mr-orchestrate")
	if !strings.Contains(lines[1], `"route"`) || !strings.Contains(lines[1], `"strategy_dispatch"`) ||
		!strings.Contains(lines[1], `"run"`) || !strings.Contains(lines[1], `"quota_status"`) {
		t.Fatalf("tools/list must carry all four tools: %s", lines[1])
	}
	// The route result JSON is a string inside the tools/call text content, so
	// its quotes are escaped on the wire — assert the (unambiguous) model id.
	if !strings.Contains(lines[2], `claude-opus-4-8`) {
		t.Fatalf("route tool must return the hard-repo recommendation: %s", lines[2])
	}
	// strategy_dispatch with a bare goal and NEITHER steps[] NOR a named strategy
	// is a clear isError (needs one or the other; named-template expansion IS live
	// now but requires a template name).
	if !strings.Contains(lines[3], "steps") || !strings.Contains(lines[3], `"isError":true`) {
		t.Fatalf("strategy_dispatch with no steps and no strategy must isError: %s", lines[3])
	}
	if !strings.Contains(lines[4], "-32601") {
		t.Fatalf("unknown method must -32601: %s", lines[4])
	}
}

// S2R-14(a)+(c): an initialize carrying an UNSUPPORTED protocolVersion must NOT
// be blindly echoed — the server responds with its OWN latest supported version.
func TestServeMCPUnsupportedVersionReturnsOwnLatest(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01-garbage","capabilities":{},"clientInfo":{"name":"t"}}}` + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("want 1 response, got %d:\n%s", len(lines), out.String())
	}
	// Must NOT echo the junk; must return the server's own latest.
	if strings.Contains(lines[0], "1999-01-01-garbage") {
		t.Fatalf("unsupported version must NOT be echoed: %s", lines[0])
	}
	assertJSONPath(t, lines[0], "result.protocolVersion", mcpLatestVersion)
}

// S2R-14(b)+(c): stderr-cleanliness — the ONLY thing serveMCP writes to w is
// newline-delimited JSON-RPC. Every emitted line must be a JSON object with the
// jsonrpc="2.0" field; a stray plain-text print would corrupt the transport.
func TestServeMCPWriterIsPureJSONRPC(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"quota_status","arguments":{}}}`,
	}, "\n") + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	for _, ln := range nonEmptyLines(out.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("non-JSON line on the transport (would corrupt stdio): %q", ln)
		}
		if m["jsonrpc"] != "2.0" {
			t.Fatalf("every transport line must be JSON-RPC 2.0: %q", ln)
		}
	}
}

// A2R-#6: the `run` tool advertises "outcome_class + exit_code" — the result
// envelope must CARRY the numeric exit_code, and a deferral (3), a notional
// guard (4), and a not-ok (5) must be distinguishable via exit_code even though
// 4 and 5 both map to isError:true. doRun's own output is preserved under
// "result".
func TestRunToolEnvelopeCarriesExitCode(t *testing.T) {
	type parsed struct {
		ExitCode int             `json:"exit_code"`
		Result   json.RawMessage `json:"result"`
	}
	cases := []struct {
		code        int
		out         string
		wantIsError bool
	}{
		{code: exitDeferred, out: `{"deferred":true,"reason":"exhausted"}`, wantIsError: false},
		{code: exitNotional, out: `{"outcome_class":"ok"}`, wantIsError: true},
		{code: exitNotOK, out: `{"outcome_class":"rate_limit"}`, wantIsError: true},
		{code: 0, out: `{"dry_run":true}`, wantIsError: false},
	}
	seen := map[int]bool{}
	for _, tc := range cases {
		res := runToolEnvelope(tc.code, tc.out)
		if res.IsError != tc.wantIsError {
			t.Fatalf("code %d: isError=%v want %v", tc.code, res.IsError, tc.wantIsError)
		}
		if len(res.Content) == 0 {
			t.Fatalf("code %d: no content", tc.code)
		}
		var p parsed
		if err := json.Unmarshal([]byte(res.Content[0].Text), &p); err != nil {
			t.Fatalf("code %d: envelope is not JSON: %v\n%s", tc.code, err, res.Content[0].Text)
		}
		if p.ExitCode != tc.code {
			t.Fatalf("envelope must carry exit_code=%d, got %d", tc.code, p.ExitCode)
		}
		if len(p.Result) == 0 {
			t.Fatalf("code %d: doRun output must be preserved under result", tc.code)
		}
		seen[p.ExitCode] = true
	}
	// The whole point: 4 and 5 are distinguishable via exit_code (both isError).
	if !seen[exitNotional] || !seen[exitNotOK] || seen[exitNotional] == false {
		t.Fatal("exit 4 and exit 5 must both be representable and distinguishable via exit_code")
	}
}

// A2R-#6 end-to-end: a real deferral through the MCP `run` tool (seed an
// exhausted claude ledger so gate defers) returns isError:false with exit_code
// 3 embedded in the envelope.
func TestRunToolDeferralCarriesExitCode3(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	// Exhaust the claude 7d window so an auto/claude run defers (exit 3).
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win7d, 99, time.Now().UTC().Add(48*time.Hour), time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	args := []byte(`{"name":"run","arguments":{"prompt":"do a thing","lane":"claude","model":"claude-opus-4-8","dry_run":true}}`)
	res := callTool(args)
	if len(res.Content) == 0 {
		t.Fatal("no content")
	}
	var p struct {
		ExitCode int             `json:"exit_code"`
		Result   json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &p); err != nil {
		t.Fatalf("envelope not JSON: %v\n%s", err, res.Content[0].Text)
	}
	if p.ExitCode != exitDeferred {
		t.Fatalf("an exhausted-lane run must carry exit_code=3, got %d (%s)", p.ExitCode, res.Content[0].Text)
	}
	if res.IsError {
		t.Fatalf("a deferral (3) must be isError:false (relegation is an answer): %s", res.Content[0].Text)
	}
	if !strings.Contains(string(p.Result), "deferred") {
		t.Fatalf("the deferral JSON must be preserved under result: %s", p.Result)
	}
}

// ping → empty result object; a request id must always come back.
func TestServeMCPPing(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	in := `{"jsonrpc":"2.0","id":7,"method":"ping"}` + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 || !strings.Contains(lines[0], `"id":7`) || !strings.Contains(lines[0], `"result":{}`) {
		t.Fatalf("ping must return an empty result for the id: %s", out.String())
	}
}

// quota_status tool returns the buildStatus JSON verbatim inside a text content
// block — the machine reads live ledger state through the MCP.
func TestServeMCPQuotaStatusTool(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	in := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"quota_status","arguments":{}}}` + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("want 1 response: %s", out.String())
	}
	// The result content text must itself be valid Status JSON (has lanes key).
	var resp struct {
		Result struct {
			Content []struct {
				Type, Text string
			}
			IsError bool
		}
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("bad response: %v\n%s", err, lines[0])
	}
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, `"lanes"`) {
		t.Fatalf("quota_status must return the buildStatus JSON: %s", lines[0])
	}
}

// F3 parity: the MCP quota_status surface must include the E6 quota_health block
// (runStatus emits it; the machine-facing surface must too, or a stalled quota
// signal is invisible to the machine).
func TestServeMCPQuotaStatusToolCarriesQuotaHealth(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	in := `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"quota_status","arguments":{}}}` + "\n"
	var out bytes.Buffer
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := nonEmptyLines(out.String())
	if len(lines) != 1 {
		t.Fatalf("want 1 response: %s", out.String())
	}
	var resp struct {
		Result struct {
			Content []struct {
				Type, Text string
			}
			IsError bool
		}
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("bad response: %v\n%s", err, lines[0])
	}
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, `"quota_health"`) {
		t.Fatalf("quota_status must surface quota_health (E6 parity with runStatus): %s", lines[0])
	}
}
