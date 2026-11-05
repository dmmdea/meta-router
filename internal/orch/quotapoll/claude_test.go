package quotapoll

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func claudeServer(t *testing.T, fixture string, status int) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("anthropic-beta") != "oauth-2025-04-20" {
			t.Errorf("missing anthropic-beta header")
		}
		if r.Header.Get("Authorization") != "Bearer test-token-redacted" {
			t.Errorf("missing bearer")
		}
		w.WriteHeader(status)
		w.Write(b)
	}))
}

func TestPollClaudeParsesBothWindowsAndScoped(t *testing.T) {
	srv := claudeServer(t, "claude-usage.json", 200)
	defer srv.Close()
	r := pollClaude(guardedClient(), srv.URL, filepath.Join("testdata", "claude-credentials.json"), time.Now())
	if len(r.Snapshots) != 2 {
		t.Fatalf("want 2 snapshots, got %+v", r.Snapshots)
	}
	if r.Snapshots[0].UsedPct != 18 || r.Snapshots[1].UsedPct != 65 {
		t.Fatalf("bad utilization parse: %+v", r.Snapshots)
	}
	if len(r.Absences) != 0 {
		t.Fatalf("no absences expected, got %+v", r.Absences)
	}
	found := false
	for _, s := range r.Scoped {
		if s.Kind == "weekly_scoped" && s.Severity == "critical" && s.Percent == 90 {
			found = true
		}
	}
	if !found {
		t.Fatalf("scoped critical limit must surface, got %+v", r.Scoped)
	}
}

func TestPollClaudeTypedAbsenceOnMissingWindow(t *testing.T) {
	srv := claudeServer(t, "claude-usage-missing-window.json", 200)
	defer srv.Close()
	r := pollClaude(guardedClient(), srv.URL, filepath.Join("testdata", "claude-credentials.json"), time.Now())
	if len(r.Snapshots) != 1 {
		t.Fatalf("want 1 snapshot, got %+v", r.Snapshots)
	}
	if len(r.Absences) != 1 || r.Absences[0].Reason != "window_omitted" || r.Absences[0].Window != "5h" {
		t.Fatalf("want typed 5h window_omitted absence, got %+v", r.Absences)
	}
}

func TestPollClaudeNotLoggedIn(t *testing.T) {
	r := pollClaude(guardedClient(), "https://api.anthropic.com", filepath.Join("testdata", "nope.json"), time.Now())
	if len(r.Snapshots) != 0 || len(r.Absences) != 1 || r.Absences[0].Reason != "not_logged_in" {
		t.Fatalf("missing credentials must be a typed not_logged_in absence (and no network attempt), got %+v", r)
	}
}

func TestPollClaudeHTTPFailure(t *testing.T) {
	srv := claudeServer(t, "claude-usage.json", 401)
	defer srv.Close()
	r := pollClaude(guardedClient(), srv.URL, filepath.Join("testdata", "claude-credentials.json"), time.Now())
	if len(r.Absences) != 1 || r.Absences[0].Reason != "http_401" || r.Absences[0].Window != "all" {
		t.Fatalf("want http_401 all-window absence, got %+v", r.Absences)
	}
}

// The live store lists mcpOAuth (one accessToken per MCP server) BEFORE
// claudeAiOauth; the first-match walk sent an MCP token to the usage endpoint
// and every poll answered 401 (2026-07..09). The subscription token must win.
func TestPollClaudePrefersClaudeAiOauthTokenOverMCPTokens(t *testing.T) {
	srv := claudeServer(t, "claude-usage.json", http.StatusOK) // asserts Bearer test-token-redacted
	defer srv.Close()
	r := pollClaude(guardedClient(), srv.URL, filepath.Join("testdata", "claude-credentials-mcp-first.json"), time.Now())
	if len(r.Snapshots) != 2 || len(r.Absences) != 0 {
		t.Fatalf("expected both windows with the claudeAiOauth token, got snapshots=%d absences=%+v", len(r.Snapshots), r.Absences)
	}
}

// The incident this guards (2026-09 to 2026-11): the store kept its
// claudeAiOauth block with an EMPTY accessToken while a third-party MCP
// server's token sat under mcpOAuth, and a fallback walk sent that token to
// the usage endpoint on every poll. An empty subscription token is "not logged
// in": a typed absence and no request, whatever else the store holds.
func TestPollClaudeSendsNothingWhenSubscriptionTokenIsEmpty(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		t.Errorf("a request was made with Authorization %q", r.Header.Get("Authorization"))
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "credentials.json")
	body := `{"mcpOAuth": {"some-server|0123": {"accessToken": "third-party-token", "expiresAt": 99999999999}},
	          "claudeAiOauth": {"accessToken": "", "refreshToken": "", "expiresAt": 0}}`
	if err := os.WriteFile(store, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r := pollClaude(guardedClient(), srv.URL, store, time.Now())
	if hits != 0 {
		t.Fatalf("the poll contacted the endpoint %d time(s) without a subscription token", hits)
	}
	if len(r.Snapshots) != 0 || len(r.Absences) != 1 || r.Absences[0].Reason != "not_logged_in" {
		t.Fatalf("want one typed not_logged_in absence, got %+v", r)
	}
}
