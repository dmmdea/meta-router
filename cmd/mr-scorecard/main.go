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
	"github.com/dmmdea/meta-router/internal/orch/childenv"
	"github.com/dmmdea/meta-router/internal/orch/router"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
	"github.com/dmmdea/meta-router/internal/policyeval"
	"github.com/dmmdea/meta-router/internal/policyzoo"
)

const version = "0.1.0"

// minPairedN is the floor below which a non-inferiority verdict is refused.
// BootstrapCI degenerates to zero width at very small n, so a lucky tie
// clears any margin. Not a statistical threshold — a refusal to dress a
// coincidence as the pre-registered verdict.
const minPairedN = 8

type oracleRow struct {
	Task  string `json:"task"`
	Class string `json:"class"`
	Lane  string `json:"lane"`
	// Model and Effort complete the evidence cell. Legacy rows carry neither;
	// NormalizeEffort turns a blank effort into EffortUnrecorded, which is a
	// REAL config value — such evidence never satisfies an entry naming a
	// pinned effort (B6: unknown counted, never imputed).
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Dispatched   bool   `json:"dispatched"`
	OutcomeClass string `json:"outcome_class"`
	VerifierPass bool   `json:"verifier_pass"`
}

// ran reports whether this row is EVIDENCE — i.e. the dispatch actually
// happened and produced a verdict. A hole is not evidence (B6): counting one
// as a measured FAILURE lets a lane that never ran look like a lane that ran
// and lost, which deflates the reference and inflates every ratio computed
// against it (review 2026-08-12: an all-error replay turned B15 green and
// gave the reference PassRate 0 with Unknown 0). The definition lives in
// policyeval.IsEvidence — ONE list shared with mr-goldreplay's resume set and
// the B15 canary, because three drifting copies are how a cell was
// simultaneously "already recorded" and "not evidence".
func (r oracleRow) ran() bool {
	return policyeval.IsEvidence(r.Dispatched, r.OutcomeClass)
}

// config is the row's evidence cell.
func (r oracleRow) config() policyeval.Config {
	// Lane and model are TRIMMED on ingest, matching normalizePins on the
	// write side. A padded value is a distinct string from every other
	// row's, which is precisely the mislabelling the config key exists to
	// end — the reasoning NormalizeEffort already states, applied to all
	// three fields instead of one.
	return policyeval.Config{Lane: strings.TrimSpace(r.Lane), Model: strings.TrimSpace(r.Model),
		Effort: policyeval.NormalizeEffort(r.Effort)}
}

// rankedConfigs enumerates the configs the ACTIVE rank table ranks, and the
// per-lane rank order. It reads the operator's override when present — the
// same policy liveRouterPolicy deliberately copies into its neutral state dir.
// Reading the compiled seed here while the probes route on an override
// measured a router nobody runs (the 2026-07-25 audit's exact divergence,
// re-created at this seam; review 2026-08-12).
func rankedConfigs(tbl router.Table) (byLane map[string][]policyeval.Config, all []policyeval.Config) {
	type ranked struct {
		cfg  policyeval.Config
		rank int
	}
	// A config can appear in SEVERAL classes at different ranks. Its rank here
	// is the BEST (minimum) across them — "how highly does the table rank this
	// config anywhere". Taking the first-seen rank instead would let map
	// iteration order decide it, which is the exact non-determinism this
	// function's total order exists to remove.
	best := map[string]*ranked{}
	for _, entries := range tbl {
		for _, e := range entries {
			// Trimmed exactly as the oracle side trims on ingest (config()):
			// a padded field in the operator's rank-table override otherwise
			// builds a key no oracle row can match, dropping the whole lane
			// to pass_rate 0 while oracle-best scores the same evidence fine
			// (review 2026-08-12, round 3).
			cfg := policyeval.Config{Lane: strings.TrimSpace(e.Lane), Model: strings.TrimSpace(e.Model),
				Effort: policyeval.NormalizeEffort(e.Effort)}
			if cur, ok := best[cfg.Key()]; !ok {
				best[cfg.Key()] = &ranked{cfg, e.Rank}
				all = append(all, cfg)
			} else if e.Rank < cur.rank {
				cur.rank = e.Rank
			}
		}
	}
	perLane := map[string][]ranked{}
	for _, r := range best {
		perLane[r.cfg.Lane] = append(perLane[r.cfg.Lane], *r)
	}
	byLane = map[string][]policyeval.Config{}
	for lane, rs := range perLane {
		// TOTAL order: rank, then config key. The table is a MAP, so without
		// the key tie-break Go's iteration order decides which equally-ranked
		// config represents the lane.
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].rank != rs[j].rank {
				return rs[i].rank < rs[j].rank
			}
			return rs[i].cfg.Key() < rs[j].cfg.Key()
		})
		for _, r := range rs {
			byLane[lane] = append(byLane[lane], r.cfg)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Key() < all[j].Key() })
	return byLane, all
}

