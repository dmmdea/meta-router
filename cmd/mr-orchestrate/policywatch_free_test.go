package main

import (
	"strings"
	"testing"
)

// The free-lane vendor docs (Groq rate limits, Cloudflare pricing) join the
// policy watch: baselines seed, survive an outage, and a change alerts with a
// note naming the config to re-verify.
func TestEvalPolicyFreeLaneDocs(t *testing.T) {
	seed := evalPolicy(policyState{}, observed{ClaudeVersion: "2.1.263", GroqLimitsHash: "gA", CloudflarePricingHash: "cA"}, pnow)
	if seed.Alert || seed.GroqLimitsHash != "gA" || seed.CloudflarePricingHash != "cA" {
		t.Fatalf("first run seeds: %+v", seed)
	}
	outage := evalPolicy(seed, observed{ClaudeVersion: "2.1.263", FetchNotes: []string{"groq rate-limits doc fetch failed"}}, pnow.Add(24*3600e9))
	if outage.GroqLimitsHash != "gA" || outage.CloudflarePricingHash != "cA" || outage.Alert {
		t.Fatalf("an outage must preserve both baselines without alerting: %+v", outage)
	}
	changed := evalPolicy(outage, observed{ClaudeVersion: "2.1.263", GroqLimitsHash: "gB", CloudflarePricingHash: "cA"}, pnow.Add(48*3600e9))
	if !changed.Alert || changed.LastGroqLimitsHash != "gA" || changed.GroqLimitsHash != "gB" {
		t.Fatalf("groq doc change must alert: %+v", changed)
	}
	notes := strings.Join(changed.Notes, "\n")
	if !strings.Contains(notes, "free_providers.groq") {
		t.Fatalf("the alert must name the config to re-verify: %s", notes)
	}
	changed2 := evalPolicy(seed, observed{ClaudeVersion: "2.1.263", GroqLimitsHash: "gA", CloudflarePricingHash: "cB"}, pnow.Add(24*3600e9))
	if !changed2.Alert || !strings.Contains(strings.Join(changed2.Notes, "\n"), "free_cloudflare_neuron_rates") {
		t.Fatalf("cloudflare pricing change must alert naming the rate table: %+v", changed2)
	}
}
