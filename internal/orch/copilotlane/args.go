// Package copilotlane drives the GitHub Copilot CLI (`copilot`),
// process-per-turn, mirroring codexlane's contract: every path returns a
// CLASSIFIED Outcome, the error return is reserved for config failures, and
// the spawn is stdin-closed with a scrubbed env.
//
// v1 is a TEXT-DISPATCH lane: all agent tools are denied (`--deny-tool` with
// no value denies the whole set — live-verified on CLI 1.0.82), the ask_user
// tool is disabled, and built-in MCP servers are off. Two live spawns proved
// why the isolation flags are load-bearing: without a pinned COPILOT_HOME the
// CLI loads the operator's ~/.copilot desktop config and SPAWNS its MCP
// servers (network, auth flows) inside an orchestrated dispatch.
package copilotlane

import (
	"fmt"
	"strings"
)

type RunReq struct {
	Prompt, Model, CWD, Home string
	// Token is the GitHub OAuth token this dispatch authenticates with; the
	// caller mints it (per-dispatch, never persisted) from the operator's
	// configured account. Empty is a config error: falling through to ambient
	// GH_TOKEN/GITHUB_TOKEN would bill whatever account the environment
	// happens to carry — the exact cross-account hazard the config field
	// exists to prevent.
	Token           string
	TimeoutSec      int
	SkipVersionGate bool     // --force plumbing (compat gate, R11-overridable)
	Extra           []string // operator passthrough (R11), validated
}

// forbiddenExtraPrefixes: permission-widening, session-bleeding, or
// egress-creating flags the orchestrator must never allow through the
// passthrough. The lane pins its own permission surface (deny-all tools); a
// widening flag from Extra would override it. --resume/--continue re-enter a
// prior session's context (cross-dispatch bleed); --share*/--cloud create
// egress; --agent swaps in a custom agent with its own tool grants.
var forbiddenExtraPrefixes = []string{
	"--allow-all", "--yolo",
	"--allow-all-tools", "--allow-all-paths", "--allow-all-urls",
	"--allow-tool", "--allow-url",
	"--add-dir", "--add-github-mcp-tool", "--add-github-mcp-toolset",
	"--additional-mcp-config", "--agent",
	"--resume", "-r", "--continue",
	"--share", "--share-gist", "--cloud",
	"--interactive", "-i",
}

// forbiddenExtra reports whether an Extra token matches a forbidden flag,
// in either the split (`--flag value`) or single-token (`--flag=value`) form.
func forbiddenExtra(x string) (needle string, forbidden bool) {
	for _, p := range forbiddenExtraPrefixes {
		if x == p || strings.HasPrefix(x, p+"=") {
			return strings.TrimLeft(p, "-"), true
		}
	}
	return "", false
}

// BuildArgs assembles the non-interactive dispatch argv. The prompt rides as
// the -p value (never stdin: the interactive UI owns stdin, and the codex
// lane's proven stdin-hang discipline applies to every CLI lane until a live
// capture proves otherwise).
func BuildArgs(r RunReq) ([]string, error) {
	if r.Model == "" {
		return nil, fmt.Errorf("model is required: pin --model on every copilot dispatch ('auto' is the vendor-routed default and a deliberate choice, not an omission)")
	}
	if r.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	if r.Home == "" {
		return nil, fmt.Errorf("home is required: an unpinned COPILOT_HOME loads the operator's desktop ~/.copilot config (MCP servers, plugins) into an orchestrated dispatch — live-verified 2026-09-01")
	}
	if r.Token == "" {
		return nil, fmt.Errorf("token is required: set copilot_token_user in the orchestrator config (the lane mints the token per dispatch; ambient env tokens are forbidden)")
	}
	for _, x := range r.Extra {
		if tok, ok := forbiddenExtra(x); ok {
			return nil, fmt.Errorf("forbidden extra flag %q: %s widens permissions, re-enters a prior session, or creates egress — the orchestrator pins the dispatch surface (R10/R12)", x, tok)
		}
	}
	args := []string{
		"-p", r.Prompt,
		"--model", r.Model,
		"--output-format", "json",
		"--no-color", "--log-level", "none",
		"--deny-tool", "--no-ask-user", "--disable-builtin-mcps",
	}
	return append(args, r.Extra...), nil
}
