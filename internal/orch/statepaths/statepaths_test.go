package statepaths

import (
	"path/filepath"
	"strings"
	"testing"
)

// S2R-13: the plan's hardcoded `C:\tmp\orchtest` breaks the WSL -race gate —
// portable temp dirs instead (behavior under test is identical).
func TestEnvOverrideWins(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "orchtest")
	t.Setenv("MR_ORCH_STATE", dir)
	if StateDir() != dir {
		t.Fatalf("MR_ORCH_STATE must override: %q", StateDir())
	}
	if got := Ledger(); got != filepath.Join(dir, "ledger.json") {
		t.Fatalf("ledger path: %q", got)
	}
}

func TestDefaultLandsUnderHomeMetaRouter(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", "")
	want := filepath.Join(".meta-router", "orchestrate")
	if got := StateDir(); !strings.HasSuffix(got, want) {
		t.Fatalf("default state dir must end in %q: %q", want, got)
	}
}

func TestStrategyPaths(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", filepath.Join(t.TempDir(), "s"))
	id := "abc123"
	if got := StrategyDir(id); filepath.Base(got) != id {
		t.Fatalf("StrategyDir base = %q", filepath.Base(got))
	}
	if filepath.Base(StrategyState(id)) != "state.json" {
		t.Fatalf("StrategyState = %q", StrategyState(id))
	}
	if filepath.Base(StrategyJournal(id)) != "journal.jsonl" {
		t.Fatalf("StrategyJournal = %q", StrategyJournal(id))
	}
	if filepath.Base(StrategyArtifacts(id)) != "artifacts" {
		t.Fatalf("StrategyArtifacts = %q", StrategyArtifacts(id))
	}
	// StrategyDir must live under StateDir()/strategy/<id>.
	if filepath.Base(filepath.Dir(StrategyDir(id))) != "strategy" {
		t.Fatalf("StrategyDir parent = %q, want strategy", filepath.Dir(StrategyDir(id)))
	}
}

func TestEveryPathLivesInStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "orchtest")
	t.Setenv("MR_ORCH_STATE", dir)
	for name, got := range map[string]string{
		"fuses.json":           Fuses(),
		"config.json":          Config(),
		"dispatch.jsonl":       Dispatch(),
		"statusline-drop.json": Drop(),
		"quota-trace.jsonl":    QuotaTrace(),
		"policy-watch.json":    PolicyAlert(),
		"glm-alert.json":       GLMAlert(),
		"codex-alert.json":     CodexAlert(),
		"glm-token":            GLMToken(),
		"rank-table.json":      RankTable(),
	} {
		if got != filepath.Join(dir, name) {
			t.Fatalf("%s path: %q", name, got)
		}
	}
}
