package main

import (
	"fmt"
	"os"
	"regexp"
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
var hintLanes = []string{"claude", "codex", "copilot", "glm",
	// free-provider lanes (W4): they render only once a dispatch has metered a
	// window, so an unprovisioned lane adds nothing (fail-open, as always).
	"groq", "cloudflare", "openrouter", "nim", "gemini"}

// windowOrder pins a stable per-lane window order in the render.
var windowOrder = []ledger.WindowKind{ledger.Win5h, ledger.Win7d, ledger.WinMonth, ledger.WinDay, ledger.WinTrial}

// quotaHint builds a one-line quota+route pointer from the ledger file DIRECTLY
// (no subprocess, no network — the hook's deadline stands (300ms binary default; 1000ms via -timeout-ms in production); a file read is
// microseconds). ANY failure or absent signal → "" (inject nothing; fail-open
// is absolute). Zero policy content: it reports STATE and points at the oracle —
// it NEVER names a preferred lane or a rank-table model (§6c: policy lives in
// the rank table only). The GLM 1313 latch renders `glm HARD-STOP(1313)`.
func quotaHint(now time.Time) string {
	h, _ := quotaHintWithFreshness(now)
	return h
}

// quotaHintWithFreshness returns the hint AND whether it carries news. The content
// logic is unchanged; freshness is SEMANTIC, from the same single ledger read (no
// second stat, so no read/stat race):
//   - the GLM hard-stop latch: always fresh -- account protection, never gated;
//   - a rendered lane whose bucket ChangedAt is within quotaHintMaxAge -- the
//     ledger stamps it only when a DISPLAYED value moves, never when route/status
//     merely re-report the same number (which is what defeated an mtime gate);
//   - a ChangedAt in the future (clock skew): fresh, so skew can never silence it;
//   - a rendered lane whose window reset within quotaHintMaxAge -- a lane recovering
//     changes what the banner shows even if nothing has written the ledger since.
func quotaHintWithFreshness(now time.Time) (string, bool) {
	// Read-only: the hook NEVER writes state. OpenChecked fails open (empty
	// buckets + a warn string) on a missing/corrupt file — both yield "".
	l, warn := ledger.OpenChecked(statepaths.Ledger())
	if warn != "" {
		return "", false // corrupt/unreadable ledger: a hint with no trustworthy signal is noise
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
	fresh := false
	for _, lane := range hintLanes {
		buckets := byLane[lane]
		// GLM hard-stop latch: render the marker even with no buckets — it is a
		// real, ledger-truth account-protection signal.
		if lane == "glm" && glmLatched {
			rows = append(rows, "glm HARD-STOP(1313)")
			fresh = true
			continue
		}
		if len(buckets) == 0 {
			continue // omit lanes with no buckets (no signal to report)
		}
		// Freshness looks at every bucket the banner is BUILT from, before the
		// render decides whether this lane gets a row: a window that just reset
		// drops its lane's row, and a row disappearing ("no longer EXHAUSTED") is
		// a change in what the banner says.
		for _, b := range buckets {
			if bucketIsNews(b, now) {
				fresh = true
			}
		}
		var parts []string
		live := false // does this lane carry ANY real number?
		for _, w := range windowOrder {
			b, ok := buckets[w]
			if !ok {
				continue
			}
			// An EXPIRED window's percentage is dead history — rendering it as a
			// live number is what made this banner contradict `route` for 6 days
			// (audit 2026-07-25). Unknown and expired both render "?": the hook
			// reports live pressure or nothing.
			if b.UsedPct < 0 || b.Expired(now) {
				parts = append(parts, fmt.Sprintf("%s ?", w))
			} else {
				parts = append(parts, fmt.Sprintf("%s %.0f%%", w, b.UsedPct))
				live = true
			}
		}
		// Fail-open is ABSOLUTE: a lane with no live number contributes nothing.
		// An all-"?" row reads like a report while carrying no signal (review
		// 2026-07-25).
		if len(parts) == 0 || !live {
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
		return "", false // no signal at all: inject nothing (fail-open)
	}

	// rows are already in hintLanes order (deterministic render). The pointer
	// names the oracle only — an earlier version pointed at
	// ~/.claude/rules/mr-orchestrate.md, which has never existed on either
	// machine (audit 2026-07-25: dangling escape hatch).
	return "mr-orchestrate quota: " + strings.Join(rows, " · ") +
		" — delegable work: consult `mr-orchestrate route` first", fresh
}

// quotaHintMaxAge bounds how long after a real change the banner still carries news.
const quotaHintMaxAge = 15 * time.Minute

func bucketIsNews(b ledger.Bucket, now time.Time) bool {
	if !b.ChangedAt.IsZero() {
		age := now.Sub(b.ChangedAt)
		// A small future stamp (writer/reader skew) is fresh. One further ahead than
		// the window itself is a clock error and is NOT trusted: skew can neither
		// silence the banner nor pin it on. The first-prompt rule still shows it once
		// per session regardless.
		if age >= -quotaHintMaxAge && age <= quotaHintMaxAge {
			return true
		}
	}
	if !b.ResetsAt.IsZero() && !b.ResetsAt.After(now) && now.Sub(b.ResetsAt) <= quotaHintMaxAge {
		return true
	}
	return false
}

// firstPromptChunk is the transcript read size; firstPromptCeiling bounds the scan.
const (
	firstPromptChunk   = 64 * 1024
	firstPromptCeiling = 16 << 20
)

// assistantMarker tolerates whitespace around the colon, so a transcript writer that
// switched to spaced JSON could not silently turn every prompt into a "first" one.
var assistantMarker = regexp.MustCompile(`"type"\s*:\s*"assistant"`)

// markerTail is how much of a chunk is carried into the next read: enough for any
// spacing variant of the marker that straddles a read boundary.
const markerTail = 64

// isFirstPrompt reports whether the session has not produced an assistant turn yet,
// so the banner shows once at session start even when nothing is fresh. It streams
// from the START and stops at the first assistant record: measured on real
// transcripts that record sits 200-270 KB in, behind the first turn's hook
// attachments, so a fixed-prefix check would misread long sessions as new.
// No path, an unreadable file, or one not yet written all mean "first" -- the safe
// direction, since showing the banner is the pre-gate behaviour. Past the ceiling
// with no marker, the session is treated as established.
func isFirstPrompt(path string) bool {
	if path == "" {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, firstPromptChunk+markerTail)
	carry := 0
	total := 0
	zeroReads := 0
	for total < firstPromptCeiling {
		// read exactly one chunk after the carried tail, so the chunk boundary is real
		n, rerr := f.Read(buf[carry : carry+firstPromptChunk])
		if n > 0 {
			total += n
			if assistantMarker.Match(buf[:carry+n]) {
				return false
			}
			// keep a tail so a marker split across reads is still found
			keep := markerTail - 1
			if carry+n < keep {
				keep = carry + n
			}
			copy(buf, buf[carry+n-keep:carry+n])
			carry = keep
		}
		if rerr != nil {
			return true // EOF with no assistant record: nothing has answered yet
		}
		if n == 0 {
			zeroReads++
			if zeroReads > 3 {
				return true // a reader making no progress: treat like EOF, never spin
			}
		}
	}
	return false
}

// appendHint appends the quota hint to ctx (returns the bare hint when ctx is
// empty). Mirrors appendNudge so the additionalContext block stays uniform.
func appendHint(ctx, hint string) string {
	if ctx == "" {
		return hint
	}
	return ctx + "\n\n" + hint
}
