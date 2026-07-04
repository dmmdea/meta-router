package retrievers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"time"
	"github.com/dmmdea/meta-router/internal/catalog"
)

type Embed struct {
	ids  []string
	vecs [][]float64
	ep   string
	hc   *http.Client
}

// ConnectTimeout bounds the TCP dial separately from the total request
// budget (MR-13): against a dead or unroutable endpoint the dial fails in
// ~200ms instead of burning the whole per-prompt deadline (up to ~950ms)
// before mr-hook can fall back. A warm local llama-swap accepts connections
// in microseconds, so 200ms adds no risk on the happy path.
var ConnectTimeout = 200 * time.Millisecond

// newHTTPClient builds a client with the split connect/total timeouts.
func newHTTPClient(total time.Duration) *http.Client {
	return &http.Client{
		Timeout: total,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: ConnectTimeout}).DialContext,
		},
	}
}

// EmbedTexts embeds inputs via the OpenAI-compatible /v1/embeddings endpoint
// with a caller-chosen timeout. Single shared entrypoint for all embedding.
func EmbedTexts(endpoint string, timeout time.Duration, inputs []string) ([][]float64, error) {
	return embed(newHTTPClient(timeout), endpoint, inputs)
}

func embed(hc *http.Client, ep string, inputs []string) ([][]float64, error) {
	body, _ := json.Marshal(map[string]any{"model": "embeddinggemma", "input": inputs})
	req, err := http.NewRequest("POST", ep+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("embed: status %d: %s", resp.StatusCode, snippet)
	}
	return parseEmbedResponse(raw, len(inputs))
}

// parseEmbedResponse pairs each returned embedding with its input by the API's
// `index` field (never by array position) and validates the count, so a
// reordered or short response can never misalign a skill with another's vector.
func parseEmbedResponse(body []byte, wantN int) ([][]float64, error) {
	var out struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	if len(out.Data) != wantN {
		return nil, fmt.Errorf("embed: got %d vectors, want %d", len(out.Data), wantN)
	}
	vs := make([][]float64, wantN)
	for _, d := range out.Data {
		if d.Index < 0 || d.Index >= wantN {
			return nil, fmt.Errorf("embed: index %d out of range [0,%d)", d.Index, wantN)
		}
		if vs[d.Index] != nil {
			return nil, fmt.Errorf("embed: duplicate index %d", d.Index)
		}
		vs[d.Index] = d.Embedding
	}
	return vs, nil
}

// Scored pairs a skill id with a similarity or fused score.
type Scored struct {
	ID    string
	Score float64
}

func NewEmbed(skills []catalog.Skill, endpoint string) (*Embed, error) {
	hc := newHTTPClient(30 * time.Second)
	texts := make([]string, len(skills))
	ids := make([]string, len(skills))
	for i, s := range skills {
		texts[i] = s.EmbedText()
		ids[i] = s.ID
	}
	// Reuse the stored client for the build embed rather than letting EmbedTexts
	// allocate a separate throwaway one.
	vs, err := embed(hc, endpoint, texts)
	if err != nil {
		return nil, err
	}
	return &Embed{ids: ids, vecs: vs, ep: endpoint, hc: hc}, nil
}

// NewEmbedFromVectors builds an Embed from already-computed skill vectors (from
// the persisted index) — it does NOT embed the skills. Only the query is
// embedded at retrieve time. timeout bounds the per-query embed HTTP call.
func NewEmbedFromVectors(ids []string, vecs [][]float64, endpoint string, timeout time.Duration) *Embed {
	return &Embed{ids: ids, vecs: vecs, ep: endpoint, hc: newHTTPClient(timeout)}
}

func (e *Embed) Name() string { return "embed-egemma" }

// rankByCosine embeds the prompt once and returns every skill ranked by cosine
// similarity (desc). The first element's Score is the max cosine — the
// confidence signal the surfacer gates on. Shared by Retrieve and the hybrid.
func (e *Embed) rankByCosine(prompt string) ([]Scored, error) {
	qv, err := embed(e.hc, e.ep, []string{prompt})
	if err != nil {
		return nil, err
	}
	if len(qv) == 0 {
		return nil, fmt.Errorf("embed: empty query vector")
	}
	out := make([]Scored, len(e.vecs))
	for i, v := range e.vecs {
		out[i] = Scored{e.ids[i], cosine(qv[0], v)}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// RetrieveScored returns the top-k skills by cosine and the max cosine over
// the whole catalog (the confidence signal the surfacer gates on). Same
// contract as Hybrid.RetrieveScored, so the hook can rank embed-only.
func (e *Embed) RetrieveScored(prompt string, k int) ([]Scored, float64, error) {
	ranked, err := e.rankByCosine(prompt)
	if err != nil {
		return nil, 0, err
	}
	var topCos float64
	if len(ranked) > 0 {
		topCos = ranked[0].Score
	}
	out := make([]Scored, 0, k)
	for i := 0; i < len(ranked) && i < k; i++ {
		out = append(out, ranked[i])
	}
	return out, topCos, nil
}

func (e *Embed) Retrieve(prompt string, k int) ([]string, error) {
	ranked, err := e.rankByCosine(prompt)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, k)
	for i := 0; i < len(ranked) && i < k; i++ {
		out = append(out, ranked[i].ID)
	}
	return out, nil
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a { dot += a[i] * b[i]; na += a[i] * a[i]; nb += b[i] * b[i] }
	if na == 0 || nb == 0 { return 0 }
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
