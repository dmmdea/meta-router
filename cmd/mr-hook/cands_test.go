package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/retrievers"
	"github.com/dmmdea/meta-router/internal/usagelog"
)

// W9 R9.2b: usage.jsonl logged only the top-1 cosine and bare surfaced names,
// so every retrospective curve used the event's top-1 as a proxy for the
// invoked skill's own score — an upper bound, documented as such in the R9.2
// doc. Logging per-candidate scores (names + numbers only; no prompt text, so
// the privacy posture is unchanged) makes the next curve exact. Surfacing
// behavior must be untouched.

func scoredPri(pairs ...retrievers.Scored) fakePrimary {
	top := 0.0
	if len(pairs) > 0 {
		top = pairs[0].Score
	}
	return fakePrimary{res: pairs, topCos: top}
}

func mustJSONStr(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// EmbedRerank propagates every failure BY DESIGN — correct for the eval, where
// a silent fallback would score embed-only under the "embed+rerank" label. In
// PRODUCTION that same strictness is a regression: a reranker hiccup would make
// decide() report embedder-down and surface NOTHING, when the embed ordering
// was available the whole time. Degrade to embed order, and say so in the mode
// so the log never claims a rerank that did not happen.
func TestRerankDegradesToEmbedOrderWhenRerankerFails(t *testing.T) {
	embedRes := []retrievers.Scored{
		{ID: "a", Score: 0.61},
		{ID: "b", Score: 0.55},
	}
	ro := &rerankOrEmbed{
		embed:     fakePrimary{res: embedRes, topCos: 0.61},
		reorderer: failReorder{errors.New("connection refused")},
		depth:     20,
	}

	ids, _, top, mode, cands, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, ro, "rerank", nil)

	if len(ids) != 2 || ids[0] != "a" {
		t.Fatalf("a reranker outage must still surface the EMBED ordering, got %v", ids)
	}
	if top != 0.61 {
		t.Fatalf("the gate must still see the embed cosine, got %v", top)
	}
	if len(cands) != 2 {
		t.Fatalf("candidates must still be logged, got %d", len(cands))
	}
	if !ro.degraded {
		t.Fatal("the degradation must be observable so the mode can tell the truth")
	}
	if ro.degradeErr == "" {
		t.Fatal("the reranker's error must be kept, not dropped — otherwise the outage is invisible in usage.jsonl")
	}
	if mode != "rerank" {
		t.Fatalf("decide reports the requested mode; the caller rewrites it on degradation (mode=%q)", mode)
	}
	if got := ro.mode("rerank"); got != "embed" {
		t.Fatalf("a degraded run must be logged as %q, not as a rerank that never ran; got %q", "embed", got)
	}
}

// The degradation must be recorded on OUTCOMES OTHER than the surfaced one.
// Reading it only on the surfaced path hid the outage on gated-empty rows —
// ~50% of live traffic — making them byte-identical to healthy rows.
func TestRerankDegradationIsRecordedOnGatedEmptyToo(t *testing.T) {
	ro := &rerankOrEmbed{
		// below the 0.40 gate, so decide returns "gated-empty", not "rerank"
		embed:     fakePrimary{res: []retrievers.Scored{{ID: "a", Score: 0.30}}, topCos: 0.30},
		reorderer: failReorder{errors.New("HTTP 503")},
		depth:     20,
	}
	_, _, _, mode, _, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, ro, "rerank", nil)
	if mode != "gated-empty" {
		t.Fatalf("expected the gated outcome, got %q", mode)
	}
	if !ro.degraded || ro.degradeErr == "" {
		t.Fatal("a gated-empty row must still carry the fact that the reranker was down")
	}
}

// A genuine EMBEDDER failure must still propagate — degrading only covers the
// cross-encoder. Swallowing an embedder outage would surface nothing while
// reporting success.
func TestRerankOrEmbedPropagatesEmbedderFailure(t *testing.T) {
	ro := &rerankOrEmbed{
		embed:     fakePrimary{err: errors.New("embedder down")},
		reorderer: failReorder{errors.New("unused")},
		depth:     20,
	}
	_, _, _, mode, _, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, ro, "rerank", nil)
	if mode != "embedder-down" {
		t.Fatalf("an embedder failure must not be masked by the rerank wrapper, got %q", mode)
	}
	if ro.degraded {
		t.Fatal("an embedder failure is not a cross-encoder degradation")
	}
}

// The cross-encoder must see a WIDE pool: reranking only the already-surfaced
// few cannot rescue a skill the embedder buried, which is the whole point.
func TestRerankOrEmbedRetrievesDepthNotK(t *testing.T) {
	spy := &depthSpy{res: []retrievers.Scored{{ID: "a", Score: 0.9}}, topCos: 0.9}
	ro := &rerankOrEmbed{embed: spy, reorderer: identityReorder{}, depth: 20}
	if _, _, err := ro.RetrieveScored("q", 3); err != nil {
		t.Fatal(err)
	}
	if spy.lastK != 20 {
		t.Fatalf("the embed pool must be the rerank depth (20), got %d", spy.lastK)
	}
}

