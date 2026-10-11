package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func newStubGate(p retrievers.EmbedProbe, warmState string, warmErr error) (*coldSkipRetriever, *stubPrimary, *[]string) {
	inner := &stubPrimary{}
	var warmed []string
	return &coldSkipRetriever{
		inner:   inner,
		probe:   func() retrievers.EmbedProbe { return p },
		warmTO:  warmUpCap,
		history: func() (int, int64) { return 0, 0 },
		now:     time.Now,
		warm: func(p retrievers.EmbedProbe) (string, error) {
			warmed = append(warmed, p.Target)
			return warmState, warmErr
		},
	}, inner, &warmed
}

func TestColdSkipRetriever_ColdWarmableWarmsOnceAndSkipsTheEmbed(t *testing.T) {
	g, inner, warmed := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "embed model not loaded"}, "starting", nil)
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
	if !strings.Contains(err.Error(), "load confirmed (starting)") || !strings.Contains(err.Error(), "embed model not loaded") {
		t.Fatalf("the row must say why and what was done: %q", err.Error())
	}
}

func TestColdSkipRetriever_LoadInFlightSendsNoSecondRequest(t *testing.T) {
	g, _, warmed := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: false, Reason: "embed model is loading"}, "", nil)
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if !errors.Is(err, errColdEmbedder) || len(*warmed) != 0 {
		t.Fatalf("err=%v warm-ups=%v, want a skip with no warm-up", err, *warmed)
	}
}

func TestColdSkipRetriever_FailedWarmUpIsRecordedNotFatal(t *testing.T) {
	g, _, _ := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "r"}, "", errors.New("boom"))
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if !errors.Is(err, errColdEmbedder) || !strings.Contains(err.Error(), "warm-up failed: boom") {
		t.Fatalf("a failed warm-up must still skip the embed and say so: %v", err)
	}
}

func TestColdSkipRetriever_UnconfirmedWarmUpIsSaidSo(t *testing.T) {
	g, _, warmed := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "r"}, "", nil)
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if !errors.Is(err, errColdEmbedder) || len(*warmed) != 1 || !strings.Contains(err.Error(), "load not confirmed") {
		t.Fatalf("err=%v warm-ups=%v, want a skip that says the load was not confirmed", err, *warmed)
	}
}

func TestColdSkipRetriever_WarmAndUnknownEmbedAsBefore(t *testing.T) {
	for _, st := range []retrievers.EmbedState{retrievers.EmbedWarm, retrievers.EmbedUnknown} {
		g, inner, warmed := newStubGate(retrievers.EmbedProbe{State: st}, "", nil)
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
		embed, probe, warm := hookBudgets(c.timeoutMs, false)
		if embed != time.Duration(c.wantEmbedMs)*time.Millisecond || probe != 0 || warm != 0 {
			t.Fatalf("off, %dms: embed=%v probe=%v warm=%v, want embed=%dms and no cold-path budgets", c.timeoutMs, embed, probe, warm, c.wantEmbedMs)
		}
	}
	// On: the probe ceiling is carved out of the embed budget, so probe + embed never exceeds
	// what embed alone had, whatever the deadline; and the cold path (probe, then warm-up, with
	// no embed) never exceeds it either, so a small -timeout-ms cannot be overrun.
	for _, ms := range []int{1000, 300, 100, 60, 50, 20, 6000} {
		baseEmbed, _, _ := hookBudgets(ms, false)
		embed, probe, warm := hookBudgets(ms, true)
		if probe <= 0 || probe > runningProbeCap || warm <= 0 || warm > warmUpCap || embed <= 0 {
			t.Fatalf("%dms: embed=%v probe=%v warm=%v", ms, embed, probe, warm)
		}
		if embed+probe > baseEmbed {
			t.Fatalf("%dms: probe %v + embed %v overruns the %v the embed call alone had", ms, probe, embed, baseEmbed)
		}
		if probe+warm > baseEmbed/2 {
			t.Fatalf("%dms: cold path probe %v + warm-up %v takes more than half of the %v budget", ms, probe, warm, baseEmbed)
		}
	}
	if embed, probe, warm := hookBudgets(1000, true); embed != 850*time.Millisecond || probe != 100*time.Millisecond || warm != 150*time.Millisecond {
		t.Fatalf("production deadline: embed=%v probe=%v warm=%v, want 850ms, 100ms and 150ms", embed, probe, warm)
	}
	if _, _, warm := hookBudgets(100, true); warm >= warmUpCap {
		t.Fatalf("a 100 ms deadline must clamp the warm-up below its %v ceiling, got %v", warmUpCap, warm)
	}
}

func TestColdSkipRetriever_NamesTheWarmUpBudgetItActuallyHad(t *testing.T) {
	g, _, _ := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "r"}, "", nil)
	g.warmTO = 40 * time.Millisecond
	_, _, err := g.RetrieveScored("a prompt long enough", 3)
	if err == nil || !strings.Contains(err.Error(), "within 40ms") {
		t.Fatalf("the row must name the clamped budget, got %v", err)
	}
}

// ---- a run of cold skips that outlasts any real load ----------------------------------------

