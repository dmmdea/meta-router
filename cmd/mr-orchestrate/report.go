package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
)

// report (W7b) is the receipts-driven operator dashboard: a pure render of
// dispatch.jsonl. No network, no ledger transaction, no clock — every number,
// including the activity window, derives from the receipts themselves (the
// newest receipt's timestamp is the anchor), so a fixed log renders an
// identical report on every run, forever. That is the acceptance contract:
// renders from real receipts; no network; deterministic on fixed input.
//
// Taxonomy is audit.go's (S2R-1/S3R-4): runs exclude consults, egress
// refusals, and strategy steps. The LANE table is deliberately broader — it
// aggregates everything that consumed tokens (runs AND strategy steps),
// because burn is its point; adherence percentages stay on the audit
// definitions via the embedded ReceiptsSummary.

// LaneReport aggregates one lane's executed work (runs + strategy steps).
type LaneReport struct {
	Runs          int     `json:"runs"`
	StrategySteps int     `json:"strategy_steps,omitempty"`
	TokensIn      int64   `json:"tokens_in"`
	TokensOut     int64   `json:"tokens_out"`
	NotionalUSD   float64 `json:"notional_usd"`
	CashUSD       float64 `json:"cash_usd"`
	Deviated      int     `json:"deviated,omitempty"`
	EgressDenied  int     `json:"egress_denied,omitempty"`
	Good          int     `json:"good,omitempty"`
	Bad           int     `json:"bad,omitempty"`
}

// Report is the full dashboard. Maps marshal with sorted keys (Go JSON), and
// the text renderer sorts explicitly, so both outputs are deterministic.
type Report struct {
	From       time.Time `json:"from"`        // oldest rendered receipt
	To         time.Time `json:"to"`          // newest receipt — the window anchor
	WindowDays int       `json:"window_days"` // 0 = whole log
	Lane       string    `json:"lane,omitempty"`

	Receipts     int `json:"receipts"`                // rendered (post window/lane filter)
	SkippedLines int `json:"skipped_lines,omitempty"` // unparseable JSONL lines — surfaced, never silent

	Consults      int `json:"consults"`
	Runs          int `json:"runs"`
	StrategySteps int `json:"strategy_steps"`
	StrategyRuns  int `json:"strategy_runs"` // distinct dispatch_ids
	Retries       int `json:"retries"`       // receipts with attempt > 0
	EgressDenied  int `json:"egress_denied"`

	Lanes    map[string]*LaneReport `json:"lanes"`
	Outcomes map[string]int         `json:"outcomes"` // outcome_class → count (runs+steps)
	Models   map[string]int         `json:"models"`   // requested model → count (runs+steps)

	// Silent-fallback signal: receipts whose AttributedModels answer set is not
	// exactly the requested model. Pairs are "requested→actual" for the table.
	SilentFallbacks int            `json:"silent_fallbacks"`
	FallbackPairs   map[string]int `json:"fallback_pairs,omitempty"`

	RotationsByReason map[string]int `json:"rotations_by_reason,omitempty"` // W2 typed-limit rotations
	BatchConsults     int            `json:"batch_consults,omitempty"`      // E2 spend-down
	BoostInfluenced   int            `json:"boost_influenced,omitempty"`

	Unrated int            `json:"unrated_runs"` // runs with no operator quality verdict
	ByDay   map[string]int `json:"by_day"`       // UTC date → receipts rendered

	Adherence ReceiptsSummary `json:"adherence"` // S2R-10 coverage/obedience block
}

// fallbackPair reports whether the answering models differ from the request,
// and the "requested→actual" label when they do.
func fallbackPair(r dispatch.Record) (string, bool) {
	if len(r.AttributedModels) == 0 {
		return "", false // no attribution recorded — not evidence of a fallback
	}
	if len(r.AttributedModels) == 1 && r.AttributedModels[0] == r.Model {
		return "", false
	}
	return r.Model + "→" + strings.Join(r.AttributedModels, "+"), true
}

