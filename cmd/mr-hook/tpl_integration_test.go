package main

// W9-P item 1 integration, at the binary level (the refusal lives in main(),
// past where unit tests reach):
//
//   - an index whose identity names a template this binary does not know
//     surfaces NOTHING (no BM25 consolation), sends the prompt to no
//     embedder, exits 0, and logs mode "tpl-mismatch" with the reason;
//   - a templated index it DOES know puts the templated query on the wire
//     and surfaces normally.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"net/http"
	"net/http/httptest"

	"github.com/dmmdea/meta-router/internal/usagelog"
)

func buildMRHook(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mr-hook-test.exe")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// runHook runs the built hook with a prompt on stdin and returns stdout.
func runHook(t *testing.T, bin string, prompt string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	in, _ := json.Marshal(map[string]string{"prompt": prompt})
	cmd.Stdin = bytes.NewReader(in)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// exit 0 is the hook's contract on EVERY path.
		t.Fatalf("mr-hook exited non-zero: %v\nstderr: %s", err, stderr.String())
	}
	return stdout.String()
}

// writeIndex writes a one-skill index fixture. guard is the tpl_guard field
// ("" omits it — a legacy index, or a templated one whose guard a
// pre-template binary stripped).
func writeIndex(t *testing.T, dir, model, guard string) string {
	t.Helper()
	g := ""
	if guard != "" {
		g = `,"tpl_guard":` + jsonQuote(guard)
	}
	idx := `{"model":` + jsonQuote(model) + `,"dim":2,"built_unix":1` + g + `,"entries":[` +
		`{"skill":{"id":"gstack-qa","name":"gstack-qa","source":"skills","description":"QA test a web application"},"vec":[1,0],"hash":"h"}]}`
	p := filepath.Join(dir, "index.json")
	if err := os.WriteFile(p, []byte(idx), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func readLastRecord(t *testing.T, logPath string) usagelog.Record {
	t.Helper()
	recs, err := usagelog.ReadRecords(logPath)
	if err != nil || len(recs) == 0 {
		t.Fatalf("no usage records at %s: %v", logPath, err)
	}
	return recs[len(recs)-1]
}

func TestHookTemplateMismatchRefusesE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	var embedHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&embedHits, 1)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	idxPath := writeIndex(t, work, "embeddinggemma/tpl99", "tpl99")
	logPath := filepath.Join(work, "usage.jsonl")
	// The prompt contains "QA test a web application" verbatim — under the
	// old behavior BM25 would happily surface gstack-qa. The refusal must
	// beat even that: nothing surfaces on a template mismatch.
	out := runHook(t, bin, "QA test a web application for me please",
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-quota-hint=false", "-timeout-ms", "5000")
	if out != "" {
		t.Fatalf("mismatch must surface nothing, got: %s", out)
	}
	if n := atomic.LoadInt32(&embedHits); n != 0 {
		t.Fatalf("the prompt must never reach an embedder on mismatch (%d calls)", n)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "tpl-mismatch" {
		t.Fatalf("mode: %q", rec.Mode)
	}
	if !strings.Contains(rec.Err, "tpl99") {
		t.Fatalf("row must name the unknown template: %q", rec.Err)
	}
	if len(rec.Surfaced) != 0 {
		t.Fatalf("surfaced must be empty: %v", rec.Surfaced)
	}
}

func TestHookTemplatedIndexQueriesTemplatedE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	var lastInput atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		lastInput.Store(req.Model + "|" + strings.Join(req.Input, "§"))
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	idxPath := writeIndex(t, work, "embeddinggemma/tpl1", "tpl1")
	logPath := filepath.Join(work, "usage.jsonl")
	prompt := "QA test my running web application"
	out := runHook(t, bin, prompt,
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-quota-hint=false", "-timeout-ms", "5000")

	// Query vector [1,0] vs entry vector [1,0] → cosine 1.0 ≥ 0.55 → surfaced.
	if !strings.Contains(out, "gstack-qa") {
		t.Fatalf("expected gstack-qa surfaced, got: %s", out)
	}
	got, _ := lastInput.Load().(string)
	want := "embeddinggemma|task: search result | query: " + prompt
	if got != want {
		t.Fatalf("wire:\n got %q\nwant %q", got, want)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "embed" {
		t.Fatalf("mode: %q", rec.Mode)
	}
}

// A legacy (bare-identity) index keeps today's exact behavior: raw query on
// the wire.
func TestHookLegacyIndexQueriesRawE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	var lastInput atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		lastInput.Store(strings.Join(req.Input, "§"))
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	idxPath := writeIndex(t, work, "embeddinggemma", "")
	logPath := filepath.Join(work, "usage.jsonl")
	prompt := "QA test my running web application"
	runHook(t, bin, prompt,
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-quota-hint=false", "-timeout-ms", "5000")
	if got, _ := lastInput.Load().(string); got != prompt {
		t.Fatalf("legacy index must embed the raw prompt, got %q", got)
	}
}

// The TplGuard tripwire: a templated identity whose guard was stripped (what a
// pre-template mr-index's save does after re-embedding everything raw) must
// refuse exactly like an unknown template — same model, same dim, hashes
// coherent, and the ONLY tell is the missing guard.
func TestHookStrippedGuardRefusesE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	var embedHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&embedHits, 1)
		w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
	}))
	defer srv.Close()

	idxPath := writeIndex(t, work, "embeddinggemma/tpl1", "") // guard stripped
	logPath := filepath.Join(work, "usage.jsonl")
	out := runHook(t, bin, "QA test a web application for me please",
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-quota-hint=false", "-timeout-ms", "5000")
	if out != "" {
		t.Fatalf("stripped guard must surface nothing, got: %s", out)
	}
	if n := atomic.LoadInt32(&embedHits); n != 0 {
		t.Fatalf("no embedder call may happen on a stripped guard (%d calls)", n)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "tpl-mismatch" {
		t.Fatalf("mode: %q", rec.Mode)
	}
	if !strings.Contains(rec.Err, "template guard") {
		t.Fatalf("row must name the guard failure: %q", rec.Err)
	}
}

// Regression (review 2026-08-16, MAJOR-adjacent): the offload nudge is
// prompt-shaped and used to run OUTSIDE the tpl gate, so a nudge-triggering
// prompt against a mismatched index surfaced the bare nudge despite the
// "nothing but the quota hint" contract.
func TestHookMismatchSuppressesOffloadNudgeE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	idxPath := writeIndex(t, work, "embeddinggemma/tpl99", "tpl99")
	logPath := filepath.Join(work, "usage.jsonl")
	// Leading offload verb + >400 chars → offloadNudge(prompt) is true.
	prompt := "summarize the following build log please: " + strings.Repeat("error TS2307 cannot find module x ", 15)
	if len(prompt) < 400 {
		t.Fatal("fixture must exceed the nudge length gate")
	}
	out := runHook(t, bin, prompt,
		"-index", idxPath, "-log", logPath, "-endpoint", "http://127.0.0.1:1", "-quota-hint=false", "-timeout-ms", "5000")
	if out != "" {
		t.Fatalf("mismatch must suppress the nudge too, got: %s", out)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "tpl-mismatch" || rec.NudgeOffload {
		t.Fatalf("mode=%q nudge=%v — nudge must not fire on a refusal", rec.Mode, rec.NudgeOffload)
	}
}

// The other half of the refusal contract: the quota hint DOES survive a
// mismatch. MR_ORCH_STATE points at a state dir whose only content is a GLM
// hard-stop latch — a deterministic, ledger-independent hint.
func TestHookMismatchKeepsQuotaHintE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	stateDir := filepath.Join(work, "orchstate")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "glm-alert.json"), []byte(`{"note":"test latch"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	idxPath := writeIndex(t, work, "embeddinggemma/tpl99", "tpl99")
	logPath := filepath.Join(work, "usage.jsonl")

	cmd := exec.Command(bin, "-index", idxPath, "-log", logPath, "-endpoint", "http://127.0.0.1:1", "-timeout-ms", "5000")
	in, _ := json.Marshal(map[string]string{"prompt": "QA test a web application for me please"})
	cmd.Stdin = bytes.NewReader(in)
	cmd.Env = append(os.Environ(), "MR_ORCH_STATE="+stateDir)
	outB, err := cmd.Output()
	if err != nil {
		t.Fatalf("mr-hook exited non-zero: %v", err)
	}
	out := string(outB)
	if !strings.Contains(out, "glm HARD-STOP(1313)") {
		t.Fatalf("the quota hint must survive a tpl-mismatch, got: %q", out)
	}
	if strings.Contains(out, "gstack-qa") || strings.Contains(out, "relevant installed skills") {
		t.Fatalf("nothing but the hint may surface: %q", out)
	}
	rec := readLastRecord(t, logPath)
	if rec.Mode != "tpl-mismatch" || !rec.QuotaHint {
		t.Fatalf("mode=%q quotaHint=%v", rec.Mode, rec.QuotaHint)
	}
}

// The usage row must carry EVERY cause: a -ranker notice pre-sets rec.Err,
// and the primary retriever's failure must JOIN it, not be masked by it —
// round 2 proved the primaryErr wiring was a mutation survivor and that a
// first-cause-wins slot hid the "rebuild the index" diagnostic permanently
// on any host with a ranker misconfiguration.
func TestHookRowJoinsRankerNoticeAndPrimaryErrorE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	idxPath := writeIndex(t, work, "embeddinggemma", "")
	logPath := filepath.Join(work, "usage.jsonl")
	// Unknown ranker (pre-sets rec.Err, degrades to embed) + a dead endpoint
	// (primary retriever fails) + a prompt with no lexical pull (no fallback).
	runHook(t, bin, "completely unrelated wording with no lexical pull",
		"-index", idxPath, "-log", logPath, "-endpoint", "http://127.0.0.1:1",
		"-ranker", "bogus", "-quota-hint=false", "-timeout-ms", "5000")
	rec := readLastRecord(t, logPath)
	if rec.Mode != "embedder-down" {
		t.Fatalf("mode: %q", rec.Mode)
	}
	if !strings.Contains(rec.Err, "unknown -ranker") {
		t.Fatalf("row lost the ranker notice: %q", rec.Err)
	}
	if !strings.Contains(rec.Err, "primary retriever:") {
		t.Fatalf("row lost the primary error: %q", rec.Err)
	}
}

// templatedRankerServer fakes llama-swap for the hybrid/rerank ranker paths:
// /v1/models 200 (liveEndpoint probe), /v1/embeddings capturing model|input,
// /v1/rerank capturing model and query.
func templatedRankerServer(t *testing.T, embedCap, rerankCap *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"embeddinggemma"}]}`))
		case "/v1/embeddings":
			var req struct {
				Model string   `json:"model"`
				Input []string `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			embedCap.Store(req.Model + "|" + strings.Join(req.Input, "§"))
			w.Write([]byte(`{"data":[{"index":0,"embedding":[1,0]}]}`))
		case "/v1/rerank":
			var req struct {
				Model     string   `json:"model"`
				Query     string   `json:"query"`
				Documents []string `json:"documents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			rerankCap.Store(req.Model + "|" + req.Query)
			type res struct {
				Index          int     `json:"index"`
				RelevanceScore float64 `json:"relevance_score"`
			}
			out := struct {
				Results []res `json:"results"`
			}{}
			for i := range req.Documents {
				out.Results = append(out.Results, res{Index: i, RelevanceScore: float64(len(req.Documents) - i)})
			}
			json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The hybrid ranker's embed leg must template the query under a templated
// index — mutating its spec threading to Raw was a full-suite survivor
// (review 2026-08-16, G3).
func TestHookHybridTemplatedQueryE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	var embedCap, rerankCap atomic.Value
	srv := templatedRankerServer(t, &embedCap, &rerankCap)

	idxPath := writeIndex(t, work, "embeddinggemma/tpl1", "tpl1")
	logPath := filepath.Join(work, "usage.jsonl")
	prompt := "QA test my running web application"
	out := runHook(t, bin, prompt,
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-ranker", "hybrid", "-quota-hint=false", "-timeout-ms", "5000")
	if !strings.Contains(out, "gstack-qa") {
		t.Fatalf("expected gstack-qa surfaced, got: %s", out)
	}
	got, _ := embedCap.Load().(string)
	if want := "embeddinggemma|task: search result | query: " + prompt; got != want {
		t.Fatalf("hybrid embed leg wire:\n got %q\nwant %q", got, want)
	}
	if rec := readLastRecord(t, logPath); rec.Mode != "hybrid" {
		t.Fatalf("mode: %q", rec.Mode)
	}
}

// The rerank ranker under a templated index: embed leg templated, rerank leg
// requesting the PRODUCTION bge model with the RAW (unformatted) query — a
// qwen-instructed rerank query here would mean the wrong RerankSpec reached
// the production call site (review 2026-08-16, G5).
func TestHookRerankTemplatedQueryE2E(t *testing.T) {
	bin := buildMRHook(t)
	work := t.TempDir()
	var embedCap, rerankCap atomic.Value
	srv := templatedRankerServer(t, &embedCap, &rerankCap)

	idxPath := writeIndex(t, work, "embeddinggemma/tpl1", "tpl1")
	logPath := filepath.Join(work, "usage.jsonl")
	prompt := "QA test my running web application"
	// -ranker=rerank refuses below 6000ms and serves embed instead.
	out := runHook(t, bin, prompt,
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-ranker", "rerank", "-quota-hint=false", "-timeout-ms", "8000")
	if !strings.Contains(out, "gstack-qa") {
		t.Fatalf("expected gstack-qa surfaced, got: %s", out)
	}
	gotEmbed, _ := embedCap.Load().(string)
	if want := "embeddinggemma|task: search result | query: " + prompt; gotEmbed != want {
		t.Fatalf("rerank path embed leg wire:\n got %q\nwant %q", gotEmbed, want)
	}
	gotRerank, _ := rerankCap.Load().(string)
	if want := "bge-reranker-v2-m3|" + prompt; gotRerank != want {
		t.Fatalf("rerank leg must request bge with the RAW query:\n got %q\nwant %q", gotRerank, want)
	}
	if rec := readLastRecord(t, logPath); rec.Mode != "rerank" {
		t.Fatalf("mode: %q", rec.Mode)
	}
}
