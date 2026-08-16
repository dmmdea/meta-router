package retrievers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/embedtpl"
)

// DefaultRerankModel is the production cross-encoder: resident on the local
// endpoint, plain (uninstructed) formatting per its registry entry.
const DefaultRerankModel = "bge-reranker-v2-m3"

// W9 R9.3: EmbedRerank is the cross-encoder eval leg — embed retrieves
// rerankDepth candidates, bge-reranker-v2-m3 (already resident on the local
// endpoint; zero new models per the local-stack-reuse rule) re-scores each
// (query, skill-text) pair, and the top k by relevance win. It exists to be
// MEASURED on the gold set against embed-only; production wiring happens only
// behind W9's recall/precision gate.
//
// Scores are raw cross-encoder logits (negative is normal); only their ORDER
// matters here, and they are never logged as cosines — the R9.2b lesson
// (hybrid's RRF scores nearly landing in the cosine denominator) applies to
// this producer identically.

// rerankDepth is how many embed candidates the cross-encoder re-scores. Wide
// enough to promote mid-band burials (the 0.40–0.55 region is 57.4% of scored
// live traffic), narrow enough that one rerank call stays cheap on CPU.
const rerankDepth = 20

// scoredNamed is the minimal upstream surface EmbedRerank needs — *Embed
// satisfies it, and eval fixtures fake it.
type scoredNamed interface {
	Name() string
	RetrieveScored(prompt string, k int) ([]Scored, float64, error)
}

type EmbedRerank struct {
	primary  scoredNamed
	docByID  map[string]string
	endpoint string // base URL; /v1/rerank appended
	timeout  time.Duration
	rspec    embedtpl.RerankSpec
}

// NewEmbedRerank composes an embed-style retriever with the local reranker.
// Documents derive from each skill's EmbedText — the same canonical text the
// embedder indexed — wrapped in the RERANKER's own formatting (rspec): each
// model ranks the representation it was trained on (embedtpl registry).
func NewEmbedRerank(primary scoredNamed, skills []catalog.Skill, endpoint string, timeout time.Duration, rspec embedtpl.RerankSpec) *EmbedRerank {
	docs := make(map[string]string, len(skills))
	for _, s := range skills {
		docs[s.ID] = rspec.ApplyDoc(s.EmbedText())
	}
	return &EmbedRerank{primary: primary, docByID: docs, endpoint: endpoint, timeout: timeout, rspec: rspec}
}

func (e *EmbedRerank) Name() string { return "embed+rerank" }

// Retrieve pulls rerankDepth candidates from the primary, re-scores them with
// the cross-encoder, and returns the top k IDs by relevance (ties broken by
// the primary's order, which the stable sort preserves).
//
// EVERY failure propagates. This retriever exists to measure the reranker; a
// silent fallback to embed order would score embed-only under the
// "embed+rerank" label and quietly poison the comparison.
// RetrieveScored is the PRODUCTION surface: it returns the reranked order
// while each candidate keeps its own EMBED COSINE in .Score, and topCos is the
// primary's max cosine, untouched.
//
// Both of those are deliberate and load-bearing:
//
//   - mr-hook gates on topCos against -min-cosine. Returning a cross-encoder
//     logit there would silently re-tune the production gate to a scale it was
//     never calibrated on (logits are negative; every prompt would gate out).
//     Passing the embed max through means EXACTLY the same prompts pass the
//     gate as embed-only — reranking changes WHICH skills surface, never
//     WHETHER anything surfaces.
//   - usage.jsonl's cands field is cosines-or-nothing (R9.2b). Keeping each
//     candidate's own cosine preserves that invariant, and the reordering is
//     still visible because cands is logged in surfaced order.
//
// Failures propagate exactly as in Retrieve. mr-hook already degrades a
// primary-retriever error to the BM25 fallback, so a reranker outage costs the
// rerank ordering, never the surfacing.
func (e *EmbedRerank) RetrieveScored(prompt string, k int) ([]Scored, float64, error) {
	cands, topCos, err := e.primary.RetrieveScored(prompt, rerankDepth)
	if err != nil {
		return nil, 0, fmt.Errorf("rerank primary: %w", err)
	}
	if len(cands) == 0 {
		return nil, topCos, nil
	}
	order, err := e.rerankOrder(prompt, cands)
	if err != nil {
		return nil, 0, err
	}
	if k > len(order) {
		k = len(order)
	}
	out := make([]Scored, k)
	for i := 0; i < k; i++ {
		out[i] = cands[order[i]] // ID + its EMBED cosine, in reranked position
	}
	return out, topCos, nil
}

