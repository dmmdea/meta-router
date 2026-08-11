package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dmmdea/meta-router/internal/catalog"
	"github.com/dmmdea/meta-router/internal/roots"
)

// Review 2026-07-30, MAJOR: `mr-index build` always rediscovered and SAVED
// roots.json, and Discover can never emit a kind — so the next build after an
// operator hand-enabled the flat roots destroyed the enablement AND rebuilt
// the index without its 46 entries in one stroke. These pin the repairs.

func writeRootsFile(t *testing.T, dir string, content string) string {
	t.Helper()
	p := filepath.Join(dir, "roots.json")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// hermeticHome points discovery at a throwaway home with one stub skills root,
// so these tests neither depend on nor leak the real machine's ~/.claude
// (closure audit 2026-07-30: the first version failed on any box without
// installed skills and baked real paths into its fixture).
func hermeticHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	stub := filepath.Join(home, ".claude", "skills", "stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stub, "SKILL.md"), []byte("---\nname: stub\ndescription: stub.\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	return home
}

// End-to-end reproduction of the production decay (2026-08-10): roots.json
// names a VERSIONED plugin cache path, the plugin updates, the path vanishes,
// and refresh — which reads roots.json and never re-discovers — keeps handing
// back the dead root. Index went 155 -> 112 entries with the whole superpowers
// pack gone, and index.Refresh's removal guard never fired because each decay
// step was under its 30% threshold.
func TestRefreshRediscoversWhenADiscoveryRootHasVanished(t *testing.T) {
	home := hermeticHome(t)
	dir := t.TempDir()
	dead := filepath.ToSlash(filepath.Join(home, ".claude", "plugins", "cache", "sp", "6.1.1", "skills"))
	writeRootsFile(t, dir, `{"version":1,"roots":[
		{"path":"`+dead+`","pack":"superpowers"}]}`)
	out := filepath.Join(dir, "index.json")

	rs, notes, err := resolveRoots(config{cmd: "refresh"}, out)
	if err != nil {
		t.Fatalf("resolveRoots(refresh): %v", err)
	}
	for _, r := range rs {
		if filepath.ToSlash(r.Path) == dead {
			t.Fatal("refresh handed back the vanished root — this is the silent decay")
		}
	}
	// The stub skills dir from hermeticHome must have been rediscovered, so the
	// refresh indexes something real instead of nothing.
	if len(rs) == 0 {
		t.Fatal("rediscovery produced no roots")
	}
	if len(notes) == 0 {
		t.Fatal("the vanished root must be reported, not silently repaired")
	}
}

// The counterpart: an operator's out-of-tree root that has vanished must NOT
// trigger rediscovery, because rediscovery calls roots.Save and would erase the
// human's entry.
func TestRefreshKeepsVanishedOperatorRootInsteadOfOverwriting(t *testing.T) {
	hermeticHome(t)
	dir := t.TempDir()
	gone := filepath.ToSlash(filepath.Join(t.TempDir(), "handadded-skills"))
	writeRootsFile(t, dir, `{"version":1,"roots":[
		{"path":"`+gone+`","pack":"handadded"}]}`)
	out := filepath.Join(dir, "index.json")

	rs, notes, err := resolveRoots(config{cmd: "refresh"}, out)
	if err != nil {
		t.Fatalf("resolveRoots(refresh): %v", err)
	}
	if len(rs) != 1 || filepath.ToSlash(rs[0].Path) != gone {
		t.Fatalf("operator's out-of-tree root must survive verbatim, got %+v", rs)
	}
	if len(notes) != 1 {
		t.Fatalf("it must still be reported, got %q", notes)
	}
}

func TestBuildPreservesOperatorFlatRoots(t *testing.T) {
	hermeticHome(t)
	dir := t.TempDir()
	flat := filepath.ToSlash(filepath.Join(dir, "commands"))
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRootsFile(t, dir, `{"version":1,"roots":[
		{"path":"`+flat+`","pack":"commands","kind":"commands"}]}`)
	out := filepath.Join(dir, "index.json")

	rs, _, err := resolveRoots(config{cmd: "build"}, out)
	if err != nil {
		t.Fatalf("resolveRoots(build): %v", err)
	}
	found := false
	for _, r := range rs {
		if r.Kind == catalog.KindCommands {
			found = true
		}
	}
	if !found {
		t.Fatalf("build dropped the operator's flat root: %+v", rs)
	}
	saved, err := roots.Load(roots.ConfigPathFor(out))
	if err != nil {
		t.Fatalf("re-load persisted roots: %v", err)
	}
	found = false
	for _, r := range saved {
		if r.Kind == catalog.KindCommands {
			found = true
		}
	}
	if !found {
		t.Fatal("build PERSISTED roots.json without the operator's flat root — the destroy path is back")
	}
}

// Ownership rule: discovery owns the ~/.claude space (an uninstalled pack's
// root SHOULD drop on rebuild), the operator owns everything else — a
// hand-added skills-class root OUTSIDE claudeDir survives a build, one INSIDE
// that discovery no longer finds does not (closure audit 2026-07-30, MINOR:
// the flat-only carry-over left valid outside-tree hand-edits on the old
// destroy path while an invalid edit was sacred).
func TestBuildOwnershipRuleForHandAddedSkillsRoots(t *testing.T) {
	home := hermeticHome(t)
	dir := t.TempDir()
	outside := filepath.ToSlash(filepath.Join(t.TempDir(), "myrepo-skills"))
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	insideGone := filepath.ToSlash(filepath.Join(home, ".claude", "plugins", "uninstalled-pack", "skills"))
	writeRootsFile(t, dir, `{"version":1,"roots":[
		{"path":"`+outside+`","pack":"custompack"},
		{"path":"`+insideGone+`","pack":"uninstalled-pack"}]}`)
	out := filepath.Join(dir, "index.json")

	rs, _, err := resolveRoots(config{cmd: "build"}, out)
	if err != nil {
		t.Fatalf("resolveRoots(build): %v", err)
	}
	var keptOutside, keptInsideGone bool
	for _, r := range rs {
		if filepath.Clean(r.Path) == filepath.Clean(outside) {
			keptOutside = true
		}
		if filepath.Clean(r.Path) == filepath.Clean(insideGone) {
			keptInsideGone = true
		}
	}
	if !keptOutside {
		t.Fatal("a hand-added skills root OUTSIDE ~/.claude must survive a build")
	}
	if keptInsideGone {
		t.Fatal("an under-~/.claude root discovery no longer finds must DROP (uninstalled-pack cleanup)")
	}
}

// An invalid roots.json must be fatal, and the file must be LEFT ALONE:
// "warn and rediscover" ended in roots.Save overwriting the operator's edits —
// a typo'd kind was repaid with the destruction of the enablement it carried.
func TestInvalidRootsFileIsFatalAndUntouched(t *testing.T) {
	for _, cmd := range []string{"build", "refresh"} {
		t.Run(cmd, func(t *testing.T) {
			dir := t.TempDir()
			bad := `{"version":1,"roots":[{"path":"C:/x","pack":"commands","kind":"command"}]}`
			p := writeRootsFile(t, dir, bad)
			out := filepath.Join(dir, "index.json")

			_, _, err := resolveRoots(config{cmd: cmd}, out)
			if err == nil {
				t.Fatal("unknown kind must be fatal, not silently rediscovered")
			}
			if !strings.Contains(err.Error(), `unknown kind "command"`) {
				t.Fatalf("error must name the bad kind: %v", err)
			}
			after, rerr := os.ReadFile(p)
			if rerr != nil || string(after) != bad {
				t.Fatal("the invalid roots.json must be left byte-identical for the operator to fix")
			}
		})
	}
}
