// Package quotasig ingests the sanctioned quota signals into the ledger:
// the statusline drop file (rate_limits teed by the operator's statusline
// command, RS1) and — since W1 (Daniel-approved 2026-07-23, superseding the
// old D3 off-gate) — vendor-polled snapshots from quotapoll via
// ApplySnapshots. Every observation that changes a bucket is appended to the
// scarcity trace with an ORIGIN tag, so drop-vs-poll parity is measurable
// (the W1 soak gate) and the calibration fitter keeps its sample stream.
package quotasig

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/quotapoll"
)

type Observation struct {
	Window   ledger.WindowKind
	UsedPct  float64
	ResetsAt time.Time
}

type rateWindow struct {
	UsedPercentage *float64 `json:"used_percentage"`
	ResetsAt       int64    `json:"resets_at"`
}

type drop struct {
	RateLimits struct {
		FiveHour *rateWindow `json:"five_hour"`
		SevenDay *rateWindow `json:"seven_day"`
	} `json:"rate_limits"`
}

// ParseDrop extracts the per-window observations from a raw statusline JSON
// blob. Each window may be independently absent (fact refresh §3).
func ParseDrop(raw []byte) ([]Observation, error) {
	var d drop
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("statusline drop: %w", err)
	}
	var out []Observation
	if w := d.RateLimits.FiveHour; w != nil && w.UsedPercentage != nil {
		out = append(out, Observation{Window: ledger.Win5h, UsedPct: *w.UsedPercentage, ResetsAt: epoch(w.ResetsAt)})
	}
	if w := d.RateLimits.SevenDay; w != nil && w.UsedPercentage != nil {
		out = append(out, Observation{Window: ledger.Win7d, UsedPct: *w.UsedPercentage, ResetsAt: epoch(w.ResetsAt)})
	}
	return out, nil
}

func epoch(s int64) time.Time {
	if s == 0 {
		return time.Time{}
	}
	return time.Unix(s, 0).UTC()
}

// Ingest reads the drop file and feeds fresh windows into the ledger via
// ObserveProvider. Fail-open: a missing file is (0, nil); a corrupt file
// returns an error the caller logs and ignores (the shadow floor governs).
// Skipped as unusable, never observed:
//   - windows whose resets_at already passed (a dead percentage must not
//     masquerade as a fresh provider observation), and
//   - windows with NO resets_at (schema drift — a zero-reset provider bucket
//     would never roll and could lock the lane exhausted forever).
func Ingest(l *ledger.Ledger, path, lane string, now time.Time) (int, error) {
	n, _, err := IngestTraced(l, path, "", lane, now)
	return n, err
}

// IngestTraced is Ingest plus the RS2 scarcity-trace: every observation that
// CHANGED its bucket is appended to tracePath as one JSONL line, building the
// used_percentage history that sizes the slice-4 economics. Empty tracePath
// disables tracing. Trace-append failures are reported in the note (the
// observation itself still lands).
func IngestTraced(l *ledger.Ledger, path, tracePath, lane string, now time.Time) (int, string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, "", nil
	}
	obs, err := ParseDrop(raw)
	if err != nil {
		return 0, "", err
	}
	n, note := 0, ""
	for _, o := range obs {
		if o.ResetsAt.IsZero() || o.ResetsAt.Before(now) {
			continue
		}
		prev, had := l.Bucket(lane, o.Window)
		changed := !had || prev.UsedPct != o.UsedPct || !prev.ResetsAt.Equal(o.ResetsAt)
		l.ObserveProvider(lane, o.Window, o.UsedPct, o.ResetsAt, now)
		n++
		if tracePath != "" && changed {
			// ShadowTokens at observation time makes each row a CALIBRATION
			// SAMPLE (shadow, used_pct) — the regression input for learned
			// capacities (fact-refresh gap #1); fitting lands in slice 2.
			if err := appendTrace(tracePath, traceRow{TS: now, Lane: lane, Window: string(o.Window), UsedPct: o.UsedPct, ResetsAt: o.ResetsAt, ShadowTokens: prev.ShadowTokens, Origin: "drop"}); err != nil {
				note = "quota trace append failed: " + err.Error()
			}
		}
	}
	return n, note, nil
}

// TraceRow is one scarcity-trace line (exported for the quota-parity report).
type TraceRow struct {
	TS           time.Time `json:"ts"`
	Lane         string    `json:"lane"`
	Window       string    `json:"window"`
	UsedPct      float64   `json:"used_pct"`
	ResetsAt     time.Time `json:"resets_at"`
	ShadowTokens int64     `json:"shadow_tokens"` // this device's shadow count at obs time (calibration pair)
	Origin       string    `json:"origin,omitempty"` // drop | oauth_poll | wham_poll ("" = pre-W1 row)
}

type traceRow = TraceRow

// ApplySnapshots feeds vendor-polled window facts through the SAME provider
// path the drop uses: ObserveProvider + origin-tagged trace-on-change. Stale
// or unanchored snapshots are skipped with the same rules as the drop
// (a dead percentage must not masquerade as fresh provider truth).
func ApplySnapshots(l *ledger.Ledger, snaps []quotapoll.Snapshot, tracePath, origin string, now time.Time) (int, string) {
	n, note := 0, ""
	for _, s := range snaps {
		if s.ResetsAt.IsZero() || s.ResetsAt.Before(now) {
			continue
		}
		prev, had := l.Bucket(s.Lane, s.Window)
		changed := !had || prev.UsedPct != s.UsedPct || !prev.ResetsAt.Equal(s.ResetsAt)
		l.ObserveProvider(s.Lane, s.Window, s.UsedPct, s.ResetsAt, now)
		n++
		if tracePath != "" && changed {
			if err := appendTrace(tracePath, TraceRow{TS: now, Lane: s.Lane, Window: string(s.Window), UsedPct: s.UsedPct, ResetsAt: s.ResetsAt, ShadowTokens: prev.ShadowTokens, Origin: origin}); err != nil {
				note = "quota trace append failed: " + err.Error()
			}
		}
	}
	return n, note
}

func appendTrace(path string, r traceRow) error {
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