// resolveLaneConfig turns a LANE into the cell a lane-level policy
// (always-<lane>, a zoo floor) is measured as. It walks the lane's configs in
// rank order and takes the first one observed on the EVALUATION set; then the
// first observed anywhere in the oracle; then rank-1 when the lane has no
// evidence at all.
//
// Why evidence-preferring rather than simply rank-1: "route everything to this
// lane" is only measurable through a config that was measured. Rank-1 alone
// resolved three of four lanes to cells with ZERO rows on the live oracle
// (glm→glm-4.7 because '4'<'5'; local→qwythos-think), so every lane row read
// pass_rate 0 while 580 observations sat in the table unreachable — the
// operator's conclusion would have been "codex, glm and local are worthless".
//
// Why the EVAL-SET tier comes first: "observed" was the whole oracle, with no
// restriction to the tasks actually being scored — so under -split a ranked
// config with a single TUNING-side observation outranked a lower-ranked
// config with hundreds of heldout observations, and the reference resolved to
// a cell that is unmeasured (or paired_n≈1) on every task in the evaluation
// (review 2026-08-12, round 3). Walking rank order keeps the choice
// principled and deterministic; the fallbacks keep "no evidence" honest
// rather than inventing a cell.
func resolveLaneConfig(byLane map[string][]policyeval.Config, lane string, observedEval, observedAll map[string]int) (policyeval.Config, bool) {
	cfgs := byLane[lane]
	for _, c := range cfgs {
		if observedEval[c.Key()] > 0 {
			return c, true
		}
	}
	for _, c := range cfgs {
		if observedAll[c.Key()] > 0 {
			return c, true
		}
	}
	if len(cfgs) > 0 {
		return cfgs[0], false
	}
	return policyeval.Config{}, false
}

// ConfigCoverage reports, per ranked config, how much evidence exists — at
// BOTH levels. Model coverage is what B15 gates today; effort coverage is
// reported, not gated, because every legacy row is effort-unrecorded and a
// gate that fails all 18 ranked configs isolates nothing. The gap is loud
// from day one so the re-baseline that closes it is visible work.
type ConfigCoverage struct {
	Config        string `json:"config"`
	Observations  int    `json:"observations"`   // rows for the FULL (lane,model,effort) cell
	CoveredModel  bool   `json:"covered_model"`  // any evidence for (lane,model)
	CoveredEffort bool   `json:"covered_effort"` // evidence for the full cell
}

// ReferenceGap marks a scorecard whose REFERENCE policy has no evidence. Its
// presence means the comparative verdicts are undefined rather than negative —
// a distinction the artifact could not otherwise express, since an
// uncomputed ratio and a genuinely-1.0 one both serialize as absent/zero.
type ReferenceGap struct {
	Config  string `json:"config"`
	Tasks   int    `json:"evaluated_tasks"`
	Message string `json:"message"`
}

// PolicyReport is one row of the scorecard.
type PolicyReport struct {
	Policy   string  `json:"policy"`
	PassRate float64 `json:"pass_rate"`
	// The four DERIVED values are pointers so "not computed" serializes as
	// ABSENT rather than 0. A zero here is an extreme answer, not a missing
	// one: sign_flip_p 0 is the most significant p possible and passes any
	// `p < 0.05` check, and quality_ratio 0 reads as "infinitely worse".
	// Both were emitted on every row of the live artifact for tests that
	// never ran (review 2026-08-12).
	QualityRatio   *float64 `json:"quality_ratio_vs_claude,omitempty"`
	RatioCILo      *float64 `json:"ratio_ci_lo,omitempty"`
	RatioCIHi      *float64 `json:"ratio_ci_hi,omitempty"`
	SignFlipP      *float64 `json:"sign_flip_p,omitempty"`
	ClaudeFraction float64  `json:"claude_fraction"`
	RCI            float64  `json:"rci"`
	Unknown        int      `json:"unknown_cells"`
	NonInferior    *bool    `json:"non_inferior_at_margin"`
	// VerdictUndefined says WHY there is no verdict. Present exactly when
	// NonInferior is nil.
	VerdictUndefined string `json:"verdict_undefined,omitempty"`
	// Configs names the evidence cells this row's numbers belong to, sorted.
	// A fixed policy has one; a per-task policy (oracle-best, router-live)
	// has several. Without it a reader cannot tell WHICH model a pass_rate
	// describes — the mislabelling this program exists to end, at the
	// artifact layer. (Shipped as a dead field in v0.29.0; populated here.)
	Configs []string `json:"configs,omitempty"`
	// PairedN is how many tasks had evidence for BOTH this policy and the
	// reference. The ratio/CI/p are computed on those tasks only; anything
	// less than the full set means the verdict is narrower than the margin
	// was pre-registered for.
	PairedN    int    `json:"paired_n"`
	PairedNote string `json:"paired_note,omitempty"`
	// InSample marks a row whose assignment saw the cells it is scored on
	// (the per-task oracle in split mode): a CEILING, never a deployable
	// candidate - its non-inferiority verdict is suppressed.
	InSample bool `json:"in_sample,omitempty"`
	// ParetoVsRouter (zoo rows only): pass_rate >= router-live's AND
	// claude_fraction <= router-live's, with at least one strict — the W3
	// promotion precondition on top of non-inferiority.
	ParetoVsRouter *bool `json:"pareto_vs_router,omitempty"`
}

