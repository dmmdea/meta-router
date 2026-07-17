package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/orch/strategy"
)

// Golden: the three net-new tools appear in tools/list.
func TestMCPStrategyToolsRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, s := range toolSchemas() {
		names[s["name"].(string)] = true
	}
	for _, want := range []string{"strategy_dispatch", "strategy_status", "strategy_cancel"} {
		if !names[want] {
			t.Errorf("tool %q missing from tools/list", want)
		}
	}
}

// strategy_status with no dispatch_id → isError, and the tool result text has no
// stray blank lines (stdout hygiene, S3R-14b).
func TestMCPStrategyStatusMissingIDIsError(t *testing.T) {
	res := callTool([]byte(`{"name":"strategy_status","arguments":{}}`))
	if !res.IsError {
		t.Fatal("strategy_status with no dispatch_id must be isError")
	}
	if strings.Contains(res.Content[0].Text, "\n\n") {
		t.Fatal("no stray blank lines in the tool result (stdout hygiene)")
	}
}

// strategy_status on an unknown id is a clean answer (no such dispatch), NOT an
// isError-crash.
func TestMCPStrategyStatusUnknownIDIsCleanAnswer(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	res := callTool([]byte(`{"name":"strategy_status","arguments":{"dispatch_id":"nope"}}`))
	if res.IsError {
		t.Fatalf("unknown id must be a clean answer, not isError: %s", res.Content[0].Text)
	}
	if !strings.Contains(res.Content[0].Text, "no such dispatch") {
		t.Fatalf("unknown id must say no such dispatch: %s", res.Content[0].Text)
	}
}

// strategy_dispatch with an INVALID IR (a forward dep) → isError with the reason,
// NOT a crash.
func TestMCPStrategyDispatchInvalidIRIsError(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	// step 0 depends on step 1 (forward dep) → Validate rejects.
	args := `{"name":"strategy_dispatch","arguments":{"goal":"g","steps":[{"id":0,"instruction":"a","deps":[1]},{"id":1,"instruction":"b","deps":[]}]}}`
	res := callTool([]byte(args))
	if !res.IsError {
		t.Fatalf("an invalid IR must be isError: %s", res.Content[0].Text)
	}
}

// strategy_dispatch with an UNKNOWN named strategy (and no steps) → a clear
// isError listing the valid template names, not a crash.
func TestMCPStrategyDispatchUnknownTemplateIsError(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	args := `{"name":"strategy_dispatch","arguments":{"goal":"g","strategy":"fanout-judge"}}`
	res := callTool([]byte(args))
	if !res.IsError {
		t.Fatalf("an unknown template must be isError: %s", res.Content[0].Text)
	}
	// the message must name the valid templates so the caller can self-correct
	for _, want := range []string{"solo", "plan-work-verify", "fan-out-judge"} {
		if !strings.Contains(res.Content[0].Text, want) {
			t.Fatalf("unknown-template error must list valid names (missing %q): %s", want, res.Content[0].Text)
		}
	}
}

// strategy_dispatch with a KNOWN named template (and no steps) expands it via
// templates.Expand → Validate → dispatch (same detached-supervisor path). We stub
// the spawn so no real child is launched.
func TestMCPStrategyDispatchNamedTemplateExpands(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	for _, name := range []string{"solo", "plan-work-verify", "cascade", "fan-out-judge", "single-critique"} {
		args := `{"name":"strategy_dispatch","arguments":{"goal":"build the thing","strategy":"` + name + `"}}`
		res := callTool([]byte(args))
		if res.IsError {
			t.Fatalf("known template %q must expand+dispatch, got isError: %s", name, res.Content[0].Text)
		}
		var env struct {
			DispatchID string `json:"dispatch_id"`
		}
		if err := json.Unmarshal([]byte(res.Content[0].Text), &env); err != nil || env.DispatchID == "" {
			t.Fatalf("template %q dispatch must return a dispatch_id: %v\n%s", name, err, res.Content[0].Text)
		}
		st, err := strategy.Load(strategyDirFor(env.DispatchID))
		if err != nil {
			t.Fatalf("template %q must persist state: %v", name, err)
		}
		if st.IR.Name != name {
			t.Fatalf("persisted IR must carry template name %q, got %q", name, st.IR.Name)
		}
	}
}

// A named template with an explicit class threads that class into the expansion:
// plan-work-verify on a hard class yields the 3-node (thinker-present) shape.
func TestMCPStrategyDispatchNamedTemplateHonorsClass(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	args := `{"name":"strategy_dispatch","arguments":{"goal":"g","strategy":"plan-work-verify","class":"hard-repo"}}`
	res := callTool([]byte(args))
	if res.IsError {
		t.Fatalf("must not be isError: %s", res.Content[0].Text)
	}
	var env struct {
		DispatchID string `json:"dispatch_id"`
	}
	_ = json.Unmarshal([]byte(res.Content[0].Text), &env)
	st, err := strategy.Load(strategyDirFor(env.DispatchID))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(st.IR.Steps) != 3 {
		t.Fatalf("hard-repo class must yield the 3-node thinker shape, got %d", len(st.IR.Steps))
	}
}

