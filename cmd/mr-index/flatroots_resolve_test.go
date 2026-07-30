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

func TestBuildPreservesOperatorFlatRoots(t *testing.T) {
	dir := t.TempDir()
	flat := filepath.ToSlash(filepath.Join(dir, "commands"))
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRootsFile(t, dir, `{"version":1,"roots":[
		{"path":"`+flat+`","pack":"commands","kind":"commands"}]}`)
	out := filepath.Join(dir, "index.json")

	rs, err := resolveRoots(config{cmd: "build"}, out)
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

			_, err := resolveRoots(config{cmd: cmd}, out)
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
