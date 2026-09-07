package quotapoll

import (
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
