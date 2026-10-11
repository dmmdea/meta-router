package main

// F42 at the binary level. The hook is registered with a 1000 ms deadline (these runs use
// 5000 ms, with bounds relative to the fake embedding's 3 s; the 1000 ms arithmetic is pinned
// by TestHookBudgets), and the embedder
// behind the model router is unloaded after five idle minutes and needs seconds to load, so
// the first prompt after every idle spell used to wait out the whole embed budget, get
// cancelled, and fall back to BM25. The hook now asks the router whether the embed model is
// loaded BEFORE spending that budget:
//
//   - not loaded (or the router cannot be read) -> BM25 at once, plus ONE warm-up request held
//     open only until the router admits it to a load (a few milliseconds), then abandoned;
//   - loaded -> embed exactly as before.
//
// Every test drives the built binary against a fake router (an httptest server answering
// /running, /v1/models and /v1/embeddings), because the claim is about the process: how long
// it takes to exit, and what it put on the wire.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/usagelog"
)

const (
	// coldPrompt is lexical enough that the BM25 fallback gate passes on the fixture index:
	// every token is rare in it and sits in gstack-qa's description.
	coldPrompt = "qa test web application"
	// coldPromptNoLexicalMatch shares no token with any fixture skill, so BM25 surfaces nothing.
	coldPromptNoLexicalMatch = "zebra quartz nimbus fjord velvet orchid"

	runningWarm = `{"running":[{"model":"embeddinggemma","state":"ready"}]}`
	runningCold = `{"running":[]}`

	// hookDeadlineMs is the hard deadline the e2e runs use, the same 5000 ms as the older
	// e2e tests: wall-clock bounds in these tests are RELATIVE to fakeEmbedTime, so a loaded
	// machine can slow a run down a great deal before an assertion trips, while a hook that
	// really waited on the fake embedding (3 s) still cannot pass.
	hookDeadlineMs = "5000"
	// fakeEmbedTime is how long the fake embedding takes when a test makes it slow.
	fakeEmbedTime = 3 * time.Second
	// routerAdmit is how long the fake router takes to admit a request to a load. A request
	// whose client has gone before then is dropped and nothing loads, as on the live router.
	routerAdmit = 25 * time.Millisecond
)

// fakeRouter stands in for the model router. running == nil means the router has no /running
// route at all (a plain embedding server).
type fakeRouter struct {
	*httptest.Server
	runningHits atomic.Int32
	embedHits   atomic.Int32
	loading     atomic.Bool // set once the router has ADMITTED an embeddings call to a load
	mu          sync.Mutex
	embedBodies []string
}

type routerOpts struct {
	running http.HandlerFunc
	models  string        // body of /v1/models; "" = 404
	embedIn time.Duration // how long a /v1/embeddings call takes (cancelled by the client going away)
	// listsLoad makes /running report the model as "starting" once an embeddings call has been
	// admitted to a load, the way a router does while it loads. Without it /running never
	// changes. Admission takes routerAdmit and needs the request to still be open: a client
	// that went away first starts nothing.
	listsLoad bool
	// embedStatus, when set, makes /v1/embeddings answer that HTTP status at once.
	embedStatus int
}

// buildColdHook builds the hook and runs it once, so the first measured run does not pay the
// operating system's first-execution scan of a fresh binary (about a second on Windows).
func buildColdHook(t *testing.T) string {
	t.Helper()
	bin := buildMRHook(t)
	if out, err := exec.Command(bin, "-version").CombinedOutput(); err != nil {
		t.Fatalf("priming run: %v: %s", err, out)
	}
	return bin
}

