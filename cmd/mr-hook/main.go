// Command mr-hook is the UserPromptSubmit hook: it loads the skill index,
// surfaces the few most relevant installed skills for the prompt, and injects
// them as additionalContext. It is strictly additive — on ANY error or timeout
// it injects nothing and exits 0, never blocking or breaking a prompt.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/index"
	"github.com/dmmdea/meta-router/internal/retrievers"
	"github.com/dmmdea/meta-router/internal/usagelog"
)

type hookInput struct {
	Prompt string `json:"prompt"`
}

type scoredRetriever interface {
	RetrieveScored(prompt string, k int) ([]retrievers.Scored, float64, error)
}
type lexicalRetriever interface {
	Retrieve(prompt string, k int) ([]string, error)
}

// failedRetriever stands in for a hybrid that could not be built, so decide
// uniformly takes its BM25-fallback branch (no untested side path in main).
type failedRetriever struct{ err error }

func (f failedRetriever) RetrieveScored(string, int) ([]retrievers.Scored, float64, error) {
	return nil, 0, f.err
}

// decide is the pure surfacing decision: try the hybrid (semantic+lexical) and
// gate on the top cosine; if the embedder errored, surface nothing (mode
// "embedder-down"); if the prompt is too short, surface nothing (mode
// "too-short"). Returns the ids to surface, the top cosine (0 when not
// applicable), and the mode for logging.
func decide(prompt string, k int, minCos float64, minLen int, hyb scoredRetriever, lex lexicalRetriever) ([]string, float64, string) {
	if len(strings.TrimSpace(prompt)) < minLen {
		return nil, 0, "too-short"
	}
	res, topCos, err := hyb.RetrieveScored(prompt, k)
	if err != nil {
		// Embedder unavailable — log the mode but surface nothing.
		// The lexical retriever (lex) remains wired for a future gated path.
		_ = lex
		return nil, 0, "embedder-down"
	}
	if topCos < minCos {
		return nil, topCos, "gated-empty"
	}
	ids := make([]string, len(res))
	for i, s := range res {
		ids[i] = s.ID
	}
	return ids, topCos, "hybrid"
}

func formatContext(byID map[string]catalog.Skill, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("meta-router — relevant installed skills for this task:\n")
	wrote := 0
	for _, id := range ids {
		s, ok := byID[id]
		if !ok {
			continue
		}
		desc := s.Description
		if len(desc) > 140 {
			desc = desc[:140] + "…"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", s.Name, s.Source, desc)
		wrote++
	}
	if wrote == 0 {
		return "" // all ids missing from the index → surface nothing
	}
	b.WriteString("Invoke one via the Skill tool or its slash command if it fits.")
	return b.String()
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func emit(ctx string) string {
	if ctx == "" {
		return ""
	}
	var out hookOutput
	out.HookSpecificOutput.HookEventName = "UserPromptSubmit"
	out.HookSpecificOutput.AdditionalContext = ctx
	b, err := json.Marshal(out)
	if err != nil {
		return ""
	}
	return string(b)
}

func main() {
	endpoint := flag.String("endpoint", "http://127.0.0.1:11436", "embedder endpoint")
	indexPath := flag.String("index", "", "index path (default ~/.meta-router/index.json)")
	logPath := flag.String("log", "", "usage log path (default ~/.meta-router/usage.jsonl)")
	minCos := flag.Float64("min-cosine", 0.55, "min top cosine to surface (gate)")
	minLen := flag.Int("min-len", 12, "min prompt length (chars, trimmed) to attempt retrieval")
	k := flag.Int("k", 3, "max skills to surface")
	timeoutMs := flag.Int("timeout-ms", 300, "hard deadline for the whole retrieve")
	flag.Parse()

	// Always exit 0 — fail-open is absolute.
	defer os.Exit(0)

	start := time.Now()
	rec := usagelog.Record{TsUnix: start.Unix(), Mode: "error"}
	logIt := func() {
		rec.LatencyMs = time.Since(start).Milliseconds()
		lp := *logPath
		if lp == "" {
			if p, err := usagelog.DefaultLogPath(); err == nil {
				lp = p
			}
		}
		if lp != "" {
			_ = usagelog.Append(lp, rec) // log failure must not break the hook
		}
	}
	defer logIt()

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		rec.Err = "stdin: " + err.Error()
		return
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil || strings.TrimSpace(in.Prompt) == "" {
		rec.Err = "no prompt"
		return
	}
	rec.PromptHash = usagelog.HashPrompt(in.Prompt)
	rec.PromptLen = len(in.Prompt)

	ip := *indexPath
	if ip == "" {
		if p, err := index.DefaultIndexPath(); err == nil {
			ip = p
		}
	}
	idx, err := index.Load(ip)
	if err != nil {
		rec.Err = "load index: " + err.Error()
		return // no index yet → surface nothing (run mr-index build)
	}
	skills := idx.Skills()
	byID := make(map[string]catalog.Skill, len(skills))
	for _, s := range skills {
		byID[s.ID] = s
	}

	// Per-prompt embedder timeout is a fraction of the hard deadline.
	embedTO := time.Duration(*timeoutMs) * time.Millisecond
	if *timeoutMs > 50 {
		embedTO = time.Duration(*timeoutMs-50) * time.Millisecond
	}
	hyb, herr := retrievers.NewHybridFromIndex(skills, idx.Vectors(), *endpoint, embedTO)
	var sr scoredRetriever
	if herr != nil {
		sr = failedRetriever{herr}
	} else {
		sr = hyb
	}
	bm25 := retrievers.NewBM25(skills)

	// Run the decision under a hard deadline; if it overruns, surface nothing.
	type result struct {
		ids    []string
		topCos float64
		mode   string
	}
	ch := make(chan result, 1)
	go func() {
		ids, topCos, mode := decide(in.Prompt, *k, *minCos, *minLen, sr, bm25)
		ch <- result{ids, topCos, mode}
	}()

	select {
	case r := <-ch:
		rec.Surfaced, rec.TopCosine, rec.Mode = r.ids, r.topCos, r.mode
		ctx := formatContext(byID, r.ids)
		if offloadNudge(in.Prompt) {
			rec.NudgeOffload = true
			ctx = appendNudge(ctx)
		}
		if out := emit(ctx); out != "" {
			fmt.Fprintln(os.Stdout, out)
		}
	case <-time.After(time.Duration(*timeoutMs) * time.Millisecond):
		rec.Mode = "error"
		rec.Err = "deadline exceeded"
		// surface nothing
	}
}
