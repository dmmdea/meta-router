package codexlane

import (
	"fmt"
	"os"
	"path/filepath"
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
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o644); err != nil {
		return fail(err)
	}
	return home, func() { _ = os.RemoveAll(home) }, nil
}
