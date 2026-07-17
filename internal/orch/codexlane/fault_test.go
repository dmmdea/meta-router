package codexlane

// Task 15 fault matrix — codex lane. Every fault must resolve to a CLASSIFIED
// Outcome (or a config-error return), NEVER a panic. Behaviors already covered
// by the primary suites are referenced in the evidence doc, not duplicated here;
// this file adds the rows the primary suites did not exercise directly:
//   - missing binary (PATH stripped) → spawn_error, error return nil so the
//     caller dispatch-logs a receipt (the cmd-level receipt row lives in
//     cmd/mr-orchestrate/fault_test.go);
//   - version <0.142.5 → config_error naming the fix (the Run-layer message, not
//     just the comparator — comparator is TestVersionGateComparator);
//   - a timeout → the run terminates and classifies (tree-killed via cmd.Cancel
//     on Windows), never hangs.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Missing binary: with PATH stripped, os/exec cannot find `codex`. The lane must
// return a spawn_error Outcome with a NIL error (the nil error is the contract —
// it tells the caller "this is a classified outcome to dispatch-log", not a
// config failure to abort on) and name the missing executable in Result.
func TestFaultMissingBinarySpawnError(t *testing.T) {
	t.Setenv("PATH", "") // strip codex from the search path
	o, raw, err := Run(context.Background(), RunReq{Prompt: "hi", Model: "gpt-5.5", SkipVersionGate: true})
	if err != nil {
		t.Fatalf("a missing binary is a classified outcome, not a config error (nil err so the caller logs a receipt): %v", err)
	}
	if o.Class != "spawn_error" {
		t.Fatalf("missing binary must classify spawn_error, got %q", o.Class)
	}
	if raw != nil {
		t.Fatalf("no bytes were produced by the never-spawned process: %d", len(raw))
	}
	if !strings.Contains(o.Result, "codex") {
		t.Fatalf("spawn_error must name the missing executable: %q", o.Result)
	}
}

// A2R-#12: assert against the REAL production message builder (VersionGateError,
// the exact function Run calls when the gate fails), NOT a hand-copied literal
// that can silently drift. If someone edits the production message and drops a
// remedy, this fails. The numeric comparator is TestVersionGateComparator;
// versionAtLeast here just confirms the sub-floor precondition.
func TestFaultVersionGateMessageNamesFix(t *testing.T) {
	v := "codex-cli 0.140.0"
	if versionAtLeast(v, 0, 142, 5) {
		t.Fatalf("0.140.0 must be below the 0.142.5 floor")
	}
	// The production message — whatever Run actually emits — must name the fix.
	msg := VersionGateError(v).Error()
	if !strings.Contains(msg, v) {
		t.Fatalf("the version-gate error must name the offending version %q: %s", v, msg)
	}
	for _, want := range []string{"0.142.5", "npm i -g @openai/codex", "--force"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("version-gate config_error must name the fix %q: %s", want, msg)
		}
	}
}

// sleeperMagicEnv, when set, makes the test binary re-exec sleep instead of
// running the test suite — a portable "codex" stand-in that OUTLIVES a short
// TimeoutSec so the real deadline/tree-kill path is exercised (A2R-#8).
const sleeperMagicEnv = "CODEXLANE_TEST_SLEEPER_SECS"

// TestMain intercepts the re-exec: when sleeperMagicEnv is set the process
// sleeps for that many seconds (ignoring the codex args it was handed) and
// exits, standing in for a long-running codex turn.
func TestMain(m *testing.M) {
	if s := os.Getenv(sleeperMagicEnv); s != "" {
		secs, _ := strconv.Atoi(s)
		time.Sleep(time.Duration(secs) * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A2R-#8 (was vacuous — PATH="" made spawn fail BEFORE the timeout path, so it
// would pass even if the whole timeout mechanism were deleted). This is a
// GENUINE timeout test: point the lane at a real sleeper (the test binary
// re-exec) that sleeps 30s, run with TimeoutSec=1, and assert the call
// TERMINATES promptly (well under the 30s sleep) and classifies — proving the
// deadline + tree-kill actually fire. Cross-platform: os.Args[0] + a magic env
// var; the Windows cmd.Cancel taskkill /T /F path is what reaps the sleeper.
func TestFaultTimeoutTerminatesClassified(t *testing.T) {
	// Point the lane at the sleeper (this test binary in re-exec mode).
	orig := binaryName
	binaryName = os.Args[0]
	t.Cleanup(func() { binaryName = orig })
	t.Setenv(sleeperMagicEnv, "30") // the sleeper outlives the 1s timeout

	done := make(chan struct{})
	var o Outcome
	start := time.Now()
	go func() {
		o, _, _ = Run(context.Background(), RunReq{Prompt: "hi", Model: "gpt-5.5", SkipVersionGate: true, TimeoutSec: 1})
		close(done)
	}()
	select {
	case <-done:
		elapsed := time.Since(start)
		// Must return WELL before the 30s sleep — proves the deadline fired and
		// the process was reaped, not that we waited it out.
		if elapsed > 25*time.Second {
			t.Fatalf("timeout did not fire — Run took %v (near the full 30s sleep)", elapsed)
		}
		if o.Class == "" {
			t.Fatalf("a timed-out run must return a CLASSIFIED outcome, not a hang/empty: %+v", o)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Run with TimeoutSec=1 against a 30s sleeper HUNG — the deadline/tree-kill did not fire")
	}
}

// A malformed JSONL line mid-stream classifies parse_error (fail loud —
// fixture-guarded). This complements TestParseIncompleteAndGarbage by putting
// the garbage AFTER a valid event, proving the scanner classifies per-line and
// does not silently swallow a torn line.
func TestFaultMalformedLineMidStreamParseError(t *testing.T) {
	jsonl := []byte(`{"type":"thread.started","thread_id":"x"}` + "\n" +
		`{"type":"item.completed","item":{"type":"agent_message"` + "\n") // truncated JSON
	if o := Parse(jsonl); o.Class != "parse_error" {
		t.Fatalf("a torn line mid-stream must classify parse_error, got %q (%q)", o.Class, o.Result)
	}
}