func newFakeRouter(t *testing.T, o routerOpts) *fakeRouter {
	t.Helper()
	fr := &fakeRouter{}
	mux := http.NewServeMux()
	if o.running != nil {
		mux.HandleFunc("/running", func(w http.ResponseWriter, r *http.Request) {
			fr.runningHits.Add(1)
			if o.listsLoad && fr.loading.Load() {
				w.Write([]byte(`{"running":[{"model":"embeddinggemma","state":"starting"}]}`))
				return
			}
			o.running(w, r)
		})
	}
	if o.models != "" {
		mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(o.models)) })
	}
	mux.HandleFunc("/v1/embeddings", func(w http.ResponseWriter, r *http.Request) {
		var buf bytes.Buffer
		buf.ReadFrom(r.Body)
		fr.mu.Lock()
		fr.embedBodies = append(fr.embedBodies, buf.String())
		fr.mu.Unlock()
		fr.embedHits.Add(1) // counted on ARRIVAL, not completion: a cancelled call still counts
		if o.embedStatus != 0 {
			http.Error(w, "refused", o.embedStatus)
			return
		}
		if o.listsLoad {
			select {
			case <-time.After(routerAdmit):
				fr.loading.Store(true)
			case <-r.Context().Done():
				return // gone before the router admitted it: nothing loads
			}
		}
		if o.embedIn > 0 {
			select {
			case <-time.After(o.embedIn):
			case <-r.Context().Done():
				return
			}
		}
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	})
	fr.Server = httptest.NewServer(mux)
	t.Cleanup(fr.Close)
	return fr
}

// writeColdIndex writes an eight-skill untemplated index. One skill is not enough corpus for
// BM25 to clear the fallback's precision gate (document frequency 1 of 1 gives every term a
// near-zero idf), so the cold path would always read "nothing surfaced". gstack-qa carries the
// vector the fake embedder returns for any query; the rest are orthogonal to it.
func writeColdIndex(t *testing.T, dir string) string {
	t.Helper()
	descs := [][2]string{
		{"gstack-qa", "QA test a web application"},
		{"superpowers:brainstorming", "explore intent and requirements before creative work"},
		{"lavish", "render plans and diffs as annotated documents"},
		{"clean-ship", "isolate repository changes in a worktree and merge through review"},
		{"ffmpeg", "transcode trim concatenate audio video streams"},
		{"windows-disk-cleanup", "reclaim system drive space"},
		{"roast", "pressure test a business idea with a council"},
		{"copywriting", "write persuasive marketing headlines"},
	}
	type entry struct {
		Skill map[string]string `json:"skill"`
		Vec   []float64         `json:"vec"`
		Hash  string            `json:"hash"`
	}
	var entries []entry
	for i, d := range descs {
		vec := []float64{0, 1}
		if i == 0 {
			vec = []float64{1, 0}
		}
		entries = append(entries, entry{
			Skill: map[string]string{"id": d[0], "name": d[0], "source": "skills", "description": d[1]},
			Vec:   vec, Hash: fmt.Sprintf("h%d", i),
		})
	}
	raw, err := json.Marshal(map[string]any{"model": "embeddinggemma", "dim": 2, "built_unix": 1, "entries": entries})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "index.json")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonRunning(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(body)) }
}