// flipReorder reverses the candidate order, standing in for a cross-encoder
// that disagrees with the embedder.
type flipReorder struct{}

func (flipReorder) Reorder(_ string, c []retrievers.Scored) ([]int, error) {
	out := make([]int, len(c))
	for i := range c {
		out[i] = len(c) - 1 - i
	}
	return out, nil
}

type failReorder struct{ err error }

func (f failReorder) Reorder(string, []retrievers.Scored) ([]int, error) { return nil, f.err }

type identityReorder struct{}

func (identityReorder) Reorder(_ string, c []retrievers.Scored) ([]int, error) {
	out := make([]int, len(c))
	for i := range c {
		out[i] = i
	}
	return out, nil
}

type depthSpy struct {
	res    []retrievers.Scored
	topCos float64
	lastK  int
}

func (d *depthSpy) RetrieveScored(_ string, k int) ([]retrievers.Scored, float64, error) {
	d.lastK = k
	return d.res, d.topCos, nil
}

// When the reranker works, nothing is degraded and the mode stands.
func TestRerankOrEmbedReportsRerankWhenHealthy(t *testing.T) {
	// embed returns a,b; the reranker flips it to b,a — the reordering must win.
	ro := &rerankOrEmbed{
		embed:     fakePrimary{res: []retrievers.Scored{{ID: "a", Score: 0.61}, {ID: "b", Score: 0.55}}, topCos: 0.61},
		reorderer: flipReorder{},
		depth:     20,
	}
	ids, _, _, _, _, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, ro, "rerank", nil)
	if len(ids) != 2 || ids[0] != "b" {
		t.Fatalf("healthy rerank ordering must win, got %v", ids)
	}
	if ro.degraded {
		t.Fatal("a healthy rerank must not report degradation")
	}
	if got := ro.mode("rerank"); got != "rerank" {
		t.Fatalf("healthy mode must stay %q, got %q", "rerank", got)
	}
}

// Wiring the reranker must NOT move the gate. EmbedRerank passes the embed max
// cosine through as topCos, so for any given prompt the gate decision is
// byte-identical to embed-only — reranking changes WHICH skills surface, never
// WHETHER anything surfaces. If this ever regresses, the reranker would be
// silently re-tuning a gate calibrated on a different scale.
func TestRerankModeGatesIdenticallyToEmbed(t *testing.T) {
	// topCos below the gate: both modes must suppress, and both must still log
	// candidates so the gated region stays measurable.
	pri := scoredPri(
		retrievers.Scored{ID: "a", Score: 0.30},
		retrievers.Scored{ID: "b", Score: 0.20},
	)
	for _, mode := range []string{"embed", "rerank"} {
		ids, _, top, m, cands, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, pri, mode, nil)
		if m != "gated-empty" || len(ids) != 0 {
			t.Fatalf("%s: below-gate must surface nothing, got mode=%q ids=%v", mode, m, ids)
		}
		if top != 0.30 {
			t.Fatalf("%s: topCos must be the embed cosine, got %v", mode, top)
		}
		if len(cands) != 2 {
			t.Fatalf("%s: gated rows must still carry candidates, got %d", mode, len(cands))
		}
	}

	// topCos above the gate: both modes must surface.
	pri = scoredPri(
		retrievers.Scored{ID: "a", Score: 0.61},
		retrievers.Scored{ID: "b", Score: 0.55},
	)
	for _, mode := range []string{"embed", "rerank"} {
		ids, _, _, m, _, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, pri, mode, nil)
		if m != mode || len(ids) != 2 {
			t.Fatalf("%s: above-gate must surface, got mode=%q ids=%v", mode, m, ids)
		}
	}
}

// Rerank rows carry REAL cosines (only the order comes from the cross-encoder),
// so unlike hybrid they must be logged.
func TestRerankModeLogsCosineCandidates(t *testing.T) {
	pri := scoredPri(
		retrievers.Scored{ID: "deploy", Score: 0.52}, // reranked to the front
		retrievers.Scored{ID: "ship", Score: 0.71},
	)
	_, _, _, _, cands, _ := decide("a long enough prompt to pass minLen", 3, 0.40, 5, 0, pri, "rerank", nil)
	if len(cands) != 2 {
		t.Fatalf("rerank rows must log candidates, got %d", len(cands))
	}
	// Each candidate keeps ITS OWN cosine, not a rank-derived or logit value —
	// note they are deliberately NOT descending here, because the order is the
	// reranker's while the numbers stay the embedder's.
	if cands[0].ID != "deploy" || cands[0].Cos != 0.52 || cands[1].Cos != 0.71 {
		t.Fatalf("candidates must carry their own embed cosines in surfaced order, got %+v", cands)
	}
	for _, c := range cands {
		if c.Cos < 0 {
			t.Fatalf("a negative score means a cross-encoder logit leaked into the cosine field: %+v", cands)
		}
	}
}

