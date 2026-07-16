package retrievers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// slowServer accepts the connection and then stalls past the caller's budget —
// llama-swap mid-model-swap, or a busy queue. This is the case that matters:
// the dial SUCCEEDS, so ConnectTimeout does not save us.
func slowServer(t *testing.T, delay time.Duration, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		time.Sleep(delay)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
}

// REGRESSION (adversarial review, MAJOR): http.Client.Timeout is applied PER
// REQUEST. Walking N candidates with one client could therefore burn N × the
// budget. mr-hook sets its embed budget to (hard deadline − 50ms) precisely so
// that when the embedder times out there is still room to run the BM25 fallback
// inside the deadline. If the walk overruns, the hook's outer select fires and
// the user gets NOTHING surfaced — strictly worse than the bug being fixed.
//
// The whole walk must fit in the client's Timeout, however many candidates.
func TestEmbed_WalkSharesOneDeadline(t *testing.T) {
	var h1, h2 int32
	s1 := slowServer(t, 2*time.Second, &h1)
	defer s1.Close()
	s2 := slowServer(t, 2*time.Second, &h2)
	defer s2.Close()

	budget := 300 * time.Millisecond
	eps := []Endpoint{{URL: s1.URL}, {URL: s2.URL}}

	start := time.Now()
	_, err := embed(newHTTPClient(budget), eps, []string{"hi"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the walk to fail against two stalled endpoints")
	}
	// Allow generous slack for scheduling, but it must be ~one budget, not two.
	if elapsed > budget*2 {
		t.Fatalf("the failover walk took %v — it is spending the budget PER CANDIDATE instead of sharing one deadline (budget %v)", elapsed, budget)
	}
}

// A candidate must not even be dialed once the budget is gone.
func TestEmbed_DoesNotDialAfterBudgetSpent(t *testing.T) {
	var h1, h2 int32
	s1 := slowServer(t, 2*time.Second, &h1)
	defer s1.Close()
	s2 := slowServer(t, 2*time.Second, &h2)
	defer s2.Close()

	eps := []Endpoint{{URL: s1.URL}, {URL: s2.URL}}
	if _, err := embed(newHTTPClient(200*time.Millisecond), eps, []string{"hi"}); err == nil {
		t.Fatal("expected failure")
	}
	if atomic.LoadInt32(&h2) != 0 {
		t.Fatalf("second candidate was dialed with no budget left (hits=%d)", h2)
	}
}

// REGRESSION (adversarial review, MAJOR): only a live embedder REJECTING this
// request (400/413/422) is fatal. A 401/403/405/429 means "this port is not
// usefully serving us embeddings" — a foreign service holding the port, or a
// full queue — and must fall through to the next candidate rather than halting
// the walk and silently degrading to BM25.
func TestEmbed_FailsOverOnAuthAndRateLimitStatuses(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusTooManyRequests} {
		var badHits, liveHits int32
		bad := embedStatus(t, code, &badHits)
		live := embedOK(t, &liveHits)

		_, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: bad.URL}, {URL: live.URL}}, []string{"hi"})
		if err != nil {
			t.Fatalf("status %d must fail over to the healthy endpoint, got: %v", code, err)
		}
		if atomic.LoadInt32(&liveHits) != 1 {
			t.Fatalf("status %d: healthy endpoint never reached", code)
		}
		bad.Close()
		live.Close()
	}
}

// ...but a genuine request rejection still surfaces, so "input too large" is not
// masked by the next candidate's connection error.
func TestEmbed_RequestRejectionIsFatal(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		var badHits, nextHits int32
		bad := embedStatus(t, code, &badHits)
		next := embedOK(t, &nextHits)

		_, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: bad.URL}, {URL: next.URL}}, []string{"hi"})
		if err == nil {
			t.Fatalf("status %d from a live embedder must surface", code)
		}
		if atomic.LoadInt32(&nextHits) != 0 {
			t.Fatalf("status %d must NOT fail over (it is a real answer); next was hit %d times", code, nextHits)
		}
		bad.Close()
		next.Close()
	}
}

