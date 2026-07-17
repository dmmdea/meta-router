package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/calib"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
)

var defaultThresholds = admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95}

type LaneStatus struct {
	State         string          `json:"state"`
	Reason        string          `json:"reason,omitempty"`
	ResumeAt      *time.Time      `json:"resume_at,omitempty"`
	Windows       []ledger.Bucket `json:"windows"`
	BurnDownshift int             `json:"burn_downshift,omitempty"` // E1: 0-3, >=2 demotes in Route
}

type Status struct {
	Lanes            map[string]LaneStatus `json:"lanes"`
	ActiveFuses      []fuses.Fuse          `json:"active_fuses"`
	BillingMode      string                `json:"claude_billing_mode"`
	PolicyAlert      json.RawMessage       `json:"policy_alert,omitempty"`
	PolicyWatchStale string                `json:"policy_watch_stale,omitempty"`
	CodexAlert       json.RawMessage       `json:"codex_alert,omitempty"`  // burn-anomaly latch (Task 4)
	GLMAlert         json.RawMessage       `json:"glm_alert,omitempty"`    // 1313 hard-stop latch (Task 6)
	Receipts         *ReceiptsSummary      `json:"receipts,omitempty"`     // S2R-10 audit block (additive JSON)
	QuotaHealth      *QuotaHealth          `json:"quota_health,omitempty"` // E6 signal-liveness block
}

// QuotaHealth is the E6 surface: is the quota signal ALIVE? A stale trace means
// capacity fitting has no rows (calib.Fit silently never learns); stale provider
// buckets mean the statusline tee stopped or the schema drifted. Structural
// fallback already exists (shadow governance) — this block makes the degradation
// VISIBLE instead of silent.
type QuotaHealth struct {
	TraceExists  bool     `json:"trace_exists"`
	TraceRows    int      `json:"trace_rows"`
	TraceStale   bool     `json:"trace_stale"`
	TraceNote    string   `json:"trace_note,omitempty"`
	StaleBuckets []string `json:"stale_buckets,omitempty"`
}

func buildQuotaHealth(bs []ledger.Bucket, tracePath string, cfg orchcfg.Config, now time.Time) *QuotaHealth {
	staleAfter := time.Duration(cfg.QuotaStaleHours) * time.Hour
	if staleAfter <= 0 {
		staleAfter = 48 * time.Hour
	}
	th := calib.AssessTrace(tracePath, now, staleAfter)
	qh := &QuotaHealth{TraceExists: th.Exists, TraceRows: th.Rows, TraceStale: th.Stale,
		StaleBuckets: quotasig.StaleBuckets(bs, now, staleAfter)}
	if th.Rows == 0 {
		qh.TraceNote = "statusline tee latent: quota-trace.jsonl has no rows — capacity fitting (calib.Fit) has nothing to learn from"
	} else if th.Stale {
		qh.TraceNote = "quota trace stalled: last row " + th.LastTS.UTC().Format(time.RFC3339)
	}
	return qh
}

// buildStatus is the pure core of the status command (unit-tested directly).
func buildStatus(bs []ledger.Bucket, fs []fuses.Fuse, cfg orchcfg.Config, now time.Time, down map[string]int) Status {
	byLane := map[string][]ledger.Bucket{}
	for _, b := range bs {
		byLane[b.Lane] = append(byLane[b.Lane], b)
	}
	st := Status{Lanes: map[string]LaneStatus{}, ActiveFuses: fuses.Active(fs, now), BillingMode: cfg.ClaudeBillingMode}
	for lane, buckets := range byLane {
		d := admission.Decide(bs, lane, now, defaultThresholds)
		ls := LaneStatus{State: string(d.State), Reason: d.Reason, Windows: buckets, BurnDownshift: down[lane]}
		if !d.ResumeAt.IsZero() {
			t := d.ResumeAt
			ls.ResumeAt = &t
		}
		st.Lanes[lane] = ls
	}
	return st
}

