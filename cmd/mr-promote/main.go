// Command mr-promote is the V7 promotion-gate CLI: it reads paired
// template-vs-solo run records (JSONL of promotion.PairedRun) and prints a
// verdict per template under the Q5 conjunctive rule. With no qualifying
// pairs it refuses honestly — templates remain manual --strategy seams until
// they EARN defaults here.
//
//	mr-promote -pairs eval/template-pairs.jsonl
//
// Exit codes: 0 = at least one template promoted · 1 = ran clean, nothing
// promoted (the correct default) · 2 = usage/IO error (including a truncated
// scan — a partial verdict set must never present as complete).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"

	"github.com/dmmdea/meta-router/internal/promotion"
)

const version = "0.2.0"

func main() {
	pairsPath := flag.String("pairs", "", "paired-run JSONL (required): one promotion.PairedRun per line")
	iters := flag.Int("iters", 4000, "bootstrap/permutation iterations (MC path only; n≤20 uses exact enumeration)")
	seed := flag.Int64("seed", 42, "RNG seed (MC path; verdicts are deterministic)")
	flag.Parse()
	if *pairsPath == "" || *iters < 1 {
		fmt.Fprintln(os.Stderr, "usage: mr-promote -pairs <pairs.jsonl> [-iters N>=1] [-seed S]")
		os.Exit(2)
	}

	f, err := os.Open(*pairsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pairs: %v\n", err)
		os.Exit(2)
	}
	defer f.Close()

	byTemplate := map[string][]promotion.PairedRun{}
	skipped := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r promotion.PairedRun
		if json.Unmarshal(line, &r) != nil || r.Template == "" {
			skipped++ // counted and reported — never silently dropped (unparseable OR missing template)
			continue
		}
		byTemplate[r.Template] = append(byTemplate[r.Template], r)
	}
	if err := sc.Err(); err != nil {
		// A scan error (e.g. an oversized line) truncates the input: later
		// templates would silently vanish from judgment. Hard error.
		fmt.Fprintf(os.Stderr, "pairs scan aborted: %v — verdicts would be incomplete\n", err)
		os.Exit(2)
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d line(s) skipped (unparseable or missing template)\n", skipped)
	}

	names := make([]string, 0, len(byTemplate))
	for n := range byTemplate {
		names = append(names, n)
	}
	sort.Strings(names)

	verdicts := make([]promotion.Verdict, 0, len(names))
	promoted := false
	for _, n := range names {
		v := promotion.Gate(n, byTemplate[n], *iters, *seed)
		promoted = promoted || v.Promote
		verdicts = append(verdicts, v)
	}
	out := struct {
		Rule     string              `json:"rule"`
		Skipped  int                 `json:"skipped_lines"`
		Verdicts []promotion.Verdict `json:"verdicts"`
	}{"Q5 conjunctive: paired BCa CI floor > 0 AND sign-flip p < .05 at equal token budget (exact p at n≤20)", skipped, verdicts}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(2)
	}
	if !promoted {
		if len(verdicts) == 0 {
			fmt.Fprintln(os.Stderr, "no templates found in pairs file — nothing promoted (correct default)")
		}
		os.Exit(1)
	}
}