// hangUntilGone holds the request open until the client gives up.
func hangUntilGone(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

type hookRun struct {
	out  string
	rec  usagelog.Record
	wall time.Duration // process start to exit, stdout/stderr pipes drained (what the caller waits on)
}

// runColdHook runs the built hook against fr with an empty usage log.
func runColdHook(t *testing.T, bin string, fr *fakeRouter, prompt string, extra ...string) hookRun {
	t.Helper()
	return runColdHookSeeded(t, bin, fr, prompt, nil, extra...)
}

// runColdHookSeeded is runColdHook with the usage log pre-filled by seed.
func runColdHookSeeded(t *testing.T, bin string, fr *fakeRouter, prompt string, seed func(logPath string), extra ...string) hookRun {
	t.Helper()
	work := t.TempDir()
	idx := writeColdIndex(t, work)
	logPath := filepath.Join(work, "usage.jsonl")
	if seed != nil {
		seed(logPath)
	}
	args := append([]string{"-index", idx, "-log", logPath, "-endpoint", fr.URL, "-quota-hint=false", "-timeout-ms", hookDeadlineMs}, extra...)
	cmd := exec.Command(bin, args...)
	in, _ := json.Marshal(map[string]string{"prompt": prompt})
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	t0 := time.Now()
	if err := cmd.Run(); err != nil {
		t.Fatalf("mr-hook exited non-zero: %v\nstderr: %s", err, stderr.String())
	}
	return hookRun{out: stdout.String(), rec: readLastRecord(t, logPath), wall: time.Since(t0)}
}

// settle waits until cond holds or the window closes, and reports whether it held.
func settle(window time.Duration, cond func() bool) bool {
	for end := time.Now().Add(window); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// exactlyOneWarmUp asserts the router saw ONE embeddings call, that it is a warm-up (the
// model name and a fixed token, never the prompt), and that no second one trickles in.
func exactlyOneWarmUp(t *testing.T, fr *fakeRouter, prompt string) {
	t.Helper()
	if !settle(2*time.Second, func() bool { return fr.embedHits.Load() >= 1 }) {
		t.Fatalf("the cold hook never issued its warm-up (embed calls: 0) — the load would not start")
	}
	time.Sleep(300 * time.Millisecond) // a second warm-up, if the code sent one, lands in this window
	if n := fr.embedHits.Load(); n != 1 {
		t.Fatalf("want exactly 1 warm-up, got %d embeddings calls", n)
	}
	fr.mu.Lock()
	body := fr.embedBodies[0]
	fr.mu.Unlock()
	if strings.Contains(body, prompt) || strings.Contains(body, "application") {
		t.Fatalf("the warm-up carried prompt text: %s", body)
	}
	var req struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil || req.Model != "embeddinggemma" || len(req.Input) != 1 {
		t.Fatalf("warm-up must name the embed model with one fixed input, got %s (%v)", body, err)
	}
}

// ---- (a) a cold embedder: the hook does not wait on /v1/embeddings -----------------------

func TestColdEmbedderSkipsTheWaitE2E(t *testing.T) {
	bin := buildColdHook(t)
	// The embedding itself would take 3 s: waiting on it is the failure.
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), embedIn: fakeEmbedTime, listsLoad: true})
	r := runColdHook(t, bin, fr, coldPrompt)

	if time.Duration(r.rec.LatencyMs)*time.Millisecond >= fakeEmbedTime/2 {
		t.Fatalf("hook latency %d ms: it waited on the cold embedder (its embedding takes %v)", r.rec.LatencyMs, fakeEmbedTime)
	}
	if r.wall >= fakeEmbedTime*4/5 {
		t.Fatalf("process took %v to exit: the warm-up held the hook open", r.wall)
	}
	if r.rec.Mode != "cold-skip-bm25" {
		t.Fatalf("mode %q, want cold-skip-bm25 (BM25 surfaced the skill); err=%q", r.rec.Mode, r.rec.Err)
	}
	if !strings.Contains(r.out, "gstack-qa") || len(r.rec.Surfaced) != 1 {
		t.Fatalf("the BM25 fallback must surface the lexical match at once: out=%q surfaced=%v", r.out, r.rec.Surfaced)
	}
	if !strings.Contains(r.rec.Err, "cold") || !strings.Contains(r.rec.Err, "load confirmed (starting)") {
		t.Fatalf("the row must say why it skipped the embedder and that the load was confirmed: %q", r.rec.Err)
	}
	exactlyOneWarmUp(t, fr, coldPrompt)
	t.Logf("cold: latency_ms=%d wall=%v mode=%s", r.rec.LatencyMs, r.wall.Round(time.Millisecond), r.rec.Mode)
}

