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

// StripGoComments is load-bearing for B13/B14: without it a comment naming a
// call satisfied a canary whose call had been deleted. These pin both
// directions — prose removed, code and string literals kept.
func TestStripGoComments(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string // must survive
		gone     []string // must not
	}{
		{"line comment naming a call", "// egress.Plan(x) is called below\nfoo()\n",
			[]string{"foo()"}, []string{"egress.Plan("}},
		{"trailing comment", "bar() // childenv.Scrub(env)\n",
			[]string{"bar()"}, []string{"childenv.Scrub("}},
		{"block comment", "/* egress.Plan(\nstill comment */ baz()\n",
			[]string{"baz()"}, []string{"egress.Plan("}},
		{"url in a string is not a comment", "u := \"http://x/y\"\nkeep()\n",
			[]string{"http://x/y", "keep()"}, nil},
		{"call in a raw string is kept (loud, not silent)", "s := `egress.Plan(`\n",
			[]string{"egress.Plan("}, nil},
		{"escaped quote does not end the string", "s := \"a\\\"// b\"\nkeep()\n",
			[]string{"keep()"}, nil},
		{"rune literal with a quote", "c := '\\''\nkeep()\n",
			[]string{"keep()"}, nil},
		{"crlf normalized", "func f() {\r\n}\r\n",
			[]string{"func f() {\n}\n"}, []string{"\r"}},
	}
	for _, c := range cases {
		got := StripGoComments(c.in)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: %q must survive stripping, got %q", c.name, w, got)
			}
		}
		for _, g := range c.gone {
			if strings.Contains(got, g) {
				t.Errorf("%s: %q must be stripped, got %q", c.name, g, got)
			}
		}
	}
	// Line structure is preserved so body-boundary regexes still work.
	if got := StripGoComments("a()\n// x\nb()\n"); got != "a()\n\nb()\n" {
		t.Errorf("comment lines must collapse to empty lines, not vanish: %q", got)
	}
}
