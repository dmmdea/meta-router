// Package dispatch is the append-only JSONL dispatch log. Rich dispatch
// records future-proof counterfactual replay and any v4 bandit (design brief);
// slice 2 extends Record with origin / recommendation-vs-action / deviation
// fields (intent spec §6c adherence metrics). Treat as an asset, not plumbing.
package dispatch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type Record struct {
	TS   time.Time `json:"ts"`
	Lane string    `json:"lane"`
	// Model is the REQUESTED model/alias; AttributedModels are the models that
	// actually answered (from modelUsage) — the silent-fallback detection
	// signal must survive into the replay substrate, not just the parser.
	Model            string   `json:"model"`
	AttributedModels []string `json:"attributed_models,omitempty"`
	Rule             string   `json:"rule,omitempty"`
	OutcomeClass     string   `json:"outcome_class"`
	Admit            bool     `json:"admit"`
	AdmitState       string   `json:"admit_state"`
	AdmitReason      string   `json:"admit_reason,omitempty"`
	TokensIn         int64    `json:"tokens_in"`
	TokensOut        int64    `json:"tokens_out"`
	NumTurns         int      `json:"num_turns"`
	NotionalUSD      float64  `json:"notional_usd"`
	// CashUSD is actual cash settled for this dispatch — structurally 0 for
	// subscription lanes (R10). NotionalUSD remains the VALUATION column; the
	// split is the W8 cash-vs-valuation carry-over (claudexor ledger lineage).
	CashUSD float64 `json:"cash_usd"`
	// PaceSlack is the recommended/dispatched lane's binding pace slack at
	// decision time (W1; nil = unknown) — the calibration substrate for any
	// future slack-conditioned policy.
	PaceSlack *float64 `json:"pace_slack,omitempty"`
	// Subject is the credential subject that carried the dispatch (W2; ""=
	// default). RotationFrom + RotationReason are populated only when the
	// selected subject was NOT the registry-first one — the typed-limit
	// rotation provenance (claudexor discipline: never a network error).
	Subject        string `json:"subject,omitempty"`
	RotationFrom   string `json:"rotation_from,omitempty"`
	RotationReason string `json:"rotation_reason,omitempty"`

	// Adherence fields (RS9 / §6c): origin, recommendation-vs-action, and
	// deviation make delegation obedience COUNTABLE. All omitempty — old JSONL
	// lines keep unmarshalling; the log stays append-compatible.
	Origin          string `json:"origin,omitempty"` // "cli"|"mcp"|"route"|"nightshift"|""
	TaskClass       string `json:"task_class,omitempty"`
	RecLane         string `json:"rec_lane,omitempty"`  // what the oracle said
	RecModel        string `json:"rec_model,omitempty"` //
	RecRule         string `json:"rec_rule,omitempty"`  // rank-table rule that fired
	Deviated        bool   `json:"deviated,omitempty"`  // action != recommendation
	DeviationReason string `json:"deviation_reason,omitempty"`
	// E2 spend-down provenance (slice-4): the batch tag on the consult and the
	// boost the winning recommendation carried — boost-influenced decisions
	// must be countable from receipts (the spend_down_* calibration substrate).
	// Both omitempty: old JSONL lines keep unmarshalling.
	Batch          bool `json:"batch,omitempty"`
	SpendDownBoost int  `json:"spend_down_boost,omitempty"`

	// Replay substrate (S2R-9): without desc, slice-4 replay and gold-set
	// harvesting from receipts are amputated at birth; quality is the operator
	// verdict channel (`mr-orchestrate feedback <ts> good|bad`).
	Desc    string `json:"desc,omitempty"`    // task text or prompt-hash/file ref
	Quality string `json:"quality,omitempty"` // "good"|"bad"

	// Strategy fields (slice 3 / §6c async): tie a receipt to a strategy DAG node.
	// All omitempty — old JSONL lines keep unmarshalling; a non-strategy receipt has
	// an empty DispatchID and is never read as a step. StepID is plain int (see plan
	// Task 1 decision): consumers filter DispatchID first, so StepID:0 is unambiguous.
	DispatchID string `json:"dispatch_id,omitempty"`
	StepID     int    `json:"step_id,omitempty"`
	Deps       []int  `json:"deps,omitempty"`
	Attempt    int    `json:"attempt,omitempty"` // re-lane retry # (0 = first attempt)
}

// Append writes one record as a JSONL line, creating the parent dir as needed.
func Append(path string, r Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}
