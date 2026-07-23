package canary

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepoRootFindsGoMod(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("RepoRoot %q has no go.mod: %v", root, err)
	}
}

func TestGoSourceFilesExcludesTestsAndTestdata(t *testing.T) {
	root, err := RepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := GoSourceFiles(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no source files found")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			t.Errorf("test file leaked into source scan: %s", f)
		}
		if strings.Contains(filepath.ToSlash(f), "/testdata/") {
			t.Errorf("testdata leaked into source scan: %s", f)
		}
	}
}

func TestScanForbiddenFindsViolation(t *testing.T) {
	// Uses the PRODUCTION pattern (B1Forbidden), not a copy — a divergent
	// test regex proves nothing about the canary (review finding, 2026-07-23).
	hits, err := ScanForbidden([]string{filepath.Join("testdata", "violation_apikey.txt")}, B1Forbidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "violation_apikey.txt:3") {
		t.Fatalf("want exactly 1 hit at line 3, got %v", hits)
	}
	lk, err := ScanForbidden([]string{filepath.Join("testdata", "violation_lookupenv.txt")}, B1Forbidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(lk) != 1 || !strings.Contains(lk[0], "violation_lookupenv.txt:2") {
		t.Fatalf("want exactly 1 LookupEnv/APIKEY hit at line 2, got %v", lk)
	}
	clean, err := ScanForbidden([]string{filepath.Join("testdata", "clean.txt")}, B1Forbidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(clean) != 0 {
		t.Fatalf("clean file produced hits: %v", clean)
	}
}
