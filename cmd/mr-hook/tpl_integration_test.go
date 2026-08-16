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

func writeIndex(t *testing.T, dir, model string) string {
	t.Helper()
	idx := `{"model":` + jsonQuote(model) + `,"dim":2,"built_unix":1,"entries":[` +
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

	idxPath := writeIndex(t, work, "embeddinggemma/tpl99")
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

	idxPath := writeIndex(t, work, "embeddinggemma/tpl1")
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

	idxPath := writeIndex(t, work, "embeddinggemma")
	logPath := filepath.Join(work, "usage.jsonl")
	prompt := "QA test my running web application"
	runHook(t, bin, prompt,
		"-index", idxPath, "-log", logPath, "-endpoint", srv.URL, "-quota-hint=false", "-timeout-ms", "5000")
	if got, _ := lastInput.Load().(string); got != prompt {
		t.Fatalf("legacy index must embed the raw prompt, got %q", got)
	}
}
