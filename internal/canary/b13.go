package canary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
)

// Spawn is one exec.Command / exec.CommandContext site, resolved structurally.
type Spawn struct {
	File string
	Line int
	// Command is argv[0] when it is a string literal, "" when it is a variable
	// (a variable command name could be a model lane, so it is never exempt).
	Command string
	// Var is the identifier the *exec.Cmd was assigned to, "" for a chained
	// call like exec.Command(...).Run() that keeps no handle.
	Var string
	// Scrubbed is whether the LAST assignment to <Var>.Env in the enclosing
	// function passes through childenv.Scrub. Order matters: an assignment that
	// overwrites a scrubbed env with the ambient one must count as unscrubbed.
	Scrubbed bool
	// Func names the enclosing function, for a failure message that can be found.
	Func string
}

// nonLaneCommands are argv[0] literals that cannot be a model lane, so spawning
// them cannot convert a subscription dispatch into metered spend.
//
// This is keyed on the SPAWN's own literal command, not on the containing file's
// contents. The previous B13 excused any FILE mentioning "taskkill" as a
// "process-control helper" — and claudelane, codexlane and locallane all kill a
// timed-out child, so the three most important lane spawn sites were exempt and
// deleting their scrub still passed (mutation-tested 2026-07-25).
var nonLaneCommands = map[string]bool{
	"git": true, "go": true, "gofmt": true,
	"taskkill": true, "kill": true, "tasklist": true,
	"npm": true, "npx": true, "pnpm": true, "yarn": true,
	"cmd": true, "powershell": true, "pwsh": true, "sh": true, "bash": true,
}

// Spawns parses every non-test Go file under root and returns each process-spawn
// site with the facts B13 needs. Structural, not textual: a comment naming
// childenv.Scrub proves nothing, one scrub does not cover a second spawn in the
// same file, and a later cmd.Env assignment can undo an earlier one — all three
// of which a grep-based check got wrong.
func Spawns(root string) ([]Spawn, error) {
	files, err := GoSourceFiles(root, false)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	scrubbers, err := scrubberFuncs(root, files, fset)
	if err != nil {
		return nil, err
	}
	var out []Spawn
	for _, f := range files {
		rel := relTo(root, f)
		if strings.Contains(rel, "internal/canary/") {
			continue // this package's own fixtures and helpers
		}
		file, perr := parser.ParseFile(fset, f, nil, 0)
		if perr != nil {
			return nil, fmt.Errorf("%s: %w", rel, perr)
		}
		accounted := 0
		total := countSpawns(file)
		// Every OUTERMOST function body is a scope. Declared functions and
		// package-level func literals both count — `var spawnSupervisor = func(...)`
		// holds a real lane spawn, and walking only ast.FuncDecl missed it (caught
		// by this function's own accounting tripwire, which is why it exists).
		// Returning false stops the descent, so a nested closure's spawns are
		// attributed to the enclosing scope, where its cmd variable lives.
		ast.Inspect(file, func(n ast.Node) bool {
			var body *ast.BlockStmt
			name := ""
			switch x := n.(type) {
			case *ast.FuncDecl:
				body, name = x.Body, x.Name.Name
			case *ast.FuncLit:
				body = x.Body
				name = fmt.Sprintf("func literal at line %d", fset.Position(x.Pos()).Line)
			default:
				return true
			}
			if body == nil {
				return false
			}
			ss := spawnsInScope(fset, body, name, rel, scrubbers[filepath.Dir(rel)])
			accounted += len(ss)
			out = append(out, ss...)
			return false
		})
		if accounted != total {
			// A spawn outside any function body (a package-level var initializer)
			// would otherwise be invisible to this canary. Fail loudly rather
			// than silently under-count — an under-counting security canary is
			// the exact failure mode being fixed here.
			return nil, fmt.Errorf("%s: %d spawn site(s) found but %d attributed to a function; a spawn outside a function body is not covered by B13", rel, total, accounted)
		}
	}
	return out, nil
}

func relTo(root, f string) string {
	return strings.TrimPrefix(filepath.ToSlash(f), filepath.ToSlash(root)+"/")
}

func countSpawns(n ast.Node) int {
	c := 0
	ast.Inspect(n, func(m ast.Node) bool {
		if call, ok := m.(*ast.CallExpr); ok && isSpawn(call) != "" {
			c++
		}
		return true
	})
	return c
}

// isSpawn returns "Command", "CommandContext" or "" for a call expression.
func isSpawn(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "exec" {
		return ""
	}
	if sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext" {
		return sel.Sel.Name
	}
	return ""
}

