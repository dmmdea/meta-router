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

// The 2026-09-06 LIVE capture on the operator's upgraded plan: plan_type
// "prolite", ONLY a 7-day window for the main allowance, model_usage naming
// gpt-6-astra, and an additional_rate_limits block for GPT-5.3-Codex-Spark
// that carries its own 5h + 7d windows. The facts are surfaced, the Result is
// unchanged in shape (one 7d snapshot, a typed 5h absence).
func TestPollCodexFactsProlite(t *testing.T) {
	srv := codexServer(t, "codex-usage-prolite.json", 200)
	defer srv.Close()
	r, f := pollCodexFacts(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if len(r.Snapshots) != 1 || r.Snapshots[0].Window != ledger.Win7d || r.Snapshots[0].UsedPct != 0 {
		t.Fatalf("main allowance: want one 7d snapshot at 0%%, got %+v", r.Snapshots)
	}
	if len(r.Absences) != 1 || r.Absences[0].Window != "5h" || r.Absences[0].Reason != "window_omitted" {
		t.Fatalf("5h must stay a typed absence: %+v", r.Absences)
	}
	if f.PlanType != "prolite" || f.Has5h {
		t.Fatalf("plan facts: %+v", f)
	}
	if m, ok := f.Models["gpt-6-astra"]; !ok || !m.Available {
		t.Fatalf("model_usage must surface astra as available: %+v", f.Models)
	}
	if len(f.Additional) != 1 || f.Additional[0].Name != "GPT-5.3-Codex-Spark" || f.Additional[0].Feature != "codex_bengalfox" {
		t.Fatalf("additional limits: %+v", f.Additional)
	}
	wins := map[ledger.WindowKind]bool{}
	for _, s := range f.Additional[0].Snapshots {
		wins[s.Window] = true
	}
	if !wins[ledger.Win5h] || !wins[ledger.Win7d] {
		t.Fatalf("the Spark block carries BOTH windows (mapped by seconds): %+v", f.Additional[0].Snapshots)
	}
	// The corroboration the codex_5h_estimate_off gate needs: no 5h on the
	// main allowance, a 5h in a sibling block of the same body.
	if !f.Saw5hElsewhere() {
		t.Fatal("Saw5hElsewhere must be true on the prolite capture (Spark carries a 5h window)")
	}
	if (CodexFacts{}).Saw5hElsewhere() {
		t.Fatal("no additional blocks → no corroboration")
	}
	if (CodexFacts{Additional: []CodexAdditionalLimit{{Name: "x", Snapshots: []Snapshot{{Window: ledger.Win7d}}}}}).Saw5hElsewhere() {
		t.Fatal("a sibling block with only a 7d window is not 5h corroboration")
	}
	// The additional block's windows are NOT main-lane snapshots.
	for _, s := range r.Snapshots {
		if s.Window == ledger.Win5h {
			t.Fatal("a per-model 5h window must not masquerade as the lane's 5h window")
		}
	}
}

// The Plus-era fixture still reads as Plus with both windows and no extras —
// the facts are additive, the old contract holds.
func TestPollCodexFactsPlusFixture(t *testing.T) {
	srv := codexServer(t, "codex-usage.json", 200)
	defer srv.Close()
	r, f := pollCodexFacts(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if len(r.Snapshots) != 2 || len(r.Absences) != 0 {
		t.Fatalf("plus fixture contract regressed: %+v %+v", r.Snapshots, r.Absences)
	}
	if f.PlanType != "plus" || !f.Has5h || len(f.Additional) != 0 || len(f.Models) != 0 {
		t.Fatalf("plus facts: %+v", f)
	}
}

// Failure paths return zero facts, never partial ones.
func TestPollCodexFactsZeroOnFailure(t *testing.T) {
	srv := codexServer(t, "codex-usage.json", 500)
	defer srv.Close()
	r, f := pollCodexFacts(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if len(r.Absences) != 1 || r.Absences[0].Reason != "http_500" || f.PlanType != "" || f.Has5h {
		t.Fatalf("%+v %+v", r, f)
	}
}

// A short-span window that is PRESENT but unreadable (null used_percent) must
// record Undecodable5h — Has5h is false either way, and that conflation is
// how a decoding failure became a capacity decision.
func TestPollCodexFactsFlagsAnUndecodableShortWindow(t *testing.T) {
	srv := codexServer(t, "codex-usage-undecodable-5h.json", 200)
	defer srv.Close()
	r, f := pollCodexFacts(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if f.Has5h {
		t.Fatalf("an unreadable window yields no snapshot: %+v", f)
	}
	if !f.Undecodable5h {
		t.Fatal("a PRESENT short-span window that yielded no snapshot must record Undecodable5h")
	}
	if !f.Saw5hElsewhere() {
		t.Fatal("premise: the sibling block still reports its own 5h")
	}
	// The Result shape is unchanged: still a typed 5h absence, no snapshot.
	for _, s := range r.Snapshots {
		if s.Window == ledger.Win5h {
			t.Fatalf("no 5h snapshot may be invented: %+v", s)
		}
	}
	// And the clean capture is NOT flagged — otherwise the gate never fires.
	srv2 := codexServer(t, "codex-usage-prolite.json", 200)
	defer srv2.Close()
	if _, f2 := pollCodexFacts(guardedClient(), srv2.URL, filepath.Join("testdata", "codex-auth.json"), time.Now()); f2.Undecodable5h {
		t.Fatal("a genuinely absent short window must not be flagged unreadable")
	}
}

// An unrecognised SPAN counts as present too: wham re-spanning the short
// window (5h -> 3h) must read as "there is a short window we cannot map",
// never as "this plan has no short window".
func TestPollCodexFactsFlagsARespannedShortWindow(t *testing.T) {
	dir := t.TempDir()
	body, err := os.ReadFile(filepath.Join("testdata", "codex-usage-prolite.json"))
	if err != nil {
		t.Fatal(err)
	}
	var u map[string]any
	if err := json.Unmarshal(body, &u); err != nil {
		t.Fatal(err)
	}
	u["rate_limit"].(map[string]any)["secondary_window"] = map[string]any{
		"used_percent": 12.0, "limit_window_seconds": 10800.0, "reset_at": 1788752893.0,
	}
	nb, _ := json.Marshal(u)
	if err := os.WriteFile(filepath.Join(dir, "respanned.json"), nb, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(nb)
	}))
	defer srv.Close()
	_, f := pollCodexFacts(guardedClient(), srv.URL, filepath.Join("testdata", "codex-auth.json"), time.Now())
	if f.Has5h {
		t.Fatalf("a 3h span is not our 5h bucket: %+v", f)
	}
	if !f.Undecodable5h {
		t.Fatal("a re-spanned short window must read as unreadable, not absent")
	}
}
