package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
)

// The endpoint default is deliberately EMPTY: a hardcoded host default is what
// broke this on a second machine (the shared settings.json pinned one box's
// port on every box). Empty means "resolve for whatever machine this is" —
// retrievers.ResolveEndpoints handles env / machine-local file / failover chain.
// The production decay (2026-08-10): refresh reused roots.json verbatim while 7
// of its 13 roots no longer existed, dropping the index 155 -> 112 entries with
// no diagnostic anywhere. A vanished discovery-owned pack must force
// rediscovery; operator-owned roots must survive and merely warn.
func TestReuseRoots_DeadDiscoveryRootForcesRediscovery(t *testing.T) {
	claude := t.TempDir()

	rs := []catalog.Root{
		{Path: filepath.Join(claude, "plugins", "cache", "sp", "6.1.1", "skills"), Pack: "superpowers"},
	}
	reuse, notes, dead := reuseRoots(rs, claude)
	if reuse {
		t.Fatal("a vanished pack root must NOT be reused — that is the silent decay")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "superpowers") {
		t.Fatalf("the drop must be diagnosed by pack name, got %q", notes)
	}
	if len(dead) != 1 || dead[0] != "superpowers" {
		t.Fatalf("the dead pack must be named so rediscovery can be VERIFIED, got %q", dead)
	}
}

// packsMissing is what stops a failed rediscovery becoming permanent: if the
// pack did not come back, roots.json must be left alone so the next refresh
// retries, instead of Save erasing the only record the pack existed.
func TestPacksMissing_ReportsPacksRediscoveryDidNotReplace(t *testing.T) {
	got := packsMissing([]string{"superpowers", "gone-for-good"}, []catalog.Root{
		{Path: "x", Pack: "superpowers"},
		{Path: "y", Pack: "skills"},
	})
	if len(got) != 1 || got[0] != "gone-for-good" {
		t.Fatalf("only the pack with no replacement root is missing, got %q", got)
	}
	if n := packsMissing(nil, []catalog.Root{{Path: "x", Pack: "a"}}); n != nil {
		t.Fatalf("no dead packs means nothing missing, got %q", n)
	}
}

func TestReuseRoots_OperatorRootsWarnButSurvive(t *testing.T) {
	claude := t.TempDir()
	outside := t.TempDir()

	rs := []catalog.Root{
		{Path: filepath.Join(outside, "gone"), Pack: "handadded"},
		{Path: filepath.Join(claude, "commands"), Pack: "skills", Kind: catalog.KindCommands},
	}
	// Nothing discovery-owned died, so refresh keeps the operator's file.
	reuse, notes, _ := reuseRoots(rs, claude)
	if !reuse {
		t.Fatal("operator-owned dead roots must NOT trigger rediscovery — that overwrites hand edits")
	}
	out := strings.Join(notes, "\n")
	if !strings.Contains(out, "handadded") || !strings.Contains(out, "flat") {
		t.Fatalf("both operator-owned roots must be reported, got %q", out)
	}
}

func TestReuseRoots_AllLiveIsSilent(t *testing.T) {
	claude := t.TempDir()
	live := filepath.Join(claude, "skills")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	reuse, notes, _ := reuseRoots([]catalog.Root{{Path: live, Pack: "skills"}}, claude)
	if !reuse {
		t.Fatal("a healthy roots.json must be reused")
	}
	if len(notes) != 0 {
		t.Fatalf("healthy refresh must stay silent, got %q", notes)
	}
}

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := parseArgs([]string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.cmd != "build" || cfg.endpoint != "" || cfg.force {
		t.Fatalf("bad defaults: %+v", cfg)
	}
}

// An explicitly passed endpoint must still be honored verbatim.
func TestParseArgs_EndpointFlagIsHonored(t *testing.T) {
	cfg, err := parseArgs([]string{"refresh", "-endpoint", "http://10.0.0.1:9999", "-force"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.endpoint != "http://10.0.0.1:9999" || !cfg.force {
		t.Fatalf("flag not honored: %+v", cfg)
	}
}

func TestParseArgs_RejectsUnknownCmd(t *testing.T) {
	if _, err := parseArgs([]string{"frobnicate"}); err == nil {
		t.Fatal("expected error on unknown subcommand")
	}
}

func TestParseArgs_EmptyArgv(t *testing.T) {
	if _, err := parseArgs(nil); err == nil {
		t.Fatal("expected error on empty argv")
	}
}
