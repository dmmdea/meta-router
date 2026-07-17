package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/admission"
	"github.com/dmmdea/meta-router/internal/orch/glmlane"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

// hintThresholds mirror the orchestrator's admission thresholds so the hint's
// state word matches what `route` will actually decide.
var hintThresholds = admission.Thresholds{ThrottlePct: 80, ExhaustPct: 95}

// hintLanes is the fixed lane order the hint renders (deterministic output).
var hintLanes = []string{"claude", "codex", "glm"}

// windowOrder pins a stable per-lane window order in the render.
var windowOrder = []ledger.WindowKind{ledger.Win5h, ledger.Win7d}

// quotaHint builds a one-line quota+route pointer from the ledger file DIRECTLY
// (no subprocess, no network — the hook's ~300ms deadline stands; a file read is
// microseconds). ANY failure or absent signal → "" (inject nothing; fail-open
// is absolute). Zero policy content: it reports STATE and points at the oracle —
// it NEVER names a preferred lane or a rank-table model (§6c: policy lives in
// the rank table only). The GLM 1313 latch renders `glm HARD-STOP(1313)`.
func quotaHint(now time.Time) string {
	// Read-only: the hook NEVER writes state. OpenChecked fails open (empty
	// buckets + a warn string) on a missing/corrupt file — both yield "".
	l, warn := ledger.OpenChecked(statepaths.Ledger())
	if warn != "" {
		return "" // corrupt/unreadable ledger: a hint with no trustworthy signal is noise
	}
	snap := l.Snapshot()

	// Index buckets by lane→window.
	byLane := map[string]map[ledger.WindowKind]ledger.Bucket{}
	for _, b := range snap {
		if byLane[b.Lane] == nil {
			byLane[b.Lane] = map[ledger.WindowKind]ledger.Bucket{}
		}
		byLane[b.Lane][b.Window] = b
	}

	_, glmLatched := glmlane.Latched(statepaths.GLMAlert())

	var rows []string
	for _, lane := range hintLanes {
		buckets := byLane[lane]
		// GLM hard-stop latch: render the marker even with no buckets — it is a
		// real, ledger-truth account-protection signal.
		if lane == "glm" && glmLatched {
			rows = append(rows, "glm HARD-STOP(1313)")
			continue
		}
		if len(buckets) == 0 {
			continue // omit lanes with no buckets (no signal to report)
		}
		var parts []string
		for _, w := range windowOrder {
			b, ok := buckets[w]
			if !ok {
				continue
			}
			if b.UsedPct < 0 {
				parts = append(parts, fmt.Sprintf("%s ?", w))
			} else {
				parts = append(parts, fmt.Sprintf("%s %.0f%%", w, b.UsedPct))
			}
		}
		if len(parts) == 0 {
			continue
		}
		// State word from admission (open → no word; throttled/exhausted → append).
		row := lane + " " + strings.Join(parts, " · ")
		d := admission.Decide(snap, lane, now, hintThresholds)
		if d.State != admission.Open {
			row += " " + strings.ToUpper(string(d.State))
		}
		rows = append(rows, row)
	}

	if len(rows) == 0 {
		return "" // no signal at all: inject nothing (fail-open)
	}

	// rows are already in hintLanes order (deterministic render).
	return "mr-orchestrate quota: " + strings.Join(rows, " · ") +
		" — delegable work: consult `mr-orchestrate route` first (rules: ~/.claude/rules/mr-orchestrate.md)"
}

// appendHint appends the quota hint to ctx (returns the bare hint when ctx is
// empty). Mirrors appendNudge so the additionalContext block stays uniform.
func appendHint(ctx, hint string) string {
	if ctx == "" {
		return hint
	}
	return ctx + "\n\n" + hint
}
