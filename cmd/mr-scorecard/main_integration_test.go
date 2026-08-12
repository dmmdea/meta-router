package main

// Artifact-level integration tests: build the REAL binary and assert on the
// JSON it emits. The scorecard's read loop, lane registration, resolution
// tiers and drop counters all live in main() where no unit test reaches —
// v0.28.1's own changelog records that gap ("the scorecard has no end-to-end
// artifact assertion") and the round-4 review confirmed the new behaviors
// (trimmed lane registration, non_evidence_oracle_rows, the zero-config
// diagnostics, the eval-set resolution tier) were all invisible to the suite.
//
// Probes never run: -route is never passed, so nothing spawns; and every run
// pins MR_ORCH_STATE to the test's own temp dir, so the rank table under test
// is the fixture's, never the machine's.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	orchOnce sync.Once
	orchBin  string
	orchDir  string
	orchOut  string
	orchErr  error

	scBuildOnce sync.Once
	scBin       string
	scBuildDir  string
	scBuildOut  string
	scBuildErr  error
)

// TestMain owns the build directory's lifetime: sync.Once means no single
// test can, and without it every suite run leaked a temp dir holding a
// multi-MB binary (review round 4).
func TestMain(m *testing.M) {
	code := m.Run()
	if scBuildDir != "" {
		_ = os.RemoveAll(scBuildDir)
	}
	if orchDir != "" {
		_ = os.RemoveAll(orchDir)
	}
	os.Exit(code)
}

func scorecardBin(t *testing.T) string {
	t.Helper()
	scBuildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mr-scorecard-int-*")
		if err != nil {
			scBuildErr = err
			return
		}
		scBuildDir = dir
		bin := filepath.Join(dir, "mr-scorecard-test.exe")
		// scBuildOut, never scBin: overwriting a variable whose declared
		// meaning is a PATH with compiler stdout is a trap for the next reader.
		if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
			scBuildErr, scBuildOut = err, string(out)
			return
		}
		scBin = bin
	})
	if scBuildErr != nil {
		t.Fatalf("go build: %v\n%s", scBuildErr, scBuildOut)
	}
	return scBin
}

// orchestrateBin builds the ROUTER binary the zoo needs for its baseline.
// The router is deterministic and LLM-free (B2) and `route` is a local
// sub-ms decision with -no-receipt, so this stays zero-spend.
func orchestrateBin(t *testing.T) string {
	t.Helper()
	orchOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mr-orch-int-*")
		if err != nil {
			orchErr = err
			return
		}
		orchDir = dir
		bin := filepath.Join(dir, "mr-orchestrate-test.exe")
		if out, err := exec.Command("go", "build", "-o", bin, "../mr-orchestrate").CombinedOutput(); err != nil {
			orchErr, orchOut = err, string(out)
			return
		}
		orchBin = bin
	})
	if orchErr != nil {
		t.Fatalf("go build mr-orchestrate: %v\n%s", orchErr, orchOut)
	}
	return orchBin
}

const scGoldset = `{"id":"T-01","class":"research","split":"tuning","repo":"meta-router","prompt":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
{"id":"T-02","class":"research","split":"tuning","repo":"meta-router","prompt":"p2","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
{"id":"T-03","class":"research","split":"heldout","repo":"meta-router","prompt":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
`

type scArtifact struct {
	RefCfg      string `json:"reference_config"`
	NonEvidence int    `json:"non_evidence_oracle_rows"`
	RefGap      *struct {
		Message string `json:"message"`
	} `json:"reference_unmeasured"`
	Policies []struct {
		Policy   string   `json:"policy"`
		Configs  []string `json:"configs"`
		PassRate float64  `json:"pass_rate"`
	} `json:"policies"`
	Frontier []struct {
		ClaudeBudget   int     `json:"claude_budget"`
		ClaudeFraction float64 `json:"claude_fraction"`
		PassRate       float64 `json:"pass_rate"`
	} `json:"frontier"`
	FrontierFreeUnmeasured int `json:"frontier_free_unmeasured_tasks"`
	Zoo                    []struct {
		Family          string  `json:"family"`
		Chosen          string  `json:"chosen_config"`
		TuningPassRate  float64 `json:"tuning_pass_rate"`
		TuningDiverged  int     `json:"tuning_diverged"`
		HeldoutDiverged int     `json:"heldout_diverged"`
	} `json:"zoo"`
}

// scRun executes the built scorecard with MR_ORCH_STATE pointed at stateDir
// (so the rank table is the test's own override, never the machine's).
func scRun(t *testing.T, stateDir string, args ...string) (scArtifact, string) {
	t.Helper()
	cmd := exec.Command(scorecardBin(t), args...)
	cmd.Env = append(os.Environ(), "MR_ORCH_STATE="+stateDir)
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	if err := cmd.Run(); err != nil {
		t.Fatalf("scorecard: %v\nstderr: %s", err, se.String())
	}
	var a scArtifact
	if err := json.Unmarshal([]byte(so.String()), &a); err != nil {
		t.Fatalf("artifact does not parse: %v\n%s", err, so.String())
	}
	return a, se.String()
}

