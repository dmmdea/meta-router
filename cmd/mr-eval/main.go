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
)

const version = "0.1.0"

func main() {
	skillRoots := flag.String("skill-roots", "", "comma-separated skill root dirs (default ~/.claude/skills)")
	goldsetPath := flag.String("goldset", "testdata/goldset.jsonl", "path to gold-set JSONL")
	endpoint := flag.String("endpoint", "http://127.0.0.1:11436", "embedder endpoint")
	flag.Parse()

	skillRootsValue := *skillRoots
	if skillRootsValue == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			fmt.Fprintf(os.Stderr, "cannot resolve home dir: %v\n", herr)
			os.Exit(1)
		}
		skillRootsValue = filepath.Join(home, ".claude", "skills")
	}

	roots := strings.Split(skillRootsValue, ",")
	for i, r := range roots {
		roots[i] = strings.TrimSpace(r)
	}

	// Harvest
	raw, err := catalog.Harvest(roots)
	if err != nil {
		fmt.Fprintf(os.Stderr, "harvest error: %v\n", err)
		os.Exit(1)
	}
	rawCount := len(raw)

	// Dedup by ID (keep first)
	skills := catalog.DedupByID(raw)
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
	fmt.Printf("Gold-set: %d cases\n\n", len(cases))

	if len(cases) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: gold-set is empty")
		os.Exit(1)
	}

	// Check embedder availability
	embedUp := isEndpointUp(*endpoint)
	if !embedUp {
		fmt.Printf("WARNING: embedder at %s is unreachable — skipping Embed and Hybrid\n\n", *endpoint)
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

	// Evaluate
	ks := []int{1, 3, 5}
	var results []eval.Metrics
	for _, r := range rlist {
		fmt.Printf("Scoring %s ...\n", r.Name())
		m, err := eval.Score(r, cases, ks)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR scoring %s: %v\n", r.Name(), err)
			os.Exit(1)
		}
		results = append(results, m)
	}

	// Print markdown metrics table
	fmt.Println()
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