// Reorder re-scores an ALREADY-RETRIEVED candidate list with the cross-encoder
// and returns indices into it, best first.
//
// It is exported so a caller that already holds the embed result can reorder it
// WITHOUT a second retrieval. mr-hook needs that: it runs under one hard
// deadline, and a fallback that re-embeds cannot fit
// (embed + rerank + embed > deadline), so a slow reranker would blow the
// deadline instead of degrading. With Reorder the degraded path costs nothing —
// the caller simply keeps the order it already has.
func (e *EmbedRerank) Reorder(prompt string, cands []Scored) ([]int, error) {
	return e.rerankOrder(prompt, cands)
}

// rerankOrder re-scores cands with the cross-encoder and returns indices into
// cands, best first. Shared by Retrieve and RetrieveScored so the two can never
// disagree about ordering.
func (e *EmbedRerank) rerankOrder(prompt string, cands []Scored) ([]int, error) {
	docs := make([]string, len(cands))
	for i, c := range cands {
		d, ok := e.docByID[c.ID]
		if !ok {
			// A mismatched documents array would rerank the WRONG texts and
			// report a number that means nothing — refuse instead.
			return nil, fmt.Errorf("rerank: candidate %q has no document text", c.ID)
		}
		docs[i] = d
	}
	scores, err := rerankScores(e.endpoint, e.timeout, e.rspec.Model, e.rspec.ApplyQuery(prompt), docs)
	if err != nil {
		return nil, fmt.Errorf("rerank: %w", err)
	}
	order := make([]int, len(cands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	return order, nil
}

func (e *EmbedRerank) Retrieve(prompt string, k int) ([]string, error) {
	cands, _, err := e.primary.RetrieveScored(prompt, rerankDepth)
	if err != nil {
		return nil, fmt.Errorf("rerank primary: %w", err)
	}
	if len(cands) == 0 {
		return nil, nil
	}
	order, err := e.rerankOrder(prompt, cands)
	if err != nil {
		return nil, err
	}
	if k > len(order) {
		k = len(order)
	}
	out := make([]string, k)
	for i := 0; i < k; i++ {
		out[i] = cands[order[i]].ID
	}
	return out, nil
}

// rerankScores calls the Jina-compatible /v1/rerank endpoint and returns one
// relevance score per input document, in DOCUMENT ORDER (the server returns
// results sorted by score; the index field maps them back). query and docs
// must already carry the model's formatting (the caller owns it via rspec).
func rerankScores(endpoint string, timeout time.Duration, model, query string, docs []string) ([]float64, error) {
	if model == "" {
		// Same discipline as embed(): a blank model would rank under whatever
		// the server picks — a score nobody can attribute. Loud, no default.
		return nil, fmt.Errorf("rerank: no model specified")
	}
	body, err := json.Marshal(map[string]any{
		"model":     model,
		"query":     query,
		"documents": docs,
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint+"/v1/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rerank endpoint: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Results []struct {
			Index          int     `json:"index"`
			RelevanceScore float64 `json:"relevance_score"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Results) != len(docs) {
		return nil, fmt.Errorf("rerank: %d results for %d documents", len(out.Results), len(docs))
	}
	scores := make([]float64, len(docs))
	seen := make([]bool, len(docs))
	for _, r := range out.Results {
		if r.Index < 0 || r.Index >= len(docs) || seen[r.Index] {
			return nil, fmt.Errorf("rerank: invalid or duplicate result index %d", r.Index)
		}
		seen[r.Index] = true
		scores[r.Index] = r.RelevanceScore
	}
	return scores, nil
}