// Explicit steps[] still win over a named strategy (back-compat with Group E).
func TestMCPStrategyDispatchExplicitStepsStillWork(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	args := `{"name":"strategy_dispatch","arguments":{"goal":"g","steps":[{"id":0,"instruction":"only","deps":[]}]}}`
	res := callTool([]byte(args))
	if res.IsError {
		t.Fatalf("explicit steps[] must still dispatch: %s", res.Content[0].Text)
	}
}

// dispatchNamedStrategy (the `run --strategy` seam, R11) expands + dispatches a
// known template and persists the IR under its name. An unknown name errors with
// the valid list — never a crash.
func TestDispatchNamedStrategySeam(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	id, err := dispatchNamedStrategy("build the thing", "plan-work-verify", "hard-repo")
	if err != nil {
		t.Fatalf("known template must dispatch: %v", err)
	}
	st, err := strategy.Load(strategyDirFor(id))
	if err != nil {
		t.Fatalf("must persist state: %v", err)
	}
	if st.IR.Name != "plan-work-verify" || len(st.IR.Steps) != 3 {
		t.Fatalf("hard-repo plan-work-verify must persist 3-node named IR: %+v", st.IR)
	}

	if _, err := dispatchNamedStrategy("g", "no-such-template", ""); err == nil {
		t.Fatal("unknown template must error")
	}
}

// With no explicit class, dispatchNamedStrategy falls back to the heuristic
// classifier (goal → class) and still produces a valid dispatch.
func TestDispatchNamedStrategyClassFallback(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	id, err := dispatchNamedStrategy("summarize this document", "plan-work-verify", "")
	if err != nil {
		t.Fatalf("class fallback must dispatch: %v", err)
	}
	if _, err := strategy.Load(strategyDirFor(id)); err != nil {
		t.Fatalf("must persist: %v", err)
	}
}

// strategy_dispatch with a valid explicit steps[] mints a dispatch_id and returns
// it within the tool result (does NOT block on the child). We stub the spawn so
// no real child process is launched in the test.
func TestMCPStrategyDispatchReturnsID(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawned := ""
	spawnSupervisor = func(id string) error { spawned = id; return nil }

	args := `{"name":"strategy_dispatch","arguments":{"goal":"g","steps":[{"id":0,"instruction":"only","deps":[]}]}}`
	res := callTool([]byte(args))
	if res.IsError {
		t.Fatalf("a valid dispatch must not be isError: %s", res.Content[0].Text)
	}
	var env struct {
		DispatchID string `json:"dispatch_id"`
	}
	if err := json.Unmarshal([]byte(res.Content[0].Text), &env); err != nil {
		t.Fatalf("result must be JSON with dispatch_id: %v\n%s", err, res.Content[0].Text)
	}
	if env.DispatchID == "" {
		t.Fatalf("dispatch_id missing: %s", res.Content[0].Text)
	}
	if spawned != env.DispatchID {
		t.Fatalf("supervisor must be spawned for the minted id: spawned=%q id=%q", spawned, env.DispatchID)
	}
	// The state must have been persisted (WriteInitial) so the child can find it.
	if _, err := strategy.Load(strategyDirFor(env.DispatchID)); err != nil {
		t.Fatalf("WriteInitial must persist state for the child: %v", err)
	}
}

// strategy_cancel drops the between-step sentinel; a running dispatch's Execute
// would then set state=cancelled at the next wave. Here we just assert the
// sentinel is written and the tool answers cleanly.
func TestMCPStrategyCancelWritesSentinel(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	id := "cxl1"
	dir := strategyDirFor(id)
	_ = strategy.WriteInitial(dir, strategy.IR{Goal: "g", Steps: []strategy.Step{{ID: 0, Instruction: "x", Deps: []int{}}}}, id, timeNowUTC())
	res := callTool([]byte(`{"name":"strategy_cancel","arguments":{"dispatch_id":"cxl1"}}`))
	if res.IsError {
		t.Fatalf("cancel must answer cleanly: %s", res.Content[0].Text)
	}
	if !strategy.CancelRequested(dir) {
		t.Fatal("cancel must write the between-step sentinel")
	}
}

// Golden transcript: strategy_dispatch/status/cancel over the wire, stdout is
// JSON-only (S3R-14b). Uses the spawn stub so no child is launched.
func TestMCPStrategyGoldenTranscript(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	old := spawnSupervisor
	defer func() { spawnSupervisor = old }()
	spawnSupervisor = func(id string) error { return nil }

	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"strategy_dispatch","arguments":{"goal":"g","steps":[{"id":0,"instruction":"only","deps":[]}]}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"strategy_status","arguments":{"dispatch_id":"nope"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"strategy_cancel","arguments":{"dispatch_id":"nope"}}}`,
	}, "\n") + "\n"
	var out strings.Builder
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
	lines := nonEmptyLines(out.String())
	if len(lines) != 4 {
		t.Fatalf("want 4 responses, got %d:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "strategy_dispatch") || !strings.Contains(lines[0], "strategy_status") || !strings.Contains(lines[0], "strategy_cancel") {
		t.Fatalf("tools/list must carry the three strategy tools: %s", lines[0])
	}
	if !strings.Contains(lines[1], "dispatch_id") {
		t.Fatalf("strategy_dispatch must return a dispatch_id: %s", lines[1])
	}
}
