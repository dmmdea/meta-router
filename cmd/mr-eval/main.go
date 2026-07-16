// Command mr-eval evaluates skill retrievers against a labeled gold-set.
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/eval"
	"github.com/dmmdea/meta-router/internal/goldset"
	"github.com/dmmdea/meta-router/internal/retrievers"
	"github.com/dmmdea/meta-router/internal/roots"
)

const version = "0.1.0"

func main() {
	skillRoots := flag.String("skill-roots", "", "comma-separated skill root dirs (default: discovered ~/.claude/skills + installed plugin packs)")
	goldsetPath := flag.String("goldset", "testdata/goldset.jsonl", "path to gold-set JSONL")
	endpoint := flag.String("endpoint", "", "embedder endpoint; empty = resolve for this machine (see retrievers.ResolveEndpoints)")
	flag.Parse()

	var rootSet []catalog.Root
	if *skillRoots != "" {
		for _, p := range strings.Split(*skillRoots, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			rootSet = append(rootSet, catalog.Root{Path: p, Pack: filepath.Base(filepath.Clean(p))})
		}
	} else {
		// Same corpus the production hook indexes: user skills + plugin packs.
		// Read-only discovery — mr-eval never writes roots.json.
		claudeDir, herr := roots.DefaultClaudeDir()
		if herr != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve home dir: %v\n", herr)
			os.Exit(1)
		}
		rootSet = roots.Discover(claudeDir)
	}
	if len(rootSet) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: no skill roots")
		os.Exit(1)
	}

	// Harvest
	raw, err := catalog.HarvestRoots(rootSet)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest error: %v\n", err)
		os.Exit(1)
	}
	rawCount := len(raw)

	// Hygiene pipeline: description-twin collapse + ID dedup (keep first)
	skills := catalog.Dedup(raw)
	dedupCount := len(skills)

	fmt.Printf("Catalog: %d raw → %d unique skills (deduped %d)\n", rawCount, dedupCount, rawCount-dedupCount)

	if dedupCount == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: catalog is empty — check skill roots")
		os.Exit(1)
	}

	// Load gold-set
	cases, err := goldset.Load(*goldsetPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goldset load error: %v\n", err)
		os.Exit(1)
	}
	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: gold-set is empty")
		os.Exit(1)
	}

	// Coverage: a case is only winnable if at least one expected skill is
	// actually installed. Reporting recall over uncovered cases (e.g. a gold
	// set written when GSD was installed) would punish every retriever
	// equally and hide real ranking regressions — so both views are printed.
	idSet := make(map[string]bool, len(skills))
	for _, s := range skills {
		idSet[s.ID] = true
	}
	var covered []goldset.Case
	for _, c := range cases {
		for _, e := range c.Expect {
			if idSet[e] {
				covered = append(covered, c)
				break
			}
		}
	}
	fmt.Printf("Gold-set: %d cases (%d covered by installed skills, %d dead)\n\n",
		len(cases), len(covered), len(cases)-len(covered))

	// Check embedder availability. Probe the RESOLVED candidates, not the raw
	// spec: the spec is empty by default (it resolves per-machine), and probing
	// "" would report the embedder down and silently score BM25 only — exactly
	// the silent-degradation this design exists to prevent.
	eps := retrievers.ResolveEndpoints(*endpoint)
	embedUp := false
	for _, ep := range eps {
		if isEndpointUp(ep) {
			embedUp = true
			break
		}
	}
	if !embedUp {
		fmt.Printf("WARNING: no embedder reachable at %v — skipping Embed and Hybrid\n\n", eps)
	}

	// Build retrievers
	var rlist []eval.Retriever

	bm25 := retrievers.NewBM25(skills)
	rlist = append(rlist, bm25)

	if embedUp {
		emb, err := retrievers.NewEmbed(skills, *endpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: embed init failed: %v — skipping\n", err)
		} else {
			rlist = append(rlist, emb)
		}

		hyb, err := retrievers.NewHybrid(skills, *endpoint)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: hybrid init failed: %v — skipping\n", err)
		} else {
			rlist = append(rlist, hyb)
		}
	}

	// Evaluate: full set + covered-only subset.
	ks := []int{1, 3, 5}
	var resultsAll, resultsCov []eval.Metrics
	for _, r := range rlist {
		fmt.Printf("Scoring %s ...\n", r.Name())
		m, err := eval.Score(r, cases, ks)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR scoring %s: %v\n", r.Name(), err)
			os.Exit(1)
		}
		resultsAll = append(resultsAll, m)
		mc, err := eval.Score(r, covered, ks)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR scoring %s (covered): %v\n", r.Name(), err)
			os.Exit(1)
		}
		resultsCov = append(resultsCov, mc)
	}

	printTable(fmt.Sprintf("All %d cases", len(cases)), resultsAll)
	printTable(fmt.Sprintf("Covered-only (%d cases with an installed expected skill)", len(covered)), resultsCov)
}

func printTable(title string, results []eval.Metrics) {
	fmt.Println()
	fmt.Println(title + ":")
	fmt.Printf("| retriever     | recall@1 | recall@3 | recall@5 |  MRR  | median_ms |\n")
	fmt.Printf("|---------------|----------|----------|----------|-------|-----------|\n")
	for _, m := range results {
		fmt.Printf("| %-13s | %8.3f | %8.3f | %8.3f | %.3f | %9.1f |\n",
			m.Retriever,
			m.RecallAt[1],
			m.RecallAt[3],
			m.RecallAt[5],
			m.MRR,
			m.MedianLatencyMs,
		)
	}
	fmt.Println()
}

// isEndpointUp checks if the embedder endpoint responds within 3 s.
func isEndpointUp(endpoint string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(endpoint + "/v1/models")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