// A router that never admits to loading (its /running does not change) must not stretch the
// warm-up past its ceiling: the hook waits for confirmation, but only that long.
func TestUnconfirmedWarmUpIsBoundedE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), embedIn: fakeEmbedTime}) // no listsLoad
	r := runColdHook(t, bin, fr, coldPrompt)
	if r.rec.Mode != "cold-skip-bm25" || !strings.Contains(r.rec.Err, "load not confirmed") {
		t.Fatalf("mode=%q err=%q, want a cold skip that says the load was not confirmed", r.rec.Mode, r.rec.Err)
	}
	// Lower bound: it did wait for a confirmation that never came. Upper bound: it stopped at
	// its ceiling long before the fake embedding (3 s) could have finished.
	if r.rec.LatencyMs < 100 || time.Duration(r.rec.LatencyMs)*time.Millisecond >= fakeEmbedTime/2 || r.wall >= fakeEmbedTime*4/5 {
		t.Fatalf("latency_ms=%d wall=%v: waiting for confirmation must stop at its ceiling", r.rec.LatencyMs, r.wall)
	}
	exactlyOneWarmUp(t, fr, coldPrompt)
}

// A cold skip whose BM25 consultation declines must not read as "embedder-down": the
// embedder is fine, it is just unloaded.
func TestColdEmbedderBM25DeclinesIsNotEmbedderDownE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), embedIn: fakeEmbedTime, listsLoad: true})
	r := runColdHook(t, bin, fr, coldPromptNoLexicalMatch)
	if r.rec.Mode != "cold-skip-empty" {
		t.Fatalf("mode %q, want cold-skip-empty", r.rec.Mode)
	}
	if r.out != "" || len(r.rec.Surfaced) != 0 {
		t.Fatalf("nothing may surface when BM25 declines: out=%q surfaced=%v", r.out, r.rec.Surfaced)
	}
	exactlyOneWarmUp(t, fr, coldPromptNoLexicalMatch)
}

// ---- (b) a warm embedder: embed as before ------------------------------------------------

func TestWarmEmbedderEmbedsAsBeforeE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningWarm)})
	r := runColdHook(t, bin, fr, coldPrompt)

	if r.rec.Mode != "embed" || r.rec.TopCosine != 1 {
		t.Fatalf("a loaded embedder must embed: mode=%q cos=%v err=%q", r.rec.Mode, r.rec.TopCosine, r.rec.Err)
	}
	if !strings.Contains(r.out, "gstack-qa") {
		t.Fatalf("expected the embedded ranking to surface gstack-qa, got %q", r.out)
	}
	if n := fr.embedHits.Load(); n != 1 {
		t.Fatalf("want exactly 1 embeddings call (the query), got %d", n)
	}
	fr.mu.Lock()
	body := fr.embedBodies[0]
	fr.mu.Unlock()
	if !strings.Contains(body, coldPrompt) {
		t.Fatalf("the one call must be the real query, got %s", body)
	}
	if fr.runningHits.Load() != 1 {
		t.Fatalf("the router must be asked once whether the model is loaded, got %d asks", fr.runningHits.Load())
	}
	t.Logf("warm: latency_ms=%d wall=%v mode=%s", r.rec.LatencyMs, r.wall.Round(time.Millisecond), r.rec.Mode)
}

// ---- (c) an unreadable /running behaves like a cold embedder -----------------------------

func TestUnreadableRunningBehavesLikeColdE2E(t *testing.T) {
	bin := buildColdHook(t)
	cases := []struct {
		name    string
		running http.HandlerFunc
	}{
		{"router error", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }},
		{"router hangs", hangUntilGone},
		{"unavailable", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "loading", http.StatusServiceUnavailable) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := newFakeRouter(t, routerOpts{running: c.running, embedIn: fakeEmbedTime})
			r := runColdHook(t, bin, fr, coldPrompt)
			if r.rec.Mode != "cold-skip-bm25" {
				t.Fatalf("mode %q, want cold-skip-bm25; err=%q", r.rec.Mode, r.rec.Err)
			}
			if r.wall >= fakeEmbedTime*4/5 {
				t.Fatalf("process took %v: an unreadable /running must not stall the hook", r.wall)
			}
			exactlyOneWarmUp(t, fr, coldPrompt)
			t.Logf("unreadable (%s): latency_ms=%d wall=%v", c.name, r.rec.LatencyMs, r.wall.Round(time.Millisecond))
		})
	}
}

