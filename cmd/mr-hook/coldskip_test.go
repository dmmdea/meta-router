package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/retrievers"
)

type stubPrimary struct{ calls int }

func (s *stubPrimary) RetrieveScored(string, int) ([]retrievers.Scored, float64, error) {
	s.calls++
	return []retrievers.Scored{{ID: "a", Score: 0.9}}, 0.9, nil
}

func newStubGate(p retrievers.EmbedProbe, warmErr error) (*coldSkipRetriever, *stubPrimary, *[]string) {
	inner := &stubPrimary{}
	var warmed []string
	return &coldSkipRetriever{
		inner: inner,
		probe: func() retrievers.EmbedProbe { return p },
		warm:  func(target string) error { warmed = append(warmed, target); return warmErr },
	}, inner, &warmed
}

func TestColdSkipRetriever_ColdWarmableWarmsOnceAndSkipsTheEmbed(t *testing.T) {
	g, inner, warmed := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "embed model not loaded"}, nil)
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if !errors.Is(err, errColdEmbedder) {
		t.Fatalf("want errColdEmbedder, got %v", err)
	}
	if inner.calls != 0 {
		t.Fatalf("a cold embedder must not be embedded into (%d calls)", inner.calls)
	}
	if len(*warmed) != 1 || (*warmed)[0] != "http://router" {
		t.Fatalf("want exactly one warm-up to the answering router, got %v", *warmed)
	}
	if !strings.Contains(err.Error(), "warm-up sent") || !strings.Contains(err.Error(), "embed model not loaded") {
		t.Fatalf("the row must say why and what was done: %q", err.Error())
	}
}

func TestColdSkipRetriever_LoadInFlightSendsNoSecondRequest(t *testing.T) {
	g, _, warmed := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: false, Reason: "embed model is loading"}, nil)
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if !errors.Is(err, errColdEmbedder) || len(*warmed) != 0 {
		t.Fatalf("err=%v warm-ups=%v, want a skip with no warm-up", err, *warmed)
	}
}

func TestColdSkipRetriever_FailedWarmUpIsRecordedNotFatal(t *testing.T) {
	g, _, _ := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "r"}, errors.New("boom"))
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if !errors.Is(err, errColdEmbedder) || !strings.Contains(err.Error(), "warm-up failed: boom") {
		t.Fatalf("a failed warm-up must still skip the embed and say so: %v", err)
	}
}

func TestColdSkipRetriever_WarmAndUnknownEmbedAsBefore(t *testing.T) {
	for _, st := range []retrievers.EmbedState{retrievers.EmbedWarm, retrievers.EmbedUnknown} {
		g, inner, warmed := newStubGate(retrievers.EmbedProbe{State: st}, nil)
		res, top, err := g.RetrieveScored("a prompt long enough", 3)
		if err != nil || inner.calls != 1 || len(*warmed) != 0 || top != 0.9 || len(res) != 1 {
			t.Fatalf("state %d: err=%v inner calls=%d warm-ups=%v", st, err, inner.calls, *warmed)
		}
	}
}

// decide() must log a cold skip as cold-skip-*, never embedder-down, while keeping the same
// BM25 precision gate.
func TestDecide_ColdSkipModes(t *testing.T) {
	cold := failedRetriever{err: errors.Join(errColdEmbedder, errors.New("detail"))}
	strong := stubLex{{ID: "qa", Score: 30}}
	ids, cos, mode, cands, errStr := decide("qa test web application", 3, 0.55, 6, cold, "embed", strong)
	if mode != "cold-skip-bm25" || len(ids) != 1 || ids[0] != "qa" || cos != 0 || cands != nil || !strings.Contains(errStr, "cold") {
		t.Fatalf("strong lexical match: mode=%q ids=%v cos=%v cands=%v err=%q", mode, ids, cos, cands, errStr)
	}
	weak := stubLex{{ID: "qa", Score: 1.0}}
	ids, _, mode, _, errStr = decide("one two three four five six", 3, 0.55, 6, cold, "embed", weak)
	if mode != "cold-skip-empty" || len(ids) != 0 || !strings.Contains(errStr, "cold") {
		t.Fatalf("weak lexical match: mode=%q ids=%v err=%q", mode, ids, errStr)
	}
	// A genuinely failed embedder keeps its old classification.
	dead := failedRetriever{err: errors.New("connection refused")}
	if _, _, mode, _, _ = decide("one two three four five six", 3, 0.55, 6, dead, "embed", weak); mode != "embedder-down" {
		t.Fatalf("a dead embedder must stay embedder-down, got %q", mode)
	}
}

type stubLex []retrievers.Scored

func (s stubLex) RetrieveScored(string, int) []retrievers.Scored { return s }

func TestHookBudgets(t *testing.T) {
	// Off: exactly the arithmetic the hook always had.
	for _, c := range []struct{ timeoutMs, wantEmbedMs int }{{1000, 950}, {300, 250}, {50, 50}, {6000, 5950}} {
		embed, probe := hookBudgets(c.timeoutMs, false)
		if embed != time.Duration(c.wantEmbedMs)*time.Millisecond || probe != 0 {
			t.Fatalf("off, %dms: embed=%v probe=%v, want embed=%dms probe=0", c.timeoutMs, embed, probe, c.wantEmbedMs)
		}
	}
	// On: the probe ceiling is carved out of the embed budget, so probe + embed never exceeds
	// what embed alone had, whatever the deadline.
	for _, ms := range []int{1000, 300, 100, 60, 50, 20, 6000} {
		baseEmbed, _ := hookBudgets(ms, false)
		embed, probe := hookBudgets(ms, true)
		if probe <= 0 || probe > runningProbeCap || embed <= 0 {
			t.Fatalf("%dms: embed=%v probe=%v", ms, embed, probe)
		}
		if embed+probe > baseEmbed {
			t.Fatalf("%dms: probe %v + embed %v overruns the %v the embed call alone had", ms, probe, embed, baseEmbed)
		}
	}
	if embed, probe := hookBudgets(1000, true); embed != 850*time.Millisecond || probe != 100*time.Millisecond {
		t.Fatalf("production deadline: embed=%v probe=%v, want 850ms and 100ms", embed, probe)
	}
}
