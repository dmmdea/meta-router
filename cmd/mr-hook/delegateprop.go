package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/quotasig"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// The delegate-mode session file is a CROSS-REPO INTERFACE: the Claude-side
// hooks (~/.claude/hooks/delegate-mode-lib.js) write it; this hook only reads.
// Schema (spec 2026-09-01 §1):
//
//	{ "active": bool, "ts": RFC3339, "armed_by": "operator"|"proposal-accepted", "scope": "substantive" }
//
// CLAUDE_STATE_DIR overrides ~/.claude/state (tests).
func delegateModeDir() string {
	if v := os.Getenv("CLAUDE_STATE_DIR"); v != "" {
		return filepath.Join(v, "delegate-mode")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "state", "delegate-mode")
}

var sidClean = regexp.MustCompile(`[^a-zA-Z0-9-]`)

// delegateModeTTL mirrors TTL_MS in delegate-mode-lib.js (8h, spec §1).
const delegateModeTTL = 8 * time.Hour

// delegateModeArmed: file exists, active, unexpired. READ-ONLY — the hook never
// writes state; deleting an expired file is the Node guard's job.
func delegateModeArmed(sessionID string, now time.Time) bool {
	sid := sidClean.ReplaceAllString(sessionID, "")
	dir := delegateModeDir()
	if sid == "" || dir == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(dir, sid+".json"))
	if err != nil {
		return false
	}
	var st struct {
		Active bool   `json:"active"`
		TS     string `json:"ts"`
	}
	if json.Unmarshal(raw, &st) != nil || !st.Active {
		return false
	}
	ts, err := time.Parse(time.RFC3339, st.TS)
	if err != nil {
		return false
	}
	age := now.Sub(ts)
	return age >= 0 && age < delegateModeTTL
}

// claudePressurePct reads the statusline drop DIRECTLY (ParseDrop; no ledger
// write) and returns the worst LIVE window, or -1 for no signal. Reading the
// drop rather than the ledger is deliberate (spec §4, R7): interactive Claude
// usage enters the ledger only when `route` ingests the drop, so a session
// that has not consulted recently would otherwise read a stale figure. An
// expired window is dead history and is skipped (the quotahint.go lesson).
func claudePressurePct(now time.Time) float64 {
	raw, err := os.ReadFile(statepaths.Drop())
	if err != nil {
		return -1
	}
	obs, err := quotasig.ParseDrop(raw)
	if err != nil {
		return -1
	}
	worst := -1.0
	for _, o := range obs {
		if !o.ResetsAt.IsZero() && !o.ResetsAt.After(now) {
			continue
		}
		if o.UsedPct > worst {
			worst = o.UsedPct
		}
	}
	return worst
}

// delegateProposal returns the one-line proposal or "". It PROPOSES only —
// arming is always an explicit operator act (/delegate-mode). Fail-open: any
// missing signal is "". Zero policy content: it never names a lane to prefer.
func delegateProposal(now time.Time, sessionID string) string {
	cfg := orchcfg.Load(statepaths.Config())
	if cfg.DelegateProposePct < 0 {
		return ""
	}
	pct := claudePressurePct(now)
	if pct < 0 || pct < cfg.DelegateProposePct {
		return ""
	}
	if delegateModeArmed(sessionID, now) {
		return ""
	}
	return fmt.Sprintf("delegate-mode: claude worst live window %.0f%% (threshold %.0f%%) and this session is NOT armed — propose `/delegate-mode` to the operator (substantive work goes to the lanes; you keep judgment + verification). Never arm it yourself.", pct, cfg.DelegateProposePct)
}
