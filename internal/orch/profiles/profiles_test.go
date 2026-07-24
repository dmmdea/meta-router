package profiles

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMissingIsImplicitDefault(t *testing.T) {
	reg, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, lane := range []string{"claude", "codex"} {
		ps := reg.Lane(lane)
		if len(ps) != 1 || ps[0].Subject != "default" || ps[0].Home != "" {
			t.Fatalf("%s must have the implicit default profile, got %+v", lane, ps)
		}
	}
}

func TestLoadValidatesAndMarksProvisioning(t *testing.T) {
	dir := t.TempDir()
	// Two EXPLICIT homes so the test never depends on the machine's real
	// ~/.claude login state (a headless CI runner has none — the assertion
	// must be self-contained): one credentialed, one bare.
	provHome := filepath.Join(dir, "claude-prov")
	bareHome := filepath.Join(dir, "claude-bare")
	if err := os.MkdirAll(bareHome, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, provHome, ".credentials.json", `{"claudeAiOauth":{"accessToken":"x"}}`)
	regPath := write(t, dir, "profiles.json", `{
		"claude": [
			{"subject": "acct1", "home": "`+filepath.ToSlash(provHome)+`"},
			{"subject": "acct2", "home": "`+filepath.ToSlash(bareHome)+`"}
		]
	}`)
	reg, err := Load(regPath)
	if err != nil {
		t.Fatal(err)
	}
	ps := reg.Lane("claude")
	if len(ps) != 2 {
		t.Fatalf("want 2 claude profiles, got %+v", ps)
	}
	if ps[0].Subject != "acct1" || !ps[0].Provisioned {
		t.Fatalf("credentialed home must be provisioned, got %+v", ps[0])
	}
	if ps[1].Subject != "acct2" || ps[1].Provisioned {
		t.Fatalf("credential-less home must be marked NOT provisioned, got %+v", ps[1])
	}
	// Provision the bare one → flips.
	write(t, bareHome, ".credentials.json", `{"claudeAiOauth":{"accessToken":"y"}}`)
	reg2, _ := Load(regPath)
	if !reg2.Lane("claude")[1].Provisioned {
		t.Fatal("credentialed home must be provisioned")
	}
	// codex was absent from the file → implicit default preserved (provisioning
	// of the default home depends on the machine, so only assert identity).
	if cs := reg.Lane("codex"); len(cs) != 1 || cs[0].Subject != "default" {
		t.Fatalf("absent lane must fall back to implicit default, got %+v", cs)
	}
}

func TestLoadRejectsDuplicateSubjects(t *testing.T) {
	dir := t.TempDir()
	regPath := write(t, dir, "profiles.json", `{"claude":[{"subject":"a","home":"`+filepath.ToSlash(dir)+`"},{"subject":"a","home":"`+filepath.ToSlash(dir)+`"}]}`)
	if _, err := Load(regPath); err == nil {
		t.Fatal("duplicate subjects must be a load error")
	}
}

func TestLoadRejectsMissingHomeDir(t *testing.T) {
	dir := t.TempDir()
	regPath := write(t, dir, "profiles.json", `{"claude":[{"subject":"x","home":"`+filepath.ToSlash(filepath.Join(dir, "gone"))+`"}]}`)
	if _, err := Load(regPath); err == nil {
		t.Fatal("nonexistent home dir must be a load error (typo protection)")
	}
}
