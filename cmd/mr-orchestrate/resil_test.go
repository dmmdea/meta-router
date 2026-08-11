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
	"github.com/dmmdea/meta-router/internal/orch/router"
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

// W6 canary (breaker PRODUCER side): the class→action mapping of the
// breaker's only writer. Review proved the entire noteLaneHealth wiring could
// be deleted with a green suite — the arming path had zero coverage.
func TestNoteLaneHealthClassRouting(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())

	noteLaneHealth(false, "codex", "spawn_error", wnow)
	s, _ := exclusion.Load(exclusionsPath())
	if s["codex"].Fails != 1 {
		t.Fatal("spawn_error must record a qualifying failure on any lane")
	}

	noteLaneHealth(false, "claude", "api_error", wnow)
	s, _ = exclusion.Load(exclusionsPath())
	if _, tracked := s["claude"]; tracked {
		t.Fatal("cloud api_error is the VENDOR's incident and must never arm the infra breaker")
	}

	noteLaneHealth(false, "local", "api_error", wnow)
	s, _ = exclusion.Load(exclusionsPath())
	if s["local"].Fails != 1 {
		t.Fatal("local api_error is a harness fault and must arm")
	}

	noteLaneHealth(false, "codex", "rate_limit", wnow)
	s, _ = exclusion.Load(exclusionsPath())
	if s["codex"].Fails != 1 {
		t.Fatal("rate_limit is quota, never an adapter failure")
	}

	noteLaneHealth(false, "codex", "ok", wnow)
	s, _ = exclusion.Load(exclusionsPath())
	if _, tracked := s["codex"]; tracked {
		t.Fatal("ok must clear the entry")
	}

	// ok on an untracked lane skips the write entirely.
	before, _ := os.ReadFile(exclusionsPath())
	noteLaneHealth(false, "glm", "ok", wnow)
	after, _ := os.ReadFile(exclusionsPath())
	if !bytes.Equal(before, after) {
		t.Fatal("ok on an untracked lane must skip the write")
	}

	// The kill-switch disarms RECORDING, not just consumption.
	noteLaneHealth(true, "glm", "spawn_error", wnow)
	s, _ = exclusion.Load(exclusionsPath())
	if _, tracked := s["glm"]; tracked {
		t.Fatal("exclusion_off must disarm the recorder")
	}

	// A corrupt state file is re-materialized CLEAN by the next update — even
	// one that changes nothing — never left to warn forever.
	if err := os.WriteFile(exclusionsPath(), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	noteLaneHealth(false, "glm", "ok", wnow) // untracked + no-op, but corrupt → repair
	if _, warn := exclusion.Load(exclusionsPath()); warn != "" {
		t.Fatalf("corrupt state must be repaired by the next update: %q", warn)
	}
}

// W6 canary (breaker end-to-end): two real spawn_errors through runLocalLane
// arm the breaker and mask the lane — the producer wiring itself, not a
// hand-seeded state file.
func TestAdapterFailuresArmBreakerEndToEnd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	cb, _ := json.Marshal(map[string]any{
		"local_offload_bin": filepath.Join(dir, "no-such-binary-w6"), "local_max_per_min": -1})
	if err := os.WriteFile(configPath(), cb, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	for i := 0; i < 2; i++ { // threshold 2
		if _, err := runLocalLane(&out, "p", "doc-summarize", "gemma4-cascade", "", 5, true, "cli", "", recFields{}, strategyFields{}); err != nil {
			t.Fatal(err)
		}
	}
	ls := laneStates(nil, fuses.Seed(), orchcfg.Defaults(), time.Now().UTC().Add(time.Second))
	if ls["local"].State != "unavailable" {
		t.Fatalf("two spawn_errors through runLocalLane must arm the breaker: %+v", ls["local"])
	}
}

// W6 canary (limiter PRODUCER side): the stamp an allowed dispatch persists is
// what denies the next one — review proved dropping the Save left the suite
// green, making the limiter a production no-op.
func TestLimiterPersistsStampsAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MR_ORCH_STATE", dir)
	cb, _ := json.Marshal(map[string]any{
		"local_offload_bin": filepath.Join(dir, "no-such-binary-w6"), "local_max_per_min": 1})
	if err := os.WriteFile(configPath(), cb, 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code, err := runLocalLane(&out, "p", "doc-summarize", "gemma4-cascade", "", 5, true, "cli", "", recFields{}, strategyFields{}); err != nil || code == exitDeferred {
		t.Fatalf("first dispatch must be allowed (spawn_error is fine): code=%d err=%v", code, err)
	}
	out.Reset()
	code, err := runLocalLane(&out, "p", "doc-summarize", "gemma4-cascade", "", 5, true, "cli", "", recFields{}, strategyFields{})
	if err != nil {
		t.Fatal(err)
	}
	if code != exitDeferred || !strings.Contains(out.String(), `"rate_limit"`) {
		t.Fatalf("second dispatch must be denied by the stamp the FIRST persisted: code=%d out=%s", code, out.String())
	}
}

// The exhausted+excluded merge takes the LATER resume: a lane must satisfy
// BOTH constraints, and advertising the earlier one sends a scheduled resumer
// to a premature retry (review 2026-08-12). The stronger masked label also
// survives — an exclusion must not rename hard_stop/exhausted.
func TestExclusionResumeMergeTakesLater(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	short := wnow.Add(time.Minute)
	s := exclusion.Set{"codex": {Fails: 2, Until: short, LastFail: wnow, LastNote: "parse_error"}}
	if err := exclusion.Save(exclusionsPath(), s); err != nil {
		t.Fatal(err)
	}
	far := wnow.Add(5 * time.Hour)
	snap := []ledger.Bucket{{Lane: "codex", Window: ledger.Win5h, UsedPct: 100,
		Source: "provider", ResetsAt: far}}
	ls := laneStates(snap, fuses.Seed(), orchcfg.Defaults(), wnow)
	if ls["codex"].State != "exhausted" {
		t.Fatalf("exclusion must not downgrade the exhausted label: %+v", ls["codex"])
	}
	if !ls["codex"].ResumeAt.Equal(far) {
		t.Fatalf("resume must be the LATER of admission and exclusion: got %v want %v", ls["codex"].ResumeAt, far)
	}
}

// W6 canary (knob wiring): incident_mode_on must actually reach the router
// through buildRouteDecision — review proved severing `Incident:
// cfg.IncidentModeOn` left the suite green, and a knob that ships OFF is
// discovered dead exactly when the operator needs it. Seed-table Workhorse
// candidates are glm+claude; both throttled = a strict candidate majority.
func TestIncidentKnobReachesRouter(t *testing.T) {
	t.Setenv("MR_ORCH_STATE", t.TempDir())
	far := wnow.Add(4 * time.Hour)
	snap := []ledger.Bucket{
		{Lane: "glm", Window: ledger.Win5h, UsedPct: 85, Source: "provider", ResetsAt: far},
		{Lane: "claude", Window: ledger.Win5h, UsedPct: 85, Source: "provider", ResetsAt: far},
	}
	off := buildRouteDecision(orchcfg.Defaults(), fuses.Seed(), snap, router.Workhorse, 0, wnow, spendDownReq{})
	if off.Incident {
		t.Fatalf("knob off must never engage incident mode: %+v", off)
	}
	on := orchcfg.Defaults()
	on.IncidentModeOn = true
	d := buildRouteDecision(on, fuses.Seed(), snap, router.Workhorse, 0, wnow, spendDownReq{})
	if !d.Incident {
		t.Fatalf("incident_mode_on with a pressured candidate majority must engage and be recorded: %+v", d)
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
