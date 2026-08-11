package router

import (
	"strings"
	"testing"
	"time"
)

var incT0 = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// incTable: claude rank 1, glm rank 2, codex rank 3 for one class — crafted so
// the shadow price flips the winner and incident mode flips it back.
func incTable() Table {
	return Table{Workhorse: []Entry{
		{Lane: "claude", Model: "opus", Rank: 1, Evidence: "test"},
		{Lane: "glm", Model: "glm-5", Rank: 2, Evidence: "test"},
		{Lane: "codex", Model: "gpt-5", Rank: 3, Evidence: "test"},
	}}
}

// 2 of the 3 CANDIDATE lanes pressured (claude throttled, codex exhausted;
// glm calm) — a strict majority of the class's own candidates. local is
// deliberately calm to prove the denominator is the candidate set, not the
// fleet. NORMAL routing prices claude +1 → tie at effRank 2 → glm wins on
// lower depletion. INCIDENT mode suspends the price → claude wins on pure
// rank: exploitation-only.
func incStates() map[string]LaneState {
	return map[string]LaneState{
		"claude": {State: "throttled", WorstPct: 85},
		"codex":  {State: "exhausted", WorstPct: 99},
		"glm":    {State: "open", WorstPct: 20},
		"local":  {State: "open", WorstPct: 0},
	}
}

// W6 canary (incident mode): reverting the shadow-price suspension — or the
// candidate-scoped majority rule — goes red here.
func TestIncidentModeExploitationOnly(t *testing.T) {
	if !IncidentActive(incStates(), []string{"claude", "glm", "codex"}) {
		t.Fatal("2 of 3 candidate lanes pressured must activate incident mode")
	}
	normal := Route(incTable(), Workhorse, incStates(), 0, incT0)
	if normal.Lane != "glm" || normal.Incident {
		t.Fatalf("without the opt the shadow price must demote throttled claude: got %s incident=%v", normal.Lane, normal.Incident)
	}
	inc := Route(incTable(), Workhorse, incStates(), 0, incT0, Opts{Incident: true})
	if inc.Lane != "claude" {
		t.Fatalf("incident mode must suspend the shadow price and route pure-rank: got %s", inc.Lane)
	}
	// Engagement is RECORDED — a B8 feature whose engagement is invisible
	// cannot accumulate promotion evidence (review 2026-08-12).
	if !inc.Incident || !strings.Contains(inc.Reason, "incident mode") {
		t.Fatalf("engagement must be visible on the Decision: incident=%v reason=%q", inc.Incident, inc.Reason)
	}
	// Masking is NOT suspended: exhausted codex stays out either way.
	for _, m := range inc.Masked {
		if m.Lane == "codex" {
			return
		}
	}
	t.Fatal("incident mode must keep masking dead lanes (codex exhausted)")
}

// The denominator is the CLASS'S CANDIDATE lanes: pressure on lanes that were
// never candidates must not suspend the price when a calm candidate exists.
func TestIncidentDenominatorIsCandidateSet(t *testing.T) {
	// Two-lane class: glm throttled, claude calm. Fleet-wide 3 of 4 lanes are
	// pressured — but only 1 of the 2 CANDIDATES is, so incident must NOT
	// engage and the throttled candidate keeps its +1 price.
	tbl := Table{LatencyIter: []Entry{
		{Lane: "glm", Model: "glm-5", Rank: 1, Evidence: "test"},
		{Lane: "claude", Model: "opus", Rank: 2, Evidence: "test"},
	}}
	states := map[string]LaneState{
		"claude": {State: "open", WorstPct: 10},
		"glm":    {State: "throttled", WorstPct: 85},
		"codex":  {State: "exhausted", WorstPct: 99},
		"local":  {State: "unavailable", WorstPct: 0},
	}
	if IncidentActive(states, []string{"glm", "claude"}) {
		t.Fatal("1 of 2 candidates is not a strict majority")
	}
	d := Route(tbl, LatencyIter, states, 0, incT0, Opts{Incident: true})
	if d.Lane != "claude" || d.Incident {
		t.Fatalf("calm candidate exists: price must apply, incident must not engage: got %s incident=%v", d.Lane, d.Incident)
	}
}

// Boundary: exactly half pressured is NOT a strict majority; the knob alone
// never changes routing. And under a genuine candidate majority, the
// downshift price is suspended together with the throttle price.
func TestIncidentRequiresPressuredMajority(t *testing.T) {
	calm := incStates()
	calm["codex"] = LaneState{State: "open", WorstPct: 10} // now 1 of 3 candidates pressured
	if IncidentActive(calm, []string{"claude", "glm", "codex"}) {
		t.Fatal("1 of 3 is not a majority")
	}
	d := Route(incTable(), Workhorse, calm, 0, incT0, Opts{Incident: true})
	if d.Lane != "glm" || d.Incident {
		t.Fatalf("with a calm majority the shadow price must still apply: got %s incident=%v", d.Lane, d.Incident)
	}
	// Exactly-half boundary on an even candidate set: 1 of 2 → no engagement.
	two := map[string]LaneState{
		"claude": {State: "throttled", WorstPct: 85},
		"glm":    {State: "open", WorstPct: 20},
	}
	if IncidentActive(two, []string{"claude", "glm"}) {
		t.Fatal("exactly half is not a STRICT majority")
	}
	// Downshift suspension rides the same incident flag.
	st := incStates()
	cl := st["claude"]
	cl.Downshift = 2
	st["claude"] = cl
	inc := Route(incTable(), Workhorse, st, 0, incT0, Opts{Incident: true})
	if inc.Lane != "claude" {
		t.Fatalf("incident mode must suspend the downshift price too: got %s", inc.Lane)
	}
}
