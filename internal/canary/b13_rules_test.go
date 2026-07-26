package canary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

// These pin the B13 resolver's RULES against synthetic sources, so a regression is
// caught by a fast unit test instead of by the next review round.
//
// Every case below is a defect that actually shipped in some revision of this
// canary and was found by adversarial review, not invented:
//
//	round 2: one scrub in a FILE covered every spawn in it; a comment naming
//	         childenv.Scrub counted as a call.
//	round 3: the verdict was per variable NAME, so a rebind, an earlier spawn, or a
//	         sibling block shared one answer.
//	round 4: the scan broke on the FIRST assignment after a spawn, so an ambient
//	         overwrite was never seen — while every doc said "the LAST assignment
//	         decides"; a scrub inside an `if` was credited unconditionally; a scrub
//	         buried in an append ARGUMENT was credited as the base; a helper with
//	         one scrubbed and one unscrubbed return credited every call site; and an
//	         aliased `import ex "os/exec"` was invisible.
func TestSpawnResolverRules(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantScrub   bool
		wantCondNow bool
	}{
		{
			name: "plain scrub",
			src: `cmd := exec.Command("claude", "-p")
	cmd.Env = childenv.Scrub(os.Environ())`,
			wantScrub: true,
		},
		{
			name: "append onto a scrubbed base still counts",
			src: `cmd := exec.Command("claude", "-p")
	cmd.Env = append(childenv.Scrub(os.Environ()), "X=1")`,
			wantScrub: true,
		},
		{
			name: "extend an already-scrubbed env",
			src: `cmd := exec.Command("claude", "-p")
	cmd.Env = childenv.Scrub(os.Environ())
	cmd.Env = append(cmd.Env, "X=1")`,
			wantScrub: true,
		},
		{
			// Round 4's blocker: the LAST assignment decides.
			name: "ambient overwrite AFTER the scrub",
			src: `cmd := exec.Command("claude", "-p")
	cmd.Env = childenv.Scrub(os.Environ())
	cmd.Env = os.Environ()`,
			wantScrub: false,
		},
		{
			// A scrub the default path never executes is not a scrub.
			name: "scrub only inside an if",
			src: `cmd := exec.Command("claude", "-p")
	if len(pins) > 0 {
		cmd.Env = childenv.Scrub(os.Environ())
	}`,
			wantScrub: false, wantCondNow: true,
		},
		{
			// The scrub is an ARGUMENT; the base is the ambient environment.
			name: "scrub buried in an append argument",
			src: `cmd := exec.Command("claude", "-p")
	cmd.Env = append(os.Environ(), childenv.Scrub(pins)...)`,
			wantScrub: false,
		},
		{
			name:      "no env at all",
			src:       `cmd := exec.Command("claude", "-p")`,
			wantScrub: false,
		},
		{
			// Round 3: a rebind starts a fresh verdict.
			name: "rebind after the scrub",
			src: `cmd := exec.Command("claude", "-p")
	cmd.Env = childenv.Scrub(os.Environ())
	cmd = exec.Command("claude", "-p")`,
			wantScrub: false, // the LAST spawn is the one checked below
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ss := parseSpawns(t, "package p\n\nfunc f() {\n\t"+c.src+"\n}\n")
			if len(ss) == 0 {
				t.Fatal("no spawn resolved; the walker has gone blind")
			}
			got := ss[len(ss)-1] // the last spawn is the one each case is about
			if got.Scrubbed != c.wantScrub {
				t.Errorf("Scrubbed = %v, want %v (%+v)", got.Scrubbed, c.wantScrub, got)
			}
			if c.wantCondNow && !got.Conditional {
				t.Errorf("expected the spawn to be marked Conditional: %+v", got)
			}
		})
	}
}

// An aliased os/exec import is legal Go and must not hide a spawn.
func TestSpawnResolverSeesAliasedExecImport(t *testing.T) {
	ss := parseSpawns(t, `package p

import ex "os/exec"

func f() {
	cmd := ex.Command("claude", "-p")
	_ = cmd
}
`)
	if len(ss) != 1 {
		t.Fatalf("an aliased exec import must still resolve the spawn, got %d", len(ss))
	}
	if ss[0].Scrubbed {
		t.Fatalf("an unscrubbed aliased spawn must not read as scrubbed: %+v", ss[0])
	}
}

// Helper credit follows the RETURNED value, and every environment-returning path
// must carry it — otherwise a helper with one clean and one dirty return launders
// every call site that uses it.
func TestScrubberHelperCreditRequiresEveryReturn(t *testing.T) {
	both := `package p

func good(a []string) []string {
	env := childenv.Scrub(a)
	return env
}

func launder(a []string) []string {
	if len(a) == 0 {
		return os.Environ()
	}
	return childenv.Scrub(a)
}

func discard(a []string) []string {
	_ = childenv.Scrub(a)
	return os.Environ()
}
`
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "h.go"), []byte(both), 0o644); err != nil {
		t.Fatal(err)
	}
	m := scrubbersIn(t, dir)
	if !m["good"] {
		t.Error("a helper that returns the scrub must be credited")
	}
	if m["launder"] {
		t.Error("a helper with an UNSCRUBBED return path must not be credited")
	}
	if m["discard"] {
		t.Error("a helper that calls Scrub and discards it must not be credited")
	}
}

// parseSpawns resolves the spawns in a synthetic source file, exercising the same
// code path Spawns uses per file.
func parseSpawns(t *testing.T, src string) []Spawn {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	execName := execAlias(file)
	if execName == "" {
		execName = "exec" // synthetic sources usually omit the import block
	}
	var out []Spawn
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		out = append(out, spawnsInScope(fset, fn.Body, fn.Name.Name, "synthetic.go", nil, execName)...)
	}
	return out
}

// scrubbersIn returns the helper-credit map for a directory of synthetic sources.
func scrubbersIn(t *testing.T, dir string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	files, err := GoSourceFiles(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	m, err := scrubberFuncs(dir, files, fset)
	if err != nil {
		t.Fatal(err)
	}
	return m["."]
}
