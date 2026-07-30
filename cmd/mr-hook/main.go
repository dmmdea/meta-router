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
type lexicalScorer interface {
	RetrieveScored(prompt string, k int) []retrievers.Scored
}

// failedRetriever stands in for a primary ranker that could not be built, so
// decide uniformly takes its BM25-fallback branch (no untested side path in main).
type failedRetriever struct{ err error }

func (f failedRetriever) RetrieveScored(string, int) ([]retrievers.Scored, float64, error) {
	return nil, 0, f.err
}

// BM25-fallback precision gate, derived 2026-07-04 from the 236-case goldset
// (testdata/goldset*.jsonl) over the real 148-skill corpus:
//   - raw top-1 BM25 >= 18 OR top-1/(prompt tokens) >= 1.5 fired on 14
//     winnable cases with 14 correct top-1s (precision 1.000, recall 0.165),
//     and on only 3/151 cases whose target skill is not installed — each a
//     plausible neighbor (e.g. "execute all plans in the current GSD phase"
//     → superpowers:executing-plans).
//   - the obvious alternative — an exact skill-name token in the prompt —
//     measured only 0.25 precision (a name mention usually refers to the
//     FAMILY: "use gstack to investigate…" expects gstack-investigate), so
//     it is deliberately NOT a gate here.
//
// The per-token component catches short, sharply lexical prompts; the raw
// component catches longer prompts with an overwhelming match. Only the
// single top match is surfaced: this path runs with the embedder down, where
// a wrong surfacing is worse than silence (the reason the old ungated
// fallback was removed). ~16% recall is acceptable for a fallback whose
// alternative is surfacing nothing.
const (
	bm25RawGate      = 18.0
	bm25PerTokenGate = 1.5
)

// bm25Fallback returns at most one id: the top BM25 match, and only when it
// clears the precision gate above.
func bm25Fallback(prompt string, lex lexicalScorer) []string {
	if lex == nil {
		return nil
	}
	top := lex.RetrieveScored(prompt, 1)
	if len(top) == 0 {
		return nil
	}
	qTokens := len(strings.Fields(prompt))
	if qTokens < 1 {
		qTokens = 1
	}
	if top[0].Score >= bm25RawGate || top[0].Score/float64(qTokens) >= bm25PerTokenGate {
		return []string{top[0].ID}
	}
	return nil
}

// logDepth is how many scored candidates each cosine-path event records
// (W9 R9.2b). Deeper than the surface k so the curve analysis sees the
// invoked skill's own score even when it ranked below the surfacing cut;
// bounded so rows stay small.
const logDepth = 8

// decide is the pure surfacing decision: rank with the primary retriever
// (embed-only by default; hybrid RRF behind -ranker=hybrid) and gate on the
// top cosine. If the embedder errored, fall back to BM25 under a strict
// precision gate (mode "bm25-fallback") or stay silent (mode
// "embedder-down"); if the prompt is too short, surface nothing (mode
// "too-short"). Returns the ids to surface, the top cosine (0 when not
// applicable), the mode for logging, and the scored candidate list (W9
// R9.2b) — populated on BOTH surfaced and gated-empty cosine paths (the
// gated region is 27% of live traffic and exactly where the gate decision
// needs scores), nil on modes where no cosine ran, so a BM25 score can never
// recontaminate the cosine denominator the R9.2 analysis just cleaned.
//
// Retrieval depth is max(k, logDepth) but ONLY the first k surface: the
// ranking is one deterministic sort, so the top-k of a deeper retrieval is
// identical to a k-retrieval — pinned by TestSurfacedIsPrefixOfCandidates.
func decide(prompt string, k int, minCos float64, minLen int, primary scoredRetriever, primaryMode string, lex lexicalScorer) ([]string, float64, string, []usagelog.Cand) {
	if len(strings.TrimSpace(prompt)) < minLen {
		return nil, 0, "too-short", nil
	}
	kRetrieve := k
	if kRetrieve < logDepth {
		kRetrieve = logDepth
	}
	res, topCos, err := primary.RetrieveScored(prompt, kRetrieve)
	if err != nil {
		if ids := bm25Fallback(prompt, lex); len(ids) > 0 {
			return ids, 0, "bm25-fallback", nil
		}
		return nil, 0, "embedder-down", nil
	}
	// cands carries COSINES OR NOTHING. Hybrid's RetrieveScored returns RRF
	// fused scores in .Score (~0.03 scale, 1/(60+rank) sums) — real cosines
	// exist only in its topCos return. Logging those under "cos" would put
	// 0.03-scale numbers into the cosine denominator with no discriminator on
	// gated-empty rows (review 2026-07-30, MAJOR): the exact contamination
	// class this field exists to prevent. So candidates are logged on the
	// embed path only; hybrid rows carry none.
	var cands []usagelog.Cand
	if primaryMode == "embed" {
		cands = make([]usagelog.Cand, len(res))
		for i, s := range res {
			cands[i] = usagelog.Cand{ID: s.ID, Cos: s.Score}
		}
	}
	if topCos < minCos {
		return nil, topCos, "gated-empty", cands
	}
	if len(res) > k {
		res = res[:k]
	}
	ids := make([]string, len(res))
	for i, s := range res {
		ids[i] = s.ID
	}
	return ids, topCos, primaryMode, cands
}

