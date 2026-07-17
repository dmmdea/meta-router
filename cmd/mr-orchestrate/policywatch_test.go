package main

import (
	"strings"
	"testing"
	"time"
)

var pnow = time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

func obsAll(claude, codex, article, zai string) observed {
	return observed{ClaudeVersion: claude, CodexVersion: codex, ArticleHash: article, ZaiHash: zai}
}

// A fetch outage must PRESERVE baselines — wiping one would make the next
// successful run re-seed and swallow exactly the change RS7 watches for.
func TestEvalPolicyPreservesBaselinesThroughOutage(t *testing.T) {
	seed := evalPolicy(policyState{}, obsAll("2.1.199", "codex-cli 0.142.5", "hashA", "zaiA"), pnow)
	outage := evalPolicy(seed, observed{ClaudeVersion: "2.1.199", CodexVersion: "codex-cli 0.142.5",
		FetchNotes: []string{"anthropic article fetch failed", "z.ai policy fetch failed"}}, pnow.Add(24*time.Hour))
	if outage.ArticleHash != "hashA" || outage.ZaiPolicyHash != "zaiA" {
		t.Fatalf("outage must preserve both baselines: %+v", outage)
	}
	changed := evalPolicy(outage, obsAll("2.1.199", "codex-cli 0.142.5", "hashB", "zaiA"), pnow.Add(48*time.Hour))
	if !changed.Alert {
		t.Fatalf("change after an outage must still alert: %+v", changed)
	}
}

// Alerts latch: an unattended nightly that alerts once must keep alerting
// until the operator acks — a self-clearing alert is never seen.
func TestEvalPolicyAlertLatches(t *testing.T) {
	seed := evalPolicy(policyState{}, obsAll("2.1.199", "codex-cli 0.142.5", "hashA", "zaiA"), pnow)
	if seed.Alert {
		t.Fatalf("first run seeds baselines, no alert: %+v", seed)
	}
	fired := evalPolicy(seed, obsAll("2.1.205", "codex-cli 0.142.5", "hashA", "zaiA"), pnow.Add(24*time.Hour))
	if !fired.Alert || fired.AlertSince == nil {
		t.Fatalf("claude version bump must alert with alert_since: %+v", fired)
	}
	next := evalPolicy(fired, obsAll("2.1.205", "codex-cli 0.142.5", "hashA", "zaiA"), pnow.Add(48*time.Hour))
	if !next.Alert {
		t.Fatalf("alert must LATCH across quiet runs: %+v", next)
	}
	if next.AlertSince == nil || !next.AlertSince.Equal(*fired.AlertSince) {
		t.Fatalf("alert_since must be preserved: %+v", next)
	}
	acked := fired
	acked.Alert = false
	acked.AlertSince = nil
	clear := evalPolicy(acked, obsAll("2.1.205", "codex-cli 0.142.5", "hashA", "zaiA"), pnow.Add(72*time.Hour))
	if clear.Alert {
		t.Fatalf("after ack a quiet run must not alert: %+v", clear)
	}
}

// Every vendor surface is a first-class tripwire: codex CLI bumps and z.ai
// policy changes alert exactly like the Anthropic surfaces.
func TestEvalPolicyCodexAndZaiSurfacesAlert(t *testing.T) {
	seed := evalPolicy(policyState{}, obsAll("2.1.199", "codex-cli 0.142.5", "hashA", "zaiA"), pnow)
	codexBump := evalPolicy(seed, obsAll("2.1.199", "codex-cli 0.143.0", "hashA", "zaiA"), pnow.Add(time.Hour))
	if !codexBump.Alert || !strings.Contains(strings.Join(codexBump.Notes, " "), "codex CLI changed") {
		t.Fatalf("codex bump must alert: %+v", codexBump)
	}
	zaiChange := evalPolicy(seed, obsAll("2.1.199", "codex-cli 0.142.5", "hashA", "zaiB"), pnow.Add(time.Hour))
	if !zaiChange.Alert || !strings.Contains(strings.Join(zaiChange.Notes, " "), "z.ai coding-plan usage policy CHANGED") {
		t.Fatalf("z.ai policy change must alert: %+v", zaiChange)
	}
	quiet := evalPolicy(seed, obsAll("2.1.199", "codex-cli 0.142.5", "hashA", "zaiA"), pnow.Add(time.Hour))
	if quiet.Alert {
		t.Fatalf("unchanged surfaces must stay quiet: %+v", quiet)
	}
}

// The relative-time badge drifts with time, not policy — outside the hash.
func TestStripHTMLIgnoresUpdatedBadge(t *testing.T) {
	a := `<html><body><div>Updated over 2 weeks ago</div><p>Subscription covers claude -p.</p></body></html>`
	b := `<html><body><div>Updated over 3 months ago</div><p>Subscription covers claude -p.</p></body></html>`
	if hashText(stripHTMLText([]byte(a))) != hashText(stripHTMLText([]byte(b))) {
		t.Fatal("relative-time badge churn must not change the policy hash")
	}
	c := strings.Replace(a, "covers claude -p", "NO LONGER covers claude -p", 1)
	if hashText(stripHTMLText([]byte(a))) == hashText(stripHTMLText([]byte(c))) {
		t.Fatal("a real policy change MUST still change the hash")
	}
}