// ZooEntry records one family's tuning-split selection (W3): which config won
// the sweep and its tuning-side numbers. The heldout verdict lives in the
// corresponding zoo:* policy row. The diverged counts make a vacuous winner
// VISIBLE: a config that never leaves the router baseline (diverged 0/0) and
// one that fires but happens to match cannot otherwise be told apart in the
// artifact.
type ZooEntry struct {
	Family          string  `json:"family"`
	Chosen          string  `json:"chosen_config"`
	GridSize        int     `json:"grid_size"`
	TuningPassRate  float64 `json:"tuning_pass_rate"`
	TuningClaudeFr  float64 `json:"tuning_claude_fraction"`
	TuningDiverged  int     `json:"tuning_diverged"`  // tuning tasks where chosen config != router baseline
	HeldoutDiverged int     `json:"heldout_diverged"` // heldout tasks where chosen config != router baseline
}

func main() {
	oraclePath := flag.String("oracle", "", "oracle.jsonl from mr-goldreplay (required)")
	goldset := flag.String("goldset", "testdata/routing-goldset.jsonl", "gold-set JSONL")
	margin := flag.Float64("margin", 0.15, "pre-registered non-inferiority margin (Q1: ~15pp at n≈55; floored, never widened)")
	routeBin := flag.String("route", "", "mr-orchestrate binary for the live router policy (empty = skip)")
	iters := flag.Int("iters", 4000, "bootstrap/permutation iterations")
	split := flag.Bool("split", false, "B'2 cross-validation: derive the class-level oracle on the goldset's tuning split only, score every policy on the heldout split (winner's-curse test)")
	liveQuota := flag.Bool("live-quota", false, "router-live probes run against the REAL orchestrator state (today's quota weather + latches) instead of the default neutral all-open state (policy inputs preserved: rank table, config, fuses); the default answers the POLICY question. Consult receipts are suppressed in BOTH modes (probes must not pollute the delegation-coverage numerator)")
	zoo := flag.Bool("zoo", false, "W3 policy zoo: tune each candidate family's config on the TUNING split (task-mean objective), score the chosen config on HELDOUT alongside the standard policies. Requires -split and -route (candidates wrap the live router's neutral-state pick).")
	flag.Parse()
	if *oraclePath == "" {
		fmt.Fprintln(os.Stderr, "usage: mr-scorecard -oracle eval/oracle.jsonl [-goldset ...] [-route ~/.meta-router/bin/mr-orchestrate.exe]")
		os.Exit(2)
	}
	if *zoo && (!*split || *routeBin == "") {
		fmt.Fprintln(os.Stderr, "-zoo requires -split and -route (candidates are tuned on the tuning split over the live router's baseline picks)")
		os.Exit(2)
	}
	if *zoo && *liveQuota {
		// Under quota weather the router baseline degenerates (e.g. everything
		// relegates to claude), floors become structural no-ops, and the zoo
		// emits a guaranteed-trivial null indistinguishable from a real one.
		fmt.Fprintln(os.Stderr, "-zoo requires the neutral-state baseline; it cannot be combined with -live-quota")
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
	obsByConfig := map[string]int{}                // full (lane,model,effort) key → row count
	obsByConfigTask := map[string]map[string]int{} // config key → task → row count (eval-set restriction below)
	obsByModel := map[string]bool{}                // "lane|model" → any evidence
	malformed := 0                                 // unparseable/identity-less oracle lines
	nonEvidence := 0                               // parsed rows that are HOLES (see ran)
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r oracleRow
		if json.Unmarshal([]byte(line), &r) != nil || r.Task == "" {
			// Counted, not silently dropped. model and effort are load-bearing
			// now, so a torn row does not merely vanish — it would shrink the
			// table under a config nobody can see. Same rule the v0.25.0
			// dashboard adopted for exactly this reason.
			malformed++
			continue
		}
		if !r.ran() {
			// A hole is not an observation (see ran) — but a DROPPED row is
			// still a counted one. The same commit that added the ran()
			// filter counted malformed lines and warned; rows it discards
			// deserve no less, or an all-error replay produces an artifact
			// whose own drop counters all read zero and is byte-identical to
			// an empty oracle (review 2026-08-12, round 3).
			nonEvidence++
			continue
		}
		cfg := r.config()
		tb.Add(r.Task, cfg, r.VerifierPass)
		// The TRIMMED lane, matching the config key one line up: the raw
		// r.Lane registered a padded lane string as its own lane, producing a
		// spurious always-"claude " policy row resolving to the empty config.
		lanes[cfg.Lane] = true
		obsByConfig[cfg.Key()]++
		if obsByConfigTask[cfg.Key()] == nil {
			obsByConfigTask[cfg.Key()] = map[string]int{}
		}
		obsByConfigTask[cfg.Key()][r.Task]++
		obsByModel[cfg.Lane+"|"+cfg.Model] = true
	}
	if malformed > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d oracle line(s) unparseable or identity-less — skipped; the table below is smaller than the file\n", malformed)
	}
	if nonEvidence > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d oracle row(s) are holes (not dispatched / deferred / error / exit-N / verify_error) — not evidence; they are re-attemptable via mr-goldreplay\n", nonEvidence)
	}

	// The ACTIVE policy, not the compiled default: the probes route on the
	// operator's rank-table override, so resolving against the seed would score
	// a router nobody runs.
	rankTbl, tblProvenance, tblWarn := router.LoadChecked(statepaths.RankTable())
	if tblWarn != "" {
		fmt.Fprintln(os.Stderr, "WARN: rank table:", tblWarn)
	}
	byLaneRanked, rankedCfgs := rankedConfigs(rankTbl)
	ranked := map[string]bool{}
	for _, c := range rankedCfgs {
		ranked[c.Key()] = true
	}

	// B'2 (-split): the EVALUATION set becomes the heldout tasks only, and the
	// headline candidate is the class-level oracle DERIVED ON TUNING ONLY —
	// the per-task oracle-best is reported alongside as the (in-sample,
	// non-generalizable) ceiling. Without -split, behavior is unchanged.
	evalIDs := taskIDs
	policies := map[string]policyeval.Policy{"oracle-best": policyeval.OracleBest(tb)}
	var classAssign map[string]policyeval.Config
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
	// Evidence counts restricted to the EVALUATION set, built once evalIDs is
	// final. resolveLaneConfig prefers these: an "observed" config whose only
	// rows sit outside the tasks being scored (the tuning split, in -split
	// mode) resolves the lane to a cell that is then unmeasured on every task
	// actually evaluated.
	evalSet := map[string]bool{}
	for _, id := range evalIDs {
		evalSet[id] = true
	}
	obsEval := map[string]int{}
	for key, byTask := range obsByConfigTask {
		for task, n := range byTask {
			if evalSet[task] {
				obsEval[key] += n
			}
		}
	}
	// resolve is the one lane→cell seam; it prefers a ranked config observed
	// on the evaluation set (see resolveLaneConfig).
	resolve := func(lane string) policyeval.Config {
		c, _ := resolveLaneConfig(byLaneRanked, lane, obsEval, obsByConfig)
		return c
	}
	laneList := make([]string, 0, len(lanes))
	for l := range lanes {
		laneList = append(laneList, l)
	}
	sort.Strings(laneList)
	for _, l := range laneList {
		lcfg := resolve(l)
		if lcfg == (policyeval.Config{}) {
			// The lane has oracle rows but the ACTIVE table does not rank it:
			// there is no cell "route everything to this lane" can stand for.
			// Say so — the row otherwise shows configs ["||"] and pass_rate 0,
			// which reads as "measured and worthless" (review 2026-08-12, r3).
			fmt.Fprintf(os.Stderr, "WARNING: lane %q has oracle evidence but no entry in the %s rank table — always-%s resolves to NO config and its zeros mean 'unrankable', not 'measured worthless'\n",
				l, tblProvenance, l)
		}
		policies["always-"+l] = policyeval.Fixed(lcfg)
	}
	var routerPick policyeval.Policy
	if *routeBin != "" {
		// router-live = the REAL deterministic router: run the shipped classifier
		// (classify.go, via route --desc) on each raw prompt and take the lane it
		// chooses. NOT the gold-label→class proxy — that proxy mislabels e.g.
		// gold adversarial review as the cheap "verify-gate" class and fabricates
		// a local-lane misroute the production router never makes (measured
		// 2026-07-20: the label map disagrees with the live classifier on 48/56).
		probeIDs := evalIDs
		if *zoo {
			probeIDs = taskIDs // zoo tuning needs baseline picks on the tuning split too
		}
		probePrompts := map[string]string{}
		for _, id := range probeIDs {
			probePrompts[id] = promptOf[id]
		}
		if p, err := liveRouterPolicy(*routeBin, probePrompts, *liveQuota, resolve); err == nil {
			policies["router-live"] = p
			routerPick = p
		} else {
			fmt.Fprintf(os.Stderr, "WARNING: live router policy skipped: %v\n", err)
		}
	}
	var zooEntries []ZooEntry
	if *zoo {
		if routerPick == nil {
			fmt.Fprintln(os.Stderr, "zoo: live router probe failed - candidates have no baseline")
			os.Exit(2)
		}
		byID := map[string]policyzoo.Task{}
		var tuningTasks, heldoutTasks []policyzoo.Task
		for _, t := range tasks {
			zt := policyzoo.Task{ID: t.ID, Class: t.Class, Prompt: t.Prompt, BaseLane: routerPick(t.ID).Lane}
			byID[t.ID] = zt
			if t.Split == "heldout" {
				heldoutTasks = append(heldoutTasks, zt)
			} else {
				tuningTasks = append(tuningTasks, zt)
			}
		}
		diverged := func(c policyzoo.Candidate, ts []policyzoo.Task) int {
			n := 0
			for _, t := range ts {
				if c.Route(t) != t.BaseLane {
					n++
				}
			}
			return n
		}
		// The zoo resolves changed lanes with a TUNING-preferring map, frozen
		// across both splits. The eval-set-first resolve above is right for
		// the reference row but wrong here twice over: under -split its
		// tier-1 counts HELDOUT rows only, so (a) a cell with hundreds of
		// tuning rows loses to one with a single heldout row and every
		// diverging candidate's tuning objective collapses to Unknown/0 —
		// SelectBest degenerates to the no-op — and (b) heldout coverage
		// would steer the tuning-split pick, leaking the winner's-curse
		// test's own answer key. Freezing ONE resolver (tuning-preferring,
		// then whole-oracle, then rank-1) for both selection and the heldout
		// verdict keeps a single ruler chosen with tuning information only
		// (review 2026-08-12, round 4).
		tuningSet := map[string]bool{}
		for _, id := range tuningIDs {
			tuningSet[id] = true
		}
		obsTuning := map[string]int{}
		for key, byTask := range obsByConfigTask {
			for task, n := range byTask {
				if tuningSet[task] {
					obsTuning[key] += n
				}
			}
		}
		resolveTuning := func(lane string) policyeval.Config {
			c, _ := resolveLaneConfig(byLaneRanked, lane, obsTuning, obsByConfig)
			return c
		}
		fams := policyzoo.AllFamilies()
		famNames := make([]string, 0, len(fams))
		for n := range fams {
			famNames = append(famNames, n)
		}
		sort.Strings(famNames)
		for _, name := range famNames {
			// routerPick is the base: a candidate that leaves the router's
			// lane unchanged scores at the router's OWN emitted config — the
			// same cell router-live scores at — so a no-op wrapper can never
			// beat what it wraps (review 2026-08-12, round 3).
			best, tuneEv := policyzoo.SelectBest(fams[name], tb, tuningTasks, routerPick, resolveTuning)
			policies["zoo:"+name+"["+best.Desc+"]"] = policyzoo.PolicyOf(best, byID, routerPick, resolveTuning)
			zooEntries = append(zooEntries, ZooEntry{Family: name, Chosen: best.Desc,
				GridSize: len(fams[name]), TuningPassRate: tuneEv.PassRate, TuningClaudeFr: tuneEv.ClaudeFraction,
				TuningDiverged: diverged(best, tuningTasks), HeldoutDiverged: diverged(best, heldoutTasks)})
		}
	}

	refCfg := resolve("claude")
	ref := policyeval.Evaluate(tb, evalIDs, policyeval.Fixed(refCfg))
	// The reference is now a CONFIG, and a config can be unmeasured. When it
	// is, EVERY derived number below (quality ratio, its CI, the sign-flip p,
	// the non-inferiority verdict) is skipped by the `ref.PassRate > 0` guard
	// — silently, in the artifact's normal shape. That is the whole scorecard
	// answering nothing while looking like it answered.
	//
	// Reachable on day one, not hypothetically: the rank table ranks
	// claude-opus-4-8 first for the claude lane and the oracle has zero rows
	// for it, so the reference resolved to a cell with no evidence and all 56
	// verdicts vanished. Caught by running the deployed binary, not by a test.
	// Say it loudly on both channels rather than emitting a mute artifact.
	var refUnmeasured *ReferenceGap
	if ref.Unknown == len(evalIDs) && len(evalIDs) > 0 {
		msg := "the reference config has NO evidence in this oracle: every quality_ratio, CI, sign_flip_p and non_inferior_at_margin below is UNDEFINED, not computed. Measure this config (mr-goldreplay) or point -oracle at a run that covers it."
		if refCfg == (policyeval.Config{}) {
			// "||" is the ZERO config — policyeval.ParseConfig rejects it and
			// no replay can target it. Telling the operator to "measure this
			// config" prescribes an impossible action for the actual
			// condition, which is that the claude lane is not ranked at all
			// (review 2026-08-12, round 3).
			msg = fmt.Sprintf("the claude lane has no entry in the %s rank table, so the reference resolves to NOTHING ('||' is the zero config — no replay can measure it). Rank a claude config (rank-table override or seed), then measure it.", tblProvenance)
		}
		refUnmeasured = &ReferenceGap{
			Config:  refCfg.Key(),
			Tasks:   len(evalIDs),
			Message: msg,
		}
		fmt.Fprintf(os.Stderr, "WARNING: reference %s is unmeasured across all %d evaluated tasks — the non-inferiority verdict is UNDEFINED, not false.\n",
			refCfg.Key(), len(evalIDs))
	}
	var reports []PolicyReport
	for name, p := range policies {
		ev := policyeval.Evaluate(tb, evalIDs, p)
		cfgSeen := map[string]bool{}
		var cfgKeys []string
		for _, c := range ev.Assignment {
			if k := c.Key(); !cfgSeen[k] {
				cfgSeen[k] = true
				cfgKeys = append(cfgKeys, k)
			}
		}
		sort.Strings(cfgKeys)
		rep := PolicyReport{Policy: name, PassRate: ev.PassRate,
			ClaudeFraction: ev.ClaudeFraction, RCI: policyeval.RCI(ev.Assignment), Unknown: ev.Unknown,
			Configs: cfgKeys}
		// PAIRED on the tasks where BOTH arms have evidence. An unknown cell
		// scores 0 in Evaluate (correct there — it is counted, not imputed),
		// but feeding that 0 into a DIFFERENCE silently converts "we did not
		// measure the reference here" into "the reference failed here". Both
		// effects push the ratio and its CI lower bound UP, so the gate got
		// easier to pass exactly in proportion to how much reference data was
		// missing — a candidate measured half as good as the reference on
		// their shared tasks was certified non_inferior=true at p=0.04
		// (review 2026-08-12, reproduced on a synthetic oracle).
		//
		// The v0.28.1 guard keyed on ref.PassRate > 0 and only announced the
		// all-unknown case; it therefore missed BOTH the partial-coverage case
		// above and a reference that is fully measured and genuinely scores 0
		// (where a strictly better policy was reported as ratio 0.0, p 0.0).
		// Key on EVIDENCE, never on the score.
		paired := make([]float64, 0, len(evalIDs))
		var refPaired, evPaired float64
		for _, id := range evalIDs {
			if !ref.Measured[id] || !ev.Measured[id] {
				continue // no shared evidence on this task: it informs nothing
			}
			paired = append(paired, ev.PerTask[id]-ref.PerTask[id])
			refPaired += ref.PerTask[id]
			evPaired += ev.PerTask[id]
		}
		rep.PairedN = len(paired)
		// The verdict is TRI-STATE. Leaving NonInferior a plain bool made
		// "we could not compute this" and "measured, and it failed the gate"
		// the same byte — so the previous round moved the absent/zero
		// conflation off the inputs (now pointers) and straight onto the
		// output. A nil verdict means UNDEFINED and every consumer must
		// branch on it.
		switch {
		case rep.PairedN == 0:
			rep.VerdictUndefined = "no task has evidence for BOTH this policy and the reference"
		case rep.PairedN < minPairedN:
			// A single shared task yields a ZERO-WIDTH bootstrap CI (the
			// degenerate branch returns (d,d)), so a tie scores ci_lo exactly
			// 1.0 and the gate goes GREEN — silencing the weekly regression
			// alarm on the very oracle that has almost no shared evidence.
			// The margin was pre-registered for n≈55; refuse rather than
			// dress a 1-task coincidence as that verdict.
			rep.VerdictUndefined = fmt.Sprintf("only %d paired task(s); the margin is pre-registered for a full set and a CI this narrow is degenerate, not strong (minimum %d)", rep.PairedN, minPairedN)
		case refPaired == 0:
			// The reference was MEASURED and scored zero. A ratio against it
			// is a division by zero, not a failure — and a policy that beat
			// it 6-0 was being reported as non_inferior:false.
			rep.VerdictUndefined = "the reference is measured and scores 0 on every paired task: a quality RATIO against it is undefined (the candidate is not worse — there is nothing to divide by)"
		default:
			refMean := refPaired / float64(rep.PairedN)
			ratio := (evPaired / float64(rep.PairedN)) / refMean
			lo, hi := policyeval.BootstrapCI(paired, 0.95, *iters, 42)
			ciLo, ciHi := (refMean+lo)/refMean, (refMean+hi)/refMean
			p := policyeval.SignFlipP(paired, *iters, 42)
			rep.QualityRatio, rep.RatioCILo, rep.RatioCIHi, rep.SignFlipP = &ratio, &ciLo, &ciHi, &p
			verdict := ciLo >= 1-*margin && ev.ClaudeFraction < 1
			rep.NonInferior = &verdict
		}
		// A verdict computed on a handful of shared tasks is not the verdict
		// the margin was pre-registered for; say so rather than let a thin n
		// pass as the real thing.
		if rep.PairedN > 0 && rep.PairedN < len(evalIDs) {
			rep.PairedNote = fmt.Sprintf("verdict computed on the %d/%d tasks where BOTH this policy and the reference have evidence",
				rep.PairedN, len(evalIDs))
		}
		if *split && name == "oracle-best" {
			// Per-task argmax over the FULL table, scored on heldout cells it
			// has seen: the in-sample ceiling. Marked, and its verdict is
			// suppressed so the artifact can never present it as deployable.
			rep.InSample = true
			rep.NonInferior = nil
			rep.VerdictUndefined = "in-sample ceiling: this policy saw the cells it is scored on, so its verdict is not a deployability claim"
		}
		reports = append(reports, rep)
	}
	if *zoo {
		var router *PolicyReport
		for i := range reports {
			if reports[i].Policy == "router-live" {
				router = &reports[i]
			}
		}
		if router != nil {
			for i := range reports {
				if strings.HasPrefix(reports[i].Policy, "zoo:") {
					p := reports[i].PassRate >= router.PassRate && reports[i].ClaudeFraction <= router.ClaudeFraction &&
						(reports[i].PassRate > router.PassRate || reports[i].ClaudeFraction < router.ClaudeFraction)
					reports[i].ParetoVsRouter = &p
				}
			}
		}
	}
	// Stable order with a name tiebreak: zoo rows and router-live tie EXACTLY
	// on identical assignments, and a map-ordered slice would shuffle the
	// artifact between runs.
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].PassRate != reports[j].PassRate {
			return reports[i].PassRate > reports[j].PassRate
		}
		return reports[i].Policy < reports[j].Policy
	})

	// The note asserts only what this program computes. It previously
	// hard-coded "0 cap-blows recorded" — a measured-sounding value nothing in
	// either repo computes, records, or reads, on the artifact whose design
	// brief names cap-blow accounting as one of the two facts that must hold
	// (Q6 explicitly forbids asserting it uninstrumented; review 2026-08-12).
	note := "Q6 quota gate: throttles/defers during replay are graceful degradation, not violations (cap-blow accounting is not instrumented here; this artifact asserts nothing about it). Unknown cells are holes (e.g. a lane's unfilled window), counted and never imputed IN THE POLICY ROWS. The frontier is the ONE exception and says so: a task measured only on the claude side is swept with an imputed free base of 0, because excluding it would drop its measured claude evidence and break the curve's envelope over the policy rows — frontier_free_unmeasured_tasks counts exactly those, and while it is nonzero EVERY point on the curve is a LOWER BOUND, not a measurement — the max-budget point included, because the imputed 0 both depresses the base AND caps that task's withClaude, which is max(claude, free)."
	var splitInfo *SplitInfo
	if *split {
		var notRankable []string
		for cls, cfg := range classAssign {
			if cfg.Key() != "||" && !ranked[cfg.Key()] {
				notRankable = append(notRankable, cls+" -> "+cfg.Key())
			}
		}
		sort.Strings(notRankable)
		if len(notRankable) > 0 {
			fmt.Fprintf(os.Stderr, "WARNING: %d class assignment(s) name a config the rank table cannot dispatch: %v\n", len(notRankable), notRankable)
		}
		splitInfo = &SplitInfo{Mode: "tuning->heldout", TuningN: len(tuningIDs), HeldoutN: len(heldoutIDs),
			ClassAssignment: classAssign, NonRankable: notRankable, ClassCoverage: classCov}
		note += " SPLIT MODE: policies are scored on the heldout tasks only; oracle-best and the frontier are IN-SAMPLE ceilings (they see the heldout cells); class-oracle-tuned is the generalization test - derived on tuning only, over GOLD class labels (an idealized upper bound on class routing: production classifies via classify.go, which disagrees with gold labels on 48/56)."
		if *zoo {
			note += " ZOO: zoo:* rows are tuned on the tuning split only (config in zoo[].chosen_config); their heldout verdict needs non_inferior_at_margin AND pareto_vs_router for promotion."
		}
	}
	// config_coverage: every RANKED config with its evidence at both levels.
	// Emitted always — a ranked config with no evidence is the hole B15 gates
	// on (model level) or the gap the re-baseline must close (effort level),
	// and either way the operator must be able to see it without running the
	// canary.
	// Observations that match NO ranked config: with an all-zero coverage
	// table the operator cannot tell "the oracle is empty" from "the oracle
	// is full of cells nobody ranks" — which is exactly the day-one state.
	unranked := 0
	for k, n := range obsByConfig {
		if !ranked[k] {
			unranked += n
		}
	}
	cov := make([]ConfigCoverage, 0, len(rankedCfgs))
	for _, c := range rankedCfgs {
		n := obsByConfig[c.Key()]
		cov = append(cov, ConfigCoverage{Config: c.Key(), Observations: n,
			CoveredModel: obsByModel[c.Lane+"|"+c.Model], CoveredEffort: n > 0})
	}
	frontier, frontierUnmeasured, frontierFreeUnmeasured := policyeval.Frontier(tb, evalIDs)
	if frontierFreeUnmeasured > 0 {
		// The artifact's one imputation, said out loud on the noisy channel
		// too — a counter nobody reads discharges nothing (review round 4).
		fmt.Fprintf(os.Stderr, "WARNING: the frontier imputes a free-lane base of 0 for %d task(s) measured only on the claude side — EVERY point on the curve is a LOWER BOUND, not a measurement (frontier_free_unmeasured_tasks)\n", frontierFreeUnmeasured)
	}
	out := struct {
		Margin    float64 `json:"margin"`
		Ref       string  `json:"reference"`
		RefCfg    string  `json:"reference_config"`
		RankTable string  `json:"rank_table"`
		Malformed int     `json:"malformed_oracle_lines"`
		// Parsed rows the evidence rule dropped (holes: not dispatched,
		// deferred, error, exit-N, verify_error). Counted so an all-error
		// replay cannot produce an artifact byte-identical to an empty
		// oracle with every drop counter reading zero.
		NonEvidence int              `json:"non_evidence_oracle_rows"`
		Unranked    int              `json:"unranked_observations"`
		RefGap      *ReferenceGap    `json:"reference_unmeasured,omitempty"`
		Split       *SplitInfo       `json:"split,omitempty"`
		Zoo         []ZooEntry       `json:"zoo,omitempty"`
		Reports     []PolicyReport   `json:"policies"`
		Coverage    []ConfigCoverage `json:"config_coverage"`
		// Frontier rates share the policies' denominator (all evaluated
		// tasks); its budget axis spans only the tasks the sweep can place.
		Frontier []policyeval.FrontierPoint `json:"frontier"`
		// Tasks the frontier EXCLUDED for having no measured cell at all —
		// no evidence on either side, so nothing to place. Imputing them as
		// zeros made an all-holes table and an all-failures table produce
		// identical curves.
		FrontierUnmeasured int `json:"frontier_unmeasured_tasks"`
		// Tasks SWEPT WITH AN IMPUTED FREE BASE OF 0: measured on the claude
		// side only, so their free-lane value is unknown. They are NOT
		// excluded — dropping them discarded measured claude evidence from
		// the numerator while the shared denominator kept their slot, which
		// let a policy row exceed the curve's own maximum (the envelope the
		// frontier exists to draw). The imputation is the price of the
		// envelope, and THIS COUNT is what discharges it: when it is nonzero,
		// EVERY point on the curve is a LOWER BOUND ("at least this"), not a
		// measurement — the max-budget point included. The imputed 0 works
		// twice: it depresses the base, AND it caps that task's withClaude,
		// which is max(claude, free) — so even the "spend claude everywhere"
		// end of the curve can understate the truth.
		FrontierFreeUnmeasured int    `json:"frontier_free_unmeasured_tasks"`
		Note                   string `json:"note"`
	}{*margin, "always-claude", refCfg.Key(), tblProvenance, malformed, nonEvidence, unranked, refUnmeasured, splitInfo, zooEntries, reports, cov, frontier, frontierUnmeasured, frontierFreeUnmeasured, note}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}

