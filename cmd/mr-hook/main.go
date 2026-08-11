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
	"net/http"
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

// rerankOrEmbed makes the cross-encoder OPTIONAL at surfacing time.
//
// retrievers.EmbedRerank propagates every failure by design — correct for the
// eval, where a silent fallback would score embed-only under the
// "embed+rerank" label and poison the comparison. In production that same
// strictness is a regression: decide() turns a primary error into
// "embedder-down" and surfaces NOTHING, even though the embed ordering was
// available all along. A reranker outage should cost the ORDERING, never the
// surfacing.
//
// The fallback is recorded, not silent: mode() rewrites the logged mode to
// "embed" so usage.jsonl never claims a rerank that did not run.
// It embeds ONCE and then reorders that result in place. An earlier draft
// called EmbedRerank (which embeds internally) and fell back to a SECOND embed
// call — review proved that cannot fit the hard deadline: with the rerank call
// budgeted at deadline-50ms, embed + rerank + embed always overruns, so a SLOW
// reranker (the likely failure for a CPU-bound cross-encoder under contention)
// could only ever produce "deadline exceeded" and surface nothing. Reordering a
// list already in hand makes the degraded path free.
type rerankOrEmbed struct {
	embed      scoredRetriever
	reorderer  reorderer
	depth      int
	degraded   bool
	degradeErr string
}

// reorderer is the cross-encoder surface: reorder an existing candidate list.
type reorderer interface {
	Reorder(prompt string, cands []retrievers.Scored) ([]int, error)
}

const (
	// rerankMinTimeoutMs is the smallest hard deadline under which -ranker=rerank
	// is worth attempting. Measured on the live stack (CPU-bound cross-encoder,
	// --n-gpu-layers 0), degradation rate by deadline: 5000ms → 7/12 degraded,
	// 6000ms → 2/12, 8000ms → 0/12. Below 6000 the reranker is mostly not
	// running at all, so 8000 is the RECOMMENDED value and 6000 the floor.
	// A deadline it cannot meet is worse than not enabling it: the hook fails
	// open and surfaces nothing while looking perfectly healthy.
	rerankMinTimeoutMs = 6000
	// rerankSurfaceDepth is how many embed candidates the cross-encoder sees.
	// It must exceed the surfacing cut or reranking cannot rescue a burial.
	rerankSurfaceDepth = 20
	// rerankBudgetNum/Den give the rerank call a FRACTION of the deadline, so a
	// slow cross-encoder is abandoned with time left to surface the embed order.
	rerankBudgetNum, rerankBudgetDen = 3, 5
)

// rerankBudget is the slice of the deadline the cross-encoder may spend.
func rerankBudget(total time.Duration) time.Duration {
	return total * rerankBudgetNum / rerankBudgetDen
}

// liveEndpoint returns the first endpoint answering /v1/models, or "" if none
// do. This is the same probe-before-use discipline the embed path applies to
// unverified built-in ports — the reranker POSTs raw prompt text, so it must
// not talk to a port nobody confirmed.
func liveEndpoint(eps []string) string {
	c := &http.Client{Timeout: 300 * time.Millisecond}
	for _, ep := range eps {
		resp, err := c.Get(ep + "/v1/models")
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return ep
		}
	}
	return ""
}

func (r *rerankOrEmbed) RetrieveScored(prompt string, k int) ([]retrievers.Scored, float64, error) {
	// Retrieve the WIDE pool: the reranker's value is rescuing a skill the
	// embedder buried below the surfacing cut, which requires seeing past it.
	depth := r.depth
	if depth < k {
		depth = k
	}
	res, top, err := r.embed.RetrieveScored(prompt, depth)
	if err != nil {
		return nil, 0, err // a genuine embedder failure; decide() handles it
	}
	if len(res) == 0 {
		return res, top, nil
	}
	order, rerr := r.reorderer.Reorder(prompt, res)
	if rerr != nil {
		// The cross-encoder is optional: keep the embed ordering rather than
		// losing the surfacing, and record WHY so usage.jsonl can tell a
		// reranker outage from a healthy embed-only deployment.
		r.degraded = true
		r.degradeErr = rerr.Error()
	} else {
		reordered := make([]retrievers.Scored, len(order))
		for i, idx := range order {
			reordered[i] = res[idx] // ID + its own EMBED cosine, reranked position
		}
		res = reordered
	}
	if k < len(res) {
		res = res[:k]
	}
	return res, top, nil
}

