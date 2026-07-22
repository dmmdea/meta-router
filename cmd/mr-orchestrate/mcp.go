package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/router"
)

// MCP stdio transport (hand-rolled, stdlib-only — Task 12 justification): one
// newline-delimited JSON-RPC 2.0 message per line. The dependency-vs-hand-roll
// call and its revisit condition live in the plan; the golden-transcript test
// (mcp_test.go) pins the wire format.
//
// S2R-14: (a) initialize echoes the client's protocolVersion ONLY when it is in
// the supported set, else responds with our own latest (mcpLatestVersion) —
// never blindly echoes junk. (b) ALL diagnostics go to STDERR; the tool-result
// JSON is the ONLY thing written to the transport writer w (a single stray
// stdout print corrupts the stdio channel).
const mcpLatestVersion = "2025-06-18"

// mcpSupportedVersions is the set we will echo back on initialize. Anything
// outside it gets mcpLatestVersion instead (S2R-14a).
var mcpSupportedVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
	"2024-11-05": true,
}

// rpcReq is one inbound JSON-RPC message. A nil/absent ID marks a notification
// (no response is written). Params is deferred so each method decodes its own.
type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// toolContent is one MCP tools/call content item (text only here).
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError"`
}

// runMCP is the CLI entry: serve JSON-RPC over stdio. All diagnostics land on
// stderr; stdout is the transport (S2R-14b).
func runMCP(args []string) error {
	return serveMCP(os.Stdin, os.Stdout)
}

// serveMCP is the testable core: read newline-delimited JSON-RPC from r, write
// responses to w. Notifications get no response line. Fail-open: a malformed
// line is answered with a parse error (when an id can be recovered) or skipped;
// a scan error ends the loop cleanly.
func serveMCP(r io.Reader, w io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024) // 4MB lines
	enc := json.NewEncoder(w)

	reply := func(id json.RawMessage, result any) {
		if len(id) == 0 {
			return // notification: no response
		}
		_ = enc.Encode(rpcResp{JSONRPC: "2.0", ID: id, Result: result})
	}
	replyErr := func(id json.RawMessage, code int, msg string) {
		if len(id) == 0 {
			return
		}
		_ = enc.Encode(rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
	}

	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcReq
		if err := json.Unmarshal(line, &req); err != nil {
			// -32700 parse error; no id to bind → skip (diagnostic to stderr).
			fmt.Fprintln(os.Stderr, "mcp: parse error, skipping line:", err)
			continue
		}
		switch req.Method {
		case "initialize":
			reply(req.ID, initializeResult(req.Params))
		case "notifications/initialized":
			// notification, ignored
		case "ping":
			reply(req.ID, map[string]any{})
		case "tools/list":
			reply(req.ID, map[string]any{"tools": toolSchemas()})
		case "tools/call":
			res := callTool(req.Params)
			reply(req.ID, res)
		default:
			replyErr(req.ID, -32601, "method not found: "+req.Method)
		}
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "mcp: read error:", err)
		return err
	}
	return nil
}

// initializeResult builds the initialize response. S2R-14a: echo the client's
// protocolVersion only when supported, else our own latest.
func initializeResult(params json.RawMessage) map[string]any {
	ver := mcpLatestVersion
	if len(params) > 0 {
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if json.Unmarshal(params, &p) == nil && mcpSupportedVersions[p.ProtocolVersion] {
			ver = p.ProtocolVersion
		}
	}
	return map[string]any{
		"protocolVersion": ver,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": "mr-orchestrate", "version": version},
	}
}

