package main

// Call-site integration tests: build the REAL binary and drive main()'s
// safety wiring end to end. buildRunPlan's own comment records why helper
// unit tests are not enough here — the defects live at the CALL SITES
// (review 2026-07-27 mutation-tested three reverts that left every unit test
// green while a built binary re-dispatched 224 recorded cells). The round-4
// review reproduced the same blindness for this round's two call-site
// behaviors: the drift guard firing before the -plan-only exit, and the
// output oracle opening only after the preflight and the guard.
//
// Zero-spend by construction, enforced on BOTH axes rather than asserted:
// EVERY intRun passes -orchestrate at a nonexistent path (so a run that
// passes the guard records spawn-error rows and dispatches nothing), and
// intRun pins MR_ORCH_STATE to a temp dir (so even an accidental dispatch
// could not touch the operator's real ledger or receipts). The plan-only
// cases relied on the plan-only early return alone — which is the very
// behavior they exist to pin, so a regression there would have dispatched
// through the REAL installed orchestrator against the REAL state dir
// (review round 4).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	builtBin  string
	buildDir  string
	buildOut  string
	buildErr  error
)

// TestMain owns the build directory's lifetime: sync.Once means no single
// test can, and without it every suite run leaked a temp dir holding a
// multi-MB binary (review round 4).
func TestMain(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		_ = os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

func goldreplayBin(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "mr-goldreplay-int-*")
		if err != nil {
			buildErr = err
			return
		}
		buildDir = dir
		bin := filepath.Join(dir, "mr-goldreplay-test.exe")
		// buildOut, never builtBin: overwriting a variable whose declared
		// meaning is a PATH with compiler stdout is a trap for the next reader.
		if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
			buildErr, buildOut = err, string(out)
			return
		}
		builtBin = bin
	})
	if buildErr != nil {
		t.Fatalf("go build: %v\n%s", buildErr, buildOut)
	}
	return builtBin
}

const intGoldset = `{"id":"T-01","class":"research","split":"tuning","repo":"meta-router","prompt":"p1","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
{"id":"T-02","class":"research","split":"heldout","repo":"meta-router","prompt":"p2","verify":{"kind":"pure","keys":[{"name":"k","pattern":"x"}]}}
`

// intRun executes the built binary in a temp dir and returns exit code,
// stdout and stderr. The oracle and goldset paths are created per scenario.
func intRun(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(goldreplayBin(t), args...)
	cmd.Dir = dir
	// The orchestrator state is pinned to the run's own temp dir: no test
	// here may read or write the operator's real ledger, receipts or latches.
	cmd.Env = append(os.Environ(), "MR_ORCH_STATE="+filepath.Join(dir, "state"))
	var so, se strings.Builder
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("run: %v", err)
		}
		code = ee.ExitCode()
	}
	return code, so.String(), se.String()
}

func intFixture(t *testing.T, oracleRows string) (dir, goldset, oracle string) {
	t.Helper()
	dir = t.TempDir()
	goldset = filepath.Join(dir, "goldset.jsonl")
	if err := os.WriteFile(goldset, []byte(intGoldset), 0o644); err != nil {
		t.Fatal(err)
	}
	oracle = filepath.Join(dir, "oracle.jsonl")
	if oracleRows != "" {
		if err := os.WriteFile(oracle, []byte(oracleRows), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir, goldset, oracle
}

const legacyLocalRows = `{"task":"T-01","class":"research","lane":"local","model":"gemma4-cascade","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-02","class":"research","lane":"local","model":"gemma4-cascade","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`

// The 476-cell incident shape: a real effort pin against pre-effort rows must
// REFUSE before anything can dispatch.
func TestIntegrationEffortDriftRefuses(t *testing.T) {
	dir, goldset, oracle := intFixture(t, legacyLocalRows)
	code, _, se := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-lanes", "local",
		"-local-model", "gemma4-cascade", "-local-effort", "high",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (refusal)\nstderr: %s", code, se)
	}
	if !strings.Contains(se, "EFFORT DRIFT") || !strings.Contains(se, "RE-SPENDS") {
		t.Fatalf("refusal must name the drift: %s", se)
	}
	b, _ := os.ReadFile(oracle)
	if string(b) != legacyLocalRows {
		t.Fatal("a refused run must not touch the oracle")
	}
}

// A typo'd model pin re-keys every recorded cell at once; v0.31.0's aggregate
// guard refused it and the round-3 rewrite silently dropped that protection
// (round-4 MAJOR). The model tier must refuse it again.
func TestIntegrationModelDriftRefuses(t *testing.T) {
	rows := `{"task":"T-01","class":"research","lane":"claude","model":"claude-sonnet-5","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`
	dir, goldset, oracle := intFixture(t, rows)
	code, _, se := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-tasks", "T-01", "-lanes", "claude",
		"-claude-model", "claude-sonnet-55", "-claude-effort", "unrecorded",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (model-drift refusal)\nstderr: %s", code, se)
	}
	if !strings.Contains(se, "MODEL DRIFT") || !strings.Contains(se, "claude-sonnet-5") {
		t.Fatalf("refusal must name the recorded model: %s", se)
	}
}

