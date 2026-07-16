package retrievers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
)

type Embed struct {
	ids  []string
	vecs [][]float64
	eps  []Endpoint
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
// endpoint is an endpoint SPEC (see ResolveEndpoints): empty means "resolve for
// this machine", a single URL pins one, a comma-separated list gives an explicit
// failover order.
func EmbedTexts(endpoint string, timeout time.Duration, inputs []string) ([][]float64, error) {
	return embed(newHTTPClient(timeout), resolveEndpoints(endpoint), inputs)
}

// unusableErr marks a candidate endpoint as worth abandoning for the next one:
// it is not serving embeddings here. Only a request-level REJECTION by a live
// embedder is fatal — see embedOne.
type unusableErr struct{ err error }

func (u unusableErr) Error() string { return u.err.Error() }
func (u unusableErr) Unwrap() error { return u.err }

// embed POSTs to each candidate in order and returns the first success. This is
// what makes one binary work on every machine: a host serving the embedder on
// llama-swap and a host serving it from a sidecar both work with no config,
// because a candidate that is not there simply refuses the connection (a
// sub-millisecond loopback RST) and the next one answers.
//
// The whole walk shares ONE deadline (hc.Timeout). http.Client.Timeout is applied
// per request, so without this a chain of N candidates could burn N × the budget
// — overshooting mr-hook's hard deadline and leaving no time for the BM25 fallback,
// which would surface NOTHING instead of degrading. The budget is the budget,
// however many candidates it is spread across.
//
// If every candidate fails, the last error is returned and the caller degrades
// (mr-hook → BM25 fallback) — it never blocks the user.
func embed(hc *http.Client, eps []Endpoint, inputs []string) ([][]float64, error) {
	if len(eps) == 0 {
		return nil, fmt.Errorf("embed: no endpoint configured")
	}
	ctx := context.Background()
	if hc.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, hc.Timeout)
		defer cancel()
	}
	var lastErr error
	for _, ep := range eps {
		if ctx.Err() != nil {
			break // budget spent — don't start a dial we cannot finish
		}
		// Never send prompt text to a port nobody configured until we have
		// confirmed an embedder is actually the thing listening on it.
		if ep.Unverified && !probeIsEmbedder(ctx, hc, ep.URL) {
			lastErr = fmt.Errorf("%s: no embedder advertised at /v1/models", ep.URL)
			continue
		}
		vs, err := embedOne(ctx, hc, ep.URL, inputs)
		if err == nil {
			return vs, nil
		}
		lastErr = fmt.Errorf("%s: %w", ep.URL, err)
		var unusable unusableErr
		if !errors.As(err, &unusable) {
			return nil, lastErr // a live embedder rejected THIS request — surface it
		}
	}
	if lastErr == nil {
		lastErr = ctx.Err()
	}
	return nil, fmt.Errorf("embed: all %d endpoint(s) failed; last: %w", len(eps), lastErr)
}

// probeIsEmbedder asks an unconfigured candidate what it serves, and only
// approves it if it advertises an embedding model. This keeps the built-in
// convenience chain from POSTing the user's prompt to whatever unrelated service
// happens to occupy the port on some future machine.
func probeIsEmbedder(ctx context.Context, hc *http.Client, ep string) bool {
	req, err := http.NewRequestWithContext(ctx, "GET", ep+"/v1/models", nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return false
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false
	}
	for _, m := range out.Data {
		if strings.Contains(strings.ToLower(m.ID), "embed") {
			return true
		}
	}
	return false
}

func embedOne(ctx context.Context, hc *http.Client, ep string, inputs []string) ([][]float64, error) {
	body, _ := json.Marshal(map[string]any{"model": "embeddinggemma", "input": inputs})
	req, err := http.NewRequestWithContext(ctx, "POST", ep+"/v1/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, unusableErr{err} // refused / DNS / timeout → try the next
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, unusableErr{err}
	}
	if resp.StatusCode != http.StatusOK {
		snippet := raw
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		err := fmt.Errorf("embed: status %d: %s", resp.StatusCode, snippet)
		// Fatal ONLY when a live embedder rejected THIS request on its merits —
		// a bad/oversized input. Everything else (401/403/405/429/404/5xx/…) means
		// "this port is not usefully serving us embeddings right now", which is a
		// reason to try the next candidate, not to give up and silently degrade.
		switch resp.StatusCode {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return nil, err
		default:
			return nil, unusableErr{err}
		}
	}
	vs, err := parseEmbedResponse(raw, len(inputs))
	if err != nil {
		// 200 but not an embeddings payload → some other service owns this port.
		return nil, unusableErr{err}
	}
	return vs, nil
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
	eps := resolveEndpoints(endpoint)
	texts := make([]string, len(skills))
	ids := make([]string, len(skills))
	for i, s := range skills {
		texts[i] = s.EmbedText()
		ids[i] = s.ID
	}
	// Reuse the stored client for the build embed rather than letting EmbedTexts
	// allocate a separate throwaway one.
	vs, err := embed(hc, eps, texts)
	if err != nil {
		return nil, err
	}
	return &Embed{ids: ids, vecs: vs, eps: eps, hc: hc}, nil
}

// NewEmbedFromVectors builds an Embed from already-computed skill vectors (from
// the persisted index) — it does NOT embed the skills. Only the query is
// embedded at retrieve time. timeout bounds the per-query embed HTTP call.
func NewEmbedFromVectors(ids []string, vecs [][]float64, endpoint string, timeout time.Duration) *Embed {
	return &Embed{ids: ids, vecs: vecs, eps: resolveEndpoints(endpoint), hc: newHTTPClient(timeout)}
}

func (e *Embed) Name() string { return "embed-egemma" }

// rankByCosine embeds the prompt once and returns every skill ranked by cosine
// similarity (desc). The first element's Score is the max cosine — the
// confidence signal the surfacer gates on. Shared by Retrieve and the hybrid.
func (e *Embed) rankByCosine(prompt string) ([]Scored, error) {
	qv, err := embed(e.hc, e.eps, []string{prompt})
	if err != nil {
		return nil, err
	}
	if len(qv) == 0 {
		return nil, fmt.Errorf("embed: empty query vector")
	}
	// The endpoint that answered may not be the one that BUILT the index (that is
	// the price of a failover chain). A different model means a different vector
	// space: at best the cosines are meaningless, at worst the dimensions differ.
	// Refuse rather than score garbage — the caller degrades to BM25, which is the
	// honest answer. (Without this the length mismatch used to panic inside the
	// hook's worker goroutine, which no recover() can catch and which would kill
	// the fail-open exit-0 contract.)
	if len(e.vecs) > 0 && len(qv[0]) != len(e.vecs[0]) {
		return nil, fmt.Errorf("embed: query dim %d != index dim %d — the endpoint that answered serves a different model than the one that built the index; rebuild the index or pin the right endpoint", len(qv[0]), len(e.vecs[0]))
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

// cosine is length-safe by construction: it walks only the overlap. Callers are
// expected to reject dimension mismatches outright (see rankByCosine) — this is
// the belt-and-braces guard so a stray mismatch can never panic a hook that is
// contractually required to exit 0.
func cosine(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
