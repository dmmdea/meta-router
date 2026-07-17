// Package codexlane drives the official codex CLI, process-per-turn
// (`codex exec --json`), per R12 starting from the AMS run-codex-audit.ps1
// invocation shape (--skip-git-repo-check, --sandbox workspace-write,
// -c model_reasoning_effort). Differences from AMS, both deliberate: the
// prompt is a POSITIONAL arg (never '-'/stdin — codex blocks forever waiting
// on interactive stdin when spawned by the orchestrator; proven live
// 2026-07-06) and reasoning summaries are omitted (JSONL is machine-parsed).
package codexlane

import (
	"fmt"
	"slices"
	"strings"
)

type RunReq struct {
	Prompt, Model, Effort, Sandbox, CWD, Home string
	TimeoutSec                                int
	SkipVersionGate                           bool     // --force plumbing (version gate is a privacy gate, R11-overridable)
	Extra                                     []string // operator passthrough (R11), validated
}

var allowedSandboxes = []string{"read-only", "workspace-write"}

// forbiddenExtraPrefixes: an Extra token that equals one of these, or starts
// with one immediately followed by '=' (the single-token `--flag=value` form),
// is rejected. These are the sandbox/approval-weakening flags the orchestrator
// must never allow through the passthrough (A2R-#4). `-s`/`--sandbox` are here
// because the orchestrator pins the sandbox itself; a second one from Extra
// would override it (codex takes the last value).
var forbiddenExtraPrefixes = []string{
	"--sandbox", "-s",
	"--dangerously-bypass-approvals-and-sandbox",
	"--dangerously-bypass-hook-trust",
	"--full-auto", "--yolo",
	"-a", "--ask-for-approval",
}

// forbiddenExtraSubstrings: rejected when they appear ANYWHERE in an Extra
// token — the `-c key=value` config-override form (`-c sandbox_mode=…`,
// `-c approval_policy=…`) and the danger-full-access sandbox value in any
// spelling. Substring (not prefix) because the payload rides in the value.
var forbiddenExtraSubstrings = []string{
	"sandbox_mode", "sandbox_permissions", "danger-full-access", "approval_policy",
}

// forbiddenExtra reports whether an Extra token is a sandbox/approval-
// weakening override, returning the matched needle for the error message.
// It catches both `--sandbox danger-full-access` (this token is `--sandbox`),
// `--sandbox=danger-full-access` (one token), and `-c sandbox_mode=…` (the
// value token carries the needle).
func forbiddenExtra(x string) (needle string, forbidden bool) {
	for _, p := range forbiddenExtraPrefixes {
		if x == p || strings.HasPrefix(x, p+"=") {
			return strings.TrimLeft(p, "-"), true
		}
	}
	for _, s := range forbiddenExtraSubstrings {
		if strings.Contains(x, s) {
			return s, true
		}
	}
	return "", false
}

func BuildArgs(r RunReq) ([]string, error) {
	if r.Model == "" {
		return nil, fmt.Errorf("model is required: pin -m on every codex exec (unpinned models drift with vendor defaults)")
	}
	if r.Prompt == "" {
		return nil, fmt.Errorf("prompt is required")
	}
	sb := r.Sandbox
	if sb == "" {
		sb = "workspace-write"
	}
	if !slices.Contains(allowedSandboxes, sb) {
		return nil, fmt.Errorf("sandbox %q not allowed (danger-full-access is forbidden for orchestrated runs)", sb)
	}
	for _, x := range r.Extra {
		if x == "-" {
			return nil, fmt.Errorf("stdin prompt ('-') forbidden: codex hangs reading stdin under the orchestrator (proven 2026-07-06); the prompt is a positional arg")
		}
		if tok, ok := forbiddenExtra(x); ok {
			return nil, fmt.Errorf("forbidden extra flag %q: %s overrides weaken the sandbox/approval policy this orchestrator enforces (danger-full-access is never permitted for unattended dispatch, R10/R12)", x, tok)
		}
	}
	args := []string{"exec", "--json", "--skip-git-repo-check", "--sandbox", sb, "-m", r.Model}
	if r.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", r.Effort))
	}
	args = append(args, r.Extra...)
	return append(args, r.Prompt), nil
}