// mode returns the mode actually achieved: the requested one, or "embed" when
// the cross-encoder failed and the embed ordering was used instead.
func (r *rerankOrEmbed) mode(requested string) string {
	if r.degraded {
		return "embed"
	}
	return requested
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
	// "rerank" qualifies alongside "embed": EmbedRerank.RetrieveScored returns
	// each candidate's own EMBED cosine in .Score (only the ORDER comes from the
	// cross-encoder), so these rows carry real cosines. Its raw logits are never
	// exposed here. Hybrid remains excluded — its .Score is an RRF fused rank
	// score, which is the contamination this field exists to prevent.
	var cands []usagelog.Cand
	if primaryMode == "embed" || primaryMode == "rerank" {
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
	ranker := flag.String("ranker", "embed", `primary ranking: "embed" (cosine-only), "rerank" (embed then bge-reranker-v2-m3 reorder; measured better on REAL mined prompts) or "hybrid" (BM25+embed RRF)`)
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
	var degradable *rerankOrEmbed // non-nil only in rerank mode
	switch *ranker {
	case "rerank":
		// W9: embed+rerank is the only retriever that BEATS embed-only on real
		// mined prompts (recall@3 0.276 vs 0.233; organic 0.340 vs 0.321). It
		// had been retired twice on the synthetic gold set, which inverts the
		// ordering because its prompts are description-derived.
		//
		// The reranker only REORDERS: RetrieveScored passes the embed max
		// cosine through as topCos, so the -min-cosine gate admits exactly the
		// same prompts it does under embed-only, and each candidate keeps its
		// own cosine so cands stays cosines-only.
		vecs := idx.Vectors()
		if len(skills) != len(vecs) {
			sr = failedRetriever{fmt.Errorf("index: %d skills but %d vectors", len(skills), len(vecs))}
			break
		}
		ids := make([]string, len(skills))
		for i, s := range skills {
			ids[i] = s.ID
		}
		// The reranker needs enough of the deadline to actually answer. Running
		// it under a deadline it cannot meet is worse than not running it: the
		// hard deadline fires, the hook fails open, and NOTHING surfaces — on
		// every prompt, looking exactly like a healthy hook. Refuse instead,
		// say so in the row, and serve embed.
		if *timeoutMs < rerankMinTimeoutMs {
			rec.Err = fmt.Sprintf("-ranker=rerank needs -timeout-ms >= %d (measured p95 ~2.3s end-to-end); got %d — running embed",
				rerankMinTimeoutMs, *timeoutMs)
			primaryMode = "embed"
			sr = retrievers.NewEmbedFromVectors(ids, vecs, *endpoint, embedTO)
			break
		}
		// Embed resolves an empty -endpoint internally (env / machine file /
		// failover chain) AND refuses to send prompt text to an unverified
		// built-in port until /v1/models confirms a model server is there.
		// EmbedRerank does neither — it appends "/v1/rerank" to whatever it is
		// handed. Handing it eps[0] blind would (a) post the raw prompt to a
		// port nobody probed, defeating that guard, and (b) silently degrade
		// forever on any host where the live endpoint is later in the chain.
		// So probe the chain and use the first endpoint that answers.
		rerankEP := liveEndpoint(retrievers.ResolveEndpoints(*endpoint))
		emb := retrievers.NewEmbedFromVectors(ids, vecs, *endpoint, embedTO)
		if rerankEP == "" {
			rec.Err = "-ranker=rerank: no endpoint answered /v1/models — running embed"
			primaryMode = "embed"
			sr = emb
			break
		}
		// The rerank call gets a SUB-budget so the embed leg and the surfacing
		// still fit inside the hard deadline even when the cross-encoder is
		// slow; overrunning it degrades to embed order, which is free.
		ro := &rerankOrEmbed{
			embed:     emb,
			reorderer: retrievers.NewEmbedRerank(emb, skills, rerankEP, rerankBudget(embedTO)),
			depth:     rerankSurfaceDepth,
		}
		degradable = ro
		sr = ro
	case "hybrid":
		hyb, herr := retrievers.NewHybridFromIndex(skills, idx.Vectors(), *endpoint, embedTO)
		if herr != nil {
			sr = failedRetriever{herr}
		} else {
			sr = hyb
		}
	default: // "embed" — primary ranking is embed-only cosine ordering
		// A typo must not silently coerce to embed. "mode":"embed" already has
		// two legitimate causes (configured embed, degraded rerank); a third
		// silent one — a misspelled flag — is exactly the "production was
		// running a configuration nobody believed" failure this system has
		// already paid for once. Still fail-open, but say so in the row.
		if *ranker != "" && *ranker != "embed" {
			rec.Err = fmt.Sprintf("unknown -ranker %q — running embed", *ranker)
		}
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
		// degradeErr carries a cross-encoder failure OUT of the goroutine so it
		// is stamped on EVERY outcome. Reading `degraded` only on the surfaced
		// path hid the outage on gated-empty / bm25-fallback / embedder-down
		// rows — roughly half of live traffic — leaving them byte-identical to
		// healthy ones. A subsystem silently not running, with a log that looks
		// correct, is the exact failure this system already paid weeks for.
		degradeErr string
	}
	ch := make(chan result, 1)
	go func() {
		ids, topCos, mode, cands := decide(in.Prompt, *k, *minCos, *minLen, sr, primaryMode, bm25)
		// If the cross-encoder failed and the embed ordering was used instead,
		// the row must say "embed" — never claim a rerank that did not run.
		// Only rewrite the ranker modes; decide's own outcomes (gated-empty,
		// too-short, bm25-fallback, embedder-down) already describe themselves.
		var degradeErr string
		if degradable != nil {
			if mode == primaryMode {
				mode = degradable.mode(primaryMode)
			}
			// Carried out on EVERY outcome, not just the surfaced one — a
			// gated-empty row must still show that the reranker was down.
			degradeErr = degradable.degradeErr
		}
		// Quota+route hint computed INSIDE the deadline-bounded goroutine:
		// ledger-direct, fail-open ("" on any error), zero policy content.
		var hint string
		if *quotaHintOn {
			hint = quotaHint(time.Now().UTC())
		}
		ch <- result{ids, topCos, mode, cands, hint, degradeErr}
	}()

	select {
	case r := <-ch:
		rec.Surfaced, rec.TopCosine, rec.Mode, rec.Cands = r.ids, r.topCos, r.mode, r.cands
		if r.degradeErr != "" && rec.Err == "" {
			rec.Err = "ranker degraded to embed: " + r.degradeErr
		}
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
