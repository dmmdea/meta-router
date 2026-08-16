package retrievers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/embedtpl"
)

func testTimeout() time.Duration { return 5 * time.Second }

// W9 R9.3: a cross-encoder pass over the embed top-N is the standard fix for
// the 0.40–0.55 mid-band (57.4% of scored live traffic). This retriever exists
// FOR THE EVAL — production wiring only happens if it wins on the gold set.

func rerankServer(t *testing.T, scores map[string]float64, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			http.NotFound(w, r)
			return
		}
		if calls != nil {
			*calls++
		}
		var req struct {
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type res struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		}
		out := struct {
			Results []res `json:"results"`
		}{}
		for i, d := range req.Documents {
			out.Results = append(out.Results, res{Index: i, RelevanceScore: scores[d]})
		}
		// The live server returns results sorted by score desc — mirror that.
		for i := 0; i < len(out.Results); i++ {
			for j := i + 1; j < len(out.Results); j++ {
				if out.Results[j].RelevanceScore > out.Results[i].RelevanceScore {
					out.Results[i], out.Results[j] = out.Results[j], out.Results[i]
				}
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
}

func rerankSkills() []catalog.Skill {
	return []catalog.Skill{
		{ID: "ship", Name: "ship", Description: "ship the release"},
		{ID: "diagram", Name: "diagram", Description: "draw a diagram"},
		{ID: "deploy", Name: "deploy", Description: "configure deployment"},
	}
}

// docScores keys a score map by each skill's REAL document text (EmbedText:
// name + description + when_to_use) — the first fixture keyed on bare
// descriptions, every lookup missed, all scores were zero, and the "reorder"
// test was actually asserting on preserved primary order.
func docScores(byID map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for _, sk := range rerankSkills() {
		if v, ok := byID[sk.ID]; ok {
			out[sk.EmbedText()] = v
		}
	}
	return out
}

type fixedOrder struct{ ids []string }

func (f fixedOrder) Name() string { return "fixed" }
func (f fixedOrder) RetrieveScored(prompt string, k int) ([]Scored, float64, error) {
	out := make([]Scored, 0, k)
	for i, id := range f.ids {
		if i >= k {
			break
		}
		out = append(out, Scored{ID: id, Score: 1.0 - float64(i)*0.1})
	}
	top := 0.0
	if len(out) > 0 {
		top = out[0].Score
	}
	return out, top, nil
}

// RetrieveScored is what lets the reranker reach PRODUCTION: mr-hook's decide()
// needs ([]Scored, topCos, error), gates on topCos, and logs .Score as a cosine.
//
// The load-bearing property is that wiring rerank must NOT move the gate. So
// RetrieveScored returns the RERANKED ORDER but keeps each candidate's EMBED
// COSINE in .Score, and returns the primary's max cosine as topCos. Same
// prompts pass the gate as embed-only; only WHICH skills surface changes.
func TestEmbedRerankScoredKeepsCosinesAndGate(t *testing.T) {
	srv := rerankServer(t, docScores(map[string]float64{
		"ship": -3.0, "diagram": -10.0, "deploy": -1.0, // reranker prefers deploy
	}), nil)
	defer srv.Close()

	primary := fixedOrder{ids: []string{"ship", "diagram", "deploy"}}
	er := NewEmbedRerank(primary, rerankSkills(), srv.URL, 2*time.Second, embedtpl.RerankRaw(DefaultRerankModel))

	got, topCos, err := er.RetrieveScored("does not matter", 3)
	if err != nil {
		t.Fatalf("RetrieveScored: %v", err)
	}

	// Order comes from the cross-encoder.
	if len(got) != 3 || got[0].ID != "deploy" {
		t.Fatalf("reranked order must win, got %+v", got)
	}

	// topCos must be the PRIMARY's max cosine, untouched — the gate compares
	// against this, so anything else silently re-tunes the production gate.
	wantTop := 1.0 // fixedOrder's first score
	if topCos != wantTop {
		t.Fatalf("topCos must be the embed max cosine %v, got %v", wantTop, topCos)
	}

	// Each .Score must still be that candidate's EMBED cosine, not a rerank
	// logit — cands is cosines-or-nothing (R9.2b). "deploy" was embed-rank 3,
	// so its cosine is 1.0-0.2 = 0.8 even though it now surfaces first.
	if got[0].Score != 0.8 {
		t.Fatalf("surfaced-first candidate must carry its own EMBED cosine 0.8, got %v", got[0].Score)
	}
	for _, s := range got {
		if s.Score < 0 {
			t.Fatalf("a negative score means a cross-encoder logit leaked into the cosine field: %+v", got)
		}
	}
}

// k must bound the RESULT while the cross-encoder still sees the wide pool —
// otherwise reranking only the already-surfaced few cannot rescue a burial,
// which is the entire point.
func TestEmbedRerankScoredRerankspDepthButReturnsK(t *testing.T) {
	srv := rerankServer(t, docScores(map[string]float64{
		"ship": -3.0, "diagram": -10.0, "deploy": -1.0,
	}), nil)
	defer srv.Close()

	primary := &countingPrimary{inner: fixedOrder{ids: []string{"ship", "diagram", "deploy"}}}
	er := NewEmbedRerank(primary, rerankSkills(), srv.URL, 2*time.Second, embedtpl.RerankRaw(DefaultRerankModel))

	got, _, err := er.RetrieveScored("q", 1)
	if err != nil {
		t.Fatalf("RetrieveScored: %v", err)
	}
	if len(got) != 1 || got[0].ID != "deploy" {
		t.Fatalf("k must bound the result, got %+v", got)
	}
	if primary.lastK != rerankDepth {
		t.Fatalf("the cross-encoder must see rerankDepth=%d candidates, primary was asked for %d", rerankDepth, primary.lastK)
	}
}

type countingPrimary struct {
	inner fixedOrder
	lastK int
}

func (c *countingPrimary) Name() string { return "counting" }
func (c *countingPrimary) RetrieveScored(prompt string, k int) ([]Scored, float64, error) {
	c.lastK = k
	return c.inner.RetrieveScored(prompt, k)
}

// The reranker must be able to OVERRULE the embed order — that is its entire
// value over the mid-band.
func TestEmbedRerankReorders(t *testing.T) {
	srv := rerankServer(t, docScores(map[string]float64{
		"ship": -3.0, "diagram": -10.0, "deploy": -1.0, // cross-encoder likes deploy best
	}), nil)
	defer srv.Close()

	er := NewEmbedRerank(fixedOrder{ids: []string{"ship", "diagram", "deploy"}}, rerankSkills(), srv.URL, testTimeout(), embedtpl.RerankRaw(DefaultRerankModel))
	got, err := er.Retrieve("deploy the fleet", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "deploy" || got[1] != "ship" {
		t.Fatalf("rerank must reorder by cross-encoder score: %v", got)
	}
}

// In the EVAL leg a rerank failure must PROPAGATE, never fall back to embed
// order: a silent fallback would score embed-only under the embed+rerank label
// and contaminate the exact comparison this retriever exists to make.
func TestEmbedRerankErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	er := NewEmbedRerank(fixedOrder{ids: []string{"ship", "diagram"}}, rerankSkills(), srv.URL, testTimeout(), embedtpl.RerankRaw(DefaultRerankModel))
	if _, err := er.Retrieve("anything at all", 2); err == nil {
		t.Fatal("rerank failure must propagate in the eval retriever, not silently fall back")
	}
}

// Rerank depth: the wrapped retriever is asked for rerankDepth candidates even
// when the caller wants fewer, so the cross-encoder can PROMOTE something the
// embed order buried below k.
func TestEmbedRerankPullsDepthCandidates(t *testing.T) {
	srv := rerankServer(t, docScores(map[string]float64{
		"ship": -5.0, "diagram": -6.0, "deploy": -1.0, // buried at embed rank 3, must surface at k=1
	}), nil)
	defer srv.Close()

	er := NewEmbedRerank(fixedOrder{ids: []string{"ship", "diagram", "deploy"}}, rerankSkills(), srv.URL, testTimeout(), embedtpl.RerankRaw(DefaultRerankModel))
	got, err := er.Retrieve("deploy the fleet", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "deploy" {
		t.Fatalf("a candidate buried below k must be promotable: %v", got)
	}
}

// An ID the skill map cannot resolve to a document must fail loudly — sending
// a mismatched documents array would silently rerank the WRONG texts.
func TestEmbedRerankUnknownIDFails(t *testing.T) {
	srv := rerankServer(t, nil, nil)
	defer srv.Close()
	er := NewEmbedRerank(fixedOrder{ids: []string{"ghost"}}, rerankSkills(), srv.URL, testTimeout(), embedtpl.RerankRaw(DefaultRerankModel))
	if _, err := er.Retrieve("anything at all", 1); err == nil {
		t.Fatal("an unresolvable candidate id must be an error, not a silent skip")
	}
}

// Review LOW: the refusal branches were code-covered but unpinned — a server
// regression returning a truncated results array, a duplicate index, or
// garbage JSON must all REFUSE (propagate), never partially score.
func TestRerankRefusalBranches(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated results", `{"results":[{"index":0,"relevance_score":-1.0}]}`},
		{"duplicate index", `{"results":[{"index":0,"relevance_score":-1.0},{"index":0,"relevance_score":-2.0}]}`},
		{"out-of-range index", `{"results":[{"index":0,"relevance_score":-1.0},{"index":9,"relevance_score":-2.0}]}`},
		{"garbage json", `{"results": not-json`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			er := NewEmbedRerank(fixedOrder{ids: []string{"ship", "diagram"}}, rerankSkills(), srv.URL, testTimeout(), embedtpl.RerankRaw(DefaultRerankModel))
			if _, err := er.Retrieve("anything at all", 2); err == nil {
				t.Fatalf("%s must refuse, not partially score", c.name)
			}
		})
	}
}
