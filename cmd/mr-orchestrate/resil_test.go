package main

// W6 resilience canaries — each goes red when its feature is reverted (the
// charter acceptance gate for the W6 remainder).

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dmmdea/meta-router/internal/orch/claudelane"
	"github.com/dmmdea/meta-router/internal/orch/dispatch"
	"github.com/dmmdea/meta-router/internal/orch/exclusion"
	"github.com/dmmdea/meta-router/internal/orch/fuses"
	"github.com/dmmdea/meta-router/internal/orch/ledger"
	"github.com/dmmdea/meta-router/internal/orch/orchcfg"
	"github.com/dmmdea/meta-router/internal/orch/slidewin"
)

var wnow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

// W6 canary (typed 429): a LOCALLY-originated rate limit must never be
// recorded as vendor exhaustion — reverting the origin gate in
// applyRunOutcomeSubject turns this red. The upstream and untyped ("",
// pre-W6 producers) cases keep the existing exhaustion behavior.
func TestLocalRateLimitNeverExhaustsLedger(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	th := defaultThresholds

	p := filepath.Join(t.TempDir(), "ledger.json")
	if err := ledger.Update(p, func(l *ledger.Ledger) {
		applyRunOutcome(l, claudelane.Outcome{Class: "rate_limit", RateLimitOrigin: "local"}, wnow)
	}); err != nil {
		t.Fatal(err)
	}
	g := gate(ledger.Open(p).Snapshot(), "claude", "sonnet", fuses.Seed(), wnow.Add(time.Minute), orchcfg.Defaults(), false, th)
	if !g.Admit {
		t.Fatalf("a LOCAL rate limit must not exhaust the vendor window: %+v", g)
	}

	for _, origin := range []string{"upstream", ""} {
		p2 := filepath.Join(t.TempDir(), "ledger.json")
		if err := ledger.Update(p2, func(l *ledger.Ledger) {
			applyRunOutcome(l, claudelane.Outcome{Class: "rate_limit", RateLimitOrigin: origin}, wnow)
		}); err != nil {
			t.Fatal(err)
		}
		g := gate(ledger.Open(p2).Snapshot(), "claude", "sonnet", fuses.Seed(), wnow.Add(time.Minute), orchcfg.Defaults(), false, th)
		if g.Admit {
			t.Fatalf("origin %q must keep the vendor-exhaustion behavior: %+v", origin, g)
		}
	}
}

// W6 canary (self-healing exclusion): an excluded lane is masked in
// laneStates as "unavailable" with the breaker's resume; the kill-switch
// restores it. Reverting the laneStates wiring turns this red.
func TestExclusionMasksLaneInLaneStates(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	until := wnow.Add(10 * time.Minute)
	s := exclusion.Set{"codex": {Fails: 2, Until: until, LastFail: wnow, LastNote: "spawn_error"},
		"local": {Fails: 3, Until: until, LastFail: wnow, LastNote: "api_error"}}
	if err := exclusion.Save(exclusionsPath(), s); err != nil {
		t.Fatal(err)
	}

	ls := laneStates(nil, fuses.Seed(), orchcfg.Defaults(), wnow)
	if ls["codex"].State != "unavailable" || !ls["codex"].ResumeAt.Equal(until) {
		t.Fatalf("excluded codex must mask unavailable with the breaker resume: %+v", ls["codex"])
	}
	// Health masks even the always-open free lane (a broken adapter binary is
	// the one thing that may briefly mask it — S3R-10 stays a QUOTA contract).
	if ls["local"].State != "unavailable" {
		t.Fatalf("excluded local must mask too (health, not quota): %+v", ls["local"])
	}
	if ls["claude"].State == "unavailable" {
		t.Fatal("an unexcluded lane must be untouched")
	}

	off := orchcfg.Defaults()
	off.ExclusionOff = true
	ls = laneStates(nil, fuses.Seed(), off, wnow)
	if ls["codex"].State == "unavailable" {
		t.Fatal("exclusion_off must disarm the breaker")
	}

	// Expired backoff self-heals: no mask.
	ls = laneStates(nil, fuses.Seed(), orchcfg.Defaults(), until.Add(time.Second))
	if ls["codex"].State == "unavailable" {
		t.Fatal("an expired backoff must admit the lane again")
	}
}

// W6 canary (sliding-window limiter, full wiring): with the window at
// capacity, runLocalLane denies WITHOUT spawning the adapter, writes a
// receipt typed rate_limit_origin=local, and exits 3 (relegation, like an
// honest defer). The configured binary is deliberately nonexistent so a
// reverted limiter shows up as spawn_error/exit-5 — red either way.
func TestLocalLimiterDeniesAndRelegates(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	cfg := map[string]any{"local_offload_bin": filepath.Join(dir, "no-such-binary-w6"), "local_max_per_min": 2}
	cb, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath(), cb, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := slidewin.Save(localLimiterPath(), slidewin.Window{Stamps: []time.Time{now.Add(-time.Second), now.Add(-2 * time.Second)}}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code, err := runLocalLane(&out, "summarize this", "doc-summarize", "gemma4-cascade", "", 5, true, "cli", "", recFields{}, strategyFields{})
	if err != nil {
		t.Fatal(err)
	}
	if code != exitDeferred {
		t.Fatalf("a limited local dispatch must RELEGATE (exit %d), got %d; out=%s", exitDeferred, code, out.String())
	}
	if !strings.Contains(out.String(), `"rate_limit"`) || !strings.Contains(out.String(), `"local"`) {
		t.Fatalf("denial must be a typed local rate_limit: %s", out.String())
	}
	// The receipt records the typed origin — countable from the log alone.
	recs, skipped, rerr := loadReceiptsCounted(dispatchPath())
	if rerr != nil || skipped != 0 || len(recs) != 1 {
		t.Fatalf("want exactly the denial receipt: %d recs %d skipped err=%v", len(recs), skipped, rerr)
	}
	if recs[0].OutcomeClass != "rate_limit" || recs[0].RateLimitOrigin != "local" {
		t.Fatalf("receipt must be typed local: %+v", recs[0])
	}
}

// Guard the receipt schema: the typed origin must survive a JSONL round-trip
// (an omitempty typo would silently drop the distinction from the log).
func TestRateLimitOriginRoundTrips(t *testing.T) {
	b, _ := json.Marshal(dispatch.Record{TS: wnow, Lane: "local", OutcomeClass: "rate_limit", RateLimitOrigin: "local"})
	var r dispatch.Record
	if err := json.Unmarshal(b, &r); err != nil || r.RateLimitOrigin != "local" {
		t.Fatalf("rate_limit_origin must round-trip: %s err=%v", b, err)
	}
}