func TestColdSkipRetriever_StuckAfterARunLongEnough(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		n     int
		age   int64 // seconds since the oldest row in the run
		stuck bool
	}{
		{3, 120, true}, {3, 119, false}, {2, 3600, false}, {0, 0, false}, {1, 9999, false}, {10, 3600, true}, {3, 5, false},
	} {
		g, _, _ := newStubGate(retrievers.EmbedProbe{State: retrievers.EmbedCold, Target: "http://router", Warmable: true, Reason: "r"}, "starting", nil)
		g.now = func() time.Time { return now }
		g.history = func() (int, int64) { return c.n, now.Unix() - c.age }
		_, _, err := g.RetrieveScored("a prompt long enough", 3)
		if !errors.Is(err, errColdEmbedder) {
			t.Fatalf("n=%d age=%d: every cold skip is a cold skip, got %v", c.n, c.age, err)
		}
		if got := errors.Is(err, errColdStuck); got != c.stuck {
			t.Fatalf("n=%d age=%ds: stuck=%v, want %v (%v)", c.n, c.age, got, c.stuck, err)
		}
		if c.stuck && !strings.Contains(err.Error(), fmt.Sprintf("%d consecutive", c.n+1)) {
			t.Fatalf("the row must count this skip too: %v", err)
		}
	}
}

// The usage log is read only on the cold path: a warm embedder pays nothing for the check.
func TestColdSkipRetriever_WarmPathNeverReadsTheLog(t *testing.T) {
	for _, st := range []retrievers.EmbedState{retrievers.EmbedWarm, retrievers.EmbedUnknown} {
		g, _, _ := newStubGate(retrievers.EmbedProbe{State: st}, "", nil)
		g.history = func() (int, int64) { t.Fatal("the usage log was read on a path that is not cold"); return 0, 0 }
		if _, _, err := g.RetrieveScored("a prompt long enough", 3); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecide_ColdStuckModes(t *testing.T) {
	stuck := failedRetriever{err: fmt.Errorf("%w: x [stuck: %w]", errColdEmbedder, errColdStuck)}
	if _, _, mode, _, _ := decide("qa test web application", 3, 0.55, 6, stuck, "embed", stubLex{{ID: "qa", Score: 30}}); mode != "cold-stuck-bm25" {
		t.Fatalf("stuck, BM25 surfaced: mode=%q", mode)
	}
	if _, _, mode, _, _ := decide("one two three four five six", 3, 0.55, 6, stuck, "embed", stubLex{{ID: "qa", Score: 1}}); mode != "cold-stuck-empty" {
		t.Fatalf("stuck, BM25 declined: mode=%q", mode)
	}
}

func writeLogLines(t *testing.T, lines ...string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "usage.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func row(ts int64, mode string) string { return fmt.Sprintf(`{"ts_unix":%d,"mode":%q}`, ts, mode) }

func TestColdRun(t *testing.T) {
	for _, c := range []struct {
		name  string
		lines []string
		n     int
		old   int64
	}{
		{"trailing run", []string{row(1, "embed"), row(10, "cold-skip-empty"), row(20, "cold-skip-bm25"), row(30, "cold-skip-empty")}, 3, 10},
		{"an embedder outcome ends the run", []string{row(10, "cold-skip-empty"), row(20, "embed"), row(30, "cold-skip-empty")}, 1, 30},
		{"every embedder outcome ends it", []string{row(10, "cold-skip-empty"), row(20, "gated-empty"), row(30, "cold-skip-empty")}, 1, 30},
		{"a failed embed ends it", []string{row(10, "cold-skip-empty"), row(20, "bm25-fallback"), row(30, "cold-skip-empty")}, 1, 30},
		{"rows that never reached the embedder are skipped", []string{row(10, "cold-skip-empty"), row(15, "machine-turn"), row(20, "too-short"), row(25, "tpl-mismatch"), row(30, "error"), row(40, "cold-skip-bm25")}, 2, 10},
		{"cold-stuck rows belong to the run", []string{row(10, "cold-skip-empty"), row(20, "cold-stuck-empty")}, 2, 10},
		{"junk lines are skipped", []string{row(10, "cold-skip-empty"), "not json", "", row(20, "cold-skip-empty")}, 2, 10},
		{"no cold rows", []string{row(10, "embed")}, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, old := coldRun(writeLogLines(t, c.lines...))
			if n != c.n || old != c.old {
				t.Fatalf("got n=%d oldest=%d, want n=%d oldest=%d", n, old, c.n, c.old)
			}
		})
	}
	if n, old := coldRun(filepath.Join(t.TempDir(), "absent.jsonl")); n != 0 || old != 0 {
		t.Fatalf("a missing log is no history, got %d, %d", n, old)
	}
}

// Only the tail of the log is read: a log far longer than the window costs one bounded read.
func TestColdRunReadsOnlyTheTail(t *testing.T) {
	var lines []string
	for i := 0; i < 4000; i++ { // far more than the tail window
		lines = append(lines, row(int64(i), "cold-skip-empty"))
	}
	lines = append(lines, row(5000, "embed"), row(5001, "cold-skip-bm25"), row(5002, "cold-skip-bm25"))
	p := writeLogLines(t, lines...)
	if fi, _ := os.Stat(p); fi.Size() <= stuckTailBytes {
		t.Fatalf("fixture must exceed the %d-byte tail window, is %d", stuckTailBytes, fi.Size())
	}
	n, old := coldRun(p)
	if n != 2 || old != 5001 {
		t.Fatalf("got n=%d oldest=%d, want the 2 rows after the embed", n, old)
	}
	// A log that is ALL cold rows and longer than the window counts what the window holds.
	n, _ = coldRun(writeLogLines(t, lines[:4000]...))
	if n < 100 || n >= 4000 {
		t.Fatalf("a window-sized tail of an all-cold log: n=%d", n)
	}
}