// The candidates must be logged even when the gate SUPPRESSES surfacing —
// gated-empty events are 27% of live traffic and exactly the region the gate
// decision needs scores for.
func TestDecideLogsCandidatesOnGatedEmpty(t *testing.T) {
	pri := scoredPri(retrievers.Scored{ID: "a", Score: 0.42}, retrievers.Scored{ID: "b", Score: 0.31})
	ids, _, _, mode, cands, _ := decide("long enough prompt here", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "gated-empty" || ids != nil {
		t.Fatalf("expected gated-empty surface-nothing, got mode=%s ids=%v", mode, ids)
	}
	if len(cands) != 2 || cands[0].ID != "a" || cands[1].ID != "b" {
		t.Fatalf("gated-empty must still log candidates: %+v", cands)
	}
	if cands[0].Cos != 0.42 {
		t.Fatalf("candidate cosine wrong: %+v", cands[0])
	}
}

// The incident this change fixes: candidates the model cannot invoke took the k slots and
// carried the gate, and were dropped only afterwards, so 43% of gate-passing prompts surfaced
// nothing. An ineligible candidate takes no slot, carries no gate, and each shown candidate
// must clear the cosine floor itself.
func TestIneligibleCandidatesNeitherTakeSlotNorCarryGate(t *testing.T) {
	notH := func(id string) bool { return id != "H" }
	pri := scoredPri(
		retrievers.Scored{ID: "H", Score: 0.60}, retrievers.Scored{ID: "A", Score: 0.44}, retrievers.Scored{ID: "B", Score: 0.38},
	)
	ids, hidden, _, mode, _, _ := decide("long enough prompt here", 3, 0.40, testMinLen, 0, pri, "embed", notH)
	if mode != "embed" || len(ids) != 1 || ids[0] != "A" || len(hidden) != 1 || hidden[0] != "H" {
		t.Fatalf("want ids=[A] hidden=[H] mode=embed, got ids=%v hidden=%v mode=%q", ids, hidden, mode)
	}
	// The best ELIGIBLE cosine is below the floor: the ineligible 0.60 must not carry the gate.
	pri = scoredPri(
		retrievers.Scored{ID: "H", Score: 0.60}, retrievers.Scored{ID: "A", Score: 0.39}, retrievers.Scored{ID: "B", Score: 0.38},
	)
	ids, _, _, mode, _, _ = decide("long enough prompt here", 3, 0.40, testMinLen, 0, pri, "embed", notH)
	if mode != "gated-empty" || len(ids) != 0 {
		t.Fatalf("want no ids and gated-empty, got ids=%v mode=%q", ids, mode)
	}
}

// No cosine ran → no candidates. The analysis slices by mode; a non-cosine score in
// the cosine field would recontaminate the denominator R9.2 just cleaned.
func TestNoCandidatesOnNonCosineModes(t *testing.T) {
	pri := fakePrimary{err: errors.New("embedder down")}
	_, _, _, mode, cands, _ := decide("one two three four five six seven eight", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "embedder-down" {
		t.Fatalf("mode=%s", mode)
	}
	if cands != nil {
		t.Fatalf("embedder-down must log no cosine candidates: %+v", cands)
	}
	_, _, _, mode, cands, _ = decide("hi", 3, 0.55, testMinLen, 0, pri, "embed", nil)
	if mode != "too-short" || cands != nil {
		t.Fatalf("too-short must log no candidates: %s %+v", mode, cands)
	}
}

func TestRecordMarshalsCandsCompactAndOmitsWhenAbsent(t *testing.T) {
	r := usagelog.Record{TsUnix: 1, Mode: "embed", Cands: []usagelog.Cand{{ID: "a", Cos: 0.421875123}}}
	b := mustJSONStr(t, r)
	if !strings.Contains(b, `"cands":[{"id":"a","cos":0.4219}]`) {
		t.Fatalf("cands must round to 4 decimals: %s", b)
	}
	r2 := usagelog.Record{TsUnix: 1, Mode: "too-short"}
	if strings.Contains(mustJSONStr(t, r2), "cands") {
		t.Fatal("absent cands must be omitted from the row")
	}
}

// Hybrid's .Score is an RRF fused score, not a cosine — logging it under
// "cos" would contaminate the denominator with 0.03-scale numbers that
// gated-empty rows cannot even be filtered on (no ranker field). Cands is
// embed-only (review 2026-07-30, MAJOR).
func TestNoCandidatesOnHybridRanker(t *testing.T) {
	pri := scoredPri(retrievers.Scored{ID: "a", Score: 0.033}, retrievers.Scored{ID: "b", Score: 0.016})
	ids, _, _, mode, cands, _ := decide("long enough prompt here", 3, 0.01, testMinLen, 0, pri, "hybrid", nil)
	if mode != "hybrid" || len(ids) == 0 {
		t.Fatalf("expected surfaced hybrid, got mode=%s ids=%v", mode, ids)
	}
	if cands != nil {
		t.Fatalf("hybrid rows must log no cands (RRF scores are not cosines): %+v", cands)
	}
	_, _, _, mode, cands, _ = decide("long enough prompt here", 3, 0.99, testMinLen, 0, pri, "hybrid", nil)
	if mode != "gated-empty" || cands != nil {
		t.Fatalf("hybrid gated-empty must also log no cands: %s %+v", mode, cands)
	}
}
