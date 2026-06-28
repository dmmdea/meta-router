package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/retrievers"
)

type fakeHyb struct {
	res    []retrievers.Scored
	topCos float64
	err    error
}

func (f fakeHyb) RetrieveScored(p string, k int) ([]retrievers.Scored, float64, error) {
	return f.res, f.topCos, f.err
}

type fakeLex struct{ ids []string }

func (f fakeLex) Retrieve(p string, k int) ([]string, error) { return f.ids, nil }

const testMinLen = 12

func TestDecide_HybridAboveThreshold(t *testing.T) {
	hyb := fakeHyb{res: []retrievers.Scored{{ID: "skills:a"}, {ID: "skills:b"}}, topCos: 0.6}
	ids, _, mode := decide("long enough prompt here", 3, 0.55, testMinLen, hyb, fakeLex{})
	if mode != "hybrid" || len(ids) != 2 || ids[0] != "skills:a" {
		t.Fatalf("ids=%v mode=%q", ids, mode)
	}
}

func TestDecide_GatedEmptyBelowThreshold(t *testing.T) {
	hyb := fakeHyb{res: []retrievers.Scored{{ID: "skills:a"}}, topCos: 0.2}
	ids, _, mode := decide("long enough prompt here", 3, 0.55, testMinLen, hyb, fakeLex{ids: []string{"skills:z"}})
	if mode != "gated-empty" || len(ids) != 0 {
		t.Fatalf("expected gated-empty, got ids=%v mode=%q", ids, mode)
	}
}

// TestDecide_EmbedderDown verifies that an embedder failure surfaces nothing
// (mode "embedder-down"), not the BM25 fallback results.
func TestDecide_EmbedderDown(t *testing.T) {
	hyb := fakeHyb{err: errBoom}
	ids, topCos, mode := decide("long enough prompt here", 3, 0.55, testMinLen, hyb, fakeLex{ids: []string{"skills:x"}})
	if mode != "embedder-down" {
		t.Fatalf("expected mode embedder-down, got %q", mode)
	}
	if len(ids) != 0 {
		t.Fatalf("expected no surfaced ids on embedder failure, got %v", ids)
	}
	if topCos != 0 {
		t.Fatalf("expected topCos=0 on embedder failure, got %v", topCos)
	}
}

func TestDecide_TooShort(t *testing.T) {
	hyb := fakeHyb{res: []retrievers.Scored{{ID: "skills:a"}}, topCos: 0.9}
	// "ok go" is 5 chars (trimmed) — well below minLen=12
	ids, topCos, mode := decide("ok go", 3, 0.55, testMinLen, hyb, fakeLex{ids: []string{"skills:x"}})
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
	hyb := fakeHyb{res: []retrievers.Scored{{ID: "skills:a"}}, topCos: 0.9}
	// Padded with spaces but trimmed is still short
	ids, _, mode := decide("   ok   ", 3, 0.55, testMinLen, hyb, fakeLex{})
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
	if got := formatContext(map[string]catalog.Skill{}, []string{"skills:x", "skills:y"}); got != "" {
		t.Fatalf("expected empty when all ids missing, got %q", got)
	}
}

var errBoom = errBoomT{}

type errBoomT struct{}

func (errBoomT) Error() string { return "boom" }
