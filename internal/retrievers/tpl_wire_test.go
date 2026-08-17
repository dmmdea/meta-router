package retrievers

// W9-P item 1 wire tests: the template registry only matters if the templated
// bytes actually REACH the embedder/reranker. These capture the HTTP request
// bodies and assert the model id and wrapped texts on the wire — the layer a
// registry unit test cannot see.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/embedtpl"
)

type embedCapture struct {
	mu     sync.Mutex
	models []string
	inputs [][]string
}

func captureEmbedder(t *testing.T, cap *embedCapture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		cap.mu.Lock()
		cap.models = append(cap.models, req.Model)
		cap.inputs = append(cap.inputs, req.Input)
		cap.mu.Unlock()
		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i := range req.Input {
			out.Data = append(out.Data, item{Index: i, Embedding: []float64{1, float64(i)}})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The query template and model id must reach the embedder verbatim.
func TestQueryTemplateReachesTheWire(t *testing.T) {
	var cap embedCapture
	srv := captureEmbedder(t, &cap)
	spec, ok := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	if !ok {
		t.Fatal("registry missing embeddinggemma tpl1")
	}
	e := NewEmbedFromVectors([]string{"skills:a"}, [][]float64{{1, 0}}, srv.URL, time.Second, spec)
	if _, _, err := e.RetrieveScored("fix the failing test", 1); err != nil {
		t.Fatal(err)
	}
	if len(cap.inputs) != 1 || len(cap.inputs[0]) != 1 {
		t.Fatalf("expected one embed call with one input, got %+v", cap.inputs)
	}
	if got, want := cap.inputs[0][0], "task: search result | query: fix the failing test"; got != want {
		t.Fatalf("query on the wire:\n got %q\nwant %q", got, want)
	}
	if cap.models[0] != "embeddinggemma" {
		t.Fatalf("model on the wire: %q", cap.models[0])
	}
}

// An untemplated (legacy) spec must put the RAW prompt on the wire —
// byte-identical to pre-registry behavior.
func TestRawSpecKeepsWireBytesUnchanged(t *testing.T) {
	var cap embedCapture
	srv := captureEmbedder(t, &cap)
	e := NewEmbedFromVectors([]string{"skills:a"}, [][]float64{{1, 0}}, srv.URL, time.Second, embedtpl.Raw("embeddinggemma"))
	if _, _, err := e.RetrieveScored("fix the failing test", 1); err != nil {
		t.Fatal(err)
	}
	if got := cap.inputs[0][0]; got != "fix the failing test" {
		t.Fatalf("raw spec must not touch the prompt, got %q", got)
	}
}

// NewEmbed (build-side) must doc-template every skill text and request the
// spec's model.
func TestDocTemplateReachesTheWire(t *testing.T) {
	var cap embedCapture
	srv := captureEmbedder(t, &cap)
	spec, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	skills := []catalog.Skill{{ID: "skills:a", Name: "a", Description: "alpha"}}
	if _, err := NewEmbed(skills, srv.URL, spec); err != nil {
		t.Fatal(err)
	}
	if got, want := cap.inputs[0][0], "title: none | text: "+skills[0].EmbedText(); got != want {
		t.Fatalf("doc on the wire:\n got %q\nwant %q", got, want)
	}
}

// A blank model must refuse loudly BEFORE any bytes leave the process — a
// server default silently deciding the vector space is the failure this
// guards against.
func TestBlankModelRefusesBeforeTheWire(t *testing.T) {
	var cap embedCapture
	srv := captureEmbedder(t, &cap)
	e := NewEmbedFromVectors([]string{"skills:a"}, [][]float64{{1, 0}}, srv.URL, time.Second, embedtpl.Spec{})
	if _, _, err := e.RetrieveScored("prompt", 1); err == nil {
		t.Fatal("blank model must error")
	}
	if len(cap.inputs) != 0 {
		t.Fatalf("no request may be sent with a blank model, got %+v", cap.inputs)
	}
}

// The rerank twin of the blank-model guard: a zero RerankSpec must refuse
// loudly before any bytes leave the process (review 2026-08-16, G4 — the
// guard existed with zero coverage, i.e. deletable without a test going red).
func TestRerankBlankModelRefusesBeforeTheWire(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	skills := []catalog.Skill{{ID: "s1", Name: "n1", Description: "d1"}}
	er := NewEmbedRerank(fixedOrder{ids: []string{"s1"}}, skills, srv.URL, time.Second, embedtpl.RerankSpec{})
	_, _, err := er.RetrieveScored("prompt", 1)
	if err == nil || !strings.Contains(err.Error(), "no model") {
		t.Fatalf("blank rerank model must error, got %v", err)
	}
	if hits != 0 {
		t.Fatalf("no request may be sent with a blank rerank model (%d)", hits)
	}
}

// The embed label must carry the FULL identity: the bake-off's headline
// comparison is raw vs tpl1 of the same model, and a label that collapses
// them poisons the results table (review 2026-08-16, MAJOR).
func TestEmbedNameCarriesTemplateVersion(t *testing.T) {
	raw := NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, embedtpl.Raw("embeddinggemma"))
	if raw.Name() != "embed-egemma" {
		t.Fatalf("raw embeddinggemma label changed: %q", raw.Name())
	}
	spec, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	tpl := NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, spec)
	if tpl.Name() != "embed-embeddinggemma/tpl1" {
		t.Fatalf("templated label must carry the version: %q", tpl.Name())
	}
	qwen, _ := embedtpl.Lookup("qwen3-embedding-4b-q4", embedtpl.TplV1)
	if got := NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, qwen).Name(); got != "embed-qwen3-embedding-4b-q4/tpl1" {
		t.Fatalf("qwen templated label: %q", got)
	}
}