// toolSchemas is the tools/list payload: four tools behind three JSON-RPC
// methods. Kept as literal maps so the schema is the wire contract, not an
// abstraction over it.
func toolSchemas() []map[string]any {
	strProp := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	numProp := func(desc string) map[string]any { return map[string]any{"type": "number", "description": desc} }
	boolProp := func(desc string) map[string]any { return map[string]any{"type": "boolean", "description": desc} }
	return []map[string]any{
		{
			"name":        "route",
			"description": "Deterministic quota-masked routing recommendation (read-only oracle; no LLM). Consult BEFORE delegating work out of the session.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"task":              strProp("one-line task description (classifier input)"),
					"class":             strProp("explicit task class (skips the heuristic classifier)"),
					"ctx_tokens":        numProp("estimated input context tokens (ctx-cap masks)"),
					"latency_sensitive": boolProp("prefer the low-latency lane"),
					"batch":             boolProp("E2 spend-down tag: an already-queued BATCH task (never set for interactive work); enables the under-utilized-window rank boost"),
					"est_minutes":       numProp("expected task duration in minutes (E2 completion-fit gate; 0 = unknown → no boost)"),
				},
				"required": []string{"task"},
			},
		},
		{
			"name":        "run",
			"description": "Dispatch a prompt on the recommended lane (defaults lane=auto — the MCP caller IS the delegation path). Returns {\"exit_code\":N,\"result\":<run result JSON, carrying outcome_class>}; exit_code distinguishes 0 ok · 3 deferred · 4 notional-guard · 5 outcome-not-ok (4 and 5 are both isError but tell apart via exit_code).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"prompt":      strProp("the prompt to dispatch (required)"),
					"lane":        strProp("dispatch lane: auto|claude|codex|glm (default auto)"),
					"model":       strProp("pin a model (overrides the recommendation's model)"),
					"effort":      strProp("effort passthrough"),
					"class":       strProp("task class for the internal recommendation"),
					"cwd":         strProp("working directory for the lane-binary call"),
					"timeout_sec": numProp("hard timeout for the lane-binary call"),
					"dry_run":     boolProp("print the admission decision + args without dispatching (default false)"),
					"batch":       boolProp("E2 spend-down tag: an already-queued BATCH task (never set for interactive work); enables the under-utilized-window rank boost"),
					"est_minutes": numProp("expected task duration in minutes (E2 completion-fit gate; 0 = unknown → no boost)"),
				},
				"required": []string{"prompt"},
			},
		},
		{
			"name":        "quota_status",
			"description": "The live per-lane quota status (the buildStatus JSON verbatim).",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
		},
		{
			"name":        "strategy_dispatch",
			"description": "Async multi-step strategy dispatch. Accepts EITHER an explicit steps[] IR (id/instruction/class/lane_hint/model_hint/effort_hint/deps) OR a named strategy template (solo|plan-work-verify|cascade|fan-out-judge|single-critique) expanded from goal+class. Spawns a DETACHED supervisor and returns {dispatch_id} immediately. Poll strategy_status(dispatch_id).",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"goal":     strProp("the strategy goal (required)"),
					"strategy": strProp("named strategy template: solo|plan-work-verify|cascade|fan-out-judge|single-critique (used when no steps[] given)"),
					"class":    strProp("task class for named-template expansion (absent → heuristic classifier on goal)"),
					"origin":   strProp("receipt origin tag (default strategy)"),
					"steps":    map[string]any{"type": "array", "description": "explicit step list (the slice-3 IR); wins over a named strategy"},
				},
				"required": []string{"goal"},
			},
		},
		{
			"name":        "strategy_status",
			"description": "Poll an async strategy dispatch. Returns {state, step_receipts[], result_ref}; a stale (dead-supervisor) dispatch reports state:needs_resume + resume_cmd; a blocked dispatch surfaces a top-level blocked_step {step_id, lane, outcome_class, notional_usd} for a 5-second operator decision.",
			"inputSchema": map[string]any{"type": "object",
				"properties": map[string]any{"dispatch_id": strProp("the dispatch_id from strategy_dispatch (required)")},
				"required":   []string{"dispatch_id"}},
		},
		{
			"name":        "strategy_cancel",
			"description": "Request cancellation of an async strategy dispatch (between-step sentinel; a running node finishes first, then no new node starts and state→cancelled).",
			"inputSchema": map[string]any{"type": "object",
				"properties": map[string]any{"dispatch_id": strProp("the dispatch_id to cancel (required)")},
				"required":   []string{"dispatch_id"}},
		},
	}
}

