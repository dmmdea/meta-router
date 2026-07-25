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
	"strings"
)

// denied are exact environment names removed before any lane binary is spawned.
var denied = map[string]bool{
	// Redirect a "subscription" dispatch to metered API billing (R10).
	"ANTHROPIC_API_KEY":    true,
	"ANTHROPIC_AUTH_TOKEN": true,
	"ANTHROPIC_BASE_URL":   true,
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
var deniedPrefixes = []string{"ANTHROPIC_DEFAULT_"}

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

// Removed reports which denied names were present, for a one-time warning: an
// operator who set one deserves to know it was ignored rather than wonder why
// their key had no effect.
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
