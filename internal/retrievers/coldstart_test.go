package retrievers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// routerServer fakes the two router routes the probe reads. A nil handler means no route.
func routerServer(t *testing.T, running, models http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if running != nil {
		mux.HandleFunc("/running", running)
	}
	if models != nil {
		mux.HandleFunc("/v1/models", models)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func body(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, s) }
}

func status(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { http.Error(w, "x", code) }
}

func hang(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

const (
	modelsPlain = `{"data":[{"id":"embeddinggemma"},{"id":"other"}]}`
	modelsAlias = `{"data":[{"id":"egemma-300m","meta":{"llamaswap":{"aliases":["embeddinggemma"]}}}]}`
)

func TestProbeEmbedderVerdicts(t *testing.T) {
	cases := []struct {
		name     string
		running  http.HandlerFunc
		models   http.HandlerFunc
		want     EmbedState
		warmable bool
	}{
		{"ready", body(`{"running":[{"model":"embeddinggemma","state":"ready"}]}`), nil, EmbedWarm, false},
		{"ready beside other models", body(`{"running":[{"model":"other","state":"ready"},{"model":"embeddinggemma","state":"ready"}]}`), nil, EmbedWarm, false},
		{"ready, state in another case", body(`{"running":[{"model":"embeddinggemma","state":"Ready"}]}`), nil, EmbedWarm, false},
		{"load in flight", body(`{"running":[{"model":"embeddinggemma","state":"starting"}]}`), nil, EmbedCold, false},
		{"being stopped", body(`{"running":[{"model":"embeddinggemma","state":"stopping"}]}`), nil, EmbedCold, true},
		{"not running, listed", body(`{"running":[]}`), body(modelsPlain), EmbedCold, true},
		{"only another model running", body(`{"running":[{"model":"other","state":"ready"}]}`), body(modelsPlain), EmbedCold, true},
		{"not running, no model list at all", body(`{"running":[]}`), nil, EmbedCold, true},
		{"not running, model list errors", body(`{"running":[]}`), status(500), EmbedCold, true},
		{"not running, model list is not a list", body(`{"running":[]}`), body(`<html/>`), EmbedCold, true},
		{"alias of a loaded model", body(`{"running":[{"model":"egemma-300m","state":"ready"}]}`), body(modelsAlias), EmbedWarm, false},
		{"alias of an unloaded model", body(`{"running":[]}`), body(modelsAlias), EmbedCold, true},
		{"alias of a loading model", body(`{"running":[{"model":"egemma-300m","state":"starting"}]}`), body(modelsAlias), EmbedCold, false},
		{"model the router does not list", body(`{"running":[]}`), body(`{"data":[{"id":"other"}]}`), EmbedUnknown, false},
		{"empty model list", body(`{"running":[]}`), body(`{"data":[]}`), EmbedUnknown, false},
		{"no /running route", nil, nil, EmbedUnknown, false},
		{"/running is 405", status(405), nil, EmbedUnknown, false},
		{"/running is 401", status(401), nil, EmbedUnknown, false},
		{"/running is 200 without the list", body(`{"data":[]}`), nil, EmbedUnknown, false},
		{"/running is 200 html", body(`<html>`), nil, EmbedUnknown, false},
		{"/running is 200 empty body", body(``), nil, EmbedUnknown, false},
		{"/running errors", status(500), nil, EmbedCold, true},
		{"/running unavailable", status(503), nil, EmbedCold, true},
		{"/running hangs", hang, nil, EmbedCold, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := routerServer(t, c.running, c.models)
			p := ProbeEmbedder(srv.URL, "embeddinggemma", 150*time.Millisecond)
			if p.State != c.want || p.Warmable != c.warmable {
				t.Fatalf("got state=%d warmable=%v reason=%q; want state=%d warmable=%v", p.State, p.Warmable, p.Reason, c.want, c.warmable)
			}
			if p.State != EmbedWarm && p.Reason == "" {
				t.Fatal("a non-warm verdict must say why: the reason lands in the usage row")
			}
			if p.State != EmbedUnknown && p.Target != srv.URL {
				t.Fatalf("target %q, want %q", p.Target, srv.URL)
			}
		})
	}
}

// A cold verdict names the id /running will report once the model loads, for the warm-up to
// watch: the alias's real id when the model was asked for by alias, else the requested name.
func TestProbeEmbedderNamesWhatToWatchFor(t *testing.T) {
	alias := routerServer(t, body(`{"running":[]}`), body(modelsAlias))
	if p := ProbeEmbedder(alias.URL, "embeddinggemma", 150*time.Millisecond); p.State != EmbedCold || p.ListedAs != "egemma-300m" {
		t.Fatalf("alias: got %+v, want cold watching egemma-300m", p)
	}
	plain := routerServer(t, body(`{"running":[]}`), body(modelsPlain))
	if p := ProbeEmbedder(plain.URL, "embeddinggemma", 150*time.Millisecond); p.State != EmbedCold || p.ListedAs != "embeddinggemma" {
		t.Fatalf("plain: got %+v, want cold watching embeddinggemma", p)
	}
}

