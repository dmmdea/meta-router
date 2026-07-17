// stream.go ingests the SECOND sanctioned Claude quota signal (Task 8): the
// rate_limit_event emitted by `claude -p --output-format stream-json`
// (committed capture: testdata/fixtures/claude/stream-events-sonnet.jsonl,
// CLI 2.1.199). The event carries the window's admission status and the
// account's PROVIDER-TRUE reset — no percentage.
package quotasig

import (
	"bufio"
	"bytes"
	"encoding/json"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// streamStatusAllowed anchors the provider-true reset without a percentage.
// streamStatusExhausted is the KNOWN-EXHAUSTED set — S2R-7: ONLY these observe
// 100%; any status outside both sets is schema drift and is SKIPPED, exactly
// like an unknown rateLimitType (stream ingest fails OPEN, never guessing an
// exhaustion). NOTE: the fixture's overageStatus field ("rejected" while
// status is "allowed") is the OVERAGE billing state, NOT the window status —
// it is deliberately never read.
var (
	streamStatusAllowed   = map[string]bool{"allowed": true}
	streamStatusExhausted = map[string]bool{"rejected": true}
	streamWindows         = map[string]ledger.WindowKind{
		"five_hour": ledger.Win5h,
		"seven_day": ledger.Win7d,
	}
)

type streamEvent struct {
	Type          string `json:"type"`
	RateLimitInfo *struct {
		Status        string `json:"status"`
		ResetsAt      int64  `json:"resetsAt"`
		RateLimitType string `json:"rateLimitType"`
	} `json:"rate_limit_info"`
}

// IngestStreamEvents feeds a stream-json capture's rate_limit_events into the
// ledger and returns how many were applied. Per line (all other event types
// and undecodable lines are ignored — the stream is mostly system/assistant/
// result events):
//   - unknown rateLimitType OR unknown status ⇒ skip (schema drift, S2R-7),
//   - absent or already-passed resetsAt ⇒ skip (staleness guard, matching
//     Ingest: a dead reset must not anchor and must not lock the lane),
//   - status "allowed" ⇒ AnchorAuthoritative — the provider-true reset
//     REPLACES a self-anchored estimate; no percentage is fabricated,
//   - a known-exhausted status ⇒ ObserveProvider 100% (sanctioned exhaustion).
func IngestStreamEvents(l *ledger.Ledger, jsonl []byte, lane string, now time.Time) (n int) {
	sc := bufio.NewScanner(bytes.NewReader(jsonl))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e streamEvent
		if json.Unmarshal(line, &e) != nil || e.Type != "rate_limit_event" || e.RateLimitInfo == nil {
			continue
		}
		w, known := streamWindows[e.RateLimitInfo.RateLimitType]
		if !known {
			continue // schema drift: fail open
		}
		resetsAt := epoch(e.RateLimitInfo.ResetsAt)
		if resetsAt.IsZero() || resetsAt.Before(now) {
			continue // stale/absent reset: fail open
		}
		switch {
		case streamStatusAllowed[e.RateLimitInfo.Status]:
			l.AnchorAuthoritative(lane, w, resetsAt, now)
			n++
		case streamStatusExhausted[e.RateLimitInfo.Status]:
			l.ObserveProvider(lane, w, 100, resetsAt, now)
			n++
			// default: unknown status — schema drift, skipped (S2R-7).
		}
	}
	return n
}