func formatContext(byID map[string]catalog.Skill, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("meta-router — relevant installed skills for this task:\n")
	wrote := 0
	wroteAgent := false
	for _, id := range ids {
		s, ok := byID[id]
		if !ok {
			continue
		}
		desc := s.Description
		if len(desc) > 140 {
			desc = desc[:140] + "…"
		}
		// s.ID is the INVOCABLE name ("gstack-qa", "superpowers:brainstorming",
		// "/insights") — surface exactly what the invoking tool accepts, never
		// an internal name. Agents are the exception that proves the rule: their
		// index ID is "agent:<name>" (a namespace marker, and one that collides
		// with the plugin pack:skill grammar this same list uses), while the
		// Agent tool's subagent_type is the BARE name — so the bare name is what
		// gets surfaced, labeled "agent" (review 2026-07-30, MAJOR: the model
		// would otherwise call Skill(\"agent:context-engineer\") and fail).
		if name, isAgent := strings.CutPrefix(s.ID, "agent:"); isAgent {
			fmt.Fprintf(&b, "- %s (agent): %s\n", name, desc)
			wroteAgent = true
		} else {
			fmt.Fprintf(&b, "- %s (%s): %s\n", s.ID, s.Source, desc)
		}
		wrote++
	}
	if wrote == 0 {
		return "" // all ids missing from the index → surface nothing
	}
	// The trailer stays byte-identical to the pre-flat-roots output whenever no
	// agent surfaced — which is every production prompt until W9's gate enables
	// the flat roots — so this change is behavior-inert today by construction.
	b.WriteString("Invoke one via the Skill tool or its slash command if it fits.")
	if wroteAgent {
		b.WriteString(" Entries marked (agent) are dispatched via the Agent tool (subagent_type = the listed name), never the Skill tool.")
	}
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
	endpoint := flag.String("endpoint", "", "embedder endpoint; empty = resolve for this machine ($MR_EMBED_ENDPOINT, ~/.meta-router/endpoints.json, then the built-in :11436→:18793 failover chain). Setting it pins that endpoint exactly.")
	indexPath := flag.String("index", "", "index path (default ~/.meta-router/index.json)")
	logPath := flag.String("log", "", "usage log path (default ~/.meta-router/usage.jsonl)")
	minCos := flag.Float64("min-cosine", 0.55, "min top cosine to surface (gate)")
	minLen := flag.Int("min-len", 6, "min prompt length (chars, trimmed) to attempt retrieval")
	k := flag.Int("k", 3, "max skills to surface")
	timeoutMs := flag.Int("timeout-ms", 300, "hard deadline for the whole retrieve")
	ranker := flag.String("ranker", "embed", `primary ranking: "embed" (cosine-only; measured better on the goldset) or "hybrid" (BM25+embed RRF)`)
	quotaHintOn := flag.Bool("quota-hint", true, "append the mr-orchestrate quota+route hint (ledger-direct, fail-open, zero policy)")
	showVersion := flag.Bool("version", false, "print this binary's build revision and exit (deployed-fleet freshness — a deployed hook that cannot be asked what it is stayed 3 releases stale)")
	flag.Parse()
	if *showVersion {
		fmt.Println("mr-hook", buildRevision())
		return
	}

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
	idx, err := index.LoadFast(ip) // index.bin sidecar when fresh, else JSON
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
	primaryMode := *ranker
	var sr scoredRetriever
	switch *ranker {
	case "hybrid":
		hyb, herr := retrievers.NewHybridFromIndex(skills, idx.Vectors(), *endpoint, embedTO)
		if herr != nil {
			sr = failedRetriever{herr}
		} else {
			sr = hyb
		}
	default: // "embed" — primary ranking is embed-only cosine ordering
		primaryMode = "embed"
		vecs := idx.Vectors()
		if len(skills) != len(vecs) {
			sr = failedRetriever{fmt.Errorf("index: %d skills but %d vectors", len(skills), len(vecs))}
		} else {
			ids := make([]string, len(skills))
			for i, s := range skills {
				ids[i] = s.ID
			}
			sr = retrievers.NewEmbedFromVectors(ids, vecs, *endpoint, embedTO)
		}
	}
	bm25 := retrievers.NewBM25(skills)

	// Run the decision AND the quota hint under ONE hard deadline; if either
	// overruns, surface nothing (A2R-#11: the quota-hint file reads used to run
	// AFTER the select resolved, outside the deadline — now they are inside the
	// bounded goroutine so the ~300ms budget genuinely covers them).
	type result struct {
		ids    []string
		topCos float64
		mode   string
		cands  []usagelog.Cand // W9 R9.2b: scored candidates (cosine paths only)
		hint   string          // §6c RS1 quota+route hint ("" on any error / disabled)
	}
	ch := make(chan result, 1)
	go func() {
		ids, topCos, mode, cands := decide(in.Prompt, *k, *minCos, *minLen, sr, primaryMode, bm25)
		// Quota+route hint computed INSIDE the deadline-bounded goroutine:
		// ledger-direct, fail-open ("" on any error), zero policy content.
		var hint string
		if *quotaHintOn {
			hint = quotaHint(time.Now().UTC())
		}
		ch <- result{ids, topCos, mode, cands, hint}
	}()

	select {
	case r := <-ch:
		rec.Surfaced, rec.TopCosine, rec.Mode, rec.Cands = r.ids, r.topCos, r.mode, r.cands
		ctx := formatContext(byID, r.ids)
		if offloadNudge(in.Prompt) {
			rec.NudgeOffload = true
			ctx = appendNudge(ctx)
		}
		if r.hint != "" {
			rec.QuotaHint = true
			ctx = appendHint(ctx, r.hint)
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
