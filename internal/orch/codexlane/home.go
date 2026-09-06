package codexlane

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

// EnsureHome provisions a per-run CODEX_HOME under baseDir/codex-home/<nano>,
// seeding auth.json from the operator's ~/.codex login (R12: reuse, never
// regenerate; R10: never create credentials) and forcing file-backed
// credentials (fact refresh §3). Per-run isolation is the fix for
// cross-session interference (#30714); `--ephemeral` is deliberately NOT used
// — CODEX_HOME isolation already covers it and the flag's presence/behavior
// across versions is unverified (feature-detect later if needed).
//
// Auth-refresh caveat (watch item, recorded in the slice-2 evidence doc):
// codex may refresh tokens inside the per-run home; ~/.codex stays canonical
// and is re-seeded each run. If Plus auth develops stale-refresh symptoms,
// flip callers to a persistent home (one flag flip here).
func EnsureHome(baseDir string) (home string, cleanup func(), err error) {
	return EnsureHomeWith(baseDir, DefaultWindowsSandbox)
}

// Windows native sandbox modes (Codex CLI ≥0.153; docs "Windows sandbox").
// "elevated" is OpenAI's recommended mode (dedicated low-privilege sandbox
// users, set up once with admin rights); "unelevated" is the documented
// fallback (restricted token derived from the current user). Nothing else is
// accepted: an unknown value would be forwarded verbatim into the home and
// silently fall back to the CLI's rejection posture.
const (
	DefaultWindowsSandbox = "elevated"
	WindowsSandboxUnelev  = "unelevated"
)

// ValidWindowsSandbox reports whether mode is a seedable native sandbox mode.
func ValidWindowsSandbox(mode string) bool {
	return mode == DefaultWindowsSandbox || mode == WindowsSandboxUnelev
}

// EnsureHomeWith is EnsureHome with the Windows native sandbox mode to seed.
//
// WHY THE SEED EXISTS (live, 2026-09-06, CLI 0.153.4): on Windows the CLI
// REJECTS every model-issued shell command — stderr `exec_command failed:
// CreateProcess Rejected(... blocked by policy)`, the model reports "shell
// execution was blocked by the environment policy" — unless the active
// CODEX_HOME's config carries `[windows] sandbox = "elevated"|"unelevated"`.
// The hermetic per-run home had only the credentials-store line, so the first
// Astra roster sweep recorded nine "ok, no diff" cells that were harness
// failures, not measurements. Isolated stdin-closed probes pinned the cause:
// approval_policy=never did not help, a project trust entry did not help,
// this one table did. 0.149.1 did not enforce it; the July sweeps ran.
func EnsureHomeWith(baseDir, windowsSandbox string) (home string, cleanup func(), err error) {
	if runtime.GOOS == "windows" && !ValidWindowsSandbox(windowsSandbox) {
		return "", nil, fmt.Errorf("codex_windows_sandbox %q is not a native Windows sandbox mode: use %q (recommended; needs the one-time elevated setup) or %q (restricted-token fallback) — without one of them Codex CLI >=0.153 rejects every shell command", windowsSandbox, DefaultWindowsSandbox, WindowsSandboxUnelev)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", nil, fmt.Errorf("resolve user home: %w", err)
	}
	authPath := filepath.Join(userHome, ".codex", "auth.json")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		return "", nil, fmt.Errorf("codex auth missing at %s: run `codex login` once as the operator (R12 — the orchestrator reuses credentials, never creates them)", authPath)
	}
	home = filepath.Join(baseDir, "codex-home", strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", nil, err
	}
	fail := func(e error) (string, func(), error) {
		_ = os.RemoveAll(home)
		return "", nil, e
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"), auth, 0o600); err != nil {
		return fail(err)
	}
	cfg := "# per-run CODEX_HOME provisioned by mr-orchestrate — hermetic, deleted after the run\ncli_auth_credentials_store = \"file\"\n"
	if runtime.GOOS == "windows" {
		// Native sandbox mode: the table Codex CLI >=0.153 requires before it
		// will execute any command on Windows (see EnsureHomeWith).
		cfg += "[windows]\nsandbox = \"" + windowsSandbox + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o644); err != nil {
		return fail(err)
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}