func TestProbeEmbedderStaysInsideItsBudget(t *testing.T) {
	srv := routerServer(t, hang, nil)
	t0 := time.Now()
	p := ProbeEmbedder(srv.URL, "embeddinggemma", 100*time.Millisecond)
	if d := time.Since(t0); d > 400*time.Millisecond {
		t.Fatalf("a router that never answers held the probe for %v (budget 100ms)", d)
	}
	if p.State != EmbedCold {
		t.Fatalf("an unreadable router reads as cold, got state %d", p.State)
	}
}

// Nothing listening is neither cold nor warm: the embed path keeps its own, accurate
// diagnosis ("connection refused") and there is nothing to warm.
func TestProbeEmbedderUnreachableIsUnknown(t *testing.T) {
	srv := routerServer(t, body(runningReady), nil)
	url := srv.URL
	srv.Close()
	p := ProbeEmbedder(url, "embeddinggemma", 150*time.Millisecond)
	if p.State != EmbedUnknown || p.Target != "" || p.Warmable {
		t.Fatalf("got %+v, want unknown with no target", p)
	}
}

const runningReady = `{"running":[{"model":"embeddinggemma","state":"ready"}]}`

// The chain is walked the way embed() walks it: a refused candidate is skipped, and the first
// one that ANSWERS decides, even when a later one is warm (embed would have blocked on it).
func TestProbeEmbedderWalksTheChainLikeEmbed(t *testing.T) {
	dead := routerServer(t, nil, nil)
	deadURL := dead.URL
	dead.Close()
	cold := routerServer(t, body(`{"running":[]}`), body(modelsPlain))
	warm := routerServer(t, body(runningReady), nil)

	if p := ProbeEmbedder(deadURL+","+warm.URL, "embeddinggemma", 300*time.Millisecond); p.State != EmbedWarm || p.Target != warm.URL {
		t.Fatalf("refused first, warm second: got %+v", p)
	}
	if p := ProbeEmbedder(cold.URL+","+warm.URL, "embeddinggemma", 300*time.Millisecond); p.State != EmbedCold || p.Target != cold.URL {
		t.Fatalf("cold first, warm second: the first answer decides, got %+v", p)
	}
}

// An endpoint nobody configured (the built-in chain's guess) is never sent traffic on the
// strength of a failed read: it can still be reported cold, but not warmed.
func TestUnreadableUnverifiedEndpointIsNotWarmed(t *testing.T) {
	srv := routerServer(t, status(500), nil)
	hc := &http.Client{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p, answered := probeRouter(ctx, hc, Endpoint{URL: srv.URL, Unverified: true}, "embeddinggemma")
	if !answered || p.State != EmbedCold || p.Warmable {
		t.Fatalf("unverified + unreadable: got %+v answered=%v, want cold and not warmable", p, answered)
	}
	// ...but a parsed listing proves it is a router, so its definitive "not loaded" is warmable.
	srv2 := routerServer(t, body(`{"running":[]}`), body(modelsPlain))
	p, _ = probeRouter(ctx, hc, Endpoint{URL: srv2.URL, Unverified: true}, "embeddinggemma")
	if p.State != EmbedCold || !p.Warmable {
		t.Fatalf("unverified but proven router: got %+v, want cold and warmable", p)
	}
}

// ---- WarmUpEmbedder -------------------------------------------------------------------

// warmRouter fakes the two routes a warm-up touches. A POST to /v1/embeddings begins a "load":
// from then on /running lists listedAs in state, and the POST stays open the way a request
// does while a model loads. state "" means /running keeps reporting nothing running.
type warmRouter struct {
	*httptest.Server
	posts  atomic.Int32
	loaded atomic.Bool
	bodies chan string
}

func newWarmRouter(t *testing.T, listedAs, state string, withRunning bool) *warmRouter {
	t.Helper()
	wr := &warmRouter{bodies: make(chan string, 4)}
	mux := http.NewServeMux()
	if withRunning {
		mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
			if wr.loaded.Load() && state != "" {
				io.WriteString(w, `{"running":[{"model":"`+listedAs+`","state":"`+state+`"}]}`)
				return
			}
			io.WriteString(w, `{"running":[]}`)
		})
	}
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		wr.posts.Add(1)
		wr.bodies <- string(b)
		wr.loaded.Store(true)
		select {
		case <-time.After(3 * time.Second):
		case <-r.Context().Done():
		}
	})
	wr.Server = httptest.NewServer(mux)
	t.Cleanup(wr.Close)
	return wr
}

