package embedtpl

import (
	"strings"
	"testing"
)

// Template bytes are the contract: an index records only "model/tplN", so the
// N must always reproduce these exact strings. A byte drifting here without a
// version bump silently splits query space from doc space on every deployed
// index — hence exact-string asserts, not Contains.

func TestEmbeddingGemmaTpl1ExactBytes(t *testing.T) {
	s, ok := Lookup("embeddinggemma", TplV1)
	if !ok {
		t.Fatal("embeddinggemma tpl1 missing from registry")
	}
	if got, want := s.ApplyQuery("fix the failing test"), "task: search result | query: fix the failing test"; got != want {
		t.Fatalf("query template:\n got %q\nwant %q", got, want)
	}
	if got, want := s.ApplyDoc("clean-ship use when shipping"), "title: none | text: clean-ship use when shipping"; got != want {
		t.Fatalf("doc template:\n got %q\nwant %q", got, want)
	}
	if s.Identity() != "embeddinggemma/tpl1" {
		t.Fatalf("identity: %q", s.Identity())
	}
}

func TestQwen3EmbeddingTpl1ExactBytes(t *testing.T) {
	s, ok := Lookup("qwen3-embedding-4b-q4", TplV1)
	if !ok {
		t.Fatal("qwen3-embedding-4b-q4 tpl1 missing from registry")
	}
	// The card's f-string has NO space after "Query:" — that asymmetry is
	// deliberate and part of the bytes.
	wantQ := "Instruct: Given a user prompt, retrieve skill descriptions relevant to accomplishing it\nQuery:fix the failing test<|endoftext|>"
	if got := s.ApplyQuery("fix the failing test"); got != wantQ {
		t.Fatalf("query template:\n got %q\nwant %q", got, wantQ)
	}
	// Documents are PLAIN (no instruction) + EOS.
	if got, want := s.ApplyDoc("some skill text"), "some skill text<|endoftext|>"; got != want {
		t.Fatalf("doc template:\n got %q\nwant %q", got, want)
	}
	if s.Identity() != "qwen3-embedding-4b-q4/tpl1" {
		t.Fatalf("identity: %q", s.Identity())
	}
}

// EOS presence is a named gate in the W9-P spec: last-token pooling reads the
// final position, so a missing EOS quietly degrades every qwen3 embedding.
func TestQwen3EOSPresence(t *testing.T) {
	s, _ := Lookup("qwen3-embedding-0.6b", TplV1)
	for _, got := range []string{s.ApplyQuery("q"), s.ApplyDoc("d")} {
		if !strings.HasSuffix(got, "<|endoftext|>") {
			t.Fatalf("qwen3 input must end with EOS, got %q", got)
		}
		if strings.Count(got, "<|endoftext|>") != 1 {
			t.Fatalf("qwen3 input must carry exactly one EOS, got %q", got)
		}
	}
}

func TestQwen3FamilyPrefixMatch(t *testing.T) {
	for _, m := range []string{"qwen3-embedding-0.6b", "qwen3-embedding-4b-q4", "qwen3-embedding-8b"} {
		if _, ok := Current(m); !ok {
			t.Fatalf("family member %q should resolve", m)
		}
	}
	for _, m := range []string{"qwen2-embedding-4b", "qwen3-reranker-4b", "nomic-embed-text", ""} {
		if _, ok := Current(m); ok {
			t.Fatalf("non-member %q must not resolve to a template", m)
		}
	}
}

func TestLookupUnknownVersionFails(t *testing.T) {
	if _, ok := Lookup("embeddinggemma", "tpl9"); ok {
		t.Fatal("unknown version must not resolve")
	}
	if _, ok := Lookup("embeddinggemma", ""); ok {
		t.Fatal("Lookup with empty version must not resolve (Current is the build-time entrypoint)")
	}
}