// REGRESSION (adversarial review, MAJOR): an UNVERIFIED candidate is one the
// operator never configured — it came from the built-in convenience chain. The
// user's prompt text must not be POSTed to it until we have confirmed an embedder
// is what is actually listening there.
func TestEmbed_UnverifiedEndpointIsProbedBeforeReceivingPrompt(t *testing.T) {
	var gotPrompt int32
	// A foreign service: answers 200 on everything, advertises no embedding model.
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"some-dashboard"}]}`))
			return
		}
		atomic.AddInt32(&gotPrompt, 1) // must never happen
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
	defer foreign.Close()

	var liveHits int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"embeddinggemma"}]}`))
			return
		}
		atomic.AddInt32(&liveHits, 1)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
	defer live.Close()

	eps := []Endpoint{{URL: foreign.URL, Unverified: true}, {URL: live.URL, Unverified: true}}
	if _, err := embed(newHTTPClient(2*time.Second), eps, []string{"my secret prompt"}); err != nil {
		t.Fatalf("should have used the real embedder: %v", err)
	}
	if n := atomic.LoadInt32(&gotPrompt); n != 0 {
		t.Fatalf("PROMPT LEAKED: posted prompt text to an unconfigured non-embedder %d time(s)", n)
	}
	if atomic.LoadInt32(&liveHits) != 1 {
		t.Fatal("the real embedder should have served the request")
	}
}

// A candidate the operator explicitly configured is taken at their word — no
// probe, no extra round trip on the happy path.
func TestEmbed_ConfiguredEndpointIsNotProbed(t *testing.T) {
	var models, embeds int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			atomic.AddInt32(&models, 1)
			w.Write([]byte(`{"data":[{"id":"embeddinggemma"}]}`))
			return
		}
		atomic.AddInt32(&embeds, 1)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
	defer s.Close()

	if _, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: s.URL}}, []string{"hi"}); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&models) != 0 {
		t.Fatalf("a configured endpoint must not be probed (%d probes)", models)
	}
	if atomic.LoadInt32(&embeds) != 1 {
		t.Fatal("expected exactly one embed call")
	}
}

// REGRESSION (adversarial review, MAJOR): a failover endpoint may serve a
// DIFFERENT model than the one that built the index. Scoring a 1024-dim query
// against 768-dim index vectors used to panic inside mr-hook's worker goroutine
// — unrecoverable, bypassing `defer os.Exit(0)` and killing the fail-open
// contract. It must return a clean error so the caller degrades to BM25.
func TestRankByCosine_DimensionMismatchErrorsNotPanics(t *testing.T) {
	wrongDim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0,0,0]}]}`)) // 5-dim
	}))
	defer wrongDim.Close()

	// index vectors are 3-dim
	e := &Embed{
		ids:  []string{"a", "b"},
		vecs: [][]float64{{1, 0, 0}, {0, 1, 0}},
		eps:  []Endpoint{{URL: wrongDim.URL}},
		hc:   newHTTPClient(2 * time.Second),
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("PANICKED on a dimension mismatch (this kills the hook's exit-0 contract): %v", r)
		}
	}()

	_, _, err := e.RetrieveScored("hello", 3)
	if err == nil {
		t.Fatal("a dimension mismatch must be a clean error, not silently-wrong scores")
	}
	if !strings.Contains(err.Error(), "dim") {
		t.Fatalf("error should explain the dimension mismatch, got: %v", err)
	}
}

// cosine itself must be length-safe regardless of what callers do.
func TestCosine_IsLengthSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("cosine panicked on mismatched lengths: %v", r)
		}
	}()
	_ = cosine([]float64{1, 2, 3, 4}, []float64{1, 2})
	_ = cosine([]float64{1, 2}, []float64{1, 2, 3, 4})
}
