package quotapoll

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

const claudeUsagePath = "/api/oauth/usage"

// ClaudeCredPath is Claude Code's own credential store (subscription OAuth —
// R10: never an API key).
func ClaudeCredPath() string {
	home := statepaths.ClaudeHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".credentials.json")
}

// claudeToken returns the subscription OAuth token Claude Code keeps under
// claudeAiOauth.accessToken, read from that one documented position and from
// nowhere else.
//
// The same store carries an `mcpOAuth` block with one accessToken per
// authorised third-party MCP server. This function used to fall back to a
// tolerant walk over the whole store ("first accessToken found") whenever the
// claudeAiOauth entry was missing, and that fallback leaked twice:
//
//   - 2026-07 to 2026-09: the walk met an MCP token first on nearly every
//     attempt, the usage endpoint answered 401, and the claude lane had no live
//     quota signal. The fix preferred claudeAiOauth but KEPT the walk for "a
//     store without that block".
//   - 2026-09 to 2026-11: the store kept the claudeAiOauth block with an EMPTY
//     accessToken (the client had moved its token elsewhere), so the kept
//     fallback ran on every poll and sent a third-party server's OAuth token
//     to the vendor's usage endpoint as a Bearer credential.
//
// A credential is not something to guess at. An empty or absent entry means
// "not logged in here": the caller reports that typed absence and makes no
// request at all.
func claudeToken(credPath string) (string, error) {
	b, err := os.ReadFile(credPath)
	if err != nil {
		return "", err
	}
	var v struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	if v.ClaudeAiOauth.AccessToken == "" {
		return "", os.ErrNotExist
	}
	return v.ClaudeAiOauth.AccessToken, nil
}

type claudeWindow struct {
	Utilization *float64  `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

type claudeUsage struct {
	FiveHour *claudeWindow `json:"five_hour"`
	SevenDay *claudeWindow `json:"seven_day"`
	Limits   []struct {
		Kind     string          `json:"kind"`
		Percent  *float64        `json:"percent"`
		Severity string          `json:"severity"`
		ResetsAt time.Time       `json:"resets_at"`
		Scope    json.RawMessage `json:"scope"`
		IsActive bool            `json:"is_active"`
	} `json:"limits"`
}

// PollClaude polls the oauth usage endpoint with Claude Code's own
// subscription token. Fixture shape captured live 2026-07-23.
func PollClaude(now time.Time) Result {
	return pollClaude(guardedClient(), "https://api.anthropic.com", ClaudeCredPath(), now)
}

// PollClaudeAt polls using an explicit credential path (W2 per-profile: a
// profile's isolated CLAUDE_CONFIG_DIR home). Empty path = the default home.
func PollClaudeAt(credPath string, now time.Time) Result {
	if credPath == "" {
		credPath = ClaudeCredPath()
	}
	return pollClaude(guardedClient(), "https://api.anthropic.com", credPath, now)
}

func pollClaude(c *http.Client, baseURL, credPath string, now time.Time) Result {
	var r Result
	tok, err := claudeToken(credPath)
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneClaude, Window: "all", Reason: "not_logged_in"})
		return r
	}
	body, code, err := getJSON(c, baseURL+claudeUsagePath, tok, map[string]string{"anthropic-beta": "oauth-2025-04-20"})
	if err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneClaude, Window: "all", Reason: "refresh_failed"})
		return r
	}
	if code != http.StatusOK {
		r.Absences = append(r.Absences, Absence{Lane: LaneClaude, Window: "all", Reason: httpReason(code)})
		return r
	}
	var u claudeUsage
	if err := json.Unmarshal(body, &u); err != nil {
		r.Absences = append(r.Absences, Absence{Lane: LaneClaude, Window: "all", Reason: "parse_error"})
		return r
	}
	addWindow := func(w *claudeWindow, kind ledger.WindowKind, name string) {
		if w == nil || w.Utilization == nil || w.ResetsAt.IsZero() {
			r.Absences = append(r.Absences, Absence{Lane: LaneClaude, Window: name, Reason: "window_omitted"})
			return
		}
		r.Snapshots = append(r.Snapshots, Snapshot{Lane: LaneClaude, Window: kind, UsedPct: *w.Utilization, ResetsAt: w.ResetsAt})
	}
	addWindow(u.FiveHour, ledger.Win5h, "5h")
	addWindow(u.SevenDay, ledger.Win7d, "7d")
	for _, l := range u.Limits {
		if l.Kind == "session" || l.Kind == "weekly_all" || l.Percent == nil {
			continue // the two plain windows are already snapshots; scoped extras only
		}
		r.Scoped = append(r.Scoped, ScopedAlert{Kind: l.Kind, Percent: *l.Percent, Severity: l.Severity, ResetsAt: l.ResetsAt, Scope: string(l.Scope), IsActive: l.IsActive})
	}
	return r
}
