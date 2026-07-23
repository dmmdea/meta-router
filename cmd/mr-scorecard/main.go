// Command mr-scorecard is V5 of the slice-4 eval stack: the WF@Q scorecard
// over the V2 oracle table (decision record Q1/Q6/Q7). It evaluates the
// candidate policies by exact table lookup (V3), reports each on the two axes
// — quality ratio vs always-Claude and Claude-window fraction — with a BCa
// bootstrap CI + sign-flip permutation p on the paired per-task deltas, the
// oracle cost-quality frontier, and the RCI collapse metric. The Q1 verdict is
// the NON-INFERIORITY regression gate: quality-ratio CI lower bound ≥ 1-margin
// at a lower Claude fraction. Unknown cells are counted, never imputed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dmmdea/meta-router/internal/goldtask"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	"github.com/dmmdea/meta-router/internal/policyeval"
)

const version = "0.1.0"

type oracleRow struct {
	Task         string `json:"task"`
	Class        string `json:"class"`
	Lane         string `json:"lane"`
	OutcomeClass string `json:"outcome_class"`
	VerifierPass bool   `json:"verifier_pass"`
}

// PolicyReport is one row of the scorecard.
type PolicyReport struct {
	Policy         string  `json:"policy"`
	PassRate       float64 `json:"pass_rate"`
	QualityRatio   float64 `json:"quality_ratio_vs_claude"`
	RatioCILo      float64 `json:"ratio_ci_lo"`
	RatioCIHi      float64 `json:"ratio_ci_hi"`
	SignFlipP      float64 `json:"sign_flip_p"`
	ClaudeFraction float64 `json:"claude_fraction"`
	RCI            float64 `json:"rci"`
	Unknown        int     `json:"unknown_cells"`
	NonInferior    bool    `json:"non_inferior_at_margin"`
	// InSample marks a row whose assignment saw the cells it is scored on
	// (the per-task oracle in split mode): a CEILING, never a deployable
	// candidate - its non-inferiority verdict is suppressed.
	InSample bool `json:"in_sample,omitempty"`
}

