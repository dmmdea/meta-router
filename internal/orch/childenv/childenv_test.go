package childenv

import (
	"strings"
	"testing"
)

// The motivating case: an ambient ANTHROPIC_API_KEY is honoured unconditionally
// by headless Claude Code, ahead of OAuth and bypassing the approval gate, so a
// "subscription" dispatch would silently bill (R10) with receipts still showing
// cash_usd 0.
func TestScrubsCredentialRedirects(t *testing.T) {
	env := []string{
		"PATH=C:/Windows",
		"ANTHROPIC_API_KEY=sk-ant-leaked",
		"ANTHROPIC_AUTH_TOKEN=tok",
		"ANTHROPIC_BASE_URL=https://elsewhere",
		"OPENAI_API_KEY=sk-openai",
		"USERPROFILE=C:/Users/x",
	}
	got := strings.Join(Scrub(env), "\n")
	for _, banned := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "OPENAI_API_KEY"} {
		if strings.Contains(got, banned) {
			t.Fatalf("%s must be scrubbed: %s", banned, got)
		}
	}
	for _, keep := range []string{"PATH=", "USERPROFILE="} {
		if !strings.Contains(got, keep) {
			t.Fatalf("%s is load-bearing and must survive: %s", keep, got)
		}
	}
}

// CLAUDE_CODE_SIMPLE is equivalent to the --bare flag the arg builder bans;
// banning the flag while leaving the env open is a padlock on an open gate.
func TestScrubsBareEquivalentAndCloudRouting(t *testing.T) {
	env := []string{"CLAUDE_CODE_SIMPLE=1", "CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "KEEP=1"}
	got := Scrub(env)
	if len(got) != 1 || got[0] != "KEEP=1" {
		t.Fatalf("bare-equivalent and cloud-routing vars must be scrubbed, got %v", got)
	}
}

// Model pins are per-dispatch decisions; an ambient default would override them.
func TestScrubsModelDefaultFamilyByPrefix(t *testing.T) {
	for _, n := range []string{"ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL"} {
		if !Denied(n) {
			t.Fatalf("%s must be denied by prefix", n)
		}
	}
	if Denied("ANTHROPIC_SOMETHING_ELSE") {
		t.Fatal("the prefix rule must not over-match")
	}
}

func TestCaseInsensitiveOnWindowsNames(t *testing.T) {
	if !Denied("anthropic_api_key") {
		t.Fatal("Windows env names are case-insensitive; the deny-list must be too")
	}
}

// Windows carries entries like "=C:" (drive-relative cwd) with a leading '='.
// Mangling them would break child processes' relative-path resolution.
func TestPassesThroughOddWindowsEntries(t *testing.T) {
	env := []string{`=C:=C:\Users`, "NOEQUALS", "PATH=x"}
	got := Scrub(env)
	if len(got) != 3 {
		t.Fatalf("odd entries must pass through untouched, got %v", got)
	}
}

// A lane's DELIBERATE pin must survive: the GLM lane sets ANTHROPIC_BASE_URL and
// ANTHROPIC_AUTH_TOKEN on purpose. Scrub runs on the ambient env only, before a
// lane appends its own.
func TestDeliberatePinsAppendedAfterScrubSurvive(t *testing.T) {
	ambient := []string{"ANTHROPIC_BASE_URL=https://leak", "PATH=x"}
	final := append(Scrub(ambient), "ANTHROPIC_BASE_URL=https://api.z.ai/api/anthropic")
	joined := strings.Join(final, "\n")
	if strings.Contains(joined, "https://leak") {
		t.Fatalf("the ambient value must be gone: %s", joined)
	}
	if !strings.Contains(joined, "api.z.ai") {
		t.Fatalf("the lane's deliberate pin must survive: %s", joined)
	}
}

func TestRemovedReportsWhatWasDropped(t *testing.T) {
	hits := Removed([]string{"ANTHROPIC_API_KEY=x", "PATH=y"})
	if len(hits) != 1 || hits[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("Removed must name what was scrubbed, got %v", hits)
	}
}
