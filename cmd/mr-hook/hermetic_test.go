package main

import (
	"os"
	"testing"
)

// TestMain makes the whole package hermetic against the developer's Claude Code config.
// mr-hook reads $CLAUDE_CONFIG_DIR (default ~/.claude) to drop skills the model cannot
// invoke. Without this, every E2E that runs the built binary inherited the REAL
// ~/.claude/settings.json: on a machine whose skillOverrides hides the fixture skill
// gstack-qa, TestHookRerankTemplatedQueryE2E failed locally while CI (no ~/.claude)
// passed -- a machine-dependent suite. Tests that need a config set their own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "mr-hook-claude-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("CLAUDE_CONFIG_DIR", dir) // empty dir: no settings -> nothing filtered
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