// callTool dispatches a tools/call by name. Unknown tool → isError text.
func callTool(params json.RawMessage) toolResult {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return errText("bad tools/call params: " + err.Error())
	}
	switch call.Name {
	case "route":
		return toolRoute(call.Arguments)
	case "run":
		return toolRun(call.Arguments)
	case "quota_status":
		return toolQuotaStatus()
	case "strategy_dispatch":
		return toolStrategyDispatch(call.Arguments)
	case "strategy_status":
		return toolStrategyStatus(call.Arguments)
	case "strategy_cancel":
		return toolStrategyCancel(call.Arguments)
	default:
		return errText("unknown tool: " + call.Name)
	}
}

func okText(s string) toolResult { return toolResult{Content: []toolContent{{Type: "text", Text: s}}} }
func errText(s string) toolResult {
	return toolResult{Content: []toolContent{{Type: "text", Text: s}}, IsError: true}
}

// toolRoute runs the deterministic oracle and returns the route JSON. The
// consult receipt carries Origin "mcp" (S2R-1: mcp-origin dispatches are the
// coverage numerator). A relegation (all masked) is NOT an error — it is an
// answer with resume_at.
func toolRoute(args json.RawMessage) toolResult {
	var a struct {
		Task       string `json:"task"`
		Class      string `json:"class"`
		CtxTokens  int64  `json:"ctx_tokens"`
		Latency    bool   `json:"latency_sensitive"`
		Batch      bool   `json:"batch"`
		EstMinutes int64  `json:"est_minutes"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errText("bad route arguments: " + err.Error())
	}
	if a.Task == "" && a.Class == "" {
		return errText("route: task is required")
	}
	now := time.Now().UTC()
	cfg := orchcfg.Load(configPath())
	fzs, _ := fuses.Load(fusesPath())

	// RS1: ingest the statusline drop so interactive Claude usage participates
	// in the mask. Fail-open — a bad drop never breaks the oracle.
	var snap []ledger.Bucket
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		if _, note, ierr := quotasig.IngestTraced(l, dropPath(), quotaTracePath(), "claude", now); ierr != nil {
			fmt.Fprintln(os.Stderr, "mcp route warn: statusline drop unreadable:", ierr)
		} else if note != "" {
			fmt.Fprintln(os.Stderr, "mcp route warn:", note)
		}
		snap = l.Snapshot()
	}); err != nil {
		fmt.Fprintln(os.Stderr, "mcp route warn: ledger update failed, reading read-only snapshot:", err)
		l, warn := ledger.OpenChecked(ledgerPath())
		if warn != "" {
			fmt.Fprintln(os.Stderr, "mcp route warn:", warn)
		}
		snap = l.Snapshot()
	}

	var class router.Class
	if a.Class != "" {
		class = router.Class(a.Class)
	} else {
		class, _ = router.Classify(a.Task, a.CtxTokens, a.Latency)
	}
	d := buildRouteDecision(cfg, fzs, snap, class, a.CtxTokens, now, a.Batch, time.Duration(a.EstMinutes)*time.Minute)

	// Consult receipt (Origin "mcp"): the delegation-coverage numerator.
	writeRouteReceipt(d, class, a.Task, "mcp", now)

	if d.Lane == "" {
		dj := deferral{Deferred: true, Reason: d.Reason}
		if !d.ResumeAt.IsZero() {
			t := d.ResumeAt
			dj.ResumeAt = &t
		}
		b, _ := json.MarshalIndent(dj, "", "  ")
		return okText(string(b)) // relegation is an answer, not an error
	}
	return okText(string(routeJSON(d)))
}

// toolRun dispatches via the shared doRun core with lane defaulted to "auto"
// and Origin "mcp". The exit code maps to isError: 1/4/5 are errors; a deferral
// (3) is NOT an error (relegation is an answer). doRun writes the result JSON to
// a buffer — the ONLY thing that reaches the transport is this tool result.
func toolRun(args json.RawMessage) toolResult {
	var a struct {
		Prompt     string `json:"prompt"`
		Lane       string `json:"lane"`
		Model      string `json:"model"`
		Effort     string `json:"effort"`
		Class      string `json:"class"`
		CWD        string `json:"cwd"`
		TimeoutSec int    `json:"timeout_sec"`
		DryRun     bool   `json:"dry_run"`
		Batch      bool   `json:"batch"`
		EstMinutes int64  `json:"est_minutes"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errText("bad run arguments: " + err.Error())
	}
	if a.Prompt == "" {
		return errText("run: prompt is required")
	}
	lane := a.Lane
	if lane == "" {
		lane = "auto" // MCP callers ARE the delegation path (unlike the CLI's "claude")
	}
	var buf bytes.Buffer
	code, err := doRun(runOpts{
		Prompt: a.Prompt, Lane: lane, Model: a.Model, Effort: a.Effort, Class: a.Class,
		CWD: a.CWD, TimeoutSec: a.TimeoutSec, Live: !a.DryRun, Desc: descFromPrompt(a.Prompt),
		Origin: "mcp", Batch: a.Batch, EstMinutes: a.EstMinutes,
	}, &buf)
	if err != nil {
		return errText(err.Error()) // config_error (exit 1)
	}
	return runToolEnvelope(code, buf.String())
}