func scFixture(t *testing.T, oracleRows, rankTable string) (goldset, oracle, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	goldset = filepath.Join(dir, "goldset.jsonl")
	if err := os.WriteFile(goldset, []byte(scGoldset), 0o644); err != nil {
		t.Fatal(err)
	}
	oracle = filepath.Join(dir, "oracle.jsonl")
	if err := os.WriteFile(oracle, []byte(oracleRows), 0o644); err != nil {
		t.Fatal(err)
	}
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if rankTable != "" {
		if err := os.WriteFile(filepath.Join(stateDir, "rank-table.json"), []byte(rankTable), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return goldset, oracle, stateDir
}

// A padded lane in an oracle row must register as its TRIMMED lane — the raw
// value produced a spurious `always-claude ` policy row resolving to nothing.
// And rows the evidence rule drops must be counted in the artifact.
func TestIntegrationTrimmedLaneAndDropCounter(t *testing.T) {
	rows := `{"task":"T-01","class":"research","lane":"claude ","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-02","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":false,"outcome_class":"error"}
{"task":"T-03","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"exit-4"}
`
	table := `{"deep-reasoning":[{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":1}]}`
	goldset, oracle, state := scFixture(t, rows, table)
	a, se := scRun(t, state, "-oracle", oracle, "-goldset", goldset)

	var names []string
	for _, p := range a.Policies {
		names = append(names, p.Policy)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "always-claude") || strings.Contains(joined, "always-claude ") {
		t.Fatalf("padded lane must register trimmed: %v", names)
	}
	if a.NonEvidence != 2 {
		t.Fatalf("non_evidence_oracle_rows = %d, want 2 (one error + one exit-4)", a.NonEvidence)
	}
	if !strings.Contains(se, "holes") {
		t.Fatalf("dropped rows must be warned on stderr: %s", se)
	}
}

// When the claude lane is not ranked at all, the reference gap must say THAT
// — not prescribe measuring "||", a config no replay can target. And a lane
// with oracle evidence but no rank entry must be warned as unrankable.
func TestIntegrationZeroConfigDiagnostics(t *testing.T) {
	rows := `{"task":"T-01","class":"research","lane":"glm","model":"glm-5.2","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`
	table := `{"deep-reasoning":[{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1}]}`
	goldset, oracle, state := scFixture(t, rows, table)
	a, se := scRun(t, state, "-oracle", oracle, "-goldset", goldset)

	if a.RefCfg != "||" {
		t.Fatalf("reference_config = %q, want the zero config for an unranked claude lane", a.RefCfg)
	}
	if a.RefGap == nil || !strings.Contains(a.RefGap.Message, "rank table") || strings.Contains(a.RefGap.Message, "Measure this config") {
		t.Fatalf("the gap message must name the unranked lane, not prescribe measuring '||': %+v", a.RefGap)
	}
	if !strings.Contains(se, `lane "glm"`) || !strings.Contains(se, "unrankable") {
		t.Fatalf("an evidence-bearing unranked lane must be warned: %s", se)
	}
}

// Under -split, the reference must resolve to a config with evidence on the
// EVALUATION (heldout) set: a higher-ranked config observed only on tuning
// resolved the reference to a cell unmeasured on every scored task.
func TestIntegrationSplitPrefersEvalSetEvidence(t *testing.T) {
	rows := `{"task":"T-01","class":"research","lane":"claude","model":"claude-opus-4-8","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-03","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`
	table := `{"deep-reasoning":[{"Lane":"claude","Model":"claude-opus-4-8","Effort":"high","Rank":1},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":2}]}`
	goldset, oracle, state := scFixture(t, rows, table)
	a, _ := scRun(t, state, "-oracle", oracle, "-goldset", goldset, "-split")

	if a.RefCfg != "claude|claude-sonnet-5|high" {
		t.Fatalf("reference must prefer heldout evidence under -split: got %q (rank-1 has tuning-only rows)", a.RefCfg)
	}
}

// The frontier's rates share the policies' denominator, its ClaudeFraction
// included, and a claude-only-measured task stays in the sweep (counted, not
// excluded) so the curve remains an ENVELOPE: no policy row may exceed its
// maximum point.
func TestIntegrationFrontierEnvelopeHolds(t *testing.T) {
	// T-01: measured ONLY on claude, PASSES. T-02: measured on local, passes.
	// T-03: unmeasured.
	rows := `{"task":"T-01","class":"research","lane":"claude","model":"claude-sonnet-5","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-02","class":"research","lane":"local","model":"gemma4-cascade","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`
	table := `{"deep-reasoning":[{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":1},{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":2}]}`
	goldset, oracle, state := scFixture(t, rows, table)
	a, _ := scRun(t, state, "-oracle", oracle, "-goldset", goldset)

	if a.FrontierFreeUnmeasured != 1 {
		t.Fatalf("frontier_free_unmeasured_tasks = %d, want 1", a.FrontierFreeUnmeasured)
	}
	maxFrontier := 0.0
	for _, p := range a.Frontier {
		if p.PassRate > maxFrontier {
			maxFrontier = p.PassRate
		}
	}
	for _, p := range a.Policies {
		if p.PassRate > maxFrontier+1e-9 {
			t.Fatalf("policy %s pass_rate %.3f exceeds the frontier maximum %.3f — the envelope is broken",
				p.Policy, p.PassRate, maxFrontier)
		}
	}
	// ClaudeFraction divides by the FULL evaluation set (3 tasks), not the
	// sweepable count: budget 1 of 2 sweepable tasks is fraction 1/3.
	for _, fp := range a.Frontier {
		if fp.ClaudeBudget == 1 && (fp.ClaudeFraction < 0.33 || fp.ClaudeFraction > 0.34) {
			t.Fatalf("claude_fraction at budget 1 = %v, want 1/3 (policy-row denominator)", fp.ClaudeFraction)
		}
	}
}
