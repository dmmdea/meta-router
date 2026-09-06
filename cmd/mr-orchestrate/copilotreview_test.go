package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// recordedReviewer is the GraphQL answer the recorder returns for the
// verification read: a Copilot bot present, or nothing recorded (the live
// 2026-09-06 shape on the exhausted account).
const (
	recordedBot  = `{"data":{"repository":{"pullRequest":{"reviewRequests":{"nodes":[{"requestedReviewer":{"__typename":"Bot","login":"copilot-pull-request-reviewer"}}]}}}}}`
	recordedNone = `{"data":{"repository":{"pullRequest":{"reviewRequests":{"nodes":[]}}}}}`
)

// reviewHarness pins a scratch state dir with a configured subscription
// account and records every gh invocation instead of spawning gh. The
// verification read answers `verify`; the edit answers "".
func reviewHarness(t *testing.T, verify string) (calls *[][]string) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("MR_ORCH_STATE", state)
	if err := os.WriteFile(filepath.Join(state, "config.json"), []byte(`{"copilot_token_user":"acct"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var rec [][]string
	prevGH, prevMint := runGH, mintReviewToken
	runGH = func(token string, args ...string) (string, error) {
		if token != "minted-acct-token" {
			t.Errorf("gh must run with the minted token only, got %q", token)
		}
		rec = append(rec, args)
		if len(args) > 0 && args[0] == "api" {
			return verify, nil
		}
		return "https://github.com/acct/repo/pull/1", nil
	}
	mintReviewToken = func(user string) (string, error) {
		if user != "acct" {
			t.Errorf("token must be minted for the configured account, got %q", user)
		}
		return "minted-acct-token", nil
	}
	t.Cleanup(func() { runGH, mintReviewToken = prevGH, prevMint })
	return &rec
}

func lastReceipt(t *testing.T) dispatch.Record {
	t.Helper()
	raw, err := os.ReadFile(dispatchPath())
	if err != nil {
		t.Fatalf("no receipt written: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var r dispatch.Record
	if err := json.Unmarshal(lines[len(lines)-1], &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func exhaustCopilot(t *testing.T, now time.Time) {
	t.Helper()
	if err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		l.ObserveLimit("copilot", "", ledger.WinMonth, ledger.NextMonthlyReset(now), now)
	}); err != nil {
		t.Fatal(err)
	}
}

// An exhausted month SKIPS the review — exit-0 success for the caller, a
// typed skip in the output, a deferred receipt, and gh never spawned. This is
// the operator's requirement in one sentence: "it should get skipped when
// the quota is exhausted".
func TestCopilotReviewSkipsWhenTheMonthIsExhausted(t *testing.T) {
	calls := reviewHarness(t, recordedBot)
	now := time.Date(2026, 9, 6, 17, 0, 0, 0, time.UTC)
	exhaustCopilot(t, now)
	var out bytes.Buffer
	if err := copilotReview(&out, "acct/repo", 7, false, now); err != nil {
		t.Fatalf("a skip is not an error: %v", err)
	}
	var res reviewResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || res.Requested || res.AdmitState != "exhausted" || res.ResumeAt == nil || !res.ResumeAt.Equal(ledger.NextMonthlyReset(now)) {
		t.Fatalf("want a typed skip with the calendar resume, got %+v", res)
	}
	if len(*calls) != 0 {
		t.Fatalf("gh must not be spawned on a skip: %v", *calls)
	}
	if r := lastReceipt(t); r.OutcomeClass != "deferred" || r.Origin != "copilot_review" || r.Desc != "acct/repo#7" || r.Admit {
		t.Fatalf("skip receipt wrong: %+v", r)
	}
}

// An open month requests the review with the documented reviewer handle,
// VERIFIES GitHub recorded a Copilot bot, meters the configured credit
// estimate on the month window, and writes an ok receipt carrying it.
func TestCopilotReviewRequestsVerifiesAndMeters(t *testing.T) {
	calls := reviewHarness(t, recordedBot)
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := copilotReview(&out, "Acct/Repo", 12, false, now); err != nil {
		t.Fatal(err)
	}
	var res reviewResult
	_ = json.Unmarshal(out.Bytes(), &res)
	if !res.Requested || res.Skipped || res.CreditsMetered != 25 || res.Reviewer != "copilot-pull-request-reviewer" {
		t.Fatalf("want requested+verified with the default 25-credit estimate, got %+v", res)
	}
	if len(*calls) != 2 {
		t.Fatalf("want the edit then the verification read, got %v", *calls)
	}
	want := []string{"pr", "edit", "12", "--repo", "Acct/Repo", "--add-reviewer", "@copilot"}
	if strings.Join((*calls)[0], " ") != strings.Join(want, " ") {
		t.Fatalf("gh edit argv = %v, want %v", (*calls)[0], want)
	}
	if v := (*calls)[1]; v[0] != "api" || v[1] != "graphql" || !strings.Contains(strings.Join(v, " "), "on Bot") || !strings.Contains(strings.Join(v, " "), "name=Repo") {
		t.Fatalf("verification must read reviewRequests with the Bot fragment on the named repo: %v", v)
	}
	l := ledger.Open(ledgerPath())
	b, ok := l.Bucket("copilot", ledger.WinMonth)
	if !ok || b.ShadowTokens != 25*1000 || b.CapTokens != 1500*1000 {
		t.Fatalf("review must meter 25 credits against the 1,500 allowance: %+v ok=%v", b, ok)
	}
	if r := lastReceipt(t); r.OutcomeClass != "ok" || r.PremiumRequests != 25 || r.Model != "code-review" || !r.Admit {
		t.Fatalf("request receipt wrong: %+v", r)
	}
}

// REGRESSION of the live 2026-09-06 finding: gh exited 0 and GitHub recorded
// no reviewer. The exit code must not be believed — the result says not
// requested, names the cause class, meters nothing, and the receipt is typed.
func TestCopilotReviewDoesNotTrustGHWhenNothingWasRecorded(t *testing.T) {
	calls := reviewHarness(t, recordedNone)
	now := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := copilotReview(&out, "acct/repo", 5, false, now); err != nil {
		t.Fatalf("an unrecorded request is reported, not raised: %v", err)
	}
	var res reviewResult
	_ = json.Unmarshal(out.Bytes(), &res)
	if res.Requested || res.Skipped || res.CreditsMetered != 0 || !strings.Contains(res.Reason, "recorded no Copilot reviewer") {
		t.Fatalf("want requested=false with the unavailability reason, got %+v", res)
	}
	if len(*calls) != 2 {
		t.Fatalf("edit + verify expected: %v", *calls)
	}
	if r := lastReceipt(t); r.OutcomeClass != "not_recorded" || r.PremiumRequests != 0 {
		t.Fatalf("unrecorded receipt wrong: %+v", r)
	}
	if _, ok := ledger.Open(ledgerPath()).Bucket("copilot", ledger.WinMonth); ok {
		t.Fatal("an unrecorded request must meter nothing")
	}
}

// The request is a gh write the PreToolUse guard never sees, so the account
// rule lives here: a repository owned by anyone but the subscription account
// is refused before any token is minted.
func TestCopilotReviewRefusesAnotherAccountsRepo(t *testing.T) {
	calls := reviewHarness(t, recordedBot)
	var out bytes.Buffer
	err := copilotReview(&out, "otherbiz/store", 3, false, time.Now())
	if err == nil || !strings.Contains(err.Error(), "cross-account") {
		t.Fatalf("want a cross-account refusal, got %v", err)
	}
	if len(*calls) != 0 || out.Len() != 0 {
		t.Fatalf("refusal must spawn nothing and print nothing: calls=%v out=%q", *calls, out.String())
	}
}

// --force is the R11 override: the request goes through on an exhausted
// month, loudly marked forced — and is still verified, never assumed.
func TestCopilotReviewForceRequestsPastExhaustion(t *testing.T) {
	calls := reviewHarness(t, recordedBot)
	now := time.Date(2026, 9, 6, 17, 0, 0, 0, time.UTC)
	exhaustCopilot(t, now)
	var out bytes.Buffer
	if err := copilotReview(&out, "acct/repo", 8, true, now); err != nil {
		t.Fatal(err)
	}
	var res reviewResult
	_ = json.Unmarshal(out.Bytes(), &res)
	if !res.Requested || !res.Forced || len(*calls) != 2 {
		t.Fatalf("force must request, verify and say so: %+v calls=%v", res, *calls)
	}
}

// A gh failure is an error AFTER the receipt and output are written, and it
// meters nothing: the vendor billed nothing for a request that never landed.
func TestCopilotReviewGHFailureIsLoudAndUnmetered(t *testing.T) {
	reviewHarness(t, recordedBot)
	runGH = func(token string, args ...string) (string, error) { return "GraphQL: Could not resolve", errors.New("exit status 1") }
	var out bytes.Buffer
	err := copilotReview(&out, "acct/repo", 9, false, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "Could not resolve") {
		t.Fatalf("gh failure must surface its output: %v", err)
	}
	if r := lastReceipt(t); r.OutcomeClass != "error" || r.PremiumRequests != 0 {
		t.Fatalf("failed request receipt wrong: %+v", r)
	}
	if _, ok := ledger.Open(ledgerPath()).Bucket("copilot", ledger.WinMonth); ok {
		t.Fatal("a failed request must meter nothing")
	}
}