// SplitInfo is the B'2 cross-validation block, emitted only with -split.
type SplitInfo struct {
	Mode            string                       `json:"mode"`
	TuningN         int                          `json:"tuning_n"`
	HeldoutN        int                          `json:"heldout_n"`
	ClassAssignment map[string]policyeval.Config `json:"class_assignment"`
	// NonRankable lists class assignments naming a config the ACTIVE rank
	// table cannot dispatch. ClassBest picks from the TABLE (anything
	// measured) while routing can only emit what is RANKED, and nothing
	// reconciled the two: the headline generalization candidate recommended
	// five configs that are not deployable and the artifact presented them
	// as an assignment (review 2026-08-12).
	NonRankable   []string                 `json:"class_assignment_not_rankable,omitempty"`
	ClassCoverage policyeval.ClassCoverage `json:"class_coverage"`
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
func liveRouterPolicy(bin string, promptOf map[string]string, liveQuota bool, resolve func(string) policyeval.Config) (policyeval.Policy, error) {
	stateOverride := "" // set when probing in a neutral state dir
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
		stateOverride = dir
	}
	cfgFor := map[string]policyeval.Config{}
	for task, prompt := range promptOf {
		cmd := exec.Command(bin, "route", "-desc", prompt, "-no-receipt")
		// R10 (canary B13): even a read-only child gets a scrubbed environment.
		// The neutral-state variant EXTENDS this rather than replacing it — the
		// old shape assigned cmd.Env twice, and the second assignment named a
		// variable, so neither a reader nor the canary could tell a scrubbed
		// override from an ambient one. Same bytes, provably scrubbed.
		cmd.Env = childenv.Scrub(os.Environ())
		if stateOverride != "" {
			cmd.Env = append(cmd.Env, "MR_ORCH_STATE="+stateOverride)
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
		// The router decides a full CONFIG and says so — lane, model and effort
		// are three of route's six contract keys. Parsing only the lane and
		// re-deriving a config would collapse a per-class (lane, model, effort)
		// decision onto one global per-lane cell, scoring a verify-gate task
		// the router dispatches as claude|sonnet-5|medium against
		// claude|opus-4-8|high. That is "a model swap silently inherits its
		// predecessor's history" reintroduced at this seam — the precise thing
		// the config key exists to prevent (review 2026-08-12).
		var r struct {
			Lane   string `json:"lane"`
			Model  string `json:"model"`
			Effort string `json:"effort"`
		}
		if err := json.Unmarshal(out, &r); err != nil || r.Lane == "" {
			return nil, fmt.Errorf("route %s: unparseable", task)
		}
		if r.Model != "" {
			cfgFor[task] = policyeval.Config{Lane: r.Lane, Model: r.Model,
				Effort: policyeval.NormalizeEffort(r.Effort)}
		} else {
			// Older binary with no model in the contract: fall back to the
			// lane-level resolution rather than inventing a cell.
			cfgFor[task] = resolve(r.Lane)
		}
	}
	return func(task string) policyeval.Config { return cfgFor[task] }, nil
}
