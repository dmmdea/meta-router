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

// Every class classify.go can EMIT is ranked identically (13 of the 15
// declared classes; hard-case-reclaim is never emitted, and doc-summarize is
// ranked below). The scorecard probes with no --class, so classify.go picks.
//
// Ranking them IDENTICALLY is the load-bearing part, and the reason is not
// the one an earlier version of this comment gave: an unranked class does NOT
// refuse loudly — route falls back silently and still returns a lane, so a
// fixture that differentiated the rows would produce a silently wrong
// BaseLane, changing what diverged() counts with no failure anywhere. Uniform
// rows make the classifier's pick irrelevant to what these tests measure,
// which is lane ORDER (review round 5).
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
	// The WINNER is the load-bearing assertion, because it is what actually
	// changes under the revert. With the tuning-frozen resolver the L=1500
	// candidate's diverged tuning task lands on the MEASURED codex-TUNING
	// cell and scores; with the eval-set resolver it lands on codex-HELDOUT
	// (no tuning rows), that candidate's objective drops, and SelectBest
	// falls back to a candidate that never diverges. Asserting on
	// TuningPassRate alone was dead: under the real revert the run trips the
	// vacuity guard below first, telling the engineer their FIXTURE broke
	// rather than that they removed the fix (review round 5).
	if ctx.Chosen != "L=1500,floor=codex" {
		t.Fatalf("expected the diverging candidate to win the tuning split; got %q (%+v).\n"+
			"This is what a revert of the tuning-frozen resolver looks like: the diverged task is scored at a cell "+
			"with no TUNING evidence, so its objective collapses and a non-diverging candidate wins instead.", ctx.Chosen, *ctx)
	}
	if ctx.TuningDiverged == 0 {
		t.Fatalf("the winning candidate does not diverge on the tuning split, so the resolver was never consulted — "+
			"either the fixture is broken OR the tuning-frozen resolver was reverted (%+v)", *ctx)
	}
	if ctx.TuningPassRate == 0 {
		t.Fatalf("the zoo's tuning objective collapsed to 0 — its changed lane resolved to a cell with no TUNING evidence, "+
			"which is what happens when the eval-set resolver drives SelectBest (%+v)", *ctx)
	}
}

// The zoo's heldout rows and its tuning selection must use the SAME resolver:
// scoring the two with different rulers is the two-rulers defect SelectBest's
// doc comment warns about. Observable in the artifact: a zoo row that CHANGED
// the router's lane must be scored at the tuning-frozen cell (codex-TUNING),
// never the eval-set one (codex-HELDOUT).
//
// The heldout task must therefore DIVERGE — otherwise `PolicyOf` takes its
// `lane == BaseLane -> base(id)` branch for every scored task, the resolver is
// never consulted, and "codex-HELDOUT" is unreachable by construction. The
// first version of this test had exactly that hole: mutating the verdict call
// site alone left it green, i.e. it stayed passing under the very defect it
// names (review round 5, the same vacuity class round 5 fixed elsewhere).
// T-03 (heldout) is 2000 chars here so the ctx-floor candidate bumps it too.
func TestIntegrationZooScoresHeldoutWithTheSameResolver(t *testing.T) {
	goldset, oracle, state := scFixture(t, zooOracle, zooRankTable)
	a, _ := scRun(t, state,
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
	if ctx == nil || ctx.HeldoutDiverged == 0 {
		t.Fatalf("fixture is vacuous: the winning candidate must CHANGE the router's lane on the heldout task, "+
			"or no zoo row ever consults the resolver and this test cannot fail (%+v)", ctx)
	}

	sawChanged := false
	for _, p := range a.Policies {
		if !strings.HasPrefix(p.Policy, "zoo:") {
			continue
		}
		for _, c := range p.Configs {
			if strings.Contains(c, "codex-HELDOUT") {
				t.Fatalf("zoo row %s scored at %s — the heldout verdict used the EVAL-SET resolver while "+
					"selection used the tuning-frozen one (two rulers)", p.Policy, c)
			}
			if strings.Contains(c, "codex-TUNING") {
				sawChanged = true
			}
		}
	}
	if !sawChanged {
		t.Fatal("no zoo row was scored at a CHANGED lane's cell, so the resolver was never exercised on the " +
			"heldout split — the assertion above cannot fail and this test pins nothing")
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