// maybeFit is SetCapacity's production caller (Task 8, fact-refresh gap #1):
// fit the claude windows from quota-trace calibration samples; apply a fit
// when it differs >10% from the current cap (or none is learned yet). A
// within-10% re-fit is left alone — versioned churn on every status run would
// drown the CapVersion history. Returns the notes for applied fits; the
// caller prints them to STDERR: status stdout is a machine contract (pure
// JSON), so the plan's "stdout note" lands on stderr (adjudicated, recorded
// in the group-D evidence).
func maybeFit(l *ledger.Ledger, samples []calib.Sample, now time.Time) []string {
	var notes []string
	for _, w := range []ledger.WindowKind{ledger.Win5h, ledger.Win7d} {
		capTok, n, ok := calib.Fit(samples, "claude", w, calib.Defaults())
		if !ok {
			continue
		}
		cur := int64(0)
		if b, okb := l.Bucket("claude", w); okb {
			cur = b.CapTokens
		}
		if cur > 0 && math.Abs(float64(capTok-cur)) <= 0.10*float64(cur) {
			continue
		}
		l.SetCapacity("claude", w, capTok)
		notes = append(notes, fmt.Sprintf("capacity fitted claude/%s = %d tokens (n=%d samples)", w, capTok, n))
	}
	return notes
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	_ = fs.Bool("json", true, "emit JSON (always on; flag kept for interface stability)")
	_ = fs.Parse(args)

	now := time.Now().UTC()
	// RS1: every invocation ingests the statusline drop so interactive Claude
	// usage is visible. The write goes through the cross-process Update
	// transaction; corrupt drops are logged and ignored (fail-open).
	var snap []ledger.Bucket
	err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		if _, note, ierr := quotasig.IngestTraced(l, dropPath(), quotaTracePath(), "claude", now); ierr != nil {
			fmt.Fprintln(os.Stderr, "warn: statusline drop unreadable:", ierr)
		} else if note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
		// Task 8: fitting rides the same transaction — the trace is loaded
		// AFTER the ingest above so the freshest observation participates.
		// Status runs on every invocation + the nightly, so fitting follows
		// the tee data with zero new processes.
		for _, note := range maybeFit(l, calib.Load(quotaTracePath()), now) {
			fmt.Fprintln(os.Stderr, note)
		}
		snap = l.Snapshot()
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "warn: ledger update failed, reporting a read-only snapshot:", err)
		l, warn := ledger.OpenChecked(ledgerPath())
		if warn != "" {
			fmt.Fprintln(os.Stderr, "warn:", warn)
		}
		snap = l.Snapshot()
	}
	fzs, _ := fuses.Load(fusesPath())
	cfg := orchcfg.Load(configPath())
	down := burnDownshiftByLane(snap, calib.Load(quotaTracePath()), cfg, now)
	st := buildStatus(snap, fzs, cfg, now, down)
	st.QuotaHealth = buildQuotaHealth(snap, quotaTracePath(), cfg, now)
	if raw, err := os.ReadFile(policyAlertPath()); err == nil && json.Valid(raw) {
		st.PolicyAlert = raw
		// Watch-staleness alarm: a nightly that silently stops running is
		// itself an alert condition (the F18 class).
		var ps policyState
		if json.Unmarshal(raw, &ps) == nil && !ps.CheckedAt.IsZero() {
			if age := now.Sub(ps.CheckedAt); age > 50*time.Hour {
				st.PolicyWatchStale = fmt.Sprintf("policy watch last ran %.0fh ago (>50h): the nightly task is not firing", age.Hours())
			}
		}
	}
	// Codex burn-anomaly latch: raw JSON passthrough (policy-alert pattern) —
	// cleared only by `probe --ack-codex`.
	if raw, err := os.ReadFile(codexAlertPath()); err == nil && json.Valid(raw) {
		st.CodexAlert = raw
	}
	// GLM 1313 hard-stop latch: same passthrough — cleared only by
	// `probe --ack-glm`.
	if raw, err := os.ReadFile(glmAlertPath()); err == nil && json.Valid(raw) {
		st.GLMAlert = raw
	}
	// S2R-10 receipts audit block: coverage/obedience/deviation/per-lane counts
	// from dispatch.jsonl. Additive JSON on the machine contract — stdout stays
	// pure JSON. Fail-open: an unreadable/corrupt log yields an all-zero summary,
	// never a crash.
	rs := summarizeReceipts(loadReceipts(dispatchPath()))
	st.Receipts = &rs
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}
