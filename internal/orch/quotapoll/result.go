package quotapoll

import (
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// Lane names the pollers report on.
const (
	LaneClaude = "claude"
	LaneCodex  = "codex"
)

// Absence is a TYPED statement that a quota fact could not be obtained —
// stated with a reason, never inferred from silence (claudexor
// quota-registry lineage; reconciliation W1 Tier-A #2).
type Absence struct {
	Lane   string `json:"lane"`
	Window string `json:"window"` // "5h" | "7d" | "all"
	Reason string `json:"reason"` // not_logged_in | refresh_failed | window_omitted | http_<code> | parse_error
}

// Snapshot is one vendor-measured window fact.
type Snapshot struct {
	Lane     string
	Window   ledger.WindowKind
	UsedPct  float64
	ResetsAt time.Time
}

// ScopedAlert is a limits[] entry beyond the two plain windows — the payload
// the statusline tee cannot see (observed live 2026-07-23: a weekly_scoped
// limit at 90% severity=critical while plain seven_day read 65%).
type ScopedAlert struct {
	Kind     string    `json:"kind"`
	Percent  float64   `json:"percent"`
	Severity string    `json:"severity"` // vendor's word, passed through verbatim
	ResetsAt time.Time `json:"resets_at"`
	Scope    string    `json:"scope,omitempty"`
	IsActive bool      `json:"is_active"` // vendor's flag, passed through — surfaced, never filtered on (semantics unverified)
}

// Result is one poll outcome: facts, typed absences, scoped extras.
type Result struct {
	Snapshots []Snapshot    `json:"snapshots"`
	Absences  []Absence     `json:"absences,omitempty"`
	Scoped    []ScopedAlert `json:"scoped,omitempty"`
}