func main() {
	oraclePath := flag.String("oracle", "", "oracle.jsonl from mr-goldreplay (required)")
	goldset := flag.String("goldset", "testdata/routing-goldset.jsonl", "gold-set JSONL")
	margin := flag.Float64("margin", 0.15, "pre-registered non-inferiority margin (Q1: ~15pp at n≈55; floored, never widened)")
	routeBin := flag.String("route", "", "mr-orchestrate binary for the live router policy (empty = skip)")
	iters := flag.Int("iters", 4000, "bootstrap/permutation iterations")
	split := flag.Bool("split", false, "B'2 cross-validation: derive the class-level oracle on the goldset's tuning split only, score every policy on the heldout split (winner's-curse test)")
	liveQuota := flag.Bool("live-quota", false, "router-live probes run against the REAL orchestrator state (today's quota weather + latches) instead of the default neutral all-open state (policy inputs preserved: rank table, config, fuses); the default answers the POLICY question. Consult receipts are suppressed in BOTH modes (probes must not pollute the delegation-coverage numerator)")
	flag.Parse()
	if *oraclePath == "" {
		fmt.Fprintln(os.Stderr, "usage: mr-scorecard -oracle eval/oracle.jsonl [-goldset ...] [-route ~/.meta-router/bin/mr-orchestrate.exe]")
		os.Exit(2)
	}

	tasks, err := goldtask.Load(*goldset)
	if err != nil {
		fmt.Fprintf(os.Stderr, "goldset: %v\n", err)
		os.Exit(2)
	}
	// A mislabeled split ("held-out", "Heldout", absent) would silently land in
	// tuning AND shrink the eval set - Validate enforces split in {tuning,
	// heldout} plus the schema, so a typo is exit 2, never silent leakage.
	if err := goldtask.Validate(tasks); err != nil {
		fmt.Fprintf(os.Stderr, "goldset: %v\n", err)
		os.Exit(2)
	}
	taskIDs := make([]string, 0, len(tasks))
	classOf := map[string]string{}
	promptOf := map[string]string{}
	var tuningIDs, heldoutIDs []string
	for _, t := range tasks {
		taskIDs = append(taskIDs, t.ID)
		classOf[t.ID] = t.Class
		promptOf[t.ID] = t.Prompt
		if t.Split == "heldout" {
			heldoutIDs = append(heldoutIDs, t.ID)
		} else {
			tuningIDs = append(tuningIDs, t.ID)
		}
	}

	tb := policyeval.NewTable()
	b, err := os.ReadFile(*oraclePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "oracle: %v\n", err)
		os.Exit(2)
	}
	lanes := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r oracleRow
		if json.Unmarshal([]byte(line), &r) != nil || r.Task == "" {
			continue
		}
		if r.OutcomeClass == "deferred" {
			continue // a deferred cell is a hole, not an observation
		}
		tb.Add(r.Task, r.Lane, r.VerifierPass)
		lanes[r.Lane] = true
	}

	// B'2 (-split): the EVALUATION set becomes the heldout tasks only, and the
	// headline candidate is the class-level oracle DERIVED ON TUNING ONLY —
	// the per-task oracle-best is reported alongside as the (in-sample,
	// non-generalizable) ceiling. Without -split, behavior is unchanged.
	evalIDs := taskIDs
	policies := map[string]policyeval.Policy{"oracle-best": policyeval.OracleBest(tb)}
	var classAssign map[string]string
	var classCov policyeval.ClassCoverage
	if *split {
		if len(tuningIDs) == 0 || len(heldoutIDs) == 0 {
			fmt.Fprintf(os.Stderr, "split: goldset needs both tuning (%d) and heldout (%d) tasks\n", len(tuningIDs), len(heldoutIDs))
			os.Exit(2)
		}
		evalIDs = heldoutIDs
		classAssign, classCov = policyeval.ClassBest(tb, tuningIDs, classOf)
		if len(classAssign) == 0 {
			fmt.Fprintln(os.Stderr, "split: no tuning cell has oracle data - wrong/mismatched -oracle? refusing a vacuous class policy")
			os.Exit(2)
		}
		policies["class-oracle-tuned"] = policyeval.ByClass(classAssign, classOf)
	}
	laneList := make([]string, 0, len(lanes))
	for l := range lanes {
		laneList = append(laneList, l)
	}
	sort.Strings(laneList)
	for _, l := range laneList {
		policies["always-"+l] = policyeval.Fixed(l)
	}
	if *routeBin != "" {
		// router-live = the REAL deterministic router: run the shipped classifier
		// (classify.go, via route --desc) on each raw prompt and take the lane it
		// chooses. NOT the gold-label→class proxy — that proxy mislabels e.g.
		// gold adversarial review as the cheap "verify-gate" class and fabricates
		// a local-lane misroute the production router never makes (measured
		// 2026-07-20: the label map disagrees with the live classifier on 48/56).
		probePrompts := map[string]string{}
		for _, id := range evalIDs {
			probePrompts[id] = promptOf[id]
		}
		if p, err := liveRouterPolicy(*routeBin, probePrompts, *liveQuota); err == nil {
			policies["router-live"] = p
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: live router policy skipped: %v\n", err)
		}
	}

	ref := policyeval.Evaluate(tb, evalIDs, policyeval.Fixed("claude"))
	var reports []PolicyReport
	for name, p := range policies {
		ev := policyeval.Evaluate(tb, evalIDs, p)
		rep := PolicyReport{Policy: name, PassRate: ev.PassRate,
			ClaudeFraction: ev.ClaudeFraction, RCI: policyeval.RCI(ev.Assignment), Unknown: ev.Unknown}
		if ref.PassRate > 0 {
			rep.QualityRatio = ev.PassRate / ref.PassRate
			// Paired per-task deltas vs always-claude drive both the CI and p.
			deltas := make([]float64, 0, len(evalIDs))
			ratios := make([]float64, 0, len(evalIDs))
			for _, id := range evalIDs {
				d := ev.PerTask[id] - ref.PerTask[id]
				deltas = append(deltas, d)
				ratios = append(ratios, d) // CI on the mean delta, mapped to ratio space below
			}
			lo, hi := policyeval.BootstrapCI(ratios, 0.95, *iters, 42)
			rep.RatioCILo = (ref.PassRate + lo) / ref.PassRate
			rep.RatioCIHi = (ref.PassRate + hi) / ref.PassRate
			rep.SignFlipP = policyeval.SignFlipP(deltas, *iters, 42)
			rep.NonInferior = rep.RatioCILo >= 1-*margin && ev.ClaudeFraction < 1
		}
		if *split && name == "oracle-best" {
			// Per-task argmax over the FULL table, scored on heldout cells it
			// has seen: the in-sample ceiling. Marked, and its verdict is
			// suppressed so the artifact can never present it as deployable.
			rep.InSample = true
			rep.NonInferior = false
		}
		reports = append(reports, rep)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].PassRate > reports[j].PassRate })

	note := "Q6 quota gate: throttles/defers during replay are graceful degradation, not violations; 0 cap-blows recorded. Unknown cells are holes (e.g. a lane's unfilled window), never imputed."
	var splitInfo *SplitInfo
	if *split {
		splitInfo = &SplitInfo{Mode: "tuning->heldout", TuningN: len(tuningIDs), HeldoutN: len(heldoutIDs),
			ClassAssignment: classAssign, ClassCoverage: classCov}
		note += " SPLIT MODE: policies are scored on the heldout tasks only; oracle-best and the frontier are IN-SAMPLE ceilings (they see the heldout cells); class-oracle-tuned is the generalization test - derived on tuning only, over GOLD class labels (an idealized upper bound on class routing: production classifies via classify.go, which disagrees with gold labels on 48/56)."
	}
	out := struct {
		Margin   float64                    `json:"margin"`
		Ref      string                     `json:"reference"`
		Split    *SplitInfo                 `json:"split,omitempty"`
		Reports  []PolicyReport             `json:"policies"`
		Frontier []policyeval.FrontierPoint `json:"frontier"`
		Note     string                     `json:"note"`
	}{*margin, "always-claude", splitInfo, reports, policyeval.Frontier(tb, evalIDs), note}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// SplitInfo is the B'2 cross-validation block, emitted only with -split.
