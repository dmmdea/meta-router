package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/retrievers"
	"github.com/dmmdea/meta-router/internal/usagelog"
)

type fakePrimary struct {
	res    []retrievers.Scored
	topCos float64
	err    error
}

func (f fakePrimary) RetrieveScored(p string, k int) ([]retrievers.Scored, float64, error) {
	return f.res, f.topCos, f.err
}

const testMinLen = 6

// Adding TYPED session_id/prompt_id fields made the payload type-checked where
// it previously was not: a payload emitting either as a number decodes Prompt
// correctly but returns an UnmarshalTypeError, and treating that as fatal would
// surface NOTHING on every prompt while logging the misleading reason
// "no prompt". Optional metadata must never veto a valid prompt.
func TestHookInputSurvivesWrongTypedIdentifiers(t *testing.T) {
	var in hookInput
	err := json.Unmarshal([]byte(`{"prompt":"real prompt here","session_id":12345}`), &in)
	if err == nil {
		t.Skip("payload decoded cleanly; the guarded regression cannot occur")
	}
	if in.Prompt != "real prompt here" {
		t.Fatalf("the prompt decodes fine despite the bad id and must be used; got %q", in.Prompt)
	}
	if in.SessionID != "" {
		t.Fatalf("a wrong-typed id must be dropped, not coerced; got %q", in.SessionID)
	}
}

// Totally malformed JSON must still be caught — relaxing the id typing must not
// turn a broken payload into a surfacing attempt on an empty prompt.
func TestHookInputRejectsMalformedPayload(t *testing.T) {
	var in hookInput
	_ = json.Unmarshal([]byte(`{nope`), &in)
	if strings.TrimSpace(in.Prompt) != "" {
		t.Fatalf("malformed JSON must leave Prompt empty, got %q", in.Prompt)
	}
}

func TestDecide_PrimaryAboveThreshold(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "gstack-qa", Score: 0.6}, {ID: "gstack", Score: 0.58}}, topCos: 0.6}
	ids, _, _, mode, _, _ := decide("long enough prompt here", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "embed" || len(ids) != 2 || ids[0] != "gstack-qa" {
		t.Fatalf("ids=%v mode=%q", ids, mode)
	}
	// -ranker=hybrid keeps its own mode label for the usage log
	_, _, _, mode, _, _ = decide("long enough prompt here", 3, 0.55, testMinLen, 0, pri, "hybrid", nil)
	if mode != "hybrid" {
		t.Fatalf("mode=%q, want hybrid", mode)
	}
}

func TestDecide_GatedEmptyBelowThreshold(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "gstack-qa", Score: 0.2}}, topCos: 0.2}
	ids, _, _, mode, _, _ := decide("long enough prompt here", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "gated-empty" || len(ids) != 0 {
		t.Fatalf("expected gated-empty, got ids=%v mode=%q", ids, mode)
	}
}

// A failed embedder surfaces nothing: there is no lexical fallback (206 lexical hints in
// production were followed 1.5% of the time). The PRIMARY ERROR is returned for the log — an
// empty Err made a per-index model misconfiguration indistinguishable from a dead endpoint
// (review 2026-08-16).
func TestDecide_EmbedderDown_SurfacesNothing(t *testing.T) {
	pri := fakePrimary{err: errBoom}
	ids, hidden, topCos, mode, cands, errStr := decide("one two three four five six seven eight", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "embedder-down" || len(ids) != 0 || len(hidden) != 0 || topCos != 0 || cands != nil {
		t.Fatalf("a failed embedder must stay silent: ids=%v cos=%v mode=%q", ids, topCos, mode)
	}
	if errStr != "boom" {
		t.Fatalf("embedder-down must carry the primary error, got %q", errStr)
	}
}

func TestDecide_TooShort(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "a", Score: 0.9}}, topCos: 0.9}
	// "ok go" is 5 chars (trimmed) — below minLen=6
	ids, _, topCos, mode, _, _ := decide("ok go", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "too-short" {
		t.Fatalf("expected mode too-short, got %q", mode)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no surfaced ids for short prompt, got %v", ids)
	}
	if topCos != 0 {
		t.Fatalf("expected topCos=0 for short prompt, got %v", topCos)
	}
}