// Label canary for EVERY spec-taking retriever: two arms differing only by
// template must never share a results-table label (round 2: the Embed fix
// alone left Hybrid and EmbedRerank collapsing their raw and tpl1 arms).
func TestRetrieverNamesDifferAcrossSpecs(t *testing.T) {
	raw := embedtpl.Raw("embeddinggemma")
	tpl, _ := embedtpl.Lookup("embeddinggemma", embedtpl.TplV1)
	skills := []catalog.Skill{{ID: "s1", Name: "n1", Description: "d1"}}

	if a, b := (&Hybrid{embed: NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, raw)}).Name(),
		(&Hybrid{embed: NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, tpl)}).Name(); a == b {
		t.Fatalf("hybrid labels collapse: %q", a)
	}
	rawEmb := NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, raw)
	tplEmb := NewEmbedFromVectors(nil, nil, "http://127.0.0.1:1", time.Second, tpl)
	bge := embedtpl.RerankRaw(DefaultRerankModel)
	if a, b := NewEmbedRerank(rawEmb, skills, "e", time.Second, bge).Name(),
		NewEmbedRerank(tplEmb, skills, "e", time.Second, bge).Name(); a == b {
		t.Fatalf("embed+rerank labels collapse across embed specs: %q", a)
	}
	if a, b := NewEmbedRerank(rawEmb, skills, "e", time.Second, bge).Name(),
		NewEmbedRerank(rawEmb, skills, "e", time.Second, embedtpl.RerankFor("qwen3-reranker-4b")).Name(); a == b {
		t.Fatalf("embed+rerank labels collapse across rerank specs: %q", a)
	}
	// Historical production labels stay stable.
	if got := NewEmbedRerank(rawEmb, skills, "e", time.Second, bge).Name(); got != "embed+rerank" {
		t.Fatalf("production rerank label changed: %q", got)
	}
	if got := (&Hybrid{embed: rawEmb}).Name(); got != "hybrid-rrf" {
		t.Fatalf("production hybrid label changed: %q", got)
	}
}

// Rerank side: model id, formatted query, and formatted docs on the wire.
func TestRerankFormattingReachesTheWire(t *testing.T) {
	var gotModel, gotQuery string
	var gotDocs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		gotModel, gotQuery, gotDocs = req.Model, req.Query, req.Documents
		type res struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		out := struct {
			Results []res `json:"results"`
		}{}
		for i := range req.Documents {
			out.Results = append(out.Results, res{Index: i, RelevanceScore: float64(len(req.Documents) - i)})
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	skills := []catalog.Skill{{ID: "s1", Name: "n1", Description: "d1"}}
	primary := fixedOrder{ids: []string{"s1"}}
	rspec := embedtpl.RerankFor("qwen3-reranker-4b")
	er := NewEmbedRerank(primary, skills, srv.URL, time.Second, rspec)
	if _, _, err := er.RetrieveScored("what broke", 1); err != nil {
		t.Fatal(err)
	}
	if gotModel != "qwen3-reranker-4b" {
		t.Fatalf("model on the wire: %q", gotModel)
	}
	if want := rspec.ApplyQuery("what broke"); gotQuery != want {
		t.Fatalf("rerank query on the wire:\n got %q\nwant %q", gotQuery, want)
	}
	if len(gotDocs) != 1 || gotDocs[0] != rspec.ApplyDoc(skills[0].EmbedText()) {
		t.Fatalf("rerank docs on the wire: %q", gotDocs)
	}
}
