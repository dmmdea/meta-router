package strategy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadArtifactRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ref, err := WriteArtifact(dir, Artifact{StepID: 0, OutcomeClass: "ok", Content: "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(ref) != "0.json" {
		t.Fatalf("ref base = %q, want 0.json", filepath.Base(ref))
	}
	a, err := ReadArtifact(ref)
	if err != nil {
		t.Fatal(err)
	}
	if a.Content != "hello world" || a.OutcomeClass != "ok" {
		t.Fatalf("round-trip lost data: %+v", a)
	}
	if len(a.SHA256) != 64 {
		t.Fatalf("sha256 must be stamped, got %q", a.SHA256)
	}
}

func TestResolveContextConcatsOnlyDeps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ref0, _ := WriteArtifact(dir, Artifact{StepID: 0, OutcomeClass: "ok", Content: "FROM-ZERO"})
	ref1, _ := WriteArtifact(dir, Artifact{StepID: 1, OutcomeClass: "ok", Content: "FROM-ONE"})
	_, _ = WriteArtifact(dir, Artifact{StepID: 2, OutcomeClass: "ok", Content: "FROM-TWO"})
	st := map[int]*StepState{
		0: {OutcomeClass: "ok", ResultRef: ref0},
		1: {OutcomeClass: "ok", ResultRef: ref1},
		2: {OutcomeClass: "ok", ResultRef: filepath.Join(dir, "artifacts", "2.json")},
	}
	// step depends on [0,1] only — 2's content must NOT appear.
	block, err := ResolveContext(dir, []int{0, 1}, st)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "FROM-ZERO") || !strings.Contains(block, "FROM-ONE") {
		t.Fatalf("dep content missing: %q", block)
	}
	if strings.Contains(block, "FROM-TWO") {
		t.Fatal("non-dep content leaked into context (isolation broken)")
	}
	// empty deps → empty block (a root node gets no injected context).
	if b, _ := ResolveContext(dir, nil, st); b != "" {
		t.Fatalf("root node context must be empty, got %q", b)
	}
}

// TestResolveContextMissingRefIsMarkedNotFatal pins that a dep with no recorded
// ResultRef degrades to an inline marker (the executor already gates readiness on
// outcome_class=="ok"; a raced/absent artifact must not sink the whole resolve).
func TestResolveContextMissingRefIsMarkedNotFatal(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d1")
	ref0, _ := WriteArtifact(dir, Artifact{StepID: 0, OutcomeClass: "ok", Content: "FROM-ZERO"})
	st := map[int]*StepState{
		0: {OutcomeClass: "ok", ResultRef: ref0},
		1: {OutcomeClass: "ok"}, // no ResultRef recorded
	}
	block, err := ResolveContext(dir, []int{0, 1}, st)
	if err != nil {
		t.Fatalf("a missing dep ref must not error, got %v", err)
	}
	if !strings.Contains(block, "FROM-ZERO") {
		t.Fatalf("present dep content missing: %q", block)
	}
	if !strings.Contains(block, "step-1") || !strings.Contains(block, "missing") {
		t.Fatalf("missing dep must be marked, got %q", block)
	}
}
