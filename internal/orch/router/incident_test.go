package router

import (
	"testing"
	"time"
)

var incT0 = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// incTable: claude rank 1, glm rank 2 for one class — crafted so the shadow
// price flips the winner and incident mode flips it back.
func incTable() Table {
	return Table{Workhorse: []Entry{
		{Lane: "claude", Model: "opus", Rank: 1, Evidence: "test"},
		{Lane: "glm", Model: "glm-5", Rank: 2, Evidence: "test"},
		{Lane: "codex", Model: "gpt-5", Rank: 3, Evidence: "test"},
	}}
}

// Most lanes pressured, claude throttled at high depletion, glm open but the
// worse model. NORMAL routing prices claude +1 → tie at effRank 2 → glm wins
// on lower depletion. INCIDENT mode suspends the price → claude wins on pure
// rank: exploitation-only, because with most lanes pressured the demotion
// cannot shed load onto a calmer lane, only onto a worse model.
func incStates() map[string]LaneState {
	return map[string]LaneState{
		"claude": {State: "throttled", WorstPct: 85},
		"codex":  {State: "exhausted", WorstPct: 99},
		"glm":    {State: "open", WorstPct: 20},
		"local":  {State: "throttled", WorstPct: 90},
	}
}

// W6 canary (incident mode): reverting the shadow-price suspension — or the
// IncidentActive majority rule — goes red here.
func TestIncidentModeExploitationOnly(t *testing.T) {
	if !IncidentActive(incStates()) {
		t.Fatal("3 of 4 lanes pressured must activate incident mode")
	}
	normal := Route(incTable(), Workhorse, incStates(), 0, incT0)
	if normal.Lane != "glm" {
		t.Fatalf("without incident mode the shadow price must demote throttled claude: got %s", normal.Lane)
	}
	inc := Route(incTable(), Workhorse, incStates(), 0, incT0, Opts{Incident: true})
	if inc.Lane != "claude" {
		t.Fatalf("incident mode must suspend the shadow price and route pure-rank: got %s", inc.Lane)
	}
	// Masking is NOT suspended: exhausted codex stays out either way.
	for _, m := range inc.Masked {
		if m.Lane == "codex" {
			return
		}
	}
	t.Fatal("incident mode must keep masking dead lanes (codex exhausted)")
}

// The knob arms the mechanism; a CALM field must not engage it (the opt alone
// never changes routing — the condition is derived from live states).
func TestIncidentRequiresPressuredMajority(t *testing.T) {
	calm := map[string]LaneState{
		"claude": {State: "throttled", WorstPct: 85},
		"codex":  {State: "open", WorstPct: 10},
		"glm":    {State: "open", WorstPct: 20},
		"local":  {State: "open", WorstPct: 0},
	}
	if IncidentActive(calm) {
		t.Fatal("1 of 4 pressured is not a majority")
	}
	d := Route(incTable(), Workhorse, calm, 0, incT0, Opts{Incident: true})
	if d.Lane != "glm" {
		t.Fatalf("with a calm majority the shadow price must still apply: got %s", d.Lane)
	}
	// Downshift suspension is also incident-scoped: under a pressured majority
	// a downshifted lane keeps its base rank too.
	st := incStates()
	cl := st["claude"]
	cl.Downshift = 2
	st["claude"] = cl
	inc := Route(incTable(), Workhorse, st, 0, incT0, Opts{Incident: true})
	if inc.Lane != "claude" {
		t.Fatalf("incident mode must suspend the downshift price too: got %s", inc.Lane)
	}
}