// ---- servers that are not a model router: nothing to be cold about -----------------------

// A plain embedding server (the sidecar the failover chain also tries) has no /running. If
// that read as "unreadable", every prompt on such a host would cold-skip forever and the
// embedder would silently stop being used.
func TestServerWithoutRunningRouteEmbedsAsBeforeE2E(t *testing.T) {
	bin := buildColdHook(t)
	cases := []struct {
		name    string
		running http.HandlerFunc // nil = no route (404)
	}{
		{"404", nil},
		{"405", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", http.StatusMethodNotAllowed) }},
		{"200 json without a running list", jsonRunning(`{"data":[]}`)},
		{"200 html", jsonRunning(`<html>hello</html>`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fr := newFakeRouter(t, routerOpts{running: c.running})
			r := runColdHook(t, bin, fr, coldPrompt)
			if r.rec.Mode != "embed" {
				t.Fatalf("mode %q, want embed (no /running API means no cold-start to dodge); err=%q", r.rec.Mode, r.rec.Err)
			}
			if n := fr.embedHits.Load(); n != 1 {
				t.Fatalf("want the one real embeddings call, got %d", n)
			}
		})
	}
}

// ---- model naming: the router reports the REAL id, requests may use an alias -------------

func TestAliasedEmbedModelCountsAsLoadedE2E(t *testing.T) {
	bin := buildColdHook(t)
	models := `{"data":[{"id":"egemma-300m","meta":{"llamaswap":{"aliases":["embeddinggemma"]}}}]}`
	fr := newFakeRouter(t, routerOpts{
		running: jsonRunning(`{"running":[{"model":"egemma-300m","state":"ready"}]}`),
		models:  models,
	})
	r := runColdHook(t, bin, fr, coldPrompt)
	if r.rec.Mode != "embed" {
		t.Fatalf("the alias resolves to a loaded model, so it must embed: mode=%q err=%q", r.rec.Mode, r.rec.Err)
	}
	if n := fr.embedHits.Load(); n != 1 {
		t.Fatalf("want 1 embeddings call, got %d", n)
	}
}

func TestAliasedEmbedModelNotLoadedIsColdE2E(t *testing.T) {
	bin := buildColdHook(t)
	models := `{"data":[{"id":"egemma-300m","meta":{"llamaswap":{"aliases":["embeddinggemma"]}}}]}`
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), models: models, embedIn: fakeEmbedTime, listsLoad: true})
	r := runColdHook(t, bin, fr, coldPrompt)
	if r.rec.Mode != "cold-skip-bm25" {
		t.Fatalf("mode %q, want cold-skip-bm25; err=%q", r.rec.Mode, r.rec.Err)
	}
	exactlyOneWarmUp(t, fr, coldPrompt)
}

// A model name the router does not advertise at all is not "cold": embedding as before lets
// the router's own rejection reach the row, which is the message that names the fix.
func TestModelUnknownToRouterEmbedsAsBeforeE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), models: `{"data":[{"id":"something-else"}]}`})
	r := runColdHook(t, bin, fr, coldPrompt)
	if r.rec.Mode != "embed" {
		t.Fatalf("mode %q, want embed; err=%q", r.rec.Mode, r.rec.Err)
	}
	if n := fr.embedHits.Load(); n != 1 {
		t.Fatalf("want 1 embeddings call, got %d", n)
	}
}

// ---- a load already in flight: skip the wait, send no second request ---------------------