func TestDecide_TooShort_Whitespace(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "a", Score: 0.9}}, topCos: 0.9}
	// Padded with spaces but trimmed is still short (2 < 6)
	ids, _, _, mode, _, _ := decide("   ok   ", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "too-short" || len(ids) != 0 {
		t.Fatalf("expected too-short for whitespace-padded short prompt, got ids=%v mode=%q", ids, mode)
	}
}

func TestEmit_EmptyWhenNoSkills(t *testing.T) {
	if emit("") != "" {
		t.Fatal("emit should be empty string when context is empty")
	}
	out := emit("some context")
	if !strings.Contains(out, "additionalContext") || !strings.Contains(out, "UserPromptSubmit") {
		t.Fatalf("emit JSON malformed: %s", out)
	}
}

func TestEmit_ExactShape(t *testing.T) {
	out := emit("hello ctx")
	var got hookOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("emit output not valid JSON: %v", err)
	}
	if got.HookSpecificOutput.HookEventName != "UserPromptSubmit" || got.HookSpecificOutput.AdditionalContext != "hello ctx" {
		t.Fatalf("bad shape: %+v", got)
	}
}

func TestFormatContext_AllMissingReturnsEmpty(t *testing.T) {
	if got := formatContext(map[string]catalog.Skill{}, []string{"x", "y"}); got != "" {
		t.Fatalf("expected empty when all ids missing, got %q", got)
	}
}

// formatContext must surface the invocable ID, not an internal name.
func TestFormatContext_UsesInvocableID(t *testing.T) {
	byID := map[string]catalog.Skill{
		"superpowers:brainstorming": {ID: "superpowers:brainstorming", Name: "brainstorming", Source: "superpowers", Description: "explore intent first"},
	}
	got := formatContext(byID, []string{"superpowers:brainstorming"})
	if !strings.Contains(got, "- superpowers:brainstorming (superpowers):") {
		t.Fatalf("label must be the invocable id: %q", got)
	}
}

// The deadline branch must FOLD INTO an existing attribution at the RECORD
// level, never replace it: overwriting erased the only record that a
// tpl-mismatched index needs a rebuild whenever the quota-hint read overran
// (review 2026-08-16; round 2 proved the pure-function test alone left the
// call site a mutation survivor, so this exercises the record the branch
// actually stamps).
func TestStampDeadlinePreservesAttribution(t *testing.T) {
	prior := "index identity: index built with embedding template embeddinggemma/tpl9, unknown to this binary"
	rec := usagelog.Record{Mode: "tpl-mismatch", Err: prior}
	stampDeadline(&rec)
	if rec.Mode != "error" {
		t.Fatalf("mode: %q", rec.Mode)
	}
	if !strings.Contains(rec.Err, prior) || !strings.Contains(rec.Err, "deadline exceeded") {
		t.Fatalf("attribution lost: %q", rec.Err)
	}
	fresh := usagelog.Record{}
	stampDeadline(&fresh)
	if fresh.Err != "deadline exceeded" || fresh.Mode != "error" {
		t.Fatalf("fresh record: %+v", fresh)
	}
}

// joinErr never drops a cause on conflict — the round-2 finding: a pre-set
// -ranker notice permanently masked the primary retriever's failure.
func TestJoinErrKeepsBothCauses(t *testing.T) {
	if got := joinErr("", "b"); got != "b" {
		t.Fatalf("empty prior: %q", got)
	}
	got := joinErr("unknown -ranker \"bogus\" — running embed", "primary retriever: boom")
	if !strings.Contains(got, "unknown -ranker") || !strings.Contains(got, "primary retriever: boom") {
		t.Fatalf("a cause was dropped: %q", got)
	}
}

var errBoom = errBoomT{}

type errBoomT struct{}

func (errBoomT) Error() string { return "boom" }
