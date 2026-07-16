package retrievers

import (
	"fmt"
	"github.com/dmmdea/meta-router/internal/catalog"
	"sort"
	"time"
)

// Hybrid fuses BM25 (lexical) and embeddinggemma (semantic) rankings via
// Reciprocal Rank Fusion — a clean Go reimplementation of meta_skill's
// BM25+embedding+RRF approach, using real embeddings instead of hash embeddings.
type Hybrid struct {
	bm25  *BM25
	embed *Embed
}

func NewHybrid(skills []catalog.Skill, endpoint string) (*Hybrid, error) {
	e, err := NewEmbed(skills, endpoint)
	if err != nil {
		return nil, err
	}
	return &Hybrid{bm25: NewBM25(skills), embed: e}, nil
}

// NewHybridFromIndex builds a Hybrid from cached skill vectors (the persisted
// index): BM25 from the skill texts, Embed from the stored vectors. Only the
// query is embedded at retrieve time. Used by the per-prompt hook.
func NewHybridFromIndex(skills []catalog.Skill, vecs [][]float64, endpoint string, timeout time.Duration) (*Hybrid, error) {
	if len(skills) != len(vecs) {
		return nil, fmt.Errorf("hybrid: %d skills but %d vectors", len(skills), len(vecs))
	}
	ids := make([]string, len(skills))
	for i, s := range skills {
		ids[i] = s.ID
	}
	return &Hybrid{
		bm25:  NewBM25(skills),
		embed: NewEmbedFromVectors(ids, vecs, endpoint, timeout),
	}, nil
}

func (h *Hybrid) Name() string { return "hybrid-rrf" }

// RetrieveScored returns the top-k RRF-fused results (with fused scores) and the
// maximum embedding cosine over all skills — a confidence signal for gating.
// Embeds the query exactly once.
func (h *Hybrid) RetrieveScored(prompt string, k int) ([]Scored, float64, error) {
	const candN = 50
	const kRRF = 60.0
	b, err := h.bm25.Retrieve(prompt, candN)
	if err != nil {
		return nil, 0, err
	}
	eRanked, err := h.embed.rankByCosine(prompt)
	if err != nil {
		return nil, 0, err
	}
	var topCos float64
	if len(eRanked) > 0 {
		topCos = eRanked[0].Score
	}
	fused := map[string]float64{}
	for i, id := range b {
		fused[id] += 1.0 / (kRRF + float64(i+1))
	}
	// Cap the embed contribution to the top candN (mirrors the old
	// embed.Retrieve(prompt, candN) bound) so the RRF fusion weights match.
	for i := 0; i < len(eRanked) && i < candN; i++ {
		fused[eRanked[i].ID] += 1.0 / (kRRF + float64(i+1))
	}
	scores := make([]Scored, 0, len(fused))
	for id, s := range fused {
		scores = append(scores, Scored{id, s})
	}
	// Total order (score desc, then id asc) — deterministic despite random map
	// iteration, so results are reproducible.
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].Score != scores[j].Score {
			return scores[i].Score > scores[j].Score
		}
		return scores[i].ID < scores[j].ID
	})
	out := make([]Scored, 0, k)
	for i := 0; i < len(scores) && i < k; i++ {
		out = append(out, scores[i])
	}
	return out, topCos, nil
}

func (h *Hybrid) Retrieve(prompt string, k int) ([]string, error) {
	top, _, err := h.RetrieveScored(prompt, k)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(top))
	for i, s := range top {
		ids[i] = s.ID
	}
	return ids, nil
}
