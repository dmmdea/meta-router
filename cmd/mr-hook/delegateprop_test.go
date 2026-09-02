package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/statepaths"
)

var pnow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func seedDrop(t *testing.T, fiveHour, sevenDay float64) {
	t.Helper()
	reset := pnow.Add(2 * time.Hour).Unix()
	raw := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":%g,"resets_at":%d},"seven_day":{"used_percentage":%g,"resets_at":%d}}}`, fiveHour, reset, sevenDay, reset)
	if err := os.WriteFile(statepaths.Drop(), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func armSession(t *testing.T, sid string, ts time.Time) {
	t.Helper()
	dir := filepath.Join(os.Getenv("CLAUDE_STATE_DIR"), "delegate-mode")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"active":true,"ts":%q,"armed_by":"operator","scope":"substantive"}`, ts.Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(dir, sid+".json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setup(t *testing.T) {
	t.Helper()
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	t.Setenv("CLAUDE_STATE_DIR", t.TempDir())
}

func TestProposalFiresAboveThresholdWhenNotArmed(t *testing.T) {
	setup(t)
	seedDrop(t, 82, 40)
	p := delegateProposal(pnow, "abc123")
	for _, want := range []string{"/delegate-mode", "82%", "Never arm it yourself"} {
		if !strings.Contains(p, want) {
			t.Fatalf("proposal missing %q: %q", want, p)
		}
	}
}

func TestProposalSilentBelowThreshold(t *testing.T) {
	setup(t)
	seedDrop(t, 30, 65)
	if p := delegateProposal(pnow, "abc123"); p != "" {
		t.Fatalf("want silence below 70%%, got %q", p)
	}
}

func TestProposalSilentWhenArmed(t *testing.T) {
	setup(t)
	seedDrop(t, 90, 90)
	armSession(t, "abc123", pnow.Add(-time.Hour))
	if p := delegateProposal(pnow, "abc123"); p != "" {
		t.Fatalf("armed session must not be proposed to: %q", p)
	}
}

// An expired arm (8h TTL, spec §1) is not armed: propose again.
func TestProposalFiresWhenArmExpired(t *testing.T) {
	setup(t)
	seedDrop(t, 90, 90)
	armSession(t, "abc123", pnow.Add(-9*time.Hour))
	if p := delegateProposal(pnow, "abc123"); p == "" {
		t.Fatal("expired arm must count as not armed")
	}
}

// Fail-open is absolute: no drop, no signal, no proposal.
func TestProposalSilentWithoutDrop(t *testing.T) {
	setup(t)
	if p := delegateProposal(pnow, "abc123"); p != "" {
		t.Fatalf("no drop → no proposal, got %q", p)
	}
}

// An EXPIRED window is dead history (quotahint.go lesson): it must not fire.
func TestProposalIgnoresExpiredWindow(t *testing.T) {
	setup(t)
	past := pnow.Add(-time.Hour).Unix()
	raw := fmt.Sprintf(`{"rate_limits":{"five_hour":{"used_percentage":95,"resets_at":%d}}}`, past)
	if err := os.WriteFile(statepaths.Drop(), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := delegateProposal(pnow, "abc123"); p != "" {
		t.Fatalf("expired window fired: %q", p)
	}
}

// No drop is the NORMAL state in a VS Code-extension session (statusLine never
// runs there): the proposal must fall back to the poll-fed ledger figure.
func TestProposalFallsBackToFreshLedgerPollWithoutDrop(t *testing.T) {
	setup(t)
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 82, pnow.Add(3*time.Hour), pnow.Add(-5*time.Minute))
		l.ObserveProvider("codex", ledger.Win5h, 99, pnow.Add(2*time.Hour), pnow.Add(-5*time.Minute)) // other lanes never count
	}); err != nil {
		t.Fatal(err)
	}
	p := delegateProposal(pnow, "abc123")
	if !strings.Contains(p, "82%") || !strings.Contains(p, "/delegate-mode") {
		t.Fatalf("fresh ledger poll figure must fire the proposal, got %q", p)
	}
}

// A poll figure older than DropMaxAge is history: no drop + stale ledger = silent.
func TestProposalIgnoresStaleLedgerPoll(t *testing.T) {
	setup(t)
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 95, pnow.Add(3*time.Hour), pnow.Add(-2*time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if p := delegateProposal(pnow, "abc123"); p != "" {
		t.Fatalf("stale ledger poll must not fire, got %q", p)
	}
}

// A live drop wins over the ledger even when the ledger is fresher-looking.
func TestProposalPrefersDropOverLedger(t *testing.T) {
	setup(t)
	seedDrop(t, 40, 30) // below the default threshold
	if err := ledger.Update(statepaths.Ledger(), func(l *ledger.Ledger) {
		l.ObserveProvider("claude", ledger.Win5h, 95, pnow.Add(3*time.Hour), pnow.Add(-time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	if p := delegateProposal(pnow, "abc123"); p != "" {
		t.Fatalf("a live drop below threshold must stay silent regardless of the ledger, got %q", p)
	}
}
