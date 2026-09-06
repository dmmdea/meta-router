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

type codexRateLimit struct {
	Primary   *codexWindow `json:"primary_window"`
	Secondary *codexWindow `json:"secondary_window"`
}

type codexUsage struct {
	PlanType  string         `json:"plan_type"`
	RateLimit codexRateLimit `json:"rate_limit"`
	// model_usage / additional_rate_limits arrived with the 2026-09-06 live
	// capture on the "prolite" plan: per-model availability flags and
	// per-model limit blocks (GPT-5.3-Codex-Spark carries its own 5h + 7d
	// windows, metered under a separate feature name). Decoded as FACTS for
	// status and the probe; nothing here drives admission.
	ModelUsage map[string]CodexModelUsage `json:"model_usage"`
	Additional []struct {
		Name      string         `json:"limit_name"`
		Feature   string         `json:"metered_feature"`
		RateLimit codexRateLimit `json:"rate_limit"`
	} `json:"additional_rate_limits"`
	// rate_limit_reset_credits (E5 banked resets) is deliberately NOT parsed:
	// Q10 defers E5; consuming a scarce expiring credit is an operator act.
}

// CodexModelUsage is one model's availability on the plan (wham model_usage).
type CodexModelUsage struct {
	Available bool `json:"available"`
}

// CodexAdditionalLimit is one additional_rate_limits block: a model with its
// own windows, mapped to 5h/7d snapshots by limit_window_seconds exactly like
// the main allowance.
type CodexAdditionalLimit struct {
	Name, Feature string
	Snapshots     []Snapshot
}

// CodexFacts is the plan-level state the wham endpoint reports beside the
// windows: which plan, whether it exposed a 5h window at all, which models
// it lists, and which models carry their own limits. EVIDENCE, surfaced
// through poll/status — no admission or capacity decision reads it (Q10: a
// 5h window omitted by wham was unreliable on Plus, so Has5h=false is a
// recorded observation, not proof the plan has no 5h window).
type CodexFacts struct {
	PlanType   string
	Has5h      bool
	Models     map[string]CodexModelUsage
	Additional []CodexAdditionalLimit
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
	r, _ := PollCodexFactsAt(authPath, now)
	return r
}

// PollCodexFactsAt is PollCodexAt plus the plan facts (see CodexFacts).
func PollCodexFactsAt(authPath string, now time.Time) (Result, CodexFacts) {
	if authPath == "" {
		authPath = CodexAuthPath()
	}
	return pollCodexFacts(guardedClient(), "https://chatgpt.com", authPath, now)
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
	r, _ := pollCodexFacts(c, baseURL, authPath, now)
	return r
}

// windowSnapshots maps a wham rate_limit block onto 5h/7d snapshots by
// limit_window_seconds (never by primary/secondary position).
func windowSnapshots(rl codexRateLimit) ([]Snapshot, map[ledger.WindowKind]bool) {
	var out []Snapshot
	seen := map[ledger.WindowKind]bool{}
	for _, w := range []*codexWindow{rl.Primary, rl.Secondary} {
		if w == nil || w.UsedPercent == nil || w.ResetAt <= 0 {
			continue
		}
		kind, ok := windowKindOf(w.LimitWindowSeconds)
		if !ok {
			continue // an unrecognized window span is not one of our buckets
		}
		out = append(out, Snapshot{Lane: LaneCodex, Window: kind, UsedPct: *w.UsedPercent, ResetsAt: time.Unix(w.ResetAt, 0).UTC()})
		seen[kind] = true
	}
	return out, seen
}

func pollCodexFacts(c *http.Client, baseURL, authPath string, now time.Time) (Result, CodexFacts) {
	var r Result
	raw, err := os.ReadFile(authPath)
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "not_logged_in"})
		return r, CodexFacts{}
	}
	tok := FindStringField(raw, "access_token")
	if tok == "" {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "not_logged_in"})
		return r, CodexFacts{}
	}
	headers := map[string]string{}
	if acct := FindStringField(raw, "account_id"); acct != "" {
		headers["ChatGPT-Account-Id"] = acct
	}
	body, code, err := getJSON(c, baseURL+codexUsagePath, tok, headers)
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "refresh_failed"})
		return r, CodexFacts{}
	}
	if code != http.StatusOK {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: httpReason(code)})
		return r, CodexFacts{}
	}
	var u codexUsage
	if err := json.Unmarshal(body, &u); err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "all", Reason: "parse_error"})
		return r, CodexFacts{}
	}
	snaps, seen := windowSnapshots(u.RateLimit)
	r.Snapshots = append(r.Snapshots, snaps...)
	if !seen[ledger.Win5h] {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "5h", Reason: "window_omitted"})
	}
	if !seen[ledger.Win7d] {
		r.Absences = append(r.Absences, Absence{Lane: LaneCodex, Window: "7d", Reason: "window_omitted"})
	}
	facts := CodexFacts{PlanType: u.PlanType, Has5h: seen[ledger.Win5h], Models: u.ModelUsage}
	for _, a := range u.Additional {
		as, _ := windowSnapshots(a.RateLimit)
		facts.Additional = append(facts.Additional, CodexAdditionalLimit{Name: a.Name, Feature: a.Feature, Snapshots: as})
	}
	return r, facts
}
