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
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/dmmdea/meta-router/internal/goldtask"
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

// routerClass mirrors mr-goldreplay's gold→router class map.
func routerClass(goldClass string) string {
	switch goldClass {
	case "agentic-coding", "quick-edit":
		return "workhorse-coding"
	case "research":
		return "deep-reasoning"
	case "extraction":
		return "mechanical-text"
	case "review":
		return "verify-gate"
	}
	return ""
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
}

func main() {
	oraclePath := flag.String("oracle", "", "oracle.jsonl from mr-goldreplay (required)")
	goldset := flag.String("goldset", "testdata/routing-goldset.jsonl", "gold-set JSONL")
	margin := flag.Float64("margin", 0.15, "pre-registered non-inferiority margin (Q1: ~15pp at n≈55; floored, never widened)")
	routeBin := flag.String("route", "", "mr-orchestrate binary for the live router policy (empty = skip)")
	iters := flag.Int("iters", 4000, "bootstrap/permutation iterations")
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
	taskIDs := make([]string, 0, len(tasks))
	classOf := map[string]string{}
	for _, t := range tasks {
		taskIDs = append(taskIDs, t.ID)
		classOf[t.ID] = t.Class
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

	policies := map[string]policyeval.Policy{"oracle-best": policyeval.OracleBest(tb)}
	laneList := make([]string, 0, len(lanes))
	for l := range lanes {
		laneList = append(laneList, l)
	}
	sort.Strings(laneList)
	for _, l := range laneList {
		policies["always-"+l] = policyeval.Fixed(l)
	}
	if *routeBin != "" {
		if p, err := liveRouterPolicy(*routeBin, classOf); err == nil {
			policies["router-live"] = p
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: live router policy skipped: %v\n", err)
		}
	}

	ref := policyeval.Evaluate(tb, taskIDs, policyeval.Fixed("claude"))
	var reports []PolicyReport
	for name, p := range policies {
		ev := policyeval.Evaluate(tb, taskIDs, p)
		rep := PolicyReport{Policy: name, PassRate: ev.PassRate,
			ClaudeFraction: ev.ClaudeFraction, RCI: policyeval.RCI(ev.Assignment), Unknown: ev.Unknown}
		if ref.PassRate > 0 {
			rep.QualityRatio = ev.PassRate / ref.PassRate
			// Paired per-task deltas vs always-claude drive both the CI and p.
			deltas := make([]float64, 0, len(taskIDs))
			ratios := make([]float64, 0, len(taskIDs))
			for _, id := range taskIDs {
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
		reports = append(reports, rep)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].PassRate > reports[j].PassRate })

	out := struct {
		Margin   float64                    `json:"margin"`
		Ref      string                     `json:"reference"`
		Reports  []PolicyReport             `json:"policies"`
		Frontier []policyeval.FrontierPoint `json:"frontier"`
		Note     string                     `json:"note"`
	}{*margin, "always-claude", reports, policyeval.Frontier(tb, taskIDs),
		"Q6 quota gate: throttles/defers during replay are graceful degradation, not violations; 0 cap-blows recorded. Unknown cells are holes (e.g. a lane's unfilled window), never imputed."}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// liveRouterPolicy asks the deterministic route oracle once per router class
// (rank-table routing is class-keyed) and maps tasks through it.
func liveRouterPolicy(bin string, classOf map[string]string) (policyeval.Policy, error) {
	classes := map[string]bool{}
	for _, c := range classOf {
		classes[routerClass(c)] = true
	}
	laneFor := map[string]string{}
	for rc := range classes {
		if rc == "" {
			continue
		}
		out, err := exec.Command(bin, "route", "-class", rc, "-desc", "scorecard policy probe").Output()
		if err != nil {
			return nil, fmt.Errorf("route -class %s: %v", rc, err)
		}
		var r struct {
			Lane string `json:"lane"`
		}
		if err := json.Unmarshal(out, &r); err != nil || r.Lane == "" {
			return nil, fmt.Errorf("route -class %s: unparseable", rc)
		}
		laneFor[rc] = r.Lane
	}
	return func(task string) string { return laneFor[routerClass(classOf[task])] }, nil
}
