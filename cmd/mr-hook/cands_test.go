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

// The candidates must be logged even when the gate SUPPRESSES surfacing —
// gated-empty events are 27% of live traffic and exactly the region the gate
// decision needs scores for.
func TestDecideLogsCandidatesOnGatedEmpty(t *testing.T) {
	pri := scoredPri(retrievers.Scored{ID: "a", Score: 0.42}, retrievers.Scored{ID: "b", Score: 0.31})
	ids, _, mode, cands := decide("long enough prompt here", 3, 0.55, testMinLen, pri, "embed", fakeLex{})
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

// When the gate passes, the surfaced ids must be exactly the FIRST k logged
// candidates — retrieving deeper for logging must never change what surfaces.
func TestSurfacedIsPrefixOfCandidates(t *testing.T) {
	pri := scoredPri(
		retrievers.Scored{ID: "a", Score: 0.70}, retrievers.Scored{ID: "b", Score: 0.65},
		retrievers.Scored{ID: "c", Score: 0.60}, retrievers.Scored{ID: "d", Score: 0.58},
	)
	ids, topCos, mode, cands := decide("long enough prompt here", 2, 0.55, testMinLen, pri, "embed", fakeLex{})
	if mode != "embed" || topCos != 0.70 {
		t.Fatalf("mode=%s topCos=%v", mode, topCos)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("surface must stay the first k: %v", ids)
	}
	if len(cands) != 4 || cands[3].ID != "d" {
		t.Fatalf("all retrieved candidates must be logged: %+v", cands)
	}
	for i, c := range cands[:2] {
		if c.ID != ids[i] {
			t.Fatalf("surfaced must be a prefix of cands: ids=%v cands=%+v", ids, cands)
		}
	}
}

// No cosine ran → no candidates. The analysis slices by mode; a bm25 score in
// the cosine field would recontaminate the denominator R9.2 just cleaned.
func TestNoCandidatesOnNonCosineModes(t *testing.T) {
	pri := fakePrimary{err: errors.New("embedder down")}
	_, _, mode, cands := decide("one two three four five six seven eight", 3, 0.55, testMinLen, pri, "embed", fakeLex{res: []retrievers.Scored{{ID: "x", Score: 99}}})
	if mode != "bm25-fallback" {
		t.Fatalf("mode=%s", mode)
	}
	if cands != nil {
		t.Fatalf("bm25-fallback must log no cosine candidates: %+v", cands)
	}
	_, _, mode, cands = decide("hi", 3, 0.55, testMinLen, pri, "embed", fakeLex{})
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
