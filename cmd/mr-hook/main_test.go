package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/retrievers"
)

type fakePrimary struct {
	res    []retrievers.Scored
	topCos float64
	err    error
}

func (f fakePrimary) RetrieveScored(p string, k int) ([]retrievers.Scored, float64, error) {
	return f.res, f.topCos, f.err
}

type fakeLex struct{ res []retrievers.Scored }

func (f fakeLex) RetrieveScored(p string, k int) []retrievers.Scored {
	if k < len(f.res) {
		return f.res[:k]
	}
	return f.res
}

const testMinLen = 6

func TestDecide_PrimaryAboveThreshold(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "gstack-qa"}, {ID: "gstack"}}, topCos: 0.6}
	ids, _, mode := decide("long enough prompt here", 3, 0.55, testMinLen, pri, "embed", fakeLex{})
	if mode != "embed" || len(ids) != 2 || ids[0] != "gstack-qa" {
		t.Fatalf("ids=%v mode=%q", ids, mode)
	}
	// -ranker=hybrid keeps its own mode label for the usage log
	_, _, mode = decide("long enough prompt here", 3, 0.55, testMinLen, pri, "hybrid", fakeLex{})
	if mode != "hybrid" {
		t.Fatalf("mode=%q, want hybrid", mode)
	}
}

func TestDecide_GatedEmptyBelowThreshold(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "gstack-qa"}}, topCos: 0.2}
	ids, _, mode := decide("long enough prompt here", 3, 0.55, testMinLen, pri, "embed", fakeLex{res: []retrievers.Scored{{ID: "z", Score: 99}}})
	if mode != "gated-empty" || len(ids) != 0 {
		t.Fatalf("expected gated-empty, got ids=%v mode=%q", ids, mode)
	}
}

// Embedder down + no strong lexical signal → silent (embedder-down).
func TestDecide_EmbedderDown_WeakLexStaysSilent(t *testing.T) {
	pri := fakePrimary{err: errBoom}
	// 8 prompt tokens, top score 5: raw 5 < 18 and 5/8 < 1.5 → gated
	ids, topCos, mode := decide("one two three four five six seven eight", 3, 0.55, testMinLen, pri, "embed", fakeLex{res: []retrievers.Scored{{ID: "x", Score: 5}}})
	if mode != "embedder-down" || len(ids) != 0 || topCos != 0 {
		t.Fatalf("weak lexical match must stay silent: ids=%v cos=%v mode=%q", ids, topCos, mode)
	}
}

// Embedder down + overwhelming raw BM25 score → surface exactly the top-1.
func TestDecide_EmbedderDown_StrongRawSurfacesTop1(t *testing.T) {
	pri := fakePrimary{err: errBoom}
	lex := fakeLex{res: []retrievers.Scored{{ID: "gstack-qa", Score: 25}, {ID: "noise", Score: 24}}}
	ids, _, mode := decide("one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen", 3, 0.55, testMinLen, pri, "embed", lex)
	if mode != "bm25-fallback" {
		t.Fatalf("mode=%q, want bm25-fallback", mode)
	}
	if len(ids) != 1 || ids[0] != "gstack-qa" {
		t.Fatalf("fallback must surface only the single top match, got %v", ids)
	}
}

// Embedder down + short sharply-lexical prompt → per-token gate fires.
func TestDecide_EmbedderDown_PerTokenGate(t *testing.T) {
	pri := fakePrimary{err: errBoom}
	lex := fakeLex{res: []retrievers.Scored{{ID: "gstack-qa", Score: 8}}}
	// 4 tokens → 8/4 = 2.0 >= 1.5
	ids, _, mode := decide("run gstack qa now", 3, 0.55, testMinLen, pri, "embed", lex)
	if mode != "bm25-fallback" || len(ids) != 1 || ids[0] != "gstack-qa" {
		t.Fatalf("per-token gate should fire: ids=%v mode=%q", ids, mode)
	}
}

func TestDecide_EmbedderDown_NoLexResults(t *testing.T) {
	pri := fakePrimary{err: errBoom}
	ids, topCos, mode := decide("long enough prompt here", 3, 0.55, testMinLen, pri, "embed", fakeLex{})
	if mode != "embedder-down" || len(ids) != 0 || topCos != 0 {
		t.Fatalf("no lexical results must stay silent: ids=%v cos=%v mode=%q", ids, topCos, mode)
	}
}

func TestDecide_TooShort(t *testing.T) {
	pri := fakePrimary{res: []retrievers.Scored{{ID: "a"}}, topCos: 0.9}
	// "ok go" is 5 chars (trimmed) — below minLen=6
	ids, topCos, mode := decide("ok go", 3, 0.55, testMinLen, pri, "embed", fakeLex{res: []retrievers.Scored{{ID: "x", Score: 99}}})
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
	pri := fakePrimary{res: []retrievers.Scored{{ID: "a"}}, topCos: 0.9}
	// Padded with spaces but trimmed is still short (2 < 6)
	ids, _, mode := decide("   ok   ", 3, 0.55, testMinLen, pri, "embed", fakeLex{})
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

var errBoom = errBoomT{}

type errBoomT struct{}

func (errBoomT) Error() string { return "boom" }
