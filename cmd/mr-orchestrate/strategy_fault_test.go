package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Group G — fault matrix roll-up (cmd/mr-orchestrate boundary rows). Every row
// proves a fault is CLASSIFIED / isError / a clean answer, NEVER a crash or a
// non-JSON byte on the MCP transport. Rows already owned by a Group A–F test are
// NAMED in the evidence doc; this file adds the missing consolidation rows.

// FAULT ROW (mcp, IR validation table): strategy_dispatch with EVERY malformed
// IR shape returns isError with a reason and NEVER crashes the tool call.
// TestMCPStrategyDispatchInvalidIRIsError covers one shape (forward-dep); this is
// the consolidated table over the whole invalid space so no shape regresses into
// a panic or a false success.
func TestFaultStrategyDispatchEveryInvalidIRIsError(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	cases := map[string]string{
		"empty":            `{"goal":"g","steps":[]}`,
		"over-5-steps":     `{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[]},{"id":1,"instruction":"b","deps":[0]},{"id":2,"instruction":"c","deps":[1]},{"id":3,"instruction":"d","deps":[2]},{"id":4,"instruction":"e","deps":[3]},{"id":5,"instruction":"f","deps":[4]}]}`,
		"cyclic-forward":   `{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[1]},{"id":1,"instruction":"b","deps":[]}]}`,
		"self-dep":         `{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[0]}]}`,
		"same-lane-fanout": `{"goal":"g","steps":[{"id":0,"instruction":"a","lane_hint":"glm","deps":[]},{"id":1,"instruction":"b","lane_hint":"glm","deps":[]},{"id":2,"instruction":"j","deps":[0,1]}]}`,
		"dep-out-of-range": `{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[9]}]}`,
		"orphan-sink":      `{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[]},{"id":1,"instruction":"b","deps":[0]},{"id":2,"instruction":"c","deps":[0]}]}`,
	}
	for name, args := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("strategy_dispatch(%s) PANICKED (%v) — an invalid IR must be isError, never a crash", name, r)
				}
			}()
			res := callTool([]byte(`{"name":"strategy_dispatch","arguments":` + args + `}`))
			if !res.IsError {
				t.Errorf("strategy_dispatch(%s) must be isError, got ok: %s", name, res.Content[0].Text)
			}
		}()
	}
}

// FAULT ROW (mcp, bad steps[] JSON): a steps[] that is not even valid JSON is
// isError (a client bug), not a crash — the tool degrades to a reason string.
func TestFaultStrategyDispatchBadStepsJSONIsError(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	res := callTool([]byte(`{"name":"strategy_dispatch","arguments":{"goal":"g","steps":"not-an-array"}}`))
	if !res.IsError {
		t.Fatalf("malformed steps[] must be isError: %s", res.Content[0].Text)
	}
}

// FAULT ROW (mcp, status unknown id): strategy_status on an unknown id is the
// clean {error:"no such dispatch"} answer, NOT an isError-crash.
// TestMCPStrategyStatusUnknownIDIsCleanAnswer owns the primary assertion; this
// row additionally pins that the answer body PARSES as JSON (no torn transport)
// and that a MISSING dispatch_id is the distinct isError client-bug case.
func TestFaultStrategyStatusUnknownIDCleanJSONAnswer(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	res := callTool([]byte(`{"name":"strategy_status","arguments":{"dispatch_id":"deadbeefdeadbeef"}}`))
	if res.IsError {
		t.Fatalf("unknown id must be a clean answer, not isError: %s", res.Content[0].Text)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &m); err != nil {
		t.Fatalf("the clean answer must be valid JSON: %v (%q)", err, res.Content[0].Text)
	}
	if m["error"] != "no such dispatch" {
		t.Fatalf("unknown id must report no such dispatch, got %v", m["error"])
	}
	// A MISSING dispatch_id is the distinct client-bug case → isError.
	miss := callTool([]byte(`{"name":"strategy_status","arguments":{}}`))
	if !miss.IsError {
		t.Fatalf("a missing dispatch_id must be isError (client bug): %s", miss.Content[0].Text)
	}
}

// FAULT ROW (mcp hygiene, S3R-14b): under a FAULT-heavy transcript (invalid IR +
// unknown id + missing id + bad-json line), stdout stays JSON-only — every
// emitted line parses as JSON-RPC 2.0. A fault must never leak a bare error
// string onto the stdio transport (which would corrupt the MCP channel).
func TestFaultMCPStdoutStaysJSONOnlyUnderFaults(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"strategy_dispatch","arguments":{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[1]},{"id":1,"instruction":"b","deps":[]}]}}}`, // invalid IR
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"strategy_status","arguments":{"dispatch_id":"nope"}}}`,                                                                              // unknown id
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"strategy_status","arguments":{}}}`,                                                                                                  // missing id (isError)
		`this is not json at all`, // garbage line — must be skipped, not crash
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"strategy_dispatch","arguments":{"goal":"g","strategy":"no-such-template"}}}`, // unknown template
	}, "\n") + "\n"

	var out strings.Builder
	if err := serveMCP(strings.NewReader(in), &out); err != nil {
		t.Fatalf("a fault-heavy transcript must not error the server: %v", err)
	}
	for _, ln := range nonEmptyLines(out.String()) {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			t.Fatalf("non-JSON line on the transport under faults (would corrupt stdio): %q", ln)
		}
		if m["jsonrpc"] != "2.0" {
			t.Fatalf("every transport line must be JSON-RPC 2.0: %q", ln)
		}
	}
	// The garbage line is skipped (not answered); the four well-formed requests
	// are each answered exactly once.
	if got := len(nonEmptyLines(out.String())); got != 4 {
		t.Fatalf("want 4 responses (garbage line skipped), got %d:\n%s", got, out.String())
	}
}

// FAULT ROW (local cold/missing binary, DAG-level, R8 fail-open): with the local
// binaries NOT on PATH, a `run --lane local --live` classifies spawn_error and
// exits cleanly (no panic). TestRunLocalLaneFailOpenAppendsOneReceiptNoLedger
// owns the receipt+no-ledger assertion; this row pins the exit-code + no-panic
// contract explicitly for the fault matrix. spawn_error is a genuine hard-fail
// (kindHardFail) → re-lane-eligible in a DAG, so the DAG never wedges.
func TestFaultLocalMissingBinaryClassifiesSpawnErrorNoPanic(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	t.Setenv("PATH", "") // strip both local binaries from the search path
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a cold/missing local binary must classify, never panic: %v", r)
		}
	}()
	var out strings.Builder
	// Cascade door (default class) — the harness binary is unreachable.
	code, err := runLocalLane(&out, "reverse this string", "mechanical-text", "", "", 5, true, "cli", "fault test", recFields{}, strategyFields{})
	if err != nil {
		t.Fatalf("fail-open: a missing local binary is a classified Outcome, not a Go error: %v", err)
	}
	if code != exitNotOK {
		t.Fatalf("a spawn_error hard-fail must exit %d, got %d (out=%s)", exitNotOK, code, out.String())
	}
	// The one receipt carries spawn_error — the countable fault substrate.
	recs := loadReceipts(dispatchPath())
	if len(recs) != 1 || recs[0].OutcomeClass != "spawn_error" {
		t.Fatalf("a missing local binary must append one spawn_error receipt: %+v", recs)
	}
}