func TestIdentityRoundTrip(t *testing.T) {
	cases := []struct{ id, model, ver string }{
		{"embeddinggemma", "embeddinggemma", ""},
		{"embeddinggemma/tpl1", "embeddinggemma", "tpl1"},
		{"qwen3-embedding-4b-q4/tpl1", "qwen3-embedding-4b-q4", "tpl1"},
		// A slash inside a model id is a model id, not a version split.
		{"org/model", "org/model", ""},
		{"org/model/tpl2", "org/model", "tpl2"},
		{"", "", ""},
	}
	for _, c := range cases {
		m, v := ParseIdentity(c.id)
		if m != c.model || v != c.ver {
			t.Fatalf("ParseIdentity(%q) = (%q,%q), want (%q,%q)", c.id, m, v, c.model, c.ver)
		}
	}
	if got := Raw("embeddinggemma").Identity(); got != "embeddinggemma" {
		t.Fatalf("raw identity: %q", got)
	}
}

func TestSpecForIndexLegacy(t *testing.T) {
	// Pre-Model-field index and bare-model index both mean raw text.
	for _, id := range []string{"", "embeddinggemma"} {
		s, err := SpecForIndex(id)
		if err != nil {
			t.Fatalf("SpecForIndex(%q): %v", id, err)
		}
		if s.Model != "embeddinggemma" || s.Version != "" {
			t.Fatalf("SpecForIndex(%q) = %+v", id, s)
		}
		if got := s.ApplyQuery("raw stays raw"); got != "raw stays raw" {
			t.Fatalf("legacy spec must pass text through, got %q", got)
		}
	}
}

func TestSpecForIndexTemplated(t *testing.T) {
	s, err := SpecForIndex("embeddinggemma/tpl1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := s.ApplyQuery("q"), "task: search result | query: q"; got != want {
		t.Fatalf("templated spec query: %q", got)
	}
}

// The refusal branch: an identity naming a version (or model) this binary's
// registry does not carry must ERROR — never fall back to raw text.
func TestSpecForIndexUnknownRefuses(t *testing.T) {
	// tpl01 matches the version regex but names no registered version — it
	// must refuse (fail closed), not normalize to tpl1.
	for _, id := range []string{"embeddinggemma/tpl9", "embeddinggemma/tpl01", "mystery-model/tpl1"} {
		if _, err := SpecForIndex(id); err == nil {
			t.Fatalf("SpecForIndex(%q) must refuse", id)
		}
	}
}

// Zero-value specs flow through the hook's exit-0 worker goroutine; a nil
// template func must degrade to pass-through, never panic.
func TestApplyNilSafe(t *testing.T) {
	var s Spec
	if s.ApplyQuery("q") != "q" || s.ApplyDoc("d") != "d" {
		t.Fatal("zero Spec must pass through")
	}
	var r RerankSpec
	if r.ApplyQuery("q") != "q" || r.ApplyDoc("d") != "d" {
		t.Fatal("zero RerankSpec must pass through")
	}
}

func TestRerankForBgeIsRaw(t *testing.T) {
	r := RerankFor("bge-reranker-v2-m3")
	if r.Version != "" {
		t.Fatalf("bge must be unformatted, got version %q", r.Version)
	}
	if r.ApplyQuery("q") != "q" || r.ApplyDoc("d") != "d" {
		t.Fatal("bge rerank spec must pass text through")
	}
}

func TestRerankForQwen3ExactBytes(t *testing.T) {
	r := RerankFor("qwen3-reranker-4b")
	wantQ := "<Instruct>: Given a user prompt, retrieve skill descriptions relevant to accomplishing it\n<Query>: what broke the build"
	if got := r.ApplyQuery("what broke the build"); got != wantQ {
		t.Fatalf("qwen3 rerank query:\n got %q\nwant %q", got, wantQ)
	}
	if got, want := r.ApplyDoc("doc text"), "<Document>: doc text"; got != want {
		t.Fatalf("qwen3 rerank doc: %q", got)
	}
}