// runToolEnvelope wraps doRun's exit code + output into the `run` tool result
// (A2R-#6). The tool advertises "outcome_class + exit_code", so the numeric
// exit_code is surfaced in the content: 4 (notional guard) and 5 (dispatched
// but outcome not ok) both map to isError:true, yet a client MUST be able to
// tell them apart — the exit_code field is that discriminator. doRun's own
// output is nested under "result" (parsed when it is JSON, else the raw
// string) so no information is lost.
func runToolEnvelope(code int, out string) toolResult {
	trimmed := strings.TrimSpace(out)
	env := map[string]any{"exit_code": code}
	if trimmed == "" {
		env["result"] = nil
	} else if json.Valid([]byte(trimmed)) {
		env["result"] = json.RawMessage(trimmed)
	} else {
		env["result"] = trimmed
	}
	b, err := json.Marshal(env)
	if err != nil { // never expected; degrade to the raw output
		b = []byte(out)
	}
	// isError for 1/4/5; a deferral (3) is isError:false (relegation is an answer).
	return toolResult{
		Content: []toolContent{{Type: "text", Text: string(b)}},
		IsError: code == 1 || code == exitNotional || code == exitNotOK,
	}
}

// descFromPrompt is the S2R-9 receipt Desc for MCP runs: the prompt's leading
// slice (the replay substrate needs a task reference; the full prompt is not
// receipt material). Kept short and single-line.
func descFromPrompt(prompt string) string {
	const max = 120
	d := prompt
	if i := indexNewline(d); i >= 0 {
		d = d[:i]
	}
	if len(d) > max {
		d = d[:max]
	}
	return d
}

func indexNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			return i
		}
	}
	return -1
}

// toolQuotaStatus returns the buildStatus JSON verbatim. Read-only; no receipt.
func toolQuotaStatus() toolResult {
	now := time.Now().UTC()
	l, warn := ledger.OpenChecked(ledgerPath())
	if warn != "" {
		fmt.Fprintln(os.Stderr, "mcp quota_status warn:", warn)
	}
	fzs, _ := fuses.Load(fusesPath())
	snap := l.Snapshot()
	cfg := orchcfg.Load(configPath())
	samples := calib.Load(quotaTracePath())
	down := burnDownshiftByLane(snap, samples, cfg, now)
	st := buildStatus(snap, fzs, cfg, now, down, spendDownArmedByLane(snap, samples, cfg, now))
	// Parity with runStatus: the E6 quota_health signal-liveness block must be
	// visible on the machine-facing MCP surface too, or a stalled quota signal is
	// invisible to the machine that reads quota_status.
	st.QuotaHealth = buildQuotaHealth(snap, quotaTracePath(), cfg, now)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return errText("quota_status marshal: " + err.Error())
	}
	return okText(string(b))
}
