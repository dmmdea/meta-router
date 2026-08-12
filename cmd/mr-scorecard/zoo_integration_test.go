package main

// The zoo's TUNING arm, driven through the real `-split -zoo -route` path.
//
// This is a CALL-SITE test on purpose. Round 4 introduced a separate
// tuning-frozen resolver for the zoo, and the round-4 review found that
// reverting both call sites back to the eval-set resolver left the entire
// suite green — the fix was real and completely unpinned. A test that
// exercises `resolveLaneConfig` directly cannot see that revert; only one
// that runs the binary can.
//
// Zero-spend: `-route` builds and runs mr-orchestrate's `route`, which is the
// deterministic LLM-free hot path (B2) invoked with -no-receipt. No lane is
// ever dispatched.

import (
	"strings"
	"testing"
)

// The fixture makes the two resolvers disagree about which codex cell stands
// for the codex lane:
//
//	codex ranks codex-TUNING (rank 1) then codex-HELDOUT (rank 2)
//	codex-TUNING has evidence ONLY on tuning tasks (T-01)
//	codex-HELDOUT has evidence ONLY on the heldout task (T-03)
//
// tuning-frozen resolver → tier 1 (tuning observations) hits codex-TUNING.
// eval-set resolver under -split → tier 1 counts HELDOUT rows only, so
// codex-TUNING (0 heldout rows) is skipped and codex-HELDOUT wins.
//
// T-01's prompt is 2000 chars, so the ctx-floor L=1500 candidate bumps it to
// codex on the TUNING split — which is exactly when the zoo consults the
// resolver. With the correct resolver that task scores at the MEASURED
// codex-TUNING cell (pass → tuning_pass_rate > 0); with the eval-set
// resolver it scores at codex-HELDOUT, which has no tuning evidence at all,
// so the cell is Unknown, the task scores 0, and the tuning objective
// collapses — the degeneration the round-4 fix removed.
const zooOracle = `{"task":"T-01","class":"research","lane":"codex","model":"codex-TUNING","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-03","class":"research","lane":"codex","model":"codex-HELDOUT","effort":"high","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-01","class":"research","lane":"local","model":"gemma4-cascade","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":false}
{"task":"T-02","class":"research","lane":"local","model":"gemma4-cascade","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
{"task":"T-03","class":"research","lane":"local","model":"gemma4-cascade","effort":"unrecorded","trial":1,"dispatched":true,"outcome_class":"ok","verifier_pass":true}
`

// Every class the shipped classifier can emit is ranked identically: the
// scorecard probes with no --class, so classify.go picks, and an unranked
// class makes `route` refuse (exit 3) and the zoo abort with no baseline.
// The lane ORDER is what this fixture is about, not the class mapping.
const zooRankTable = `{
  "hard-repo":               [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "terminal-bounded":        [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "workhorse-coding":        [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "many-tool-orchestration": [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "mcp-structured":          [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "deep-reasoning":          [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "formal-math":             [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "competition-math":        [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "long-context":            [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "latency-iteration":       [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "cheap-tool-loops":        [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "mechanical-text":         [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}],
  "verify-gate":             [{"Lane":"local","Model":"gemma4-cascade","Effort":"unrecorded","Rank":1},{"Lane":"codex","Model":"codex-TUNING","Effort":"high","Rank":2},{"Lane":"codex","Model":"codex-HELDOUT","Effort":"high","Rank":3},{"Lane":"claude","Model":"claude-sonnet-5","Effort":"high","Rank":4}]
}`

func TestIntegrationZooTunesWithTuningEvidence(t *testing.T) {
	goldset, oracle, state := scFixture(t, zooOracle, zooRankTable)
	a, se := scRun(t, state,
		"-oracle", oracle, "-goldset", goldset, "-split", "-zoo",
		"-route", orchestrateBin(t))

	var ctx *struct {
		Family          string  `json:"family"`
		Chosen          string  `json:"chosen_config"`
		TuningPassRate  float64 `json:"tuning_pass_rate"`
		TuningDiverged  int     `json:"tuning_diverged"`
		HeldoutDiverged int     `json:"heldout_diverged"`
	}
	for i := range a.Zoo {
		if a.Zoo[i].Family == "ctx-floor" {
			ctx = &a.Zoo[i]
		}
	}
	if ctx == nil {
		t.Fatalf("no ctx-floor zoo entry in the artifact\nstderr: %s", se)
	}
	// The fixture must actually make the candidate diverge on tuning, or the
	// resolver is never consulted and the test pins nothing.
	if ctx.TuningDiverged == 0 {
		t.Fatalf("fixture is vacuous: the ctx-floor winner never diverges on the tuning split (%+v)", *ctx)
	}
	// With the tuning-frozen resolver the diverged tuning task lands on the
	// MEASURED codex-TUNING cell and passes. With the eval-set resolver it
	// lands on codex-HELDOUT (no tuning rows) and the objective collapses to 0.
	if ctx.TuningPassRate == 0 {
		t.Fatalf("the zoo's tuning objective collapsed to 0 — its changed lane resolved to a cell with no TUNING evidence, "+
			"which is what happens when the eval-set resolver drives SelectBest (%+v)", *ctx)
	}
}

// The zoo's heldout rows and its tuning selection must use the SAME resolver:
// scoring the two with different rulers is the two-rulers defect SelectBest's
// doc comment warns about. Observable in the artifact: every zoo policy row's
// configs must be drawable from the tuning-frozen resolution, never from the
// eval-set one — here, codex-TUNING and never codex-HELDOUT.
func TestIntegrationZooScoresHeldoutWithTheSameResolver(t *testing.T) {
	goldset, oracle, state := scFixture(t, zooOracle, zooRankTable)
	a, _ := scRun(t, state,
		"-oracle", oracle, "-goldset", goldset, "-split", "-zoo",
		"-route", orchestrateBin(t))

	for _, p := range a.Policies {
		if !strings.HasPrefix(p.Policy, "zoo:") {
			continue
		}
		for _, c := range p.Configs {
			if strings.Contains(c, "codex-HELDOUT") {
				t.Fatalf("zoo row %s scored at %s — the heldout verdict used the EVAL-SET resolver while "+
					"selection used the tuning-frozen one (two rulers)", p.Policy, c)
			}
		}
	}
}

// Guard the guard: the fixture's own premise — that the two resolvers pick
// different codex cells — must hold, or both tests above are vacuous. The
// reference row (scored on heldout) is the eval-set resolver's own output, so
// its always-codex configs show which cell THAT resolver picks.
func TestIntegrationZooFixtureResolversDisagree(t *testing.T) {
	goldset, oracle, state := scFixture(t, zooOracle, zooRankTable)
	a, _ := scRun(t, state, "-oracle", oracle, "-goldset", goldset, "-split")

	var codexCfgs []string
	for _, p := range a.Policies {
		if p.Policy == "always-codex" {
			codexCfgs = p.Configs
		}
	}
	if len(codexCfgs) != 1 || !strings.Contains(codexCfgs[0], "codex-HELDOUT") {
		t.Fatalf("the eval-set resolver must pick codex-HELDOUT on this fixture (that is what makes the zoo tests "+
			"non-vacuous), got %v", codexCfgs)
	}
}
