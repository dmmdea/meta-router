// Command mr-verifier measures the LOCAL verifier's ceiling (CC1): it runs a
// labeled good/bad snippet corpus through the offload cascade (triage door),
// reads the free logprob decision margin as graded confidence, and reports
// decisive accuracy, coverage, the defer-discounted effective ceiling, and the
// selective-risk curves AURC + AUGRC (decision record §Q9). Fail-open: a missing
// offload binary prints a WARNING and exits 0 (mirrors mr-eval's embedder-down).
//
// With the reference column armed it also runs the same corpus past an
// EXTERNAL, calibrated decision model and prints the two ceilings side by side
// (see jev.go). That column is off by default, it spends, and a missing
// credential file skips it with a warning rather than failing the run — the
// local ceiling is the measurement; the reference is what makes it readable.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
// Both columns are given this same text, so the two ceilings answer the same
// question and a difference between them is a difference between the verifiers.
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

// runConfig is one measurement run. Every field but jevEndpoint is a flag.
type runConfig struct {
	corpus     string
	outPath    string
	question   string
	bin        string
	timeoutSec int
	jevColumn  bool
	jevOut     string
	// jevEndpoint overrides the reference column's route — TEST INJECTION ONLY
	// (httptest). No flag writes it: production leaves it empty and the client's
	// own constant applies.
	jevEndpoint string
}

func main() {
	var cfg runConfig
	flag.StringVar(&cfg.corpus, "corpus", "testdata/verifier-snippets.jsonl", "labeled snippet corpus JSONL")
	flag.StringVar(&cfg.outPath, "out", "testdata/verifier-seed.jsonl", "where to write the produced Record seed JSONL")
	flag.StringVar(&cfg.question, "question", defaultQuestion, "the verification gate applied to every snippet")
	flag.StringVar(&cfg.bin, "bin", "", "offload-harness binary path; empty = resolve from orchestrator config (statepaths.Config)")
	flag.IntVar(&cfg.timeoutSec, "timeout", 60, "per-snippet local-critic timeout (seconds)")
	flag.BoolVar(&cfg.jevColumn, "jev-column", false, "also measure the external reference column (SPENDS: about $0.0002 for the committed 40-snippet corpus; skipped with a warning when the credential file is absent)")
	flag.StringVar(&cfg.jevOut, "jev-out", "testdata/verifier-seed-jev.jsonl", "where to write the reference column's Record seed JSONL")
	flag.Parse()
	os.Exit(run(cfg, os.Stdout))
}

