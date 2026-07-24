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
	"github.com/dmmdea/meta-router/internal/orch/pace"
	"github.com/dmmdea/meta-router/internal/orch/profiles"
	"github.com/dmmdea/meta-router/internal/orch/quotapoll"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

var defaultThresholds = admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95}

type LaneStatus struct {
	State         string          `json:"state"`
	Reason        string          `json:"reason,omitempty"`
	ResumeAt      *time.Time      `json:"resume_at,omitempty"`
	Windows       []ledger.Bucket `json:"windows"`
	BurnDownshift int             `json:"burn_downshift,omitempty"` // E1: 0-3, >=2 demotes in Route
	SpendDown     int             `json:"spend_down,omitempty"`     // E2: armed latch level a batch consult would boost by (pre fit-gate)
	PaceSlack     *float64        `json:"pace_slack,omitempty"`     // W1: binding pace slack (elapsed-fraction − used-ratio, min over known windows)
	// Subjects is the W2 per-credential-subject breakdown, present ONLY when a
	// lane has >1 provisioned subject (single-account output stays identical).
	// The top-level fields above always describe the DEFAULT subject.
	Subjects map[string]SubjectStatus `json:"subjects,omitempty"`
}

// SubjectStatus is one credential subject's window state (W2).
type SubjectStatus struct {
	State       string          `json:"state"`
	Reason      string          `json:"reason,omitempty"`
	Provisioned bool            `json:"provisioned"`
	Windows     []ledger.Bucket `json:"windows"`
	PaceSlack   *float64        `json:"pace_slack,omitempty"`
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
	QuotaAbsences    []quotapoll.Absence   `json:"quota_absences,omitempty"` // W1: typed poll absences — stated, never inferred
	ScopedAlerts     json.RawMessage       `json:"scoped_alerts,omitempty"`  // W1: critical/warning scoped-limit latch (vendor-refreshed)
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
// sd is the E2 armed-latch view (spendDownArmedByLane); nil when off. reg is
// the profile registry (nil → implicit single default subject). The top-level
// lane block always describes the DEFAULT subject; extra subjects appear under
// Subjects only when a lane has more than one provisioned subject.
func buildStatus(bs []ledger.Bucket, fs []fuses.Fuse, cfg orchcfg.Config, now time.Time, down, sd map[string]int, reg profiles.Registry) Status {
	defSubj := func(b ledger.Bucket) bool { return b.Subject == "" || b.Subject == "default" }
	byLane := map[string][]ledger.Bucket{} // DEFAULT-subject windows only, per lane
	lanes := map[string]bool{}
	for _, b := range bs {
		lanes[b.Lane] = true
		if defSubj(b) {
			byLane[b.Lane] = append(byLane[b.Lane], b)
		}
	}
	st := Status{Lanes: map[string]LaneStatus{}, ActiveFuses: fuses.Active(fs, now), BillingMode: cfg.ClaudeBillingMode}
	for lane := range lanes {
		buckets := byLane[lane]
		d := admission.DecideSubject(bs, lane, "", now, defaultThresholds)
		ls := LaneStatus{State: string(d.State), Reason: d.Reason, Windows: buckets,
			BurnDownshift: down[lane], SpendDown: sd[lane]}
		if s, ok := pace.Binding(buckets, now); ok {
			ls.PaceSlack = &s
		}
		if !d.ResumeAt.IsZero() {
			t := d.ResumeAt
			ls.ResumeAt = &t
		}
		// W2: per-subject sub-blocks only when the lane has >1 profile.
		if reg != nil {
			if ps := reg.Lane(lane); len(ps) > 1 {
				ls.Subjects = map[string]SubjectStatus{}
				for _, p := range ps {
					sb := subjectBuckets(bs, lane, p.Subject)
					sd2 := admission.DecideSubject(bs, lane, p.Subject, now, defaultThresholds)
					ss := SubjectStatus{State: string(sd2.State), Reason: sd2.Reason, Provisioned: p.Provisioned, Windows: sb}
					if s, ok := pace.Binding(sb, now); ok {
						ss.PaceSlack = &s
					}
					ls.Subjects[p.Subject] = ss
				}
			}
		}
		st.Lanes[lane] = ls
	}
	return st
}

func subjectBuckets(bs []ledger.Bucket, lane, subject string) []ledger.Bucket {
	if subject == "" {
		subject = "default"
	}
	var out []ledger.Bucket
	for _, b := range bs {
		s := b.Subject
		if s == "" {
			s = "default"
		}
		if b.Lane == lane && s == subject {
			out = append(out, b)
		}
	}
	return out
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
	// W1/A3: codex joins the fitted lanes — wham-fed trace pairs give it
	// measured caps, retiring the estimate-cap guess (SetCapacity clears the
	// estimate mark, so admission's exhaust gate becomes available). GLM stays
	// out: its cap is a documented plan quota, not a guess.
	for _, lane := range []string{"claude", "codex"} {
		for _, w := range []ledger.WindowKind{ledger.Win5h, ledger.Win7d} {
			capTok, n, ok := calib.Fit(samples, lane, w, calib.Defaults())
			if !ok {
				continue
			}
			cur := int64(0)
			if b, okb := l.Bucket(lane, w); okb && b.CapSource == "" {
				cur = b.CapTokens // an estimate cap never suppresses a real fit
			}
			if cur > 0 && math.Abs(float64(capTok-cur)) <= 0.10*float64(cur) {
				continue
			}
			l.SetCapacity(lane, w, capTok)
			notes = append(notes, fmt.Sprintf("capacity fitted %s/%s = %d tokens (n=%d samples)", lane, w, capTok, n))
		}
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
	cfg := orchcfg.Load(configPath())
	reg, rerr := profiles.Load(profilesPath())
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "warn: profiles registry invalid, using default subject only:", rerr)
		reg = nil // Load's nil registry → Lane() yields the implicit default
	}
	ps := loadPollState()
	// W1: the NETWORK half of polling runs BEFORE the ledger transaction —
	// HTTP under the write lock could exceed the 30s lock-steal threshold and
	// drop concurrent shadow writes (review finding). Rate-limited by the
	// per-(lane,subject) stamps; never the route hot path (B2). W2: over the
	// profile registry.
	pf := fetchPolls(cfg, reg, ps, false, now)
	var snap []ledger.Bucket
	err := ledger.Update(ledgerPath(), func(l *ledger.Ledger) {
		if _, note, ierr := quotasig.IngestTraced(l, dropPath(), quotaTracePath(), "claude", now); ierr != nil {
			fmt.Fprintln(os.Stderr, "warn: statusline drop unreadable:", ierr)
		} else if note != "" {
			fmt.Fprintln(os.Stderr, "warn:", note)
		}
		applyPolls(l, pf, now)
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
	if err == nil {
		// Stamps advance only on a committed transaction — writing stale
		// stamps after a failed txn would clobber a concurrent run's fresh
		// ones and defeat the rate limit (review finding).
		finishPolls(pf, &ps, now)
	}
	fzs, _ := fuses.Load(fusesPath())
	samples := calib.Load(quotaTracePath())
	down := burnDownshiftByLane(snap, samples, cfg, now)
	st := buildStatus(snap, fzs, cfg, now, down, spendDownArmedByLane(snap, samples, cfg, now), reg)
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
	// W1: typed poll absences from THIS run (only polling runs carry them —
	// the persistent signal is the scoped-alert latch + provider bucket
	// staleness in quota_health) + the scoped-limit latch (raw passthrough,
	// glm-alert pattern).
	st.QuotaAbsences = pf.combined().Absences
	if raw, err := os.ReadFile(statepaths.ScopedAlert()); err == nil && json.Valid(raw) {
		st.ScopedAlerts = raw
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
