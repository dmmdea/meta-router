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
	if err == nil || !strings.Contains(err.Error(), `unknown lane "claud"`) || !strings.Contains(err.Error(), "claude|codex|copilot|glm|local") {
		t.Fatalf("want a typed error listing valid lanes, got %v", err)
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
