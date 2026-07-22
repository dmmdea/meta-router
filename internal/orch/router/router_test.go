package router

import (
	"strings"
	"testing"
	"time"
)

func openStates() map[string]LaneState {
	return map[string]LaneState{
		"claude": {State: "open", WorstPct: 40}, "codex": {State: "open", WorstPct: 10},
		"glm": {State: "open", WorstPct: 20}, "local": {State: "open", WorstPct: 0},
	}
}

func TestRouteRankAndMask(t *testing.T) {
	tbl := Seed()
	now := time.Now().UTC()
	t.Run("hard-repo picks opus", func(t *testing.T) {
		d := Route(tbl, HardRepo, openStates(), 0, now)
		if d.Lane != "claude" || d.Model != "claude-opus-4-8" || d.Strategy != "solo" {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("mask before selection: claude exhausted → glm-5.2 for hard-repo", func(t *testing.T) {
		s := openStates()
		s["claude"] = LaneState{State: "exhausted", ResumeAt: now.Add(3 * time.Hour)}
		d := Route(tbl, HardRepo, s, 0, now)
		if d.Lane != "glm" || d.Model != "glm-5.2" || len(d.Masked) == 0 {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("shadow price: throttled claude demotes ONE rank — terminal goes codex anyway, hard-repo flips at tie", func(t *testing.T) {
		s := openStates()
		s["claude"] = LaneState{State: "throttled", WorstPct: 85}
		if d := Route(tbl, HardRepo, s, 0, now); d.Lane != "claude" {
			t.Fatalf("rank1+1=2 still beats rank3 glm — throttle demotes, never evicts (R14): %+v", d)
		}
		if d := Route(tbl, Workhorse, s, 0, now); d.Lane != "glm" {
			t.Fatalf("workhorse stays glm regardless: %+v", d)
		}
	})
	t.Run("parity tie breaks to lowest depletion (mcp-structured)", func(t *testing.T) {
		d := Route(tbl, MCPStructured, openStates(), 0, now) // codex 10% < glm 20% < claude 40%
		if d.Lane != "codex" {
			t.Fatalf("tie must break to least-depleted lane: %+v", d)
		}
	})
	t.Run("codex ctx cap masks (long-context safety)", func(t *testing.T) {
		d := Route(tbl, TerminalBounded, openStates(), 300_000, now)
		if d.Lane != "claude" {
			t.Fatalf("258K cap must mask codex: %+v", d)
		}
		found := false
		for _, m := range d.Masked {
			if m.Lane == "codex" && strings.Contains(m.Reason, "258") {
				found = true
			}
		}
		if !found {
			t.Fatalf("mask reason must cite the cap: %+v", d.Masked)
		}
	})
	t.Run("local ctx prohibition", func(t *testing.T) {
		if d := Route(tbl, MechanicalText, openStates(), 150_000, now); d.Lane == "local" {
			t.Fatalf("local masked >100K (baseline §2): %+v", d)
		}
	})
	t.Run("all masked → relegation with earliest resume", func(t *testing.T) {
		s := map[string]LaneState{
			"claude": {State: "exhausted", ResumeAt: now.Add(4 * time.Hour)},
			"glm":    {State: "exhausted", ResumeAt: now.Add(2 * time.Hour)},
			"codex":  {State: "exhausted", ResumeAt: now.Add(9 * time.Hour)},
			"local":  {State: "unavailable"},
		}
		d := Route(tbl, HardRepo, s, 0, now)
		if d.Lane != "" || !d.ResumeAt.Equal(now.Add(2*time.Hour)) {
			t.Fatalf("relegation must carry the earliest resume (RS5): %+v", d)
		}
	})
	t.Run("unknown class defaults quality-first (R14a)", func(t *testing.T) {
		d := Route(tbl, Class("nonsense"), openStates(), 0, now)
		if d.Model != "claude-opus-4-8" || !strings.Contains(d.Reason, "quality-first") {
			t.Fatalf("%+v", d)
		}
	})
	t.Run("glm absent from many-tool (baseline §2 prohibition encoded as data)", func(t *testing.T) {
		for _, e := range Seed()[ManyTool] {
			if e.Lane == "glm" {
				t.Fatal("many-tool must not list glm")
			}
		}
	})
	t.Run("every entry cites evidence (R14a: researched priors, not instinct)", func(t *testing.T) {
		for c, es := range Seed() {
			for _, e := range es {
				if len(e.Evidence) < 20 {
					t.Fatalf("%s/%s lacks evidence citation", c, e.Lane)
				}
			}
		}
	})
}

// S2R-12 (determinism + cold-start). The residual all-equal tie must resolve to
// a TOTAL, stable order that prefers claude over codex — codex is the surgical
// resource, never spent on run one.
func TestRouteDeterminismColdStart(t *testing.T) {
	tbl := Seed()
	now := time.Now().UTC()

	// Cold start: every lane at unknown (-1) depletion, a parity class. -1
	// normalizes to 0 (fail-open, most-available) so effRank + UsedPct are all
	// tied → the lane-priority tiebreak decides, and it must pick claude.
	t.Run("cold-start parity tie prefers claude over codex", func(t *testing.T) {
		s := map[string]LaneState{
			"claude": {State: "open", WorstPct: -1},
			"codex":  {State: "open", WorstPct: -1},
			"glm":    {State: "open", WorstPct: -1},
			"local":  {State: "open", WorstPct: -1},
		}
		d := Route(tbl, MCPStructured, s, 0, now)
		if d.Lane != "claude" {
			t.Fatalf("cold-start parity must prefer claude over codex (S2R-12): %+v", d)
		}
	})

	// The residual tiebreak is a TOTAL order: identical repeated calls yield the
	// identical winner AND identical Alternatives ordering (no map-iteration
	// nondeterminism leaks into the decision).
	t.Run("deterministic across repeated calls", func(t *testing.T) {
		s := map[string]LaneState{
			"claude": {State: "open", WorstPct: -1},
			"codex":  {State: "open", WorstPct: -1},
			"glm":    {State: "open", WorstPct: -1},
			"local":  {State: "open", WorstPct: -1},
		}
		first := Route(tbl, MCPStructured, s, 0, now)
		for i := 0; i < 50; i++ {
			d := Route(tbl, MCPStructured, s, 0, now)
			if d.Lane != first.Lane || d.Model != first.Model {
				t.Fatalf("winner not deterministic: %+v vs %+v", first, d)
			}
			if len(d.Alternatives) != len(first.Alternatives) {
				t.Fatalf("alternatives length not stable: %d vs %d", len(d.Alternatives), len(first.Alternatives))
			}
			for j := range d.Alternatives {
				if d.Alternatives[j] != first.Alternatives[j] {
					t.Fatalf("alternatives order not stable at %d: %+v vs %+v", j, first.Alternatives, d.Alternatives)
				}
			}
		}
	})

	// Pareto alternatives determinism: strictly-better replacement, stable slice
	// order. MCPStructured has three parity-rank lanes; at open states with
	// distinct depletion, the winner (codex 10%) drops, and the survivors are
	// the non-dominated runners-up in a stable order.
	t.Run("pareto alternatives are strictly-better-pruned and stably ordered", func(t *testing.T) {
		d := Route(tbl, MCPStructured, openStates(), 0, now)
		// codex won (10%); glm (20%) and claude (40%) are alternatives, both at
		// the same rank. glm dominates claude here (same effRank, lower UsedPct),
		// so claude is Pareto-dominated and pruned; glm survives.
		if len(d.Alternatives) != 1 || d.Alternatives[0].Lane != "glm" {
			t.Fatalf("dominated claude must be pruned, glm survives: %+v", d.Alternatives)
		}
		// Stable across repeats.
		for i := 0; i < 20; i++ {
			d2 := Route(tbl, MCPStructured, openStates(), 0, now)
			if len(d2.Alternatives) != 1 || d2.Alternatives[0].Lane != "glm" {
				t.Fatalf("alternatives not stable: %+v", d2.Alternatives)
			}
		}
	})
}

// E1 (slice-4): a lane whose measured burn trajectory is on pace to exhaust its
// window before reset (Downshift >= 2 = burnrate.LevelMedium) is demoted +1 rank
// — the same magnitude and mechanism as the existing throttle shadow price, and
// R14-compliant for the same reason: the trigger is a real measured account
// fact, never a reserve.
func TestRouteBurnDownshiftDemotesOneRank(t *testing.T) {
	tbl := Table{"workhorse-coding": {
		{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "test evidence long enough to pass citation checks"},
		{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 2, Evidence: "test evidence long enough to pass citation checks"},
	}}
	states := map[string]LaneState{
		"glm":    {State: "open", Downshift: 2},
		"claude": {State: "open"},
	}
	d := Route(tbl, "workhorse-coding", states, 0, time.Now().UTC())
	if d.Lane != "claude" {
		t.Fatalf("rank-1 glm at Downshift 2 must be demoted below rank-2 claude, got %q", d.Lane)
	}
}

// Downshift 1 (LevelSlow) is ADVISORY — no routing effect.
func TestRouteDownshiftLevelOneIsAdvisoryOnly(t *testing.T) {
	tbl := Table{"workhorse-coding": {
		{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "test evidence long enough to pass citation checks"},
		{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 2, Evidence: "test evidence long enough to pass citation checks"},
	}}
	states := map[string]LaneState{
		"glm":    {State: "open", Downshift: 1},
		"claude": {State: "open"},
	}
	d := Route(tbl, "workhorse-coding", states, 0, time.Now().UTC())
	if d.Lane != "glm" {
		t.Fatalf("Downshift 1 must not demote, got %q", d.Lane)
	}
}

// Throttle + downshift STACK (+2): both are independent real account facts.
// Strict +2 isolation: glm throttled(+1)+downshift(+1) on rank 1 = eff 3, tying
// claude#3 (eff 3) — the lane-priority tiebreak (claude before glm) then hands
// the win to claude. A BROKEN single-demotion leaves glm at eff 2 < claude 3, so
// glm wins outright and this test fails: the tie can ONLY be reached when both
// demotions applied.
func TestRouteThrottleAndDownshiftStack(t *testing.T) {
	states := map[string]LaneState{
		"glm":    {State: "throttled", Downshift: 2}, // eff 1+1+1 = 3
		"claude": {State: "open"},                    // eff 3
	}
	tbl2 := Table{"workhorse-coding": {
		{Lane: "glm", Model: "glm-5.2", Effort: "high", Rank: 1, Evidence: "test evidence long enough to pass citation checks"},
		{Lane: "claude", Model: "claude-sonnet-5", Effort: "high", Rank: 3, Evidence: "test evidence long enough to pass citation checks"},
	}}
	d2 := Route(tbl2, "workhorse-coding", states, 0, time.Now().UTC())
	if d2.Lane != "claude" {
		t.Fatalf("throttle+downshift must stack (glm eff 3 ties claude 3 → claude on priority), got %q", d2.Lane)
	}
}

// E2 spend-down: a caller-set Boost is a rank RAISE bounded by the caller; the
// router just subtracts it (rank-delta, never scalar) and surfaces the winning
// lane's boost for transparency.
func TestRouteSpendDownBoost(t *testing.T) {
	tbl := Seed()
	now := time.Now().UTC()
	base := Route(tbl, HardRepo, openStates(), 0, now)
	if base.Lane != "claude" || base.SpendDownBoost != 0 {
		t.Fatalf("baseline must be un-boosted claude: %+v", base)
	}
	glmRank := 0
	for _, e := range tbl[HardRepo] {
		if e.Lane == "glm" {
			glmRank = e.Rank
		}
	}
	if glmRank <= 1 {
		t.Fatalf("test premise: glm must trail claude in hard-repo, got rank %d", glmRank)
	}
	s := openStates()
	st := s["glm"]
	st.Boost = glmRank - 1 // brings glm to eff rank 1 — ties claude, wins on lower depletion (20 < 40)
	s["glm"] = st
	d := Route(tbl, HardRepo, s, 0, now)
	if d.Lane != "glm" {
		t.Fatalf("boost %d must lift glm to parity and win the depletion tie: %+v", st.Boost, d)
	}
	if d.SpendDownBoost != st.Boost || !strings.Contains(d.Reason, "spend-down boost") {
		t.Fatalf("winning boost must surface in the decision: %+v", d)
	}
	// One level short of parity must NOT flip the winner.
	st.Boost--
	s["glm"] = st
	if d := Route(tbl, HardRepo, s, 0, now); d.Lane != "claude" || d.SpendDownBoost != 0 {
		t.Fatalf("sub-parity boost must not flip nor mark the un-boosted winner: %+v", d)
	}
}