// commandLiteral returns argv[0] when it is a plain string literal.
// CommandContext takes the context first, so the command is one arg later.
func commandLiteral(call *ast.CallExpr, kind string) string {
	i := 0
	if kind == "CommandContext" {
		i = 1
	}
	if len(call.Args) <= i {
		return ""
	}
	lit, ok := call.Args[i].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return ""
	}
	s := strings.Trim(lit.Value, "`\"")
	// A path-qualified command still identifies the program.
	return strings.TrimSuffix(filepath.Base(filepath.ToSlash(s)), ".exe")
}

// spawnsInScope resolves every spawn inside one function body, together with
// whether the *exec.Cmd it produced ends up with a scrubbed environment.
func spawnsInScope(fset *token.FileSet, body *ast.BlockStmt, fname, rel string, scrubbers map[string]bool) []Spawn {
	// Pass 1, in source order: the LAST assignment to each X.Env decides.
	scrubbed := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Env" {
				continue
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok || i >= len(as.Rhs) {
				continue
			}
			// `cmd.Env = append(cmd.Env, ...)` EXTENDS the existing environment,
			// so whatever it already was it still is. Only a wholesale
			// replacement can undo a scrub, and that is what must be re-proven.
			if referencesEnvOf(as.Rhs[i], id.Name) {
				continue
			}
			scrubbed[id.Name] = containsScrub(as.Rhs[i], scrubbers)
		}
		return true
	})

	// Pass 2: spawns assigned to a variable.
	claimed := map[ast.Node]bool{}
	var out []Spawn
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, rhs := range as.Rhs {
			call, ok := rhs.(*ast.CallExpr)
			if !ok {
				continue
			}
			kind := isSpawn(call)
			if kind == "" {
				continue
			}
			claimed[call] = true
			name := ""
			if i < len(as.Lhs) {
				if id, ok := as.Lhs[i].(*ast.Ident); ok {
					name = id.Name
				}
			}
			out = append(out, Spawn{File: rel, Line: fset.Position(call.Pos()).Line,
				Command: commandLiteral(call, kind), Var: name,
				Scrubbed: name != "" && scrubbed[name], Func: fname})
		}
		return true
	})

	// Pass 3: spawns that keep no handle (chained .Run(), passed inline).
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || claimed[call] {
			return true
		}
		kind := isSpawn(call)
		if kind == "" {
			return true
		}
		out = append(out, Spawn{File: rel, Line: fset.Position(call.Pos()).Line,
			Command: commandLiteral(call, kind), Func: fname})
		return true
	})
	return out
}

// containsScrub reports whether an expression passes through childenv.Scrub —
// either directly, or via a same-package helper that itself calls it.
//
// The indirection is admitted STRUCTURALLY, not by name: scrubbers is built by
// scanning the package for functions whose own body calls childenv.Scrub. The
// claude lane legitimately wraps it (`childEnv(os.Environ(), req.Env)`) so that
// the lane's own deliberate pins are appended AFTER the scrub, and demanding a
// literal call at every site would force that abstraction to be inlined.
func containsScrub(e ast.Expr, scrubbers map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "childenv" && x.Sel.Name == "Scrub" {
				found = true
				return false
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && scrubbers[id.Name] {
				found = true
				return false
			}
		}
		return true
	})
	return found
}

// referencesEnvOf reports whether an expression reads <name>.Env, i.e. whether
// the assignment extends the existing environment rather than replacing it.
func referencesEnvOf(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Env" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// scrubberFuncs maps each package directory to the set of function names in it
// whose body calls childenv.Scrub directly. One level of indirection only: a
// helper that calls a helper that scrubs is not credited, because each extra
// hop is another place the guarantee can silently disappear.
func scrubberFuncs(root string, files []string, fset *token.FileSet) (map[string]map[string]bool, error) {
	out := map[string]map[string]bool{}
	for _, f := range files {
		rel := relTo(root, f)
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		dir := filepath.Dir(rel)
		if out[dir] == nil {
			out[dir] = map[string]bool{}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if bodyCallsScrub(fn.Body) {
				out[dir][fn.Name.Name] = true
			}
		}
	}
	return out, nil
}

func bodyCallsScrub(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "childenv" && sel.Sel.Name == "Scrub" {
			found = true
			return false
		}
		return true
	})
	return found
}

// Exempt reports whether a spawn needs no scrub, and why. Only a LITERAL
// non-lane command is exempt: a variable command name could resolve to a model
// binary, so it is gated by default.
func (s Spawn) Exempt() (bool, string) {
	if s.Command == "" {
		return false, "argv[0] is not a literal, so it could be a model lane"
	}
	if nonLaneCommands[s.Command] {
		return true, s.Command + " is not a model lane"
	}
	return false, s.Command + " is not on the non-lane list"
}
