package retrievers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// embedOK is a stand-in embedder that answers the OpenAI /v1/embeddings shape.
func embedOK(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0,0]}]}`))
	}))
}

func embedStatus(t *testing.T, code int, hits *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		http.Error(w, "nope", code)
	}))
}

// The core PC-agnostic behavior: the first candidate is dead (this is Qube's
// :18793 — nothing listening), so the second one must serve the request. No
// config change, no per-host settings.json edit.
func TestEmbed_FailsOverToLiveEndpoint(t *testing.T) {
	var liveHits int32
	live := embedOK(t, &liveHits)
	defer live.Close()

	// A closed listener == connection refused, exactly like a port with no server.
	var deadHits int32
	dead := embedStatus(t, 500, &deadHits)
	deadURL := dead.URL
	dead.Close()

	vs, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: deadURL}, {URL: live.URL}}, []string{"hello"})
	if err != nil {
		t.Fatalf("expected failover to the live endpoint, got error: %v", err)
	}
	if len(vs) != 1 || len(vs[0]) != 3 {
		t.Fatalf("bad vectors from the live endpoint: %v", vs)
	}
	if atomic.LoadInt32(&liveHits) != 1 {
		t.Fatalf("live endpoint should have been hit exactly once, got %d", liveHits)
	}
}

// A 5xx from something that IS listening (wrong service on the port, or a
// gateway) is also a reason to move on to the next candidate.
func TestEmbed_FailsOverOnServerError(t *testing.T) {
	var badHits, liveHits int32
	bad := embedStatus(t, 503, &badHits)
	defer bad.Close()
	live := embedOK(t, &liveHits)
	defer live.Close()

	if _, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: bad.URL}, {URL: live.URL}}, []string{"hi"}); err != nil {
		t.Fatalf("503 should fail over, got: %v", err)
	}
	if atomic.LoadInt32(&badHits) != 1 || atomic.LoadInt32(&liveHits) != 1 {
		t.Fatalf("expected both to be hit once: bad=%d live=%d", badHits, liveHits)
	}
}

// But a real embedder rejecting a real request (e.g. llama-swap's "input too
// large" 400) must SURFACE, not be masked by a pointless failover that reports
// the next endpoint's connection error instead. Diagnostics matter.
func TestEmbed_DoesNotFailOverOnClientError(t *testing.T) {
	var badHits, nextHits int32
	bad := embedStatus(t, 400, &badHits)
	defer bad.Close()
	next := embedOK(t, &nextHits)
	defer next.Close()

	_, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: bad.URL}, {URL: next.URL}}, []string{"hi"})
	if err == nil {
		t.Fatal("a 400 from a live embedder must surface as an error")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("error must name the real status, got: %v", err)
	}
	if atomic.LoadInt32(&nextHits) != 0 {
		t.Fatalf("must NOT fail over past a client error; next endpoint was hit %d times", nextHits)
	}
}

// When every candidate is down, the caller gets a real error (and mr-hook then
// degrades to BM25) — it must never hang or silently return empty vectors.
func TestEmbed_AllEndpointsDownReportsLastError(t *testing.T) {
	var h1, h2 int32
	s1 := embedStatus(t, 500, &h1)
	u1 := s1.URL
	s1.Close()
	s2 := embedStatus(t, 500, &h2)
	u2 := s2.URL
	s2.Close()

	_, err := embed(newHTTPClient(2*time.Second), []Endpoint{{URL: u1}, {URL: u2}}, []string{"hi"})
	if err == nil {
		t.Fatal("expected an error when every endpoint is down")
	}
	if !strings.Contains(err.Error(), u2) {
		t.Fatalf("error should name the last endpoint tried (%s), got: %v", u2, err)
	}
}

func TestEmbed_NoEndpointsIsAnError(t *testing.T) {
	if _, err := embed(newHTTPClient(time.Second), nil, []string{"hi"}); err == nil {
		t.Fatal("embedding with no configured endpoint must error, not panic or hang")
	}
}