func TestEmbedderAlreadyLoadingSkipsWithoutAWarmUpE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(`{"running":[{"model":"embeddinggemma","state":"starting"}]}`), embedIn: fakeEmbedTime})
	r := runColdHook(t, bin, fr, coldPrompt)
	if r.rec.Mode != "cold-skip-bm25" {
		t.Fatalf("mode %q, want cold-skip-bm25; err=%q", r.rec.Mode, r.rec.Err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := fr.embedHits.Load(); n != 0 {
		t.Fatalf("a load is already in flight; want no warm-up, got %d embeddings calls", n)
	}
}

// ---- nothing listening: not cold, down — the real error must still reach the row ---------

func TestUnreachableEmbedderKeepsItsRealDiagnosisE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold)})
	fr.Close() // the port now refuses connections
	r := runColdHook(t, bin, fr, coldPrompt)
	if strings.HasPrefix(r.rec.Mode, "cold-skip") {
		t.Fatalf("a refused connection is a dead embedder, not a cold one: mode=%q", r.rec.Mode)
	}
	if !strings.Contains(r.rec.Err, "endpoint") {
		t.Fatalf("the embed failure must still be recorded: %q", r.rec.Err)
	}
}

// ---- the escape hatch ---------------------------------------------------------------------

func TestColdSkipFlagOffRestoresTheOldPathE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold)}) // embeds instantly
	r := runColdHook(t, bin, fr, coldPrompt, "-cold-skip=false")
	if fr.runningHits.Load() != 0 {
		t.Fatalf("-cold-skip=false must not consult /running (%d asks)", fr.runningHits.Load())
	}
	if r.rec.Mode != "embed" || fr.embedHits.Load() != 1 {
		t.Fatalf("mode=%q embed calls=%d, want embed / 1", r.rec.Mode, fr.embedHits.Load())
	}
}

// The latency numbers the change is judged by, printed (not asserted beyond (a)/(b)) so a
// run shows cold-skip against the old wait side by side.
func TestColdVersusWarmLatencyReportE2E(t *testing.T) {
	bin := buildColdHook(t)
	var lines []string
	for _, c := range []struct {
		name string
		opts routerOpts
		args []string
	}{
		{"cold, skip on ", routerOpts{running: jsonRunning(runningCold), embedIn: fakeEmbedTime, listsLoad: true}, nil},
		{"cold, skip off", routerOpts{running: jsonRunning(runningCold), embedIn: fakeEmbedTime, listsLoad: true}, []string{"-cold-skip=false"}},
		{"warm, skip on ", routerOpts{running: jsonRunning(runningWarm)}, nil},
		{"warm, skip off", routerOpts{running: jsonRunning(runningWarm)}, []string{"-cold-skip=false"}},
	} {
		fr := newFakeRouter(t, c.opts)
		r := runColdHook(t, bin, fr, coldPrompt, c.args...)
		lines = append(lines, fmt.Sprintf("%s  latency_ms=%4d  wall=%4dms  mode=%s", c.name, r.rec.LatencyMs, r.wall.Milliseconds(), r.rec.Mode))
	}
	t.Log("\n" + strings.Join(lines, "\n"))
}

// ---- the warm-up's own answer ---------------------------------------------------------------

// A router that answers the warm-up with an error has not started loading anything, and the
// row must say what it answered instead of reading it as a warm model.
func TestWarmUpRefusedByTheRouterIsRecordedE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), embedStatus: 503})
	r := runColdHook(t, bin, fr, coldPrompt)
	if r.rec.Mode != "cold-skip-bm25" {
		t.Fatalf("mode %q, want cold-skip-bm25; err=%q", r.rec.Mode, r.rec.Err)
	}
	if !strings.Contains(r.rec.Err, "warm-up failed") || !strings.Contains(r.rec.Err, "status 503") || strings.Contains(r.rec.Err, "confirmed (") {
		t.Fatalf("the row must say the router refused the warm-up with 503: %q", r.rec.Err)
	}
}

// ---- a router that never gets the embedder loaded ---------------------------------------------

func coldRow(ageSeconds int64, mode string) usagelog.Record {
	return usagelog.Record{TsUnix: time.Now().Unix() - ageSeconds, Mode: mode}
}

func seedRows(rows ...usagelog.Record) func(string) {
	return func(logPath string) {
		for _, r := range rows {
			if err := usagelog.Append(logPath, r); err != nil {
				panic(err)
			}
		}
	}
}