func TestWarmUpEmbedderLeavesOnceTheRouterAdmitsToLoading(t *testing.T) {
	wr := newWarmRouter(t, "embeddinggemma", "starting", true)
	t0 := time.Now()
	st, err := WarmUpEmbedder(wr.URL, "embeddinggemma", "embeddinggemma", time.Second)
	if err != nil || st != "starting" {
		t.Fatalf("got state=%q err=%v, want starting", st, err)
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("warm-up took %v: it waited on the load itself", d)
	}
	var req struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.Unmarshal([]byte(<-wr.bodies), &req); err != nil || req.Model != "embeddinggemma" || len(req.Input) != 1 || req.Input[0] != "warm" {
		t.Fatalf("warm-up body %+v (%v): want the model and the fixed word", req, err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := wr.posts.Load(); n != 1 {
		t.Fatalf("want exactly 1 embeddings request, got %d", n)
	}
}

// The router reports the REAL id, so the warm-up must watch for that, not the alias it asked by.
func TestWarmUpEmbedderWatchesTheRealIDOfAnAlias(t *testing.T) {
	wr := newWarmRouter(t, "egemma-300m", "starting", true)
	if st, err := WarmUpEmbedder(wr.URL, "embeddinggemma", "egemma-300m", time.Second); err != nil || st != "starting" {
		t.Fatalf("watching the real id: got %q, %v", st, err)
	}
	wr2 := newWarmRouter(t, "egemma-300m", "starting", true)
	t0 := time.Now()
	if st, _ := WarmUpEmbedder(wr2.URL, "embeddinggemma", "embeddinggemma", 150*time.Millisecond); st != "" {
		t.Fatalf("watching the alias must not find the real id's entry, got %q", st)
	}
	if d := time.Since(t0); d < 100*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("an unconfirmed warm-up should end at its 150 ms deadline, took %v", d)
	}
}

func TestWarmUpEmbedderStoppingIsNotALoad(t *testing.T) {
	wr := newWarmRouter(t, "embeddinggemma", "stopping", true)
	if st, _ := WarmUpEmbedder(wr.URL, "embeddinggemma", "embeddinggemma", 120*time.Millisecond); st != "" {
		t.Fatalf("a model being stopped is not a load beginning, got %q", st)
	}
}

func TestWarmUpEmbedderUnconfirmedAtTheDeadline(t *testing.T) {
	wr := newWarmRouter(t, "embeddinggemma", "", true) // /running never changes
	t0 := time.Now()
	st, err := WarmUpEmbedder(wr.URL, "embeddinggemma", "embeddinggemma", 150*time.Millisecond)
	if st != "" || err != nil {
		t.Fatalf("got %q, %v, want unconfirmed without an error", st, err)
	}
	if d := time.Since(t0); d < 100*time.Millisecond || d > 600*time.Millisecond {
		t.Fatalf("took %v, want about the 150 ms deadline", d)
	}
	if n := wr.posts.Load(); n != 1 {
		t.Fatalf("want 1 embeddings request, got %d", n)
	}
}

// With no readable /running there is nothing to wait for: leave at once, the request sent.
func TestWarmUpEmbedderLeavesAtOnceWhenTheLoadCannotBeObserved(t *testing.T) {
	wr := newWarmRouter(t, "embeddinggemma", "", false) // no /running route
	t0 := time.Now()
	st, err := WarmUpEmbedder(wr.URL, "embeddinggemma", "embeddinggemma", time.Second)
	if st != "" || err != nil {
		t.Fatalf("got %q, %v", st, err)
	}
	if d := time.Since(t0); d > 300*time.Millisecond {
		t.Fatalf("took %v: nothing observable, so nothing to wait for", d)
	}
}

func TestWarmUpEmbedderReportsARefusedConnection(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	t0 := time.Now()
	if _, err := WarmUpEmbedder(url, "embeddinggemma", "embeddinggemma", time.Second); err == nil {
		t.Fatal("a refused connection must be reported")
	}
	if d := time.Since(t0); d > 500*time.Millisecond {
		t.Fatalf("refusal took %v", d)
	}
}

func TestWarmUpEmbedderEndsEarlyWhenTheRouterAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/embeddings" {
			io.WriteString(w, `{"data":[{"index":0,"embedding":[1,0]}]}`)
			return
		}
		io.WriteString(w, `{"running":[]}`)
	}))
	defer srv.Close()
	t0 := time.Now()
	st, err := WarmUpEmbedder(srv.URL, "embeddinggemma", "embeddinggemma", 5*time.Second)
	if err != nil || st != "ready" {
		t.Fatalf("got %q, %v, want ready", st, err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("an already-warm router answered at once, yet the warm-up held %v", d)
	}
}
