package glmlane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// GLM 429 discipline (fact refresh §3 + D7): the 429 signal is JSON-body
// error codes ONLY — no headers. ClassifyError refines claudelane's outer
// classification GLM-side by scanning the raw result bytes for a 13xx code
// (string/number-tolerant) and an embedded next_flush_time.
//
// Shape honesty: no live 429 body has been captured through the claude binary
// (forcing one would burn real quota and flirt with the 1313 strike class —
// NOT authorized). The scan is regex-tolerant by design; test bodies are
// SYNTHETIC and labeled. The first real production 429 is dispatch-logged raw
// for fixture promotion.

type ErrAction string

const (
	ActRetry    ErrAction = "retry_backoff"   // 1302, 1305 — transient (concurrency/backpressure), no ledger write
	ActCooldown ErrAction = "cooldown_5h"     // 1308, 1316 — until embedded next_flush_time (else now+5h, RS5)
	ActOffline  ErrAction = "offline_weekly"  // 1310, 1317, 1318, 1319, 1320, 1321 — weekly-reset class
	ActConfig   ErrAction = "config_error"    // 1311
	ActHardStop ErrAction = "hard_stop_alert" // 1313 — Fair Usage; >3 violations = ban; LATCH
	ActUnknown  ErrAction = "unknown"         // any other 13xx: classified, no ledger write (fail-open)
)

type GLMErr struct {
	Code      int
	Action    ErrAction
	NextFlush time.Time
	Raw       string
}

// The regexes tolerate BOTH plain and backslash-escaped quoting: through the
// claude binary a GLM error body typically arrives embedded in the outer
// result's "result" string field, i.e. {\"error\":{\"code\":1308,…}} —
// escape-blind matching would miss every real occurrence.
var (
	codeRe  = regexp.MustCompile(`\\?"code\\?"\s*:\s*(?:\\?")?(13\d{2})(?:\\?")?`)
	flushRe = regexp.MustCompile(`\\?"next_flush_time\\?"\s*:\s*(?:\\?")?(\d+)(?:\\?")?`)
)

// ClassifyError scans raw result bytes for a GLM 13xx error code. ok=false
// when none is present (clean results, non-GLM errors — the S2R-8 generic
// fallback handles those). next_flush_time parses VERBATIM here (pure
// extraction); the staleness guard (a reset in the past must not anchor a
// cooldown that instantly rolls) lives at the observation site, which holds
// the authoritative now.
func ClassifyError(raw []byte, now time.Time) (GLMErr, bool) {
	m := codeRe.FindSubmatch(raw)
	if m == nil {
		return GLMErr{}, false
	}
	code, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return GLMErr{}, false
	}
	e := GLMErr{Code: code, Raw: string(raw)}
	switch code {
	case 1302, 1305:
		e.Action = ActRetry
	case 1308, 1316:
		e.Action = ActCooldown
	case 1310, 1317, 1318, 1319, 1320, 1321:
		e.Action = ActOffline
	case 1311:
		e.Action = ActConfig
	case 1313:
		e.Action = ActHardStop
	default:
		e.Action = ActUnknown
	}
	if fm := flushRe.FindSubmatch(raw); fm != nil {
		if secs, err := strconv.ParseInt(string(fm[1]), 10, 64); err == nil {
			e.NextFlush = time.Unix(secs, 0).UTC()
		}
	}
	return e, true
}

// LatchAlert writes the 1313 hard-stop latch, WRITE-ONCE: the FIRST
// violation's code+timestamp is the evidence — later violations never
// overwrite it (mirrors the codex burn-anomaly latch). `probe --ack-glm`
// clears.
func LatchAlert(path string, e GLMErr, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	note := fmt.Sprintf("GLM error %d (%s): Fair-Usage strike class — >3 violations = ban; ack with `mr-orchestrate probe --ack-glm`", e.Code, e.Action)
	b, err := json.MarshalIndent(map[string]any{"note": note, "code": e.Code, "since": now}, "", "  ")
	if err != nil {
		return err
	}
	// Atomic create-once (A2R-#7): O_CREATE|O_EXCL races cannot both win, so
	// the FIRST violation's evidence stands and concurrent latchers never
	// clobber it (the old Stat-then-Write was a TOCTOU).
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil // already latched — first violation stands
		}
		return err
	}
	defer f.Close()
	_, werr := f.Write(b)
	return werr
}

// Latched reports whether the 1313 latch is set. This is the ONE place that
// fails SAFE rather than open: an existing-but-unreadable latch file stays
// latched (the latch guards an account-loss event; the explicit ack is the
// only way out — R11 override exists via ack-then-run, a deliberate two-step).
func Latched(path string) (note string, latched bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false // genuinely no latch file → not latched
		}
		// Existing-but-unreadable (EISDIR, ACL, Windows ERROR_SHARING_VIOLATION,
		// …): the latch guards an account-loss event, so an unreadable latch is
		// treated as LATCHED (fail-safe), NEVER collapsed to not-latched.
		return "glm-alert.json exists but is unreadable — treated as LATCHED (fail-safe); clear with `mr-orchestrate probe --ack-glm`", true
	}
	var v struct {
		Note string `json:"note"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Note == "" {
		return "glm-alert.json exists but is unreadable — treated as LATCHED (fail-safe); clear with `mr-orchestrate probe --ack-glm`", true
	}
	return v.Note, true
}
