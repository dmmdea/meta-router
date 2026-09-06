package quotapoll

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

// The fixture is the FULL live response of `gh api copilot_internal/user` on
// the exhausted dmmdea account (2026-09-06, tracking id redacted) — every
// key the vendor sent, not the ones the parser consumes. A filtered fixture
// is what hid the polymorphic-payload bug in the dispatch parser; the same
// discipline applies here.
func copilotFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "copilot-internal-user-exhausted.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func copilotServer(t *testing.T, body []byte, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != copilotUserPath {
			t.Errorf("path %q, want %q", r.URL.Path, copilotUserPath)
		}
		if r.Header.Get("Authorization") != "Bearer minted-token-redacted" {
			t.Errorf("the poll must authenticate with the minted token")
		}
		w.WriteHeader(status)
		w.Write(body)
	}))
}

func TestPollCopilotMapsTheExhaustedMonth(t *testing.T) {
	srv := copilotServer(t, copilotFixture(t), 200)
	defer srv.Close()
	now := time.Date(2026, 9, 6, 16, 0, 0, 0, time.UTC)
	r, f := pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", now)
	if len(r.Absences) != 0 {
		t.Fatalf("unexpected absences: %+v", r.Absences)
	}
	if len(r.Snapshots) != 1 {
		t.Fatalf("want exactly one month snapshot, got %+v", r.Snapshots)
	}
	s := r.Snapshots[0]
	if s.Lane != LaneCopilot || s.Window != ledger.WinMonth {
		t.Fatalf("snapshot must be copilot/month: %+v", s)
	}
	if s.UsedPct != 100 {
		t.Fatalf("percent_remaining 0.0 must read as 100%% used, got %v", s.UsedPct)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !s.ResetsAt.Equal(want) {
		t.Fatalf("reset must come from quota_reset_date_utc (%s), got %s", want, s.ResetsAt)
	}
	// The facts are what the ledger cap and the operator report are built
	// from: 1,500 credits entitled, 1,501 consumed, overage structurally off.
	if f.Unit != "credits" || f.Entitlement != 1500 || f.Used != 1501 || f.Remaining != -2 || f.OveragePermitted {
		t.Fatalf("facts wrong: %+v", f)
	}
}

// A mid-month reading (the fixture with the vendor's own numbers edited) must
// produce the vendor's percentage, not a ratio we compute ourselves.
func TestPollCopilotUsesTheVendorPercentage(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(copilotFixture(t), &doc); err != nil {
		t.Fatal(err)
	}
	snaps := doc["quota_snapshots"].(map[string]any)
	pi := snaps["premium_interactions"].(map[string]any)
	pi["percent_remaining"] = 61.6
	pi["credits_used"] = 576.0
	pi["remaining"] = 924.0
	body, _ := json.Marshal(doc)
	srv := copilotServer(t, body, 200)
	defer srv.Close()
	r, f := pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", time.Now())
	if len(r.Snapshots) != 1 || r.Snapshots[0].UsedPct < 38.39 || r.Snapshots[0].UsedPct > 38.41 {
		t.Fatalf("want 38.4%% used, got %+v", r.Snapshots)
	}
	if f.Used != 576 || f.Remaining != 924 || f.Entitlement != 1500 {
		t.Fatalf("facts wrong: %+v", f)
	}
}

// Overshoot is real (1501 used of 1500): a derived percentage would exceed
// 100 and a vendor percentage below zero is conceivable — both clamp.
func TestPollCopilotClampsThePercentage(t *testing.T) {
	var doc map[string]any
	_ = json.Unmarshal(copilotFixture(t), &doc)
	pi := doc["quota_snapshots"].(map[string]any)["premium_interactions"].(map[string]any)
	pi["percent_remaining"] = -1.9
	body, _ := json.Marshal(doc)
	srv := copilotServer(t, body, 200)
	defer srv.Close()
	r, _ := pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", time.Now())
	if len(r.Snapshots) != 1 || r.Snapshots[0].UsedPct != 100 {
		t.Fatalf("negative remaining must clamp to 100%% used, got %+v", r.Snapshots)
	}
}

func TestPollCopilotAbsencesAreTyped(t *testing.T) {
	now := time.Now()
	// no token: the caller failed to mint — never fall through to ambient env
	r, _ := pollCopilot(guardedClient(), "https://127.0.0.1:1", "", now)
	if len(r.Absences) != 1 || r.Absences[0].Reason != "not_logged_in" || len(r.Snapshots) != 0 {
		t.Fatalf("empty token must be a typed not_logged_in: %+v", r)
	}
	// vendor denial
	srv := copilotServer(t, []byte(`{"message":"Bad credentials"}`), 401)
	r, _ = pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", now)
	srv.Close()
	if len(r.Absences) != 1 || r.Absences[0].Reason != "http_401" {
		t.Fatalf("401 must be typed http_401: %+v", r)
	}
	// garbage body
	srv = copilotServer(t, []byte(`<html>`), 200)
	r, _ = pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", now)
	srv.Close()
	if len(r.Absences) != 1 || r.Absences[0].Reason != "parse_error" {
		t.Fatalf("undecodable body must be parse_error: %+v", r)
	}
	// plan without the premium bucket
	srv = copilotServer(t, []byte(`{"copilot_plan":"free","quota_snapshots":{"chat":{"unlimited":true}}}`), 200)
	r, _ = pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", now)
	srv.Close()
	if len(r.Absences) != 1 || r.Absences[0].Reason != "window_omitted" || r.Absences[0].Window != "month" {
		t.Fatalf("missing premium_interactions must be a month window_omitted: %+v", r)
	}
}

// An unlimited entitlement is a typed statement, never a 0% snapshot that
// would look like a fresh month to the router.
func TestPollCopilotUnlimitedIsStatedNotZero(t *testing.T) {
	var doc map[string]any
	_ = json.Unmarshal(copilotFixture(t), &doc)
	pi := doc["quota_snapshots"].(map[string]any)["premium_interactions"].(map[string]any)
	pi["unlimited"] = true
	body, _ := json.Marshal(doc)
	srv := copilotServer(t, body, 200)
	defer srv.Close()
	r, f := pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", time.Now())
	if len(r.Snapshots) != 0 || len(r.Absences) != 1 || r.Absences[0].Reason != "unlimited" {
		t.Fatalf("unlimited must produce no snapshot and a typed absence: %+v", r)
	}
	if f.Entitlement != 0 {
		t.Fatalf("no cap may be derived from an unlimited plan: %+v", f)
	}
}

// Legacy annual plans still bill premium requests; the unit must be reported
// so a caller never sets a credit cap from a request count.
func TestPollCopilotReportsTheLegacyUnit(t *testing.T) {
	var doc map[string]any
	_ = json.Unmarshal(copilotFixture(t), &doc)
	doc["token_based_billing"] = false
	pi := doc["quota_snapshots"].(map[string]any)["premium_interactions"].(map[string]any)
	pi["token_based_billing"] = false
	pi["entitlement"] = 300.0
	body, _ := json.Marshal(doc)
	srv := copilotServer(t, body, 200)
	defer srv.Close()
	_, f := pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", time.Now())
	if f.Unit != "requests" || f.Entitlement != 300 {
		t.Fatalf("legacy plan must report unit=requests: %+v", f)
	}
}

// Without a vendor reset stamp the window anchors to the documented calendar
// rule — the same anchor the ledger uses — never to a zero time.
func TestPollCopilotResetFallsBackToTheCalendar(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	srv := copilotServer(t, []byte(`{"token_based_billing":true,"quota_snapshots":{"premium_interactions":{"entitlement":1500,"credits_used":10,"remaining":1490,"percent_remaining":99.3,"token_based_billing":true}}}`), 200)
	defer srv.Close()
	r, _ := pollCopilot(guardedClient(), srv.URL, "minted-token-redacted", now)
	if len(r.Snapshots) != 1 || !r.Snapshots[0].ResetsAt.Equal(ledger.NextMonthlyReset(now)) {
		t.Fatalf("reset fallback wrong: %+v", r.Snapshots)
	}
}
