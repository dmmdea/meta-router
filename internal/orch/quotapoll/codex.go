package quotapoll

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

const codexUsagePath = "/backend-api/wham/usage"

// CodexAuthPath is the Codex CLI's own credential store.
func CodexAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "auth.json")
}

// codexWindow is one wham rate-limit window. Mapping to 5h/7d keys off
// limit_window_seconds — the 2026-07-23 live capture proved the 7-DAY window
// arrives as primary_window on Plus, so position means nothing.
type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds int64    `json:"limit_window_seconds"`
	ResetAt            int64    `json:"reset_at"` // epoch seconds
}

type codexUsage struct {
	RateLimit struct {
		Primary   *codexWindow `json:"primary_window"`
		Secondary *codexWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	// rate_limit_reset_credits (E5 banked resets) is deliberately NOT parsed:
	// Q10 defers E5; consuming a scarce expiring credit is an operator act.
}

// PollCodex polls the unofficial wham usage endpoint as BEST-EFFORT EVIDENCE
// (Q10: the 5h window is unreliably reported on Plus — an omitted window is a
// typed absence, never a zero). Wired per Daniel's 2026-07-23 approval.
func PollCodex(now time.Time) Result {
	return pollCodex(guardedClient(), "https://chatgpt.com", CodexAuthPath(), now)
}

// PollCodexAt polls using an explicit auth.json path (W2 per-profile: a
// profile's isolated CODEX_HOME). Empty path = the default home.
func PollCodexAt(authPath string, now time.Time) Result {
	if authPath == "" {
		authPath = CodexAuthPath()
	}
	return pollCodex(guardedClient(), "https://chatgpt.com", authPath, now)
}

func windowKindOf(seconds int64) (ledger.WindowKind, bool) {
	switch {
	case seconds >= 4*3600 && seconds <= 6*3600:
		return ledger.Win5h, true
	case seconds >= 6*24*3600 && seconds <= 8*24*3600:
		return ledger.Win7d, true
	}
	return "", false
}

func pollCodex(c *http.Client, baseURL, authPath string, now time.Time) Result {
	var r Result
	raw, err := os.ReadFile(authPath)
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "not_logged_in"})
		return r
	}
	tok := FindStringField(raw, "access_token")
	if tok == "" {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "not_logged_in"})
		return r
	}
	headers := map[string]string{}
	if acct := FindStringField(raw, "account_id"); acct != "" {
		headers["ChatGPT-Account-Id"] = acct
	}
	body, code, err := getJSON(c, baseURL+codexUsagePath, tok, headers)
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "refresh_failed"})
		return r
	}
	if code != http.StatusOK {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: httpReason(code)})
		return r
	}
	var u codexUsage
	if err := json.Unmarshal(body, &u); err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "parse_error"})
		return r
	}
	seen := map[ledger.WindowKind]bool{}
	for _, w := range []*codexWindow{u.RateLimit.Primary, u.RateLimit.Secondary} {
		if w == nil || w.UsedPercent == nil || w.ResetAt <= 0 {
			continue
		}
		kind, ok := windowKindOf(w.LimitWindowSeconds)
		if !ok {
			continue // an unrecognized window span is not one of our buckets
		}
		r.Snapshots = append(r.Snapshots, Snapshot{Lane: LaneCodex, Window: kind, UsedPct: *w.UsedPercent, ResetsAt: time.Unix(w.ResetAt, 0).UTC()})
		seen[kind] = true
	}
	if !seen[ledger.Win5h] {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "5h", Reason: "window_omitted"})
	}
	if !seen[ledger.Win7d] {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "7d", Reason: "window_omitted"})
	}
	return r
}
