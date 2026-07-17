package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	"github.com/dmmdea/meta-router/internal/orch/strategy"
)

// timeNowUTC is the strategy-tool clock (test seam via var? kept plain here; the
// handlers don't need injection because they never assert on the timestamp).
func timeNowUTC() time.Time { return time.Now().UTC() }

// strategyDirFor is a thin alias so cmd code + tests resolve the per-dispatch dir
// through the shared statepaths layout (MR_ORCH_STATE-aware).
func strategyDirFor(id string) string { return statepaths.StrategyDir(id) }

// spawnSupervisor launches the DETACHED supervisor child for a dispatch id
// (S3R-7). Production spawns `mr-orchestrate strategy-run <id>` via os/exec with
// Start()+Release() and NO Wait so it (a) survives the parent MCP process and
// (b) never blocks the MCP stdio transport — strategy_dispatch returns in ms.
// Injectable so tests exercise the dispatch path without a real child.
var spawnSupervisor = func(id string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve self: %w", err)
	}
	cmd := exec.Command(self, "strategy-run", id)
	// The child owns its own stdio — the MCP channel is untouched (S3R-14b). We
	// deliberately do NOT inherit the parent's stdout (the transport).
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start supervisor: %w", err)
	}
	return cmd.Process.Release() // detach: never Wait (survives the parent)
}

// buildStrategyIR converts a strategy_dispatch / --strategy request into an IR:
// explicit steps[] WIN (the slice-3 contract, back-compat with Group E);
// otherwise a named `strategy` is expanded via templates.Expand into the 3-list
// IR (Group F). An unknown template name is a clear error listing the valid
// names — never a crash. When no class is passed, the heuristic classifier picks
// one (the brain normally passes a class; this keeps the tool usable on a bare
// goal). goal is required.
func buildStrategyIR(goal, name, class string, stepsRaw json.RawMessage) (strategy.IR, error) {
	if goal == "" {
		return strategy.IR{}, fmt.Errorf("goal is required")
	}
	if hasSteps := len(stepsRaw) > 0 && string(stepsRaw) != "null"; hasSteps {
		var steps []strategy.Step
		if err := json.Unmarshal(stepsRaw, &steps); err != nil {
			return strategy.IR{}, fmt.Errorf("bad steps[]: %w", err)
		}
		return strategy.IR{Goal: goal, Name: name, Steps: steps}, nil
	}
	// No explicit steps[]: expand a named template.
	if name == "" {
		return strategy.IR{}, fmt.Errorf("strategy_dispatch needs either explicit steps[] or a named strategy (have: %v)", strategy.TemplateNames())
	}
	if class == "" {
		c, _ := router.Classify(goal, 0, false)
		class = string(c)
	}
	return strategy.Expand(name, goal, class)
}

// toolStrategyDispatch parses+validates the IR, mints a dispatch_id, WriteInitial,
// then SPAWNS the detached supervisor and returns {dispatch_id} within ms (async).
// An invalid IR is isError with the reason (NOT a crash). If the spawn fails the
// dispatch is marked failed and the error surfaces.
func toolStrategyDispatch(args json.RawMessage) toolResult {
	var a struct {
		Goal     string          `json:"goal"`
		Strategy string          `json:"strategy"`
		Class    string          `json:"class"`
		Origin   string          `json:"origin"`
		Steps    json.RawMessage `json:"steps"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return errText("bad strategy_dispatch arguments: " + err.Error())
	}
	ir, err := buildStrategyIR(a.Goal, a.Strategy, a.Class, a.Steps)
	if err != nil {
		return errText("strategy_dispatch: " + err.Error())
	}
	id, err := dispatchIR(ir)
	if err != nil {
		return errText("strategy_dispatch: " + err.Error())
	}
	b, _ := json.Marshal(map[string]any{"dispatch_id": id, "state": "working"})
	return okText(string(b))
}

// dispatchIR is the shared expand-agnostic dispatch tail: Validate → mint id →
// WriteInitial → spawn the detached supervisor → mark working. Both the
// strategy_dispatch MCP tool and `run --strategy` funnel through it so a named
// template and an explicit steps[] IR travel the SAME detached-supervisor path
// (S3R-7). A spawn failure marks the dispatch failed so strategy_status tells the
// truth rather than a stuck "pending".
func dispatchIR(ir strategy.IR) (string, error) {
	if verr := strategy.Validate(ir); verr != nil {
		return "", fmt.Errorf("invalid IR: %w", verr)
	}
	id, err := strategy.NewDispatchID()
	if err != nil {
		return "", fmt.Errorf("mint id: %w", err)
	}
	dir := strategyDirFor(id)
	if err := strategy.WriteInitial(dir, ir, id, timeNowUTC()); err != nil {
		return "", fmt.Errorf("write state: %w", err)
	}
	if err := spawnSupervisor(id); err != nil {
		_ = strategy.Mutate(dir, func(s *strategy.State) { s.State = "failed" }, timeNowUTC())
		return "", fmt.Errorf("spawn supervisor: %w", err)
	}
	_ = strategy.Mutate(dir, func(s *strategy.State) {
		if s.State == "pending" {
			s.State = "working"
		}
	}, timeNowUTC())
	return id, nil
}

// dispatchNamedStrategy expands a named template from goal+class into an IR and
// dispatches it (the `run --strategy <name>` seam, R11). When class is empty the
// heuristic classifier picks one. An unknown template name surfaces as a clear
// error (via strategy.Expand) listing the valid names.
func dispatchNamedStrategy(goal, name, class string) (string, error) {
	ir, err := buildStrategyIR(goal, name, class, nil)
	if err != nil {
		return "", err
	}
	return dispatchIR(ir)
}

// toolStrategyStatus returns the published 3-field core + S3R-7 stale detection +
// S3R-9 blocked visibility (see strategyStatusJSON). An unknown id is a clean
// answer (no such dispatch), not an isError-crash. A missing dispatch_id IS an
// isError (a client bug, not a state question).
func toolStrategyStatus(args json.RawMessage) toolResult {
	var a struct {
		DispatchID string `json:"dispatch_id"`
	}
	if json.Unmarshal(args, &a) != nil || a.DispatchID == "" {
		return errText("strategy_status: dispatch_id is required")
	}
	return okText(strategyStatusJSON(a.DispatchID))
}

// toolStrategyCancel writes the between-step cancel sentinel the supervisor
// checks between waves (a running node finishes first; no hard mid-node kill in
// slice-3). A missing dispatch_id is isError.
func toolStrategyCancel(args json.RawMessage) toolResult {
	var a struct {
		DispatchID string `json:"dispatch_id"`
	}
	if json.Unmarshal(args, &a) != nil || a.DispatchID == "" {
		return errText("strategy_cancel: dispatch_id is required")
	}
	dir := strategyDirFor(a.DispatchID)
	if err := strategy.RequestCancel(dir, timeNowUTC()); err != nil {
		return errText("strategy_cancel: " + err.Error())
	}
	return okText(`{"cancel_requested":true}`)
}
