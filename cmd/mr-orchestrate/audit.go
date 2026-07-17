package main

import (
	"bufio"
	"encoding/json"
	"math"
	"os"

	"github.com/dmmdea/meta-router/internal/orch/dispatch"
)

// ReceiptsSummary is the S2R-10 audit block: a first-class, machine-readable
// summary of the dispatch log, added to `mr-orchestrate status`. It answers the
// §6c adherence questions FROM RECEIPTS ALONE.
//
// coverage_pct uses the S2R-1 definition — run receipts whose origin is
// Origin-tagged {mcp, route} over ALL run receipts. It DELIBERATELY does not use
// `rec_lane != ""` (Task 11 makes that ~100% by construction — the circular
// metric S2R-1 kills). The missed-delegation denominator (interactive work the
// brain COULD have delegated but didn't) is not knowable from receipts; it stays
// slice-4 burn-reconciliation work — the Note says so.
type ReceiptsSummary struct {
	RunReceipts        int            `json:"run_receipts"`         // outcome_class != route_recommendation
	OriginTagged       int            `json:"origin_tagged"`        // run receipts with origin ∈ {mcp, route}
	CoveragePct        float64        `json:"coverage_pct"`         // OriginTagged / RunReceipts (S2R-1)
	ConsultedRuns      int            `json:"consulted_runs"`       // run receipts with rec_lane set
	ObediencePct       float64        `json:"obedience_pct"`        // of consulted runs, fraction NOT deviated
	DeviationsByReason map[string]int `json:"deviations_by_reason"` // deviation_reason → count
	DispatchByLane     map[string]int `json:"dispatch_by_lane"`     // lane → run-receipt count
	Consults           int            `json:"consults"`             // route_recommendation receipts (context)
	Note               string         `json:"note"`
}

// originTagged reports whether an origin counts toward S2R-1 coverage. Valid
// origins are cli|mcp|route|nightshift|strategy; only mcp+route are the
// delegation surfaces that coverage credits (strategy is internal orchestration,
// excluded from the run-receipt aggregation entirely — see summarizeReceipts).
func originTagged(origin string) bool {
	return origin == "mcp" || origin == "route"
}

// round1 rounds to one decimal so the percentages read cleanly in JSON.
func round1(f float64) float64 { return math.Round(f*10) / 10 }

// summarizeReceipts computes the audit block from a receipt slice (pure — the
// unit-tested core). Maps are always non-nil so the JSON shape is stable.
func summarizeReceipts(recs []dispatch.Record) ReceiptsSummary {
	s := ReceiptsSummary{
		DeviationsByReason: map[string]int{},
		DispatchByLane:     map[string]int{},
		Note:               "coverage_pct = origin-tagged {mcp,route} run receipts / all run receipts (S2R-1); the missed-delegation denominator (work delegable but kept in-session) is not receipt-visible and stays slice-4 burn reconciliation.",
	}
	notDeviated := 0
	for _, r := range recs {
		if r.OutcomeClass == "route_recommendation" {
			s.Consults++
			continue // consults are not run receipts
		}
		// S3R-4: a strategy DAG step is INTERNAL orchestration, not an
		// interactive delegation the brain "could have delegated but didn't" —
		// so it enters NEITHER the coverage numerator NOR the denominator (and
		// no other run-receipt aggregate). Otherwise every headless strategy run
		// would silently drag system-wide CoveragePct down, the opposite of the
		// brief §2.4 claim. The step's obedience/quality signal still lives on
		// its receipt (DispatchID/StepID tagged) for slice-4's promotion corpus.
		if r.Origin == "strategy" {
			continue
		}
		s.RunReceipts++
		if originTagged(r.Origin) {
			s.OriginTagged++
		}
		if r.Lane != "" {
			s.DispatchByLane[r.Lane]++
		}
		if r.RecLane != "" {
			s.ConsultedRuns++
			if !r.Deviated {
				notDeviated++
			}
		}
		if r.Deviated && r.DeviationReason != "" {
			s.DeviationsByReason[r.DeviationReason]++
		}
	}
	if s.RunReceipts > 0 {
		s.CoveragePct = round1(100 * float64(s.OriginTagged) / float64(s.RunReceipts))
	}
	if s.ConsultedRuns > 0 {
		s.ObediencePct = round1(100 * float64(notDeviated) / float64(s.ConsultedRuns))
	}
	return s
}

// loadReceipts reads and parses dispatch.jsonl, skipping corrupt lines
// (fail-open — a torn line must never break status). A missing file yields an
// empty slice (nothing dispatched yet is a normal state, not an error).
func loadReceipts(path string) []dispatch.Record {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []dispatch.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r dispatch.Record
		if json.Unmarshal(line, &r) != nil {
			continue // corrupt line: skip, never fatal
		}
		out = append(out, r)
	}
	return out
}
