package main

import (
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotapoll"
)

// finishPolls records the codex plan facts of a SUCCESSFUL default-subject
// fetch, keeps them through an outage, ignores other subjects, and status
// renders them (2026-09-06: the operator's plan read "prolite" with no 5h
// window and an Astra availability flag).
func TestFinishPollsRecordsCodexPlanFacts(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	now := time.Date(2026, 9, 6, 22, 50, 0, 0, time.UTC)
	facts := quotapoll.CodexFacts{PlanType: "prolite", Has5h: false,
		Models: map[string]quotapoll.CodexModelUsage{"gpt-6-astra": {Available: true}},
		Additional: []quotapoll.CodexAdditionalLimit{{Name: "GPT-5.3-Codex-Spark", Feature: "codex_bengalfox",
			Snapshots: []quotapoll.Snapshot{{Lane: "codex", Window: ledger.Win5h, UsedPct: 0}, {Lane: "codex", Window: ledger.Win7d, UsedPct: 0}}}}}
	ps := loadPollState()
	finishPolls(pollFetch{subjects: []subjectFetch{{Lane: "codex", Subject: "default", Origin: "wham_poll", OK: true, CodexFacts: &facts}}}, &ps, now)

	got := loadPollState()
	if got.CodexPlan != "prolite" || got.CodexHas5h == nil || *got.CodexHas5h || !got.CodexModels["gpt-6-astra"] || len(got.CodexAdditional) != 1 || got.CodexFactsAt == nil {
		t.Fatalf("facts must persist in poll-state: %+v", got)
	}
	if got.CodexSaw5hElsewhere == nil || !*got.CodexSaw5hElsewhere {
		t.Fatalf("the sibling 5h window must be recorded as corroboration: %+v", got)
	}
	// The negative half of the same record: this capture's main allowance
	// carried NO short-window block, so "unreadable" is false. Without an
	// assertion here, recordCodexFacts could hardcode either gate input.
	if got.CodexUndecodable5h == nil || *got.CodexUndecodable5h {
		t.Fatalf("a clean absence must record undecodable_5h=false, not nil: %+v", got.CodexUndecodable5h)
	}
	// And the POSITIVE case, or the field could simply be hardcoded false:
	// a poll whose main allowance carried an unreadable short window must
	// record it, because that is the input the gate refuses on.
	undFacts := facts
	undFacts.Undecodable5h = true
	ps2 := loadPollState()
	finishPolls(pollFetch{subjects: []subjectFetch{{Lane: "codex", Subject: "default", Origin: "wham_poll", OK: true, CodexFacts: &undFacts}}}, &ps2, now.Add(time.Minute))
	if u := loadPollState().CodexUndecodable5h; u == nil || !*u {
		t.Fatalf("an unreadable short window must be recorded as such: %+v", u)
	}
	armedCfg := orchcfg.Config{QuotaStaleHours: 6, Codex5hEstimateOff: true, Codex5hEstimateOffPlans: []string{"prolite"}}
	if g := codex5hEstimateGate(armedCfg, loadPollState(), now.Add(2*time.Minute)); g.Suppress {
		t.Fatalf("and the gate must refuse on it: %+v", g)
	}
	// Status view, knob OFF (the default): facts rendered, corroboration
	// rendered, gate reported as not armed and not suppressing.
	armed := func() orchcfg.Config {
		return orchcfg.Config{QuotaStaleHours: 6, Codex5hEstimateOff: true, Codex5hEstimateOffPlans: []string{"prolite"}}
	}
	st := codexPlanStatus(got, orchcfg.Config{QuotaStaleHours: 6}, now.Add(time.Minute))
	if st == nil || st.PlanType != "prolite" || st.Has5hWindow == nil || *st.Has5hWindow || st.AdditionalLimits[0] != "GPT-5.3-Codex-Spark" || st.Note == "" {
		t.Fatalf("status view: %+v", st)
	}
	if st.Saw5hElsewhere == nil || !*st.Saw5hElsewhere {
		t.Fatalf("status must render the corroboration: %+v", st)
	}
	if st.EstimateOffArmed || st.EstimateOffSuppressing || !strings.Contains(st.EstimateOffReason, "not armed") {
		t.Fatalf("knob off: gate view must say not armed / not suppressing: %+v", st)
	}
	// Knob ON with fresh, corroborated facts: status reports the suppression
	// with the SAME reason text the dispatch receipt carries (one function).
	on := codexPlanStatus(got, armed(), now.Add(time.Minute))
	want := codex5hEstimateGate(armed(), got, now.Add(time.Minute))
	if !on.EstimateOffArmed || !on.EstimateOffSuppressing || !want.Suppress || on.EstimateOffReason != want.Reason || !strings.Contains(on.EstimateOffReason, "suppressed") {
		t.Fatalf("knob on: gate view must mirror the gate: %+v vs %+v", on, want)
	}
	// Knob ON but stale facts: armed, NOT suppressing, and the reason names staleness.
	stale := codexPlanStatus(got, armed(), now.Add(7*time.Hour))
	if !stale.EstimateOffArmed || stale.EstimateOffSuppressing || !strings.Contains(stale.EstimateOffReason, "stale") {
		t.Fatalf("knob on + stale: %+v", stale)
	}

	// Outage: a failed fetch (OK=false) must not erase the facts.
	later := now.Add(time.Hour)
	finishPolls(pollFetch{subjects: []subjectFetch{{Lane: "codex", Subject: "default", Origin: "wham_poll", OK: false, CodexFacts: &quotapoll.CodexFacts{}}}}, &got, later)
	after := loadPollState()
	if after.CodexPlan != "prolite" || after.CodexFactsAt == nil || !after.CodexFactsAt.Equal(now) {
		t.Fatalf("an outage must preserve the last facts and their timestamp: %+v", after)
	}
	// Another subject's plan is not the primary account's.
	finishPolls(pollFetch{subjects: []subjectFetch{{Lane: "codex", Subject: "acct2", Origin: "wham_poll", OK: true, CodexFacts: &quotapoll.CodexFacts{PlanType: "plus", Has5h: true}}}}, &after, later)
	if loadPollState().CodexPlan != "prolite" {
		t.Fatal("a non-default subject must not overwrite the primary plan facts")
	}
	// A claude fetch carries no codex facts and changes nothing.
	finishPolls(pollFetch{subjects: []subjectFetch{{Lane: "claude", Subject: "default", Origin: "oauth_poll", OK: true}}}, &after, later)
	if loadPollState().CodexPlan != "prolite" {
		t.Fatal("other lanes must not touch the codex facts")
	}
}

// No facts ever recorded → no status block (absent, not empty).
func TestCodexPlanStatusAbsentWhenNeverPolled(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	if st := codexPlanStatus(loadPollState(), orchcfg.Config{}, time.Now()); st != nil {
		t.Fatalf("want nil, got %+v", st)
	}
}
