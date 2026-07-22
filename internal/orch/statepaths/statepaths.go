// Package statepaths is the single home for the orchestrator's state-file
// layout, shared by cmd/mr-orchestrate and cmd/mr-hook (the hook reads the
// ledger for the quota hint and must resolve the SAME paths). MR_ORCH_STATE
// overrides for tests/smokes so they never touch real state.
package statepaths

import (
	"os"
	"path/filepath"
)

func StateDir() string {
	if d := os.Getenv("MR_ORCH_STATE"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".meta-router", "orchestrate")
	}
	return filepath.Join(home, ".meta-router", "orchestrate")
}

func Ledger() string      { return filepath.Join(StateDir(), "ledger.json") }
func QuotaTrace() string  { return filepath.Join(StateDir(), "quota-trace.jsonl") }
func Fuses() string       { return filepath.Join(StateDir(), "fuses.json") }
func Config() string      { return filepath.Join(StateDir(), "config.json") }
func Dispatch() string    { return filepath.Join(StateDir(), "dispatch.jsonl") }
func Drop() string        { return filepath.Join(StateDir(), "statusline-drop.json") }
func PolicyAlert() string { return filepath.Join(StateDir(), "policy-watch.json") }
func GLMAlert() string    { return filepath.Join(StateDir(), "glm-alert.json") }
func CodexAlert() string  { return filepath.Join(StateDir(), "codex-alert.json") }
func GLMToken() string    { return filepath.Join(StateDir(), "glm-token") }
func RankTable() string   { return filepath.Join(StateDir(), "rank-table.json") }
func SpendDown() string   { return filepath.Join(StateDir(), "spend-down.json") }

// Strategy state (slice 3): per-dispatch dirs under StateDir()/strategy/<id>/
// hold state.json (durable state-as-bus + crash-resume), journal.jsonl (event
// log), and artifacts/<step_id>.json. Existing state layout is unchanged.
func StrategyDir(id string) string       { return filepath.Join(StateDir(), "strategy", id) }
func StrategyState(id string) string     { return filepath.Join(StrategyDir(id), "state.json") }
func StrategyJournal(id string) string   { return filepath.Join(StrategyDir(id), "journal.jsonl") }
func StrategyArtifacts(id string) string { return filepath.Join(StrategyDir(id), "artifacts") }
