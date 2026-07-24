package main

import (
	"os"
	"path/filepath"

	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// State paths delegate to the shared statepaths package (slice 2: mr-hook
// reads the ledger for the quota hint and must resolve the SAME layout).
// MR_ORCH_STATE overrides for tests and smoke runs so they never touch the
// real ledger.
func stateDir() string        { return statepaths.StateDir() }
func ledgerPath() string      { return statepaths.Ledger() }
func quotaTracePath() string  { return statepaths.QuotaTrace() }
func fusesPath() string       { return statepaths.Fuses() }
func configPath() string      { return statepaths.Config() }
func dispatchPath() string    { return statepaths.Dispatch() }
func dropPath() string        { return statepaths.Drop() }
func policyAlertPath() string { return statepaths.PolicyAlert() }
func codexAlertPath() string  { return statepaths.CodexAlert() }
func glmTokenPath() string    { return statepaths.GLMToken() }
func glmAlertPath() string    { return statepaths.GLMAlert() }
func rankTablePath() string   { return statepaths.RankTable() }
func spendDownPath() string   { return statepaths.SpendDown() }
func profilesPath() string    { return statepaths.Profiles() }

// fixturesDir locates the committed fixtures for probe/verify. CWD-relative
// by default (repo workflows); MR_ORCH_FIXTURES pins it for scheduled tasks
// that start in System32. Stays in cmd: probe-only concern, not state layout.
func fixturesDir() string {
	if d := os.Getenv("MR_ORCH_FIXTURES"); d != "" {
		return d
	}
	return filepath.Join("testdata", "fixtures", "claude")
}

// codexFixturesDir is the codex leg's fixture home (RS8 verify-codex).
func codexFixturesDir() string {
	if d := os.Getenv("MR_ORCH_FIXTURES_CODEX"); d != "" {
		return d
	}
	return filepath.Join("testdata", "fixtures", "codex")
}