// buildReport is the pure core (unit-tested directly). days=0 means the whole
// log; otherwise the window is [newest-TS − days·24h, newest-TS] — anchored to
// the receipts, never the wall clock. lane="" means all lanes.
func buildReport(recs []dispatch.Record, skipped, days int, lane string) Report {
	rep := Report{
		WindowDays:   days,
		Lane:         lane,
		SkippedLines: skipped,
		Lanes:        map[string]*LaneReport{},
		Outcomes:     map[string]int{},
		Models:       map[string]int{},
		ByDay:        map[string]int{},
	}
	if len(recs) == 0 {
		rep.Adherence = summarizeReceipts(nil)
		return rep
	}
	anchor := recs[0].TS
	for _, r := range recs {
		if r.TS.After(anchor) {
			anchor = r.TS
		}
	}
	cutoff := time.Time{}
	if days > 0 {
		cutoff = anchor.Add(-time.Duration(days) * 24 * time.Hour)
	}
	var in []dispatch.Record
	for _, r := range recs {
		if days > 0 && r.TS.Before(cutoff) {
			continue
		}
		if lane != "" && r.Lane != lane {
			continue
		}
		in = append(in, r)
	}
	rep.Adherence = summarizeReceipts(in)
	if len(in) == 0 {
		return rep
	}
	rep.From, rep.To = in[0].TS, in[0].TS
	strategyIDs := map[string]bool{}
	for _, r := range in {
		if r.TS.Before(rep.From) {
			rep.From = r.TS
		}
		if r.TS.After(rep.To) {
			rep.To = r.TS
		}
		rep.Receipts++
		rep.ByDay[r.TS.UTC().Format("2006-01-02")]++
		if r.Attempt > 0 {
			rep.Retries++
		}
		if r.DispatchID != "" {
			strategyIDs[r.DispatchID] = true
		}
		switch {
		case r.OutcomeClass == "route_recommendation":
			rep.Consults++
			if r.Batch {
				rep.BatchConsults++
			}
			if r.SpendDownBoost > 0 {
				rep.BoostInfluenced++
			}
			continue // consults ran nothing; they stay out of lane/model tables
		case r.OutcomeClass == "egress_denied":
			rep.EgressDenied++
			lr := laneRep(rep.Lanes, r.Lane)
			lr.EgressDenied++
			continue // a refused export ran nothing (audit 2026-07-25)
		}
		// Everything below EXECUTED (interactive run or strategy step): it
		// consumed tokens, so it enters the lane/model/outcome aggregates.
		lr := laneRep(rep.Lanes, r.Lane)
		lr.TokensIn += r.TokensIn
		lr.TokensOut += r.TokensOut
		lr.NotionalUSD += r.NotionalUSD
		lr.CashUSD += r.CashUSD
		if r.Model != "" {
			rep.Models[r.Model]++
		}
		if r.OutcomeClass != "" {
			rep.Outcomes[r.OutcomeClass]++
		}
		if pair, ok := fallbackPair(r); ok {
			rep.SilentFallbacks++
			if rep.FallbackPairs == nil {
				rep.FallbackPairs = map[string]int{}
			}
			rep.FallbackPairs[pair]++
		}
		if r.RotationReason != "" {
			if rep.RotationsByReason == nil {
				rep.RotationsByReason = map[string]int{}
			}
			rep.RotationsByReason[r.RotationReason]++
		}
		if r.Origin == "strategy" {
			rep.StrategySteps++
			lr.StrategySteps++
			continue // quality/deviation live on interactive runs
		}
		rep.Runs++
		lr.Runs++
		if r.Deviated {
			lr.Deviated++
		}
		switch r.Quality {
		case "good":
			lr.Good++
		case "bad":
			lr.Bad++
		default:
			rep.Unrated++
		}
	}
	rep.StrategyRuns = len(strategyIDs)
	return rep
}

func laneRep(m map[string]*LaneReport, lane string) *LaneReport {
	if lane == "" {
		lane = "(none)"
	}
	if m[lane] == nil {
		m[lane] = &LaneReport{}
	}
	return m[lane]
}