type SplitInfo struct {
	Mode            string                   `json:"mode"`
	TuningN         int                      `json:"tuning_n"`
	HeldoutN        int                      `json:"heldout_n"`
	ClassAssignment map[string]string        `json:"class_assignment"`
	ClassCoverage   policyeval.ClassCoverage `json:"class_coverage"`
}

// liveRouterPolicy runs the shipped deterministic router on each task's RAW
// PROMPT — the real production code path: `route --desc <prompt>` with no
// --class, so classify.go picks the class and the rank table picks the lane.
// One probe per task (offline scorecard; the router itself is sub-ms).
//
// By DEFAULT the probes run in a NEUTRAL, isolated state (MR_ORCH_STATE →
// fresh temp dir: empty ledger = every lane open, no receipts written) so the
// measurement answers the POLICY question — which lane would the router pick
// on merit. Probing the real state dir instead (liveQuota=true) folds today's
// quota weather into the answer: with e.g. glm exhausted + codex throttled,
// every consult relegates to claude and "router-live" degenerates to
// always-claude (observed 2026-07-23 — the same measurement-confound family
// as the 2026-07-20 label→class proxy bug). It also spams the live dispatch
// log with 56 consult receipts per scorecard run.
func liveRouterPolicy(bin string, promptOf map[string]string, liveQuota bool) (policyeval.Policy, error) {
	var extraEnv []string
	if !liveQuota {
		dir, err := os.MkdirTemp("", "mr-scorecard-neutral-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		// Neutral isolates the WEATHER (ledger, drop, trace, latches, alerts,
		// receipts) but must preserve the POLICY INPUTS — the operator's
		// rank-table override, config, and fuses. Without this copy the probes
		// silently fall back to the compiled Seed table and the scorecard
		// measures a router nobody runs (observed live 2026-07-23: the B'1
		// mechanical-text floor vanished from router-live under a bare temp
		// dir).
		// A missing policy file is a legitimate skip (Seed/defaults apply); any
		// OTHER read error (lock contention with a running orchestrator - the
		// moment states diverge most) must be FATAL, or the probes silently
		// measure the compiled Seed table again. glm-alert.json (the 1313
		// latch) is deliberately weather, not policy: neutral probes answer
		// merit; a ban-latched lane is an admission concern.
		for _, f := range []string{"rank-table.json", "config.json", "fuses.json"} {
			b, rerr := os.ReadFile(filepath.Join(statepaths.StateDir(), f))
			if rerr != nil {
				if errors.Is(rerr, fs.ErrNotExist) {
					continue
				}
				return nil, fmt.Errorf("neutral state seed %s: %w", f, rerr)
			}
			if werr := os.WriteFile(filepath.Join(dir, f), b, 0o644); werr != nil {
				return nil, fmt.Errorf("neutral state seed %s: %w", f, werr)
			}
		}
		extraEnv = append(os.Environ(), "MR_ORCH_STATE="+dir)
	}
	laneFor := map[string]string{}
	for task, prompt := range promptOf {
		cmd := exec.Command(bin, "route", "-desc", prompt, "-no-receipt")
		if extraEnv != nil {
			cmd.Env = extraEnv
		}
		out, err := cmd.Output()
		if err != nil {
			var xe *exec.ExitError
			if errors.As(err, &xe) && len(xe.Stderr) > 0 {
				// Surface stderr: "flag provided but not defined: -no-receipt"
				// self-diagnoses an outdated installed binary.
				return nil, fmt.Errorf("route %s: %v: %s", task, err, strings.TrimSpace(string(xe.Stderr)))
			}
			return nil, fmt.Errorf("route %s: %v", task, err)
		}
		var r struct {
			Lane string `json:"lane"`
		}
		if err := json.Unmarshal(out, &r); err != nil || r.Lane == "" {
			return nil, fmt.Errorf("route %s: unparseable", task)
		}
		laneFor[task] = r.Lane
	}
	return func(task string) string { return laneFor[task] }, nil
}
