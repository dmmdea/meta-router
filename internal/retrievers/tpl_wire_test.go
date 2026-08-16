package retrievers

// W9-P item 1 wire tests: the template registry only matters if the templated
// bytes actually REACH the embedder/reranker. These capture the HTTP request
// bodies and assert the model id and wrapped texts on the wire — the layer a
// registry unit test cannot see.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
