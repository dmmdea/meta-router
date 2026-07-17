// Command mr-verifier measures the LOCAL verifier's ceiling (CC1): it runs a
// labeled good/bad snippet corpus through the offload cascade (triage door),
// reads the free logprob decision margin as graded confidence, and reports
// decisive accuracy, coverage, the defer-discounted effective ceiling, and the
// selective-risk curves AURC + AUGRC (decision record §Q9). Fail-open: a missing
// offload binary prints a WARNING and exits 0 (mirrors mr-eval's embedder-down).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/locallane"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	vp "github.com/dmmdea/meta-router/internal/orch/strategy/verifierpilot"
	"github.com/dmmdea/meta-router/internal/verifierceiling"
)

const version = "0.1.0"

// defaultQuestion is the fixed verification gate applied to every snippet so the
// ceiling reflects one consistent decision, not per-snippet prompt variance.
const defaultQuestion = "Is this code correct — free of bugs and doing what its name/signature implies? Answer yes only if you are confident it is correct."

// offloadOut is the subset of offload-harness core.Result --json we read.
type offloadOut struct {
	OK       bool            `json:"ok"`
	Deferred bool            `json:"deferred"`
	Reason   string          `json:"reason"`
	Result   json.RawMessage `json:"result"`
	Meta     struct {
		Margin float64 `json:"margin"`
		Model  string  `json:"model"`
	} `json:"meta"`
}

// parseVerdict maps one offload --json blob to (verdict, confidence, model).
// yes→pass, no→fail, unsure/deferred→defer, ok:false/unparseable→error.
// Confidence is meta.margin; a non-answer carries 0 (it sorts to the bottom of
// coverage — a defer never passes and never counts as agreement, per Q9).
func parseVerdict(raw []byte) (vp.Verdict, float64, string) {
	var o offloadOut
	if json.Unmarshal(raw, &o) != nil {
		return vp.VerdictErrored, 0, ""
	}
	if o.Deferred {
		return vp.VerdictDefer, 0, o.Meta.Model
	}
	if !o.OK {
		return vp.VerdictErrored, 0, o.Meta.Model
	}
	var d struct {
		Decision string `json:"decision"`
	}
	_ = json.Unmarshal(o.Result, &d)
	switch strings.ToLower(strings.TrimSpace(d.Decision)) {
	case "yes":
		return vp.VerdictPass, o.Meta.Margin, o.Meta.Model
	case "no":
		return vp.VerdictFail, o.Meta.Margin, o.Meta.Model
	case "unsure", "":
		return vp.VerdictDefer, 0, o.Meta.Model
	default:
		return vp.VerdictErrored, 0, o.Meta.Model
	}
}

func main() {
	corpus := flag.String("corpus", "testdata/verifier-snippets.jsonl", "labeled snippet corpus JSONL")
	outPath := flag.String("out", "testdata/verifier-seed.jsonl", "where to write the produced Record seed JSONL")
	question := flag.String("question", defaultQuestion, "the verification gate applied to every snippet")
	binFlag := flag.String("bin", "", "offload-harness binary path; empty = resolve from orchestrator config (statepaths.Config)")
	timeoutSec := flag.Int("timeout", 60, "per-snippet local-critic timeout (seconds)")
	flag.Parse()

	snips, err := vp.LoadSnippets(*corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpus load error: %v\n", err)
		os.Exit(1)
	}
	if len(snips) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: snippet corpus is empty")
		os.Exit(1)
	}

	// -bin wins; otherwise the same config the orchestrator reads
	// (statepaths.Config(), MR_ORCH_STATE-aware). orchcfg.Load tolerates a missing
	// file and defaults LocalOffloadBin to "offload-harness" (resolved on PATH).
	bin := *binFlag
	if bin == "" {
		bin = orchcfg.Load(statepaths.Config()).LocalOffloadBin
	}
	if !binPresent(bin) {
		fmt.Printf("WARNING: offload binary %q not found — cannot measure a live ceiling. Skipping.\n", bin)
		return // fail-open, exit 0
	}

	recs := make([]vp.Record, 0, len(snips))
	for _, s := range snips {
		t0 := time.Now()
		_, raw, _ := locallane.RunCascade(context.Background(), bin, "triage", s.Snippet, *question, *timeoutSec)
		v, conf, model := parseVerdict(raw)
		recs = append(recs, vp.Record{
			Snippet:    idOr(s),
			Label:      s.Label,
			Verdict:    v,
			Agree:      vp.Agreement(s.Label, v),
			Confidence: conf,
			Model:      model,
			LatencyMS:  time.Since(t0).Milliseconds(),
		})
	}

	if b, err := vp.Marshal(recs); err == nil {
		if werr := os.WriteFile(*outPath, b, 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "WARNING: could not write seed %s: %v\n", *outPath, werr)
		} else {
			fmt.Printf("Wrote %d records → %s\n", len(recs), *outPath)
		}
	}

	c := verifierceiling.Compute(recs)
	printCeiling(c)
}

func idOr(s vp.Snippet) string {
	if s.ID != "" {
		return s.ID
	}
	return s.Snippet
}

func binPresent(bin string) bool {
	if bin == "" {
		return false
	}
	if _, err := os.Stat(bin); err == nil {
		return true // an explicit path
	}
	_, err := exec.LookPath(bin) // or a bare command name resolvable on PATH
	return err == nil
}

func printCeiling(c verifierceiling.Ceiling) {
	fmt.Println("\nVerifier ceiling (PILOT — small-n, not a benchmark):")
	fmt.Printf("  n=%d  decisive=%d  agree=%d  disagree=%d  defer=%d  error=%d\n",
		c.N, c.Decisive, c.Agree, c.Disagree, c.Deferred, c.Errored)
	fmt.Printf("  decisive_accuracy=%.3f  coverage=%.3f  effective_ceiling=%.3f\n",
		c.DecisiveAccuracy, c.Coverage, c.EffectiveCeiling)
	fmt.Printf("  AUGRC=%.4f (decision metric, lower=better)  AURC=%.4f\n", c.AUGRC, c.AURC)
	fmt.Printf("  distinct_confidence=%d  max_bucket_mass=%.2f\n", c.DistinctConfidence, c.MaxBucketMass)
	if c.Degenerate {
		fmt.Printf("  ⚠ DEGENERATE: %s\n", c.DegenerateReason)
	}
}