// -plan-only is a READ-ONLY preflight: it must show the drift a live run
// would refuse on, and it must not create the output oracle.
func TestIntegrationPlanOnlyIsReadOnlyAndShowsDrift(t *testing.T) {
	dir, goldset, oracle := intFixture(t, legacyLocalRows)
	code, so, _ := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-lanes", "local",
		"-local-model", "gemma4-cascade", "-local-effort", "high", "-plan-only",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 {
		t.Fatalf("plan-only must exit 0, got %d", code)
	}
	if !strings.Contains(so, "EFFORT DRIFT") || !strings.Contains(so, "would REFUSE") {
		t.Fatalf("plan-only must show what a live run refuses on: %s", so)
	}

	fresh := filepath.Join(dir, "fresh.jsonl")
	code, _, _ = intRun(t, dir,
		"-goldset", goldset, "-out", fresh, "-lanes", "local",
		"-local-model", "gemma4-cascade", "-local-effort", "unrecorded", "-plan-only",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 {
		t.Fatalf("plan-only exit = %d", code)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatal("plan-only created the output oracle — the preflight is not read-only")
	}
}

// New cells pass the guard freely: a lane the oracle has never seen proceeds
// to dispatch (and here spawn-errors on the fake orchestrator, proving the
// guard did not fire) — the round-3 over-fire, kept fixed.
func TestIntegrationNewLanePassesGuard(t *testing.T) {
	dir, goldset, oracle := intFixture(t, legacyLocalRows)
	code, so, se := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-lanes", "glm",
		"-glm-model", "glm-5.2", "-glm-effort", "unrecorded",
		"-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 {
		t.Fatalf("a new lane must pass the guard, got exit %d\nstderr: %s", code, se)
	}
	if !strings.Contains(so, "outcome=error") {
		t.Fatalf("the run must have reached dispatch (spawn errors on the fake binary): %s", so)
	}
	b, _ := os.ReadFile(oracle)
	if !strings.Contains(string(b), `"lane":"glm"`) {
		t.Fatal("dispatch-attempt rows must have been recorded")
	}
}

// -re-measure is the deliberate override: the same drifted run proceeds.
func TestIntegrationReMeasureOverridesDrift(t *testing.T) {
	dir, goldset, oracle := intFixture(t, legacyLocalRows)
	code, _, se := intRun(t, dir,
		"-goldset", goldset, "-out", oracle, "-lanes", "local",
		"-local-model", "gemma4-cascade", "-local-effort", "high",
		"-re-measure", "-orchestrate", filepath.Join(dir, "no-such-orchestrate.exe"))
	if code != 0 {
		t.Fatalf("-re-measure must override the drift refusal, got exit %d\nstderr: %s", code, se)
	}
	if !strings.Contains(se, "WARNING: -re-measure") {
		t.Fatalf("the override must be loud: %s", se)
	}
}
