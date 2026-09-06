package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseExcludeNormalises(t *testing.T) {
	got, err := parseExclude([]string{" Claude", "codex", "claude", ""})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"claude", "codex"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// A typo must be an ERROR, not a silent no-op that leaves the lane selectable.
func TestParseExcludeRejectsUnknown(t *testing.T) {
	_, err := parseExclude([]string{"claud"})
	if err == nil || !strings.Contains(err.Error(), `unknown lane "claud"`) {
		t.Fatalf("want a typed error listing valid lanes, got %v", err)
	}
	// The valid set is rendered sorted; every lane (and the free group alias)
	// must be named so the operator can fix the typo without the source.
	for _, name := range []string{"claude", "codex", "copilot", "glm", "local", "groq", "cloudflare", "openrouter", "nim", "gemini", "free"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("valid-lane list must name %q: %v", name, err)
		}
	}
}

// --exclude claude --exclude codex  ==  --exclude claude,codex
func TestExcludeFlagRepeatableAndCSV(t *testing.T) {
	var a, b excludeFlag
	for _, v := range []string{"claude", "codex"} {
		if err := a.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Set("codex,claude"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(a), []string(b)) || a.String() != "claude,codex" {
		t.Fatalf("a=%v b=%v", a, b)
	}
	if err := b.Set("nope"); err == nil {
		t.Fatal("unknown lane accepted by Set")
	}
}
