// Package glmlane drives the GLM coding plan through the OFFICIAL claude
// binary pointed at api.z.ai — the proven smoke shape (2ca0b5e/d5bbbcb). The
// lane REUSES the entire claudelane machinery (BuildArgs hard rules, Parse,
// F15 tree-kill): the only deltas are env and defaults, so the GLM result
// schema stays claudelane-parser-compatible by construction (fixture-proven,
// both models).
//
// R10 discipline: the coding-plan token is read at dispatch time from the
// statepaths-resolved file, lives ONLY in the child's env, and is excluded
// from every error/log/dispatch path — errors carry the PATH, never any
// value; argv never contains it.
package glmlane

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/dmmdea/meta-router/internal/orch/claudelane"
)

const BaseURL = "https://api.z.ai/api/anthropic" // the proven smoke shape (2ca0b5e/d5bbbcb)

// DefaultModel: R14a (operator directive, 2026-07-06) — the lane defaults to its
// strongest model; glm-4.7 is chosen ONLY where the capability baseline
// measures it admissible (cheap-tier tool loops; haiku-pin below).
const DefaultModel = "glm-5.2"

// CheapModel is the always-1× tier (fact refresh §3): pinned as the haiku
// alias so cheap subagent calls inside a dispatch never burn 5.2 quota units.
const CheapModel = "glm-4.7"

// Token reads and trims the coding-plan token. R10: the VALUE is never
// logged/echoed — every error names the PATH only.
func Token(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("glm token unreadable at %s (provision the coding-plan token there; value is never logged, R10): %w", path, err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("glm token file %s is empty — an empty ANTHROPIC_AUTH_TOKEN would silently fall back to ambient auth (R10)", path)
	}
	return tok, nil
}

// Env pins the child process to the GLM endpoint: base URL + auth token +
// the fact-refresh §3 timeout, plus ALL THREE model-alias pins (alias-drift
// belt — an unpinned alias would silently route to an Anthropic model name
// z.ai doesn't serve).
func Env(token, model string) []string {
	return []string{
		"ANTHROPIC_BASE_URL=" + BaseURL,
		"ANTHROPIC_AUTH_TOKEN=" + token,
		"API_TIMEOUT_MS=3000000", // fact refresh §3: GLM long-request ceiling
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + model,
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + model,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + CheapModel, // cheap subagent calls at 1× (fact refresh §3)
	}
}

// Run resolves the token, injects the GLM env, and delegates to
// claudelane.Run — same classified-Outcome contract (error return = config
// only). A missing/empty token is a config error carrying the path, never a
// value (R10).
func Run(ctx context.Context, req claudelane.RunReq, tokenPath string) (claudelane.Outcome, []byte, error) {
	tok, err := Token(tokenPath)
	if err != nil {
		return claudelane.Outcome{Class: "config_error", Result: err.Error()}, nil, err
	}
	req.Env = Env(tok, req.Model)
	return claudelane.Run(ctx, req)
}
