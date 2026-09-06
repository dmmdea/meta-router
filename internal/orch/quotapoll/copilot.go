package quotapoll

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// copilotUserPath is the endpoint the Copilot CLI itself consults for the
// account's plan and quota state. It is INTERNAL (no docs page, no API
// version header) — the same standing as the codex wham endpoint: polled as
// best-effort EVIDENCE, with every failure typed. It answers a plain gh OAuth
// token (no extra scope), which the documented billing-report endpoints do
// not (they need `user`), so it is the one provider-truth source the lane can
// reach with the token it already mints per dispatch.
const copilotUserPath = "/copilot_internal/user"

// CopilotFacts is the month-level plan state the vendor reports about the
// subscription: the entitlement (the cap the ledger should carry), what has
// been consumed, the calendar reset, and whether overage could ever bill.
// Unit is "credits" on token-based billing (every monthly plan since GitHub's
// 2026-06-01 switch; verified live on dmmdea 2026-09-06) or "requests" on the
// legacy premium-request plan — a caller must never mix the two.
type CopilotFacts struct {
	Unit             string // "credits" | "requests"
	Entitlement      float64
	Used             float64
	Remaining        float64
	ResetsAt         time.Time
	OveragePermitted bool
}

// copilotQuota is one quota_snapshots entry. Only the fields the lane reasons
// about are decoded; additive vendor drift on the rest cannot break a poll.
type copilotQuota struct {
	Entitlement      float64 `json:"entitlement"`
	CreditsUsed      float64 `json:"credits_used"`
	Remaining        float64 `json:"remaining"`
	PercentRemaining float64 `json:"percent_remaining"`
	Unlimited        bool    `json:"unlimited"`
	TokenBased       bool    `json:"token_based_billing"`
	OveragePermitted bool    `json:"overage_permitted"`
}

type copilotUser struct {
	CopilotPlan   string                  `json:"copilot_plan"`
	TokenBased    bool                    `json:"token_based_billing"`
	QuotaResetUTC string                  `json:"quota_reset_date_utc"` // RFC3339, e.g. 2026-10-01T00:00:00.000Z
	QuotaReset    string                  `json:"quota_reset_date"`     // YYYY-MM-DD fallback
	Snapshots     map[string]copilotQuota `json:"quota_snapshots"`
}

// PollCopilot polls the Copilot plan endpoint with a token the CALLER minted
// from the configured subscription account (copilot_token_user). It never
// reads an ambient env token: `gh` honours GH_TOKEN over its keyring, so an
// ambient token could report — and bill — a different GitHub account.
func PollCopilot(token string, now time.Time) (Result, CopilotFacts) {
	return pollCopilot(guardedClient(), "https://api.github.com", token, now)
}

func pollCopilot(c *http.Client, baseURL, token string, now time.Time) (Result, CopilotFacts) {
	var r Result
	if token == "" {
		r.Absences = append(r.Absences, Absence{Lane: LaneCopilot, Window: "all", Reason: "not_logged_in"})
		return r, CopilotFacts{}
	}
	body, code, err := getJSON(c, baseURL+copilotUserPath, token, map[string]string{"Accept": "application/vnd.github+json"})
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCopilot, Window: "all", Reason: "refresh_failed"})
		return r, CopilotFacts{}
	}
	if code != http.StatusOK {
		r.Absences = append(r.Absences, Absence{Lane: LaneCopilot, Window: "all", Reason: httpReason(code)})
		return r, CopilotFacts{}
	}
	var u copilotUser
	if err := json.Unmarshal(body, &u); err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneCopilot, Window: "all", Reason: "parse_error"})
		return r, CopilotFacts{}
	}
	q, ok := u.Snapshots["premium_interactions"]
	if !ok {
		// The plan reports no premium/credit bucket at all — a typed absence,
		// not a zero: the ledger keeps whatever it already knows.
		r.Absences = append(r.Absences, Absence{Lane: LaneCopilot, Window: string(ledger.WinMonth), Reason: "window_omitted"})
		return r, CopilotFacts{}
	}
	if q.Unlimited {
		// An unlimited entitlement has no percentage to gate on. Stated, not
		// inferred: no snapshot lands, no cap is set.
		r.Absences = append(r.Absences, Absence{Lane: LaneCopilot, Window: string(ledger.WinMonth), Reason: "unlimited"})
		return r, CopilotFacts{Unit: unitOf(q, u)}
	}
	resets := copilotReset(u, now)
	// The vendor's own percentage is the gate figure; credits_used may exceed
	// the entitlement by the last dispatch's overshoot (observed 1501/1500),
	// so the derived ratio is clamped, never trusted above 100.
	used := 100 - q.PercentRemaining
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	r.Snapshots = append(r.Snapshots, Snapshot{Lane: LaneCopilot, Window: ledger.WinMonth, UsedPct: used, ResetsAt: resets})
	return r, CopilotFacts{
		Unit: unitOf(q, u), Entitlement: q.Entitlement, Used: q.CreditsUsed, Remaining: q.Remaining,
		ResetsAt: resets, OveragePermitted: q.OveragePermitted,
	}
}

func unitOf(q copilotQuota, u copilotUser) string {
	if q.TokenBased || u.TokenBased {
		return "credits"
	}
	return "requests"
}

// copilotReset reads the vendor's reset stamp (RFC3339 first, the bare date
// second). Both absent or unparsable falls back to the documented calendar
// rule — the 1st of next month, 00:00 UTC — which is the same anchor the
// ledger already uses for this window, not a new inference.
func copilotReset(u copilotUser, now time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, u.QuotaResetUTC); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02", u.QuotaReset); err == nil {
		return t.UTC()
	}
	return ledger.NextMonthlyReset(now)
}
