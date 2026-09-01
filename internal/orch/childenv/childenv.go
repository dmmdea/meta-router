// Package childenv scrubs the environment handed to spawned lane binaries.
//
// Why (audit 2026-07-25): the lane adapters passed the ambient environment
// through unfiltered — `cmd.Env = nil` inherits everything, and the subject
// path appended to `os.Environ()` without filtering. In the shipped Claude Code
// headless mode (`claude -p`, which is all meta-router ever spawns) an ambient
// ANTHROPIC_API_KEY is honoured UNCONDITIONALLY, ahead of OAuth and bypassing
// the interactive approval gate — so a single `setx` after an SDK experiment
// would silently convert every "subscription" dispatch into metered API spend,
// with receipts still recording cash_usd 0. HKCU already holds one such token
// for another service, so this is a realistic accident, not a contrived one.
// CLAUDE_CODE_SIMPLE is likewise equivalent to the `--bare` flag the argument
// builder explicitly forbids: banning the flag while leaving the env var open
// is a padlock on an open gate.
//
// Deny-list, not allow-list: on Windows a strict allow-list plausibly breaks
// every spawn (PATH, SystemRoot, TEMP, user profile, proxy and locale vars are
// all load-bearing). The lanes' own deliberate pins are appended AFTER this
// scrub, so the GLM lane keeps setting ANTHROPIC_BASE_URL / ANTHROPIC_AUTH_TOKEN
// on purpose — the point is to remove what the OPERATOR's shell happens to
// carry, not what a lane intends.
package childenv

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// denied are exact environment names removed before any lane binary is spawned.
var denied = map[string]bool{
	// Redirect a "subscription" dispatch to metered API billing (R10).
	"ANTHROPIC_API_KEY":    true,
	"ANTHROPIC_AUTH_TOKEN": true,
	"ANTHROPIC_BASE_URL":   true,
	// GitHub tokens (copilot lane, 2026-09-01). The copilot CLI's documented
	// precedence is COPILOT_GITHUB_TOKEN > GH_TOKEN > GITHUB_TOKEN, and gh
	// itself honours GH_TOKEN over its keyring — an ambient token would bill
	// an arbitrary account from inside ANY lane child. The copilot runner
	// appends its deliberately-minted COPILOT_GITHUB_TOKEN AFTER this scrub,
	// same pattern as the lane HOME pins.
	"COPILOT_GITHUB_TOKEN": true,
	"GH_TOKEN":             true,
	"GITHUB_TOKEN":         true,
	// Config/state redirect for the copilot CLI (the lane pins its own).
	"COPILOT_HOME": true,
	// Arbitrary header injection is a complete end-run around the key deny.
	"ANTHROPIC_CUSTOM_HEADERS":      true,
	"ANTHROPIC_IDENTITY_TOKEN":      true,
	"ANTHROPIC_IDENTITY_TOKEN_FILE": true,
	// Config/account redirect. The lane PINS CLAUDE_CONFIG_DIR for W2 subject
	// rotation, so an ambient value would silently bill a different account
	// while the receipt recorded the default subject — an accounting lie.
	"CLAUDE_CONFIG_DIR":    true,
	"ANTHROPIC_CONFIG_DIR": true,
	// Alternative base URLs / proxies.
	"CLAUDE_CODE_API_BASE_URL": true,
	"CLAUDE_CODE_PROXY_URL":    true,
	"CLAUDE_CODE_HTTP_PROXY":   true,
	"CLAUDE_CODE_HTTPS_PROXY":  true,
	"CLAUDE_CODE_USE_GATEWAY":  true,
	// Model overrides outside the ANTHROPIC_DEFAULT_ family.
	"ANTHROPIC_MODEL":                 true,
	"ANTHROPIC_SMALL_FAST_MODEL":      true,
	"CLAUDE_CODE_SUBAGENT_MODEL":      true,
	"CLAUDE_CODE_BG_CLASSIFIER_MODEL": true,
	// Sibling of the --bare-equivalent.
	"CLAUDE_CODE_SIMPLE_SYSTEM_PROMPT": true,
	// Alternative credential channels that bypass the approval gate.
	"CLAUDE_CODE_OAUTH_TOKEN":                 true,
	"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR": true,
	"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":     true,
	// Equivalent to the forbidden --bare flag.
	"CLAUDE_CODE_SIMPLE": true,
	// Cloud-vendor routing: sends the dispatch to a billed backend.
	"CLAUDE_CODE_USE_BEDROCK":                true,
	"CLAUDE_CODE_USE_VERTEX":                 true,
	"CLAUDE_CODE_USE_FOUNDRY":                true,
	"CLAUDE_CODE_USE_MANTLE":                 true,
	"CLAUDE_CODE_USE_ANTHROPIC_AWS":          true,
	"CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD": true,
	// Codex lane.
	"OPENAI_API_KEY":  true,
	"OPENAI_BASE_URL": true,
}

// deniedPrefixes cover families whose exact names vary, e.g.
// ANTHROPIC_DEFAULT_SONNET_MODEL / _OPUS_MODEL / _HAIKU_MODEL — the router pins
// models deliberately per dispatch, and an ambient default would silently
// override that pin.
// CLAUDE_CODE_OAUTH_ covers the token, its refresh token, session/HFI bearers
// and the file-descriptor variants in one rule.
var deniedPrefixes = []string{"ANTHROPIC_DEFAULT_", "CLAUDE_CODE_OAUTH_"}

// Denied reports whether an environment variable name is scrubbed.
func Denied(name string) bool {
	up := strings.ToUpper(name)
	if denied[up] {
		return true
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(up, p) {
			return true
		}
	}
	return false
}

// Scrub returns env with every denied entry removed. Entries without '=' are
// passed through untouched (Windows carries a few, e.g. "=C:").
//
// Call this on os.Environ() BEFORE appending a lane's own deliberate pins, so
// an intentional pin is never scrubbed by its own adapter.
func Scrub(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 { // no name, or a leading '=' (Windows drive vars)
			out = append(out, kv)
			continue
		}
		if Denied(kv[:i]) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Removed reports which denied names were present: an operator who set one
// deserves to know it was ignored rather than wonder why their key had no effect.
func Removed(env []string) []string {
	var hit []string
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		if Denied(kv[:i]) {
			hit = append(hit, kv[:i])
		}
	}
	return hit
}

// WarnOnce prints the names Scrub will drop, at most once per process, to w.
//
// Both halves matter. ONCE: the claude lane called Removed inline and re-printed
// on every dispatch, so a batch run emitted the same warning dozens of times —
// the doc comment said "one-time" while the code said per-call. AT ALL: the other
// eight spawn sites scrubbed silently, so an operator with an ambient key learned
// about it only if their dispatch happened to take the claude path. Warning is a
// property of the scrub, not of one lane, so it lives here.
func WarnOnce(w io.Writer, env []string) {
	warnOnce.Do(func() {
		if dropped := Removed(env); len(dropped) > 0 {
			fmt.Fprintln(w, "WARN: ignoring credential/routing env for child processes:",
				strings.Join(dropped, ", "), "(R10: subscription auth only)")
		}
	})
}

var warnOnce sync.Once