// sortedKeys returns map keys sorted so the text render is deterministic.
func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// countLine renders "k v · k v · …" over sorted keys.
func countLine(m map[string]int) string {
	parts := make([]string, 0, len(m))
	for _, k := range sortedKeys(m) {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}

// renderReport writes the human dashboard (pure; unit-tested against fixed
// input for byte-stable output).
func renderReport(w io.Writer, r Report) {
	span := "whole log"
	if r.WindowDays > 0 {
		span = fmt.Sprintf("last %dd (anchored to newest receipt)", r.WindowDays)
	}
	scope := ""
	if r.Lane != "" {
		scope = " · lane " + r.Lane
	}
	fmt.Fprintf(w, "mr-orchestrate report — %s%s\n", span, scope)
	if r.Receipts == 0 {
		fmt.Fprintln(w, "no receipts in scope")
		if r.SkippedLines > 0 {
			fmt.Fprintf(w, "WARN: %d unparseable line(s) skipped — the log holds data this report cannot read\n", r.SkippedLines)
		}
		return
	}
	fmt.Fprintf(w, "%s → %s\n", r.From.UTC().Format(time.RFC3339), r.To.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "receipts %d · consults %d · runs %d · strategy %d step(s) in %d run(s) · retries %d · egress denied %d\n",
		r.Receipts, r.Consults, r.Runs, r.StrategySteps, r.StrategyRuns, r.Retries, r.EgressDenied)
	if r.SkippedLines > 0 {
		fmt.Fprintf(w, "WARN: %d unparseable line(s) skipped — the log holds data this report cannot read\n", r.SkippedLines)
	}
	fmt.Fprintf(w, "\n%-10s %6s %6s %10s %10s %9s %8s %5s %5s %9s\n",
		"LANE", "RUNS", "STEPS", "TOK-IN", "TOK-OUT", "NOTIONAL", "CASH", "DEV", "G/B", "EGR-DENY")
	for _, lane := range sortedKeys(r.Lanes) {
		lr := r.Lanes[lane]
		fmt.Fprintf(w, "%-10s %6d %6d %10d %10d %9.2f %8.2f %5d %2d/%-2d %9d\n",
			lane, lr.Runs, lr.StrategySteps, lr.TokensIn, lr.TokensOut, lr.NotionalUSD, lr.CashUSD,
			lr.Deviated, lr.Good, lr.Bad, lr.EgressDenied)
	}
	if len(r.Outcomes) > 0 {
		fmt.Fprintf(w, "\noutcomes:  %s\n", countLine(r.Outcomes))
	}
	if len(r.Models) > 0 {
		fmt.Fprintf(w, "models:    %s\n", countLine(r.Models))
	}
	if r.SilentFallbacks > 0 {
		fmt.Fprintf(w, "SILENT FALLBACKS: %d — %s\n", r.SilentFallbacks, countLine(r.FallbackPairs))
	}
	if len(r.RotationsByReason) > 0 {
		fmt.Fprintf(w, "rotations: %s\n", countLine(r.RotationsByReason))
	}
	if r.BatchConsults > 0 || r.BoostInfluenced > 0 {
		fmt.Fprintf(w, "spend-down: %d batch consult(s), %d boost-influenced\n", r.BatchConsults, r.BoostInfluenced)
	}
	fmt.Fprintf(w, "quality:   %d unrated run(s)\n", r.Unrated)
	fmt.Fprintf(w, "adherence: coverage %.1f%% · obedience %.1f%% (consulted %d of %d runs)\n",
		r.Adherence.CoveragePct, r.Adherence.ObediencePct, r.Adherence.ConsultedRuns, r.Adherence.RunReceipts)
	if len(r.Adherence.DeviationsByReason) > 0 {
		fmt.Fprintf(w, "deviations: %s\n", countLine(r.Adherence.DeviationsByReason))
	}
	fmt.Fprintf(w, "\nactivity (UTC):\n")
	for _, day := range sortedKeys(r.ByDay) {
		fmt.Fprintf(w, "  %s %5d\n", day, r.ByDay[day])
	}
}

// loadReceiptsCounted reads dispatch.jsonl distinguishing the three states the
// dashboard must not conflate (clean-ship: absent vs could-not-determine):
// a missing file is (nil, 0, fs.ErrNotExist) — a normal "nothing yet" state;
// an unreadable file or torn read returns the real error — the caller must
// NOT render a partial dashboard as if it were the whole log; unparseable
// individual lines are counted and surfaced, never silently dropped.
func loadReceiptsCounted(path string) (recs []dispatch.Record, skipped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r dispatch.Record
		if json.Unmarshal(line, &r) != nil {
			skipped++
			continue
		}
		recs = append(recs, r)
	}
	if serr := sc.Err(); serr != nil {
		return nil, 0, fmt.Errorf("dispatch log read stopped early (a partial report would misrepresent the log): %w", serr)
	}
	return recs, skipped, nil
}

func runReport(args []string) error {
	fs2 := flag.NewFlagSet("report", flag.ExitOnError)
	days := fs2.Int("days", 0, "window in days, anchored to the newest receipt (0 = whole log)")
	lane := fs2.String("lane", "", "restrict to one lane")
	asJSON := fs2.Bool("json", false, "emit the report as JSON instead of the text dashboard")
	_ = fs2.Parse(args)
	if *days < 0 {
		return errors.New("-days must be >= 0")
	}
	recs, skipped, err := loadReceiptsCounted(dispatchPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Println("no receipts yet: dispatch log absent at", dispatchPath())
			return nil
		}
		return err
	}
	rep := buildReport(recs, skipped, *days, *lane)
	if *asJSON {
		out, jerr := json.MarshalIndent(rep, "", "  ")
		if jerr != nil {
			return jerr
		}
		fmt.Println(string(out))
		return nil
	}
	renderReport(os.Stdout, rep)
	return nil
}
