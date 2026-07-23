package quotapoll

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
)

func codexServer(t *testing.T, fixture string, status int) *httptest.Server {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token-redacted" {
			t.Errorf("missing bearer")
		}
		if r.Header.Get("ChatGPT-Account-Id") != "acct-1" {
			t.Errorf("missing account id header")
		}
		w.WriteHeader(status)
		w.Write(b)
	}))
}

func TestPollCodexMapsWindowsBySeconds(t *testing.T) {
	// The 2026-07-23 live capture proved the 7-DAY window arrives as
	// primary_window (604800s) — mapping keys off limit_window_seconds,
	// never off primary/secondary position.
	srv := codexServer(t, "codex-usage.json", 200)
	defer srv.Close()
	r := pollCodex(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if len(r.Snapshots) != 2 {
		t.Fatalf("want 2 snapshots, got %+v", r.Snapshots)
	}
	byWin := map[ledger.WindowKind]Snapshot{}
	for _, s := range r.Snapshots {
		byWin[s.Window] = s
	}
	if byWin[ledger.Win7d].UsedPct != 46 {
		t.Fatalf("7d must map from the 604800s window (46%%), got %+v", byWin)
	}
	if byWin[ledger.Win5h].UsedPct != 12 {
		t.Fatalf("5h must map from the 18000s window (12%%), got %+v", byWin)
	}
	if byWin[ledger.Win7d].ResetsAt.Unix() != 1785258223 {
		t.Fatalf("reset_at epoch mapping wrong: %+v", byWin[ledger.Win7d])
	}
	if len(r.Absences) != 0 {
		t.Fatalf("no absences expected, got %+v", r.Absences)
	}
}

func TestPollCodexTypedAbsenceOn5hOmission(t *testing.T) {
	srv := codexServer(t, "codex-usage-no-5h.json", 200)
	defer srv.Close()
	r := pollCodex(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if len(r.Snapshots) != 1 || r.Snapshots[0].Window != ledger.Win7d {
		t.Fatalf("want the 7d snapshot only, got %+v", r.Snapshots)
	}
	if len(r.Absences) != 1 || r.Absences[0].Reason != "window_omitted" || r.Absences[0].Window != "5h" {
		t.Fatalf("Q10: omitted 5h must be a typed absence, got %+v", r.Absences)
	}
}

func TestPollCodexNotLoggedIn(t *testing.T) {
	r := pollCodex(guardedClient(), "https://chatgpt.com", filepath.Join("testdata", "nope.json"), time.Now())
	if len(r.Snapshots) != 0 || len(r.Absences) != 1 || r.Absences[0].Reason != "not_logged_in" {
		t.Fatalf("missing auth must be typed not_logged_in (no network attempt), got %+v", r)
	}
}

func TestPollCodexHTTPFailure(t *testing.T) {
	srv := codexServer(t, "codex-usage.json", 403)
	defer srv.Close()
	r := pollCodex(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if len(r.Absences) != 1 || r.Absences[0].Reason != "http_403" || r.Absences[0].Window != "all" {
		t.Fatalf("want http_403 all-window absence, got %+v", r.Absences)
	}
}