// run performs one measurement and returns the process exit code. Only a corpus
// that cannot be read is fatal; a verifier that cannot be reached is a warning
// and a missing column.
func run(cfg runConfig, out io.Writer) int {
	snips, err := vp.LoadSnippets(cfg.corpus)
	if err != nil {
		fmt.Fprintf(os.Stderr, "corpus load error: %v\n", err)
		return 1
	}
	if len(snips) == 0 {
		fmt.Fprintln(os.Stderr, "ERROR: snippet corpus is empty")
		return 1
	}

	// The reference column is armed first so a missing credential is reported
	// before the run spends minutes on the local column.
	var jev *jevRunner
	if cfg.jevColumn {
		j, jerr := newJevRunner(cfg.question, cfg.jevEndpoint)
		if jerr != nil {
			fmt.Fprintf(out, "WARNING: jev column skipped: %v\n", jerr)
		} else {
			jev = j
		}
	}

	// -bin wins; otherwise the same config the orchestrator reads
	// (statepaths.Config(), MR_ORCH_STATE-aware). orchcfg.Load tolerates a missing
	// file and defaults LocalOffloadBin to "offload-harness" (resolved on PATH).
	bin := cfg.bin
	if bin == "" {
		bin = orchcfg.Load(statepaths.Config()).LocalOffloadBin
	}
	localOn := binPresent(bin)
	if !localOn {
		fmt.Fprintf(out, "WARNING: offload binary %q not found — cannot measure a live ceiling. Skipping.\n", bin)
		if jev == nil {
			return 0 // fail-open, exit 0
		}
	}

	ctx := context.Background()
	recs := make([]vp.Record, 0, len(snips))
	jevRecs := make([]vp.Record, 0, len(snips))
	for _, s := range snips {
		if localOn {
			t0 := time.Now()
			_, raw, _ := locallane.RunCascade(ctx, bin, "triage", s.Snippet, cfg.question, cfg.timeoutSec)
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
		if jev != nil {
			jevRecs = append(jevRecs, jev.evaluate(ctx, s))
		}
	}

	var local, reference verifierceiling.Ceiling
	if localOn {
		writeSeed(out, recs, cfg.outPath)
		local = verifierceiling.Compute(recs)
		printCeiling(out, "Verifier ceiling (PILOT — small-n, not a benchmark):", local)
	}
	if jev != nil {
		writeSeed(out, jevRecs, cfg.jevOut)
		reference = verifierceiling.Compute(jevRecs)
		printCeiling(out, "Jev reference column (EXTERNAL, calibrated — same corpus, same question):", reference)
		fmt.Fprintln(out, jev.meter())
	}
	if localOn && jev != nil {
		printSideBySide(out, local, reference)
	}
	return 0
}

func writeSeed(out io.Writer, recs []vp.Record, path string) {
	b, err := vp.Marshal(recs)
	if err != nil {
		return
	}
	if werr := os.WriteFile(path, b, 0o644); werr != nil {
		fmt.Fprintf(os.Stderr, "WARNING: could not write seed %s: %v\n", path, werr)
		return
	}
	fmt.Fprintf(out, "Wrote %d records → %s\n", len(recs), path)
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

func printCeiling(out io.Writer, title string, c verifierceiling.Ceiling) {
	fmt.Fprintf(out, "\n%s\n", title)
	fmt.Fprintf(out, "  n=%d  decisive=%d  agree=%d  disagree=%d  defer=%d  error=%d\n",
		c.N, c.Decisive, c.Agree, c.Disagree, c.Deferred, c.Errored)
	fmt.Fprintf(out, "  decisive_accuracy=%.3f  coverage=%.3f  effective_ceiling=%.3f\n",
		c.DecisiveAccuracy, c.Coverage, c.EffectiveCeiling)
	fmt.Fprintf(out, "  AUGRC=%.4f (decision metric, lower=better)  AURC=%.4f\n", c.AUGRC, c.AURC)
	fmt.Fprintf(out, "  distinct_confidence=%d  max_bucket_mass=%.2f\n", c.DistinctConfidence, c.MaxBucketMass)
	if c.Degenerate {
		fmt.Fprintf(out, "  ⚠ DEGENERATE: %s\n", c.DegenerateReason)
	}
}

// printSideBySide is the point of the reference column: the five load-bearing
// numbers in one table, so "the local verifier ceilings at X" is read against a
// second verifier on the same corpus instead of against nothing. The two
// confidence scores are NOT the same quantity (a logprob margin on the left, a
// choice probability on the right), so the accuracy/coverage rows compare and
// the curve rows are each read within their own column.
func printSideBySide(out io.Writer, local, reference verifierceiling.Ceiling) {
	fmt.Fprintf(out, "\n%-22s %12s %12s\n", "metric", "local", "jev")
	row := func(name string, a, b float64, prec int) {
		fmt.Fprintf(out, "%-22s %12.*f %12.*f\n", name, prec, a, prec, b)
	}
	row("decisive_accuracy", local.DecisiveAccuracy, reference.DecisiveAccuracy, 3)
	row("coverage", local.Coverage, reference.Coverage, 3)
	row("effective_ceiling", local.EffectiveCeiling, reference.EffectiveCeiling, 3)
	row("AURC", local.AURC, reference.AURC, 4)
	row("AUGRC", local.AUGRC, reference.AUGRC, 4)
}
