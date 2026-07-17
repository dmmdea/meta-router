// mr-orchestrate — meta-router v3: quota-aware orchestrator over the official
// headless binaries. Shipped: slice 1 (Claude lane + ledger + admission), slice 2
// (Codex+GLM lanes + rank-table router + MCP surfaces), slice 3 (strategy engine),
// slice 4 autonomous units (burn-rate downshift + quota health + jitter + oracle
// audit gate). Governing spec: docs/specs/2026-07-05-v3-intent.md (R1–R15).
package main

import (
	"fmt"
	"os"
)

var version = "0.4.0-slice4"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "version", "--version", "-v":
		fmt.Println("mr-orchestrate", version)
	case "status":
		err = runStatus(os.Args[2:])
	case "probe":
		err = runProbe(os.Args[2:])
	case "run":
		err = runRun(os.Args[2:])
	case "route":
		err = runRoute(os.Args[2:])
	case "feedback":
		err = runFeedback(os.Args[2:])
	case "mcp":
		err = runMCP(os.Args[2:])
	case "strategy-run":
		err = runStrategyRun(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: mr-orchestrate <version|status|probe|run|route|feedback|mcp|strategy-run> [flags]
  status --json          per-lane window headroom + resets + receipts audit summary
  probe claude [flags]   capture sanitized live fixtures (authorized probes only)
  run "<prompt>" [--lane claude|codex|glm|auto] --model <id> [--effort e] [--live] [--force]
  route [--class c | --desc "…"] [--ctx-tokens n] [--origin cli|route]  deterministic quota-masked recommendation (read-only)
  feedback <ts> good|bad tag a dispatch receipt with an operator quality verdict (S2R-9)
  mcp                    stdio JSON-RPC MCP server (tools: route, run, quota_status, strategy_dispatch, strategy_status, strategy_cancel)
  strategy-run <id> [--resume|--sweep]  drain an async strategy DAG (detached supervisor; spawned by strategy_dispatch; --sweep reaps stale dispatches)`)
}
