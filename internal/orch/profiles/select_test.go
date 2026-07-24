package profiles

import "testing"

func fp(v float64) *float64 { return &v }

func TestSelectPrefersOpenThenSlackThenOrder(t *testing.T) {
	reg := Registry{"claude": {
		{Subject: "default", Provisioned: true},
		{Subject: "acct2", Provisioned: true},
	}}

	// Both open: registry order wins (operator preference — account-1 first)
	// when slack is equal/unknown.
	sub, _, why := Select(reg, "claude", map[string]SubjectState{
		"default": {State: "open"},
		"acct2":   {State: "open"},
	})
	if sub != "default" {
		t.Fatalf("equal-open must keep registry order, got %s (%s)", sub, why)
	}

	// Default exhausted, acct2 open → rotate to acct2, provenance set.
	sub, _, why = Select(reg, "claude", map[string]SubjectState{
		"default": {State: "exhausted"},
		"acct2":   {State: "open"},
	})
	if sub != "acct2" {
		t.Fatalf("must rotate off an exhausted subject, got %s", sub)
	}
	if why == "" {
		t.Fatal("rotation must carry a provenance reason")
	}

	// Both open, acct2 has more slack → slack breaks the tie before order.
	sub, _, _ = Select(reg, "claude", map[string]SubjectState{
		"default": {State: "open", Slack: fp(-0.3)},
		"acct2":   {State: "open", Slack: fp(0.4)},
	})
	if sub != "acct2" {
		t.Fatalf("higher slack must win among equal states, got %s", sub)
	}

	// Slack decides ONLY when both are known; a nil on either side falls
	// through to registry order (operator preference, R15) rather than letting
	// a partial measurement override the preferred account.
	sub, _, _ = Select(reg, "claude", map[string]SubjectState{
		"default": {State: "open"},                 // slack unknown
		"acct2":   {State: "open", Slack: fp(-0.9)}, // known but worse
	})
	if sub != "default" {
		t.Fatalf("one-sided slack must not override registry order, got %s", sub)
	}
}

func TestSelectSkipsUnprovisioned(t *testing.T) {
	reg := Registry{"claude": {
		{Subject: "default", Provisioned: true},
		{Subject: "acct2", Provisioned: false}, // never selectable
	}}
	sub, _, _ := Select(reg, "claude", map[string]SubjectState{
		"default": {State: "exhausted"},
		"acct2":   {State: "open"},
	})
	if sub != "default" {
		t.Fatalf("an unprovisioned profile must never be selected even when the provisioned one is exhausted (relegation handles that upstream), got %s", sub)
	}
}

func TestSelectSingleProfileIsDefault(t *testing.T) {
	reg := Registry{"claude": {{Subject: "default", Provisioned: true}}}
	sub, _, why := Select(reg, "claude", map[string]SubjectState{"default": {State: "throttled"}})
	if sub != "default" || why != "" {
		t.Fatalf("single profile: always default, no rotation reason, got %s (%s)", sub, why)
	}
}