func coldFakeRouter(t *testing.T) *fakeRouter {
	return newFakeRouter(t, routerOpts{running: jsonRunning(runningCold), embedIn: fakeEmbedTime, listsLoad: true})
}

// Skipping the embed wait removed the only thing that would have surfaced a router that is up
// but cannot load the embedder (the embed call erroring), so a run of cold skips that outlasts
// any real load must be visible in the row.
func TestRepeatedColdSkipsAreLoggedAsStuckE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := coldFakeRouter(t)
	r := runColdHookSeeded(t, bin, fr, coldPrompt, seedRows(
		coldRow(400, "cold-skip-empty"), coldRow(300, "cold-skip-bm25"), coldRow(200, "cold-skip-empty")))
	if r.rec.Mode != "cold-stuck-bm25" {
		t.Fatalf("mode %q, want cold-stuck-bm25; err=%q", r.rec.Mode, r.rec.Err)
	}
	if !strings.Contains(r.rec.Err, "stuck") || !strings.Contains(r.rec.Err, "4 consecutive") {
		t.Fatalf("the row must say how long the embedder has been cold: %q", r.rec.Err)
	}
	if !strings.Contains(r.out, "gstack-qa") {
		t.Fatalf("a stuck embedder still gets the BM25 fallback: out=%q", r.out)
	}
	exactlyOneWarmUp(t, fr, coldPrompt)
}

func TestStuckWithNothingSurfacedIsLoggedAsStuckEmptyE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := coldFakeRouter(t)
	r := runColdHookSeeded(t, bin, fr, coldPromptNoLexicalMatch, seedRows(
		coldRow(400, "cold-skip-empty"), coldRow(300, "cold-stuck-empty"), coldRow(200, "cold-stuck-bm25")))
	if r.rec.Mode != "cold-stuck-empty" {
		t.Fatalf("mode %q, want cold-stuck-empty (earlier cold-stuck rows keep the run going)", r.rec.Mode)
	}
}

func TestColdSkipsThatAreNotARunLongEnoughStayOrdinaryE2E(t *testing.T) {
	bin := buildColdHook(t)
	for name, seed := range map[string]func(string){
		"too few":          seedRows(coldRow(400, "cold-skip-empty"), coldRow(300, "cold-skip-empty")),
		"too recent":       seedRows(coldRow(30, "cold-skip-empty"), coldRow(20, "cold-skip-empty"), coldRow(10, "cold-skip-empty")),
		"embed ended it":   seedRows(coldRow(500, "cold-skip-empty"), coldRow(450, "cold-skip-empty"), coldRow(400, "embed"), coldRow(200, "cold-skip-empty")),
		"failure ended it": seedRows(coldRow(500, "cold-skip-empty"), coldRow(450, "cold-skip-empty"), coldRow(400, "embedder-down"), coldRow(200, "cold-skip-empty")),
		"no history":       nil,
	} {
		t.Run(name, func(t *testing.T) {
			fr := coldFakeRouter(t)
			r := runColdHookSeeded(t, bin, fr, coldPrompt, seed)
			if r.rec.Mode != "cold-skip-bm25" {
				t.Fatalf("mode %q, want an ordinary cold-skip-bm25; err=%q", r.rec.Mode, r.rec.Err)
			}
		})
	}
}

// Rows that never reached the embedder (a notification, a too-short prompt) say nothing about
// it and must not break a run.
func TestRowsThatNeverTouchedTheEmbedderDoNotBreakARunE2E(t *testing.T) {
	bin := buildColdHook(t)
	fr := coldFakeRouter(t)
	r := runColdHookSeeded(t, bin, fr, coldPrompt, seedRows(
		coldRow(500, "cold-skip-empty"), coldRow(450, "machine-turn"), coldRow(400, "cold-skip-empty"),
		coldRow(300, "too-short"), coldRow(200, "cold-skip-bm25")))
	if r.rec.Mode != "cold-stuck-bm25" {
		t.Fatalf("mode %q, want cold-stuck-bm25; err=%q", r.rec.Mode, r.rec.Err)
	}
}
