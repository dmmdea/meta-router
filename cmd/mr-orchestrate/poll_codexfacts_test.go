package main

import (
	"testing"
	"time"

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
		Models:     map[string]quotapoll.CodexModelUsage{"gpt-6-astra": {Available: true}},
		Additional: []quotapoll.CodexAdditionalLimit{{Name: "GPT-5.3-Codex-Spark", Feature: "codex_bengalfox"}}}
	ps := loadPollState()
	finishPolls(pollFetch{subjects: []subjectFetch{{Lane: "codex", Subject: "default", Origin: "wham_poll", OK: true, CodexFacts: &facts}}}, &ps, now)

	got := loadPollState()
	if got.CodexPlan != "prolite" || got.CodexHas5h == nil || *got.CodexHas5h || !got.CodexModels["gpt-6-astra"] || len(got.CodexAdditional) != 1 || got.CodexFactsAt == nil {
		t.Fatalf("facts must persist in poll-state: %+v", got)
	}
	st := codexPlanStatus(got)
	if st == nil || st.PlanType != "prolite" || st.Has5hWindow == nil || *st.Has5hWindow || st.AdditionalLimits[0] != "GPT-5.3-Codex-Spark" || st.Note == "" {
		t.Fatalf("status view: %+v", st)
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
	if st := codexPlanStatus(loadPollState()); st != nil {
		t.Fatalf("want nil, got %+v", st)
	}
}
