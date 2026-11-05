// Command mr-hook is the UserPromptSubmit hook: it loads the skill index,
// surfaces the few most relevant installed skills for the prompt, and injects
// them as additionalContext. It is strictly additive — on ANY error or timeout
// it injects nothing and exits 0, never blocking or breaking a prompt.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/embedtpl"
	"github.com/dmmdea/meta-router/internal/index"
	"github.com/dmmdea/meta-router/internal/retrievers"
	"github.com/dmmdea/meta-router/internal/usagelog"
)

type hookInput struct {
	Prompt string `json:"prompt"`
	// Claude Code supplies these on the UserPromptSubmit payload (prompt_id
	// since v2.1.196). Logging them turns a session-blind timestamp join into
	// an exact composite key — see usagelog.Record. Opaque ids, not content.
	SessionID string `json:"session_id"`
	PromptID  string `json:"prompt_id"`
	// Cwd is the session's working directory, where Claude Code reads the project
	// (.claude/settings.json) and local (.claude/settings.local.json) settings scopes.
	Cwd string `json:"cwd"`
	// TranscriptPath lets the quota banner show once at session start (isFirstPrompt).
	TranscriptPath string `json:"transcript_path"`
}

type scoredRetriever interface {
	RetrieveScored(prompt string, k int) ([]retrievers.Scored, float64, error)
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
// decide uniformly takes its embedder-down branch (no untested side path in main).
type failedRetriever struct{ err error }

func (f failedRetriever) RetrieveScored(string, int) ([]retrievers.Scored, float64, error) {
	return nil, 0, f.err
}

// logDepth is how many scored candidates each cosine-path event records
// (W9 R9.2b). Deeper than the surface k so the curve analysis sees the
// invoked skill's own score even when it ranked below the surfacing cut;
// bounded so rows stay small.
const logDepth = 8

// decide is the pure surfacing decision: rank with the primary retriever
// (embed-only by default; hybrid RRF behind -ranker=hybrid), keep only the
// candidates the model can invoke, and gate on cosine. A prompt shorter than
// minLen (mode "too-short") or longer than maxLen bytes when maxLen > 0 (mode
// "too-long") is never embedded. A cold embedder yields "cold-skip-empty" /
// "cold-stuck-empty" and any other primary error "embedder-down": there is no
// lexical fallback, because the 206 lexical hints it produced in production
// were followed 1.5% of the time.
//
// Eligibility is applied BEFORE the cut. Candidates the model cannot invoke used
// to take the k slots and carry the gate, and were dropped afterwards: over 30
// days one took a slot on 600 of 883 gate-passing prompts (68%), and on 157 of
// them (17.8%) all three, so nothing was shown. On the cosine modes (embed, rerank) a
// candidate is shown only if it is eligible AND its own cosine is at least
// minCos, in rank order, at most k; none shown is "gated-empty". On hybrid the
// scores are not cosines, so the gate stays on topCos and only the
// eligible-first cut applies. hidden lists the retrieved candidates that clear
// the cosine floor, would have taken a slot and are not eligible.
//
// Returns the ids to surface, the hidden ids, the top cosine (0 when not
// applicable), the mode for logging, the scored candidate list (W9 R9.2b) --
// populated on BOTH surfaced and gated-empty cosine paths, nil on modes where
// no cosine ran so a lexical score can never recontaminate the cosine
// denominator -- and the primary retriever's error string ("" when it didn't
// fail), so a dim mismatch, an index-recorded model the endpoint doesn't serve
// and a genuinely down embedder stay distinguishable in the log.
//
// Retrieval depth is max(k, logDepth) and cands and topCos are logged as
// retrieved. A nil eligible keeps every candidate.
func decide(prompt string, k int, minCos float64, minLen, maxLen int, primary scoredRetriever, primaryMode string, eligible func(id string) bool) (ids, hidden []string, topCos float64, mode string, cands []usagelog.Cand, primaryErr string) {
	if len(strings.TrimSpace(prompt)) < minLen {
		return nil, nil, 0, "too-short", nil, ""
	}
	if maxLen > 0 && len(prompt) > maxLen {
		return nil, nil, 0, "too-long", nil, ""
	}
	res, topCos, err := primary.RetrieveScored(prompt, max(k, logDepth))
	if err != nil {
		// F42: a cold embedder is not a dead one; the row says what happened, because
		// logging it as embedder-down would read as an outage on every idle spell.
		if errors.Is(err, errColdEmbedder) {
			kind := "cold-skip"
			if errors.Is(err, errColdStuck) {
				kind = "cold-stuck" // the same skip, but too many in a row to be a load in progress
			}
			return nil, nil, 0, kind + "-empty", nil, err.Error()
		}
		return nil, nil, 0, "embedder-down", nil, err.Error()
	}
	// cands carries COSINES OR NOTHING. Hybrid's RetrieveScored returns RRF
	// fused scores in .Score (~0.03 scale, 1/(60+rank) sums) -- real cosines
	// exist only in its topCos return. Logging those under "cos" would put
	// 0.03-scale numbers into the cosine denominator with no discriminator on
	// gated-empty rows (review 2026-07-30, MAJOR). So candidates are logged on
	// the embed path only; hybrid rows carry none. "rerank" qualifies alongside
	// "embed": EmbedRerank.RetrieveScored returns each candidate's own EMBED
	// cosine in .Score (only the ORDER comes from the cross-encoder).
	cosine := primaryMode == "embed" || primaryMode == "rerank"
	if cosine {
		cands = make([]usagelog.Cand, len(res))
		for i, s := range res {
			cands[i] = usagelog.Cand{ID: s.ID, Cos: s.Score}
		}
	}
	if topCos < minCos {
		return nil, nil, topCos, "gated-empty", cands, ""
	}
	for _, s := range res {
		if len(ids) >= k {
			break
		}
		if cosine && s.Score < minCos {
			continue // a rerank order is not cosine order: a later candidate may still clear
		}
		if eligible != nil && !eligible(s.ID) {
			hidden = append(hidden, s.ID)
			continue
		}
		ids = append(ids, s.ID)
	}
	if len(ids) == 0 {
		return nil, hidden, topCos, "gated-empty", cands, ""
	}
	return ids, hidden, topCos, primaryMode, cands, ""
}

func formatContext(byID map[string]catalog.Skill, ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("meta-router — relevant installed skills for this task:\n")
	wrote := 0
	wroteAgent := false
	wroteTool := false
	for _, id := range ids {
		s, ok := byID[id]
		if !ok {
			continue
		}
		desc := s.Description
		// Cut on a rune boundary: a byte cut split multi-byte characters into invalid UTF-8.
		if r := []rune(desc); len(r) > 140 {
			desc = string(r[:140]) + "…"
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
		} else if name, isTool := strings.CutPrefix(s.ID, "tool:"); isTool {
			// W10 tool-pointer entries: the pointer's own name is surfaced;
			// the entry's description names the actual tool and route. The
			// (local tool) label plus the trailer below tell the model this
			// is NOT a Skill-tool invocable.
			fmt.Fprintf(&b, "- %s (local tool): %s\n", name, desc)
			wroteTool = true
		} else {
			fmt.Fprintf(&b, "- %s (%s): %s\n", s.ID, s.Source, desc)
		}
		wrote++
	}
	if wrote == 0 {
		return "" // all ids missing from the index → surface nothing
	}
	// The trailer stays byte-identical to the pre-flat-roots output whenever no
	// agent or tool entry surfaced — which is every production prompt until a
	// roots.json entry enables a flat root behind its measured gate — so this
	// change is behavior-inert today by construction.
	b.WriteString("Invoke one via the Skill tool or its slash command if it fits.")
	if wroteAgent {
		b.WriteString(" Entries marked (agent) are dispatched via the Agent tool (subagent_type = the listed name), never the Skill tool.")
	}
	if wroteTool {
		b.WriteString(" Entries marked (local tool) are MCP tools, not skills: load the tool the entry's text names via ToolSearch and follow the steering rule it cites before calling.")
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
	coldSkip := flag.Bool("cold-skip", true, "ask the model router whether the embedder is loaded before embedding; when it is not, surface nothing at once and start the load with one warm-up request instead of waiting the embed budget out (F42). pass -cold-skip=false (with the =; Go reads '-cold-skip false' as true plus a stray argument) to restore the old always-embed path")
	maxPromptLen := flag.Int("max-prompt-len", 0, "prompts longer than this many bytes are logged too-long and not embedded; 0 = off. Also settable per machine in ~/.meta-router/hook.json")
	holdoutPct := flag.Int("holdout-pct", 0, "percent (0..100) of sessions, by session id, that are logged but never shown the skill list; 0 = off. Also settable per machine in ~/.meta-router/hook.json")
	ranker := flag.String("ranker", "embed", `primary ranking: "embed" (cosine-only), "rerank" (embed then bge-reranker-v2-m3 reorder; measured better on REAL mined prompts) or "hybrid" (BM25+embed RRF)`)
	quotaHintOn := flag.Bool("quota-hint", true, "append the mr-orchestrate quota+route hint (ledger-direct, fail-open, zero policy)")
	showVersion := flag.Bool("version", false, "print this binary's build revision and exit (deployed-fleet freshness — a deployed hook that cannot be asked what it is stayed 3 releases stale)")
	// Flag parsing fails open. The default flag.Parse exits 2 on a flag it does not
	// know, and exit 2 on UserPromptSubmit blocks the prompt: a machine whose
	// settings.json named a newer flag than its binary knows would lose every prompt.
	// Parsing stops at the first bad flag; what was parsed before it stands.
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	flag.CommandLine.SetOutput(io.Discard)
	flagErr := flag.CommandLine.Parse(os.Args[1:])
	if errors.Is(flagErr, flag.ErrHelp) {
		// -h asked for the flag list. Parsing discards its own output, so print it here.
		fmt.Fprintln(os.Stderr, "Usage of mr-hook:")
		flag.CommandLine.SetOutput(os.Stderr)
		flag.CommandLine.PrintDefaults()
		return
	}
	if *showVersion {
		fmt.Println("mr-hook", buildRevision())
		return
	}

	// Always exit 0 — fail-open is absolute.
	defer os.Exit(0)

	start := time.Now()
	rec := usagelog.Record{TsUnix: start.Unix(), Mode: "error"}
	if flagErr != nil {
		rec.Err = joinErr(rec.Err, "flags: "+flagErr.Error())
	}
	// Per-machine knobs: a key applies only when its flag was not given on the command line.
	given := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })
	hc, hcErr := readHookConfig(hookConfigPath(usageLogPath(*logPath)))
	if !given["max-prompt-len"] {
		*maxPromptLen = hc.MaxPromptLen
	}
	if !given["holdout-pct"] {
		*holdoutPct = hc.HoldoutPct
	}
	if hcErr != nil {
		rec.Err = joinErr(rec.Err, "hook.json: "+hcErr.Error())
	}
	logIt := func() {
		rec.LatencyMs = time.Since(start).Milliseconds()
		if lp := usageLogPath(*logPath); lp != "" {
			_ = usagelog.Append(lp, rec) // log failure must not break the hook
		}
	}
	defer logIt()

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		rec.Err = joinErr(rec.Err, "stdin: "+err.Error())
		return
	}
	var in hookInput
	// Gate on the PROMPT, not on the decode error. session_id/prompt_id are
	// optional metadata and must never be able to veto a valid prompt: they are
	// typed `string`, so a payload emitting either as a number or object yields
	// an UnmarshalTypeError even though Prompt decoded fine — and treating that
	// as fatal would surface NOTHING on every prompt while logging the
	// misleading reason "no prompt". Before these fields existed they were
	// unknown keys and were skipped untyped, so making them fatal would be a
	// regression introduced by the very change that adds them. Totally
	// malformed JSON still leaves Prompt empty and is still caught below.
	_ = json.Unmarshal(raw, &in)
	if strings.TrimSpace(in.Prompt) == "" {
		rec.Err = joinErr(rec.Err, "no prompt")
		return
	}
	rec.PromptHash = usagelog.HashPrompt(in.Prompt)
	rec.PromptLen = len(in.Prompt)
	rec.SessionID, rec.PromptID = in.SessionID, in.PromptID
	// A background-task notification is not a request, so no skill retrieval runs for it.
	// Only the retrieval is skipped: the index load, the identity check and the quota banner
	// below behave exactly as on any other prompt.
	machine := isMachineTurn(in.Prompt)

	ip := *indexPath
	if ip == "" {
		if p, err := index.DefaultIndexPath(); err == nil {
			ip = p
		}
	}
	idx, err := index.LoadFast(ip) // index.bin sidecar when fresh, else JSON
	if err != nil {
		rec.Err = joinErr(rec.Err, "load index: "+err.Error())
		return // no index yet → surface nothing (run mr-index build)
	}
	skills := idx.Skills()
	byID := make(map[string]catalog.Skill, len(skills))
	for _, s := range skills {
		byID[s.ID] = s
	}

	// W9-P item 1: the index identity names the model AND template version its
	// vectors were embedded under; the query must be embedded identically. An
	// identity this binary cannot resolve (a template it does not know, or a
	// templated identity whose TplGuard tripwire says the vectors were last
	// written raw by a pre-template binary) is a REFUSAL: embedding the query
	// raw against templated vectors — or under a guessed template — is a
	// silent vector-space mix that ranks garbage while looking healthy. Fail
	// open: no retrieval at all (per the W9-P
	// spec the mismatch surfaces nothing but the quota hint), exit 0, and the
	// row says exactly why.
	spec, tplErr := idx.SpecForIndex()
	if tplErr != nil {
		rec.Err = joinErr(rec.Err, "index identity: "+tplErr.Error())
	}

	// Per-prompt embedder timeout is a fraction of the hard deadline; with the cold skip on,
	// the router probe's ceiling is carved out of it so probe + embed still fit.
	embedTO, probeTO, warmTO := hookBudgets(*timeoutMs, *coldSkip)
	primaryMode := *ranker
	var sr scoredRetriever
	var degradable *rerankOrEmbed // non-nil only in rerank mode
	// On a template mismatch no retriever is built at all — the goroutine
	// below skips decide() and computes only the quota hint.
	if tplErr == nil {
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
				rec.Err = joinErr(rec.Err, fmt.Sprintf("-ranker=rerank needs -timeout-ms >= %d (measured p95 ~2.3s end-to-end); got %d — running embed",
					rerankMinTimeoutMs, *timeoutMs))
				primaryMode = "embed"
				sr = retrievers.NewEmbedFromVectors(ids, vecs, *endpoint, embedTO, spec)
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
			emb := retrievers.NewEmbedFromVectors(ids, vecs, *endpoint, embedTO, spec)
			if rerankEP == "" {
				rec.Err = joinErr(rec.Err, "-ranker=rerank: no endpoint answered /v1/models — running embed")
				primaryMode = "embed"
				sr = emb
				break
			}
			// The rerank call gets a SUB-budget so the embed leg and the surfacing
			// still fit inside the hard deadline even when the cross-encoder is
			// slow; overrunning it degrades to embed order, which is free.
			ro := &rerankOrEmbed{
				embed:     emb,
				reorderer: retrievers.NewEmbedRerank(emb, skills, rerankEP, rerankBudget(embedTO), embedtpl.RerankFor(retrievers.DefaultRerankModel)),
				depth:     rerankSurfaceDepth,
			}
			degradable = ro
			sr = ro
		case "hybrid":
			hyb, herr := retrievers.NewHybridFromIndex(skills, idx.Vectors(), *endpoint, embedTO, spec)
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
				rec.Err = joinErr(rec.Err, fmt.Sprintf("unknown -ranker %q — running embed", *ranker))
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
				sr = retrievers.NewEmbedFromVectors(ids, vecs, *endpoint, embedTO, spec)
			}
		}
		// F42: every ranker above needs the embedder, so one wrapper covers all of them. A
		// ranker that never got built (failedRetriever) has nothing to wait on or warm up.
		if _, failed := sr.(failedRetriever); *coldSkip && !failed {
			sr = newColdSkip(sr, *endpoint, spec.Model, probeTO, warmTO, usageLogPath(*logPath))
		}
	}

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
		// hidden are the retrieved candidates the model cannot invoke (see decide). The
		// visibility settings are read INSIDE the deadline-bounded goroutine: a slow or
		// locked settings file must degrade to "inject nothing" like every other
		// per-prompt read, never block the hook past its deadline (review of PR #70).
		hidden []string
		// degradeErr carries a cross-encoder failure OUT of the goroutine so it
		// is stamped on EVERY outcome. Reading `degraded` only on the surfaced
		// path hid the outage on gated-empty / cold-skip-empty / embedder-down
		// rows — roughly half of live traffic — leaving them byte-identical to
		// healthy ones. A subsystem silently not running, with a log that looks
		// correct, is the exact failure this system already paid weeks for.
		degradeErr string
		// primaryErr carries the primary retriever's failure the same way:
		// without it, "embedder-down" rows logged an empty Err and a
		// per-index model misconfiguration was indistinguishable from a dead
		// endpoint.
		primaryErr string
	}
	ch := make(chan result, 1)
	go func() {
		var ids, hidden []string
		var topCos float64
		var cands []usagelog.Cand
		var degradeErr, primaryErr string
		// The mismatch refusal surfaces NOTHING, but the quota hint below
		// still runs: it reads ledgers, not vectors, and is the one thing
		// the W9-P spec says survives.
		mode := "tpl-mismatch"
		if tplErr == nil && machine {
			mode = "machine-turn" // nothing is ranked, so the notification never reaches the embedder
		} else if tplErr == nil {
			// Eligibility is decided BEFORE the cut, so a candidate the model cannot invoke
			// takes no slot and carries no gate. The settings are read once here; the
			// per-candidate SKILL.md read happens lazily, only for candidates decide reaches.
			eligible := loadSkillVisibility(claudeConfigDir(), in.Cwd).eligibleFunc(byID)
			ids, hidden, topCos, mode, cands, primaryErr = decide(in.Prompt, *k, *minCos, *minLen, *maxPromptLen, sr, primaryMode, eligible)
			// If the cross-encoder failed and the embed ordering was used
			// instead, the row must say "embed" — never claim a rerank that
			// did not run. Only rewrite the ranker modes; decide's own
			// outcomes (gated-empty, too-short, too-long,
			// embedder-down) already describe themselves.
			if degradable != nil {
				if mode == primaryMode {
					mode = degradable.mode(primaryMode)
				}
				// Carried out on EVERY outcome, not just the surfaced one —
				// a gated-empty row must still show the reranker was down.
				degradeErr = degradable.degradeErr
			}
		}
		// Quota+route hint computed INSIDE the deadline-bounded goroutine:
		// ledger-direct, fail-open ("" on any error), zero policy content.
		var hint string
		if *quotaHintOn {
			now := time.Now().UTC()
			// Report deltas, never ticks: identical quota content on every prompt costs
			// context each turn and changes no decision. Render when the ledger reports a
			// real change (see quotaHintWithFreshness) or on the session's first prompt.
			if h, fresh := quotaHintWithFreshness(now); h != "" && (fresh || isFirstPrompt(in.TranscriptPath)) {
				hint = h
			}
			if p := delegateProposal(now, in.SessionID); p != "" {
				hint = appendHint(hint, p)
			}
		}
		ch <- result{ids, topCos, mode, cands, hint, hidden, degradeErr, primaryErr}
	}()

	select {
	case r := <-ch:
		// decide already dropped what the model cannot invoke; those ids are kept in Hidden,
		// so the outcome join sees only what was shown.
		rec.Hidden = r.hidden
		rec.TopCosine, rec.Mode, rec.Cands = r.topCos, r.mode, r.cands
		// A holdout session gets everything computed and logged the same, but no skill list:
		// what would have been shown goes to WouldSurface and Surfaced stays empty. Hints
		// stay in context for the rest of a session, so the arm is chosen per session, not
		// per prompt. The offload nudge and the quota hint below are not part of the arm.
		shown := r.ids
		if *holdoutPct > 0 {
			rec.Arm = "treat"
			if holdoutArm(in.SessionID, *holdoutPct) {
				rec.Arm, rec.WouldSurface, shown = "holdout", r.ids, nil
			}
		}
		rec.Surfaced = shown
		// One Err slot, causes JOINED — never dropped on conflict. A
		// first-cause-wins chain here let three non-fatal -ranker notices
		// (sub-floor timeout, no live rerank endpoint, unknown ranker) mask
		// the primary retriever's failure on EVERY prompt of such a host —
		// permanently hiding the one message that says "rebuild the index or
		// pin the right endpoint" (review 2026-08-16 round 2, the same
		// attribution-loss class appendDeadline fixed one branch down).
		if r.primaryErr != "" {
			rec.Err = joinErr(rec.Err, "primary retriever: "+r.primaryErr)
		}
		if r.degradeErr != "" {
			rec.Err = joinErr(rec.Err, "ranker degraded to embed: "+r.degradeErr)
		}
		ctx := formatContext(byID, shown)
		// The nudge is prompt-shaped, not index-shaped — but the tpl-mismatch
		// contract is "nothing but the quota hint", and appendNudge("") would
		// happily emit the bare nudge on a refusal (review 2026-08-16).
		if tplErr == nil && offloadNudge(in.Prompt) {
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
		// Never CLOBBER an earlier attribution: on a tpl-mismatch whose
		// quota-hint ledger read overran the deadline, overwriting Err erased
		// the only durable record that the index needs a rebuild — every such
		// row read as a perf problem (review 2026-08-16).
		stampDeadline(&rec)
		// surface nothing
	}
}

// usageLogPath resolves the usage log: the -log flag, else the default under the home
// directory, else "" (no log). The row writer and the cold-run reader must agree on it.
func usageLogPath(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if p, err := usagelog.DefaultLogPath(); err == nil {
		return p
	}
	return ""
}

// stampDeadline marks a record as deadline-killed, folding the overrun into
// any existing attribution. Extracted so the clobber-vs-fold semantics are
// unit-testable at the record level (the select branch itself cannot be
// deterministically forced in an e2e).
func stampDeadline(rec *usagelog.Record) {
	rec.Mode = "error"
	rec.Err = joinErr(rec.Err, "deadline exceeded")
}

// joinErr folds a new cause into an existing error attribution instead of
// replacing or dropping either — the single Err slot carries every cause.
func joinErr(prev, add string) string {
	if prev == "" {
		return add
	}
	return prev + "; " + add
}
