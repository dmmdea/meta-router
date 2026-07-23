package quotapoll

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

const claudeUsagePath = "/api/oauth/usage"

// ClaudeCredPath is Claude Code's own credential store (subscription OAuth —
// R10: never an API key).
func ClaudeCredPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", ".credentials.json")
}

func claudeToken(credPath string) (string, error) {
	b, err := os.ReadFile(credPath)
	if err != nil {
		return "", err
	}
	tok := FindStringField(b, "accessToken")
	if tok == "" {
		return "", os.ErrNotExist
	}
	return tok, nil
}

// FindStringField walks arbitrary JSON for the first string value under key.
// Tolerant by design: vendor credential-store layouts are undocumented.
// (Moved from cmd/mr-orchestrate/probe.go — one definition, both callers.)
func FindStringField(raw []byte, key string) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(n any) string {
		switch t := n.(type) {
		case map[string]any:
			if s, ok := t[key].(string); ok && s != "" {
				return s
			}
			for _, c := range t {
				if s := walk(c); s != "" {
					return s
				}
			}
		case []any:
			for _, c := range t {
				if s := walk(c); s != "" {
					return s
				}
			}
		}
		return ""
	}
	return walk(v)
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
	} `json:"limits"`
}

// PollClaude polls the oauth usage endpoint with Claude Code's own
// subscription token. Fixture shape captured live 2026-07-23.
func PollClaude(now time.Time) Result {
	return pollClaude(guardedClient(), "https://api.anthropic.com", ClaudeCredPath(), now)
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
		r.Scoped = append(r.Scoped, ScopedAlert{Kind: l.Kind, Percent: *l.Percent, Severity: l.Severity, ResetsAt: l.ResetsAt, Scope: string(l.Scope)})
	}
	return r
}
