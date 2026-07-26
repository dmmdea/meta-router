package canary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
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

// countSpawns is the independent tally the accounting tripwire compares against,
// so it must recognize exactly the same construction forms spawnsInScope does —
// otherwise a form one sees and the other does not produces a mismatch reported as
// "outside a function body", which is a true failure with a false diagnosis.
func countSpawns(n ast.Node) int {
	c := 0
	ast.Inspect(n, func(m ast.Node) bool {
		switch x := m.(type) {
		case *ast.CallExpr:
			if isSpawn(x) != "" {
				c++
			}
		case *ast.CompositeLit:
			if isCmdLit(x) {
				c++
			}
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

// spawnsInScope resolves every spawn inside one function body.
//
// ORDERED, per SITE — not per variable name. The previous version kept one
// verdict per identifier and stamped it onto every spawn bound to that name, so
// four ordinary refactors defeated it while the whole suite stayed green
// (review round 3): reassigning the variable after the scrub; an unscrubbed spawn
// BEFORE the scrub being back-credited by it; a lexically distinct `cmd` in a
// sibling block; and a retry spawn on the production claude dispatch path. A
// `claude` child then ran with Env == nil — full ambient inheritance — with B13
// green.
//
// The rule now: a spawn is scrubbed iff, AFTER its own statement and BEFORE the
// next spawn bound to the same name, there is an assignment to <name>.Env that
// derives from childenv.Scrub. An assignment that reads <name>.Env (append) keeps
// whatever the previous verdict was, because extending an environment cannot
// unscrub it.
func spawnsInScope(fset *token.FileSet, body *ast.BlockStmt, fname, rel string, scrubbers map[string]bool) []Spawn {
	type event struct {
		pos     token.Pos
		kind    int // 0 = spawn, 1 = env assignment
		name    string
		cmd     string
		scrub   bool // env assignment: derives from Scrub
		extends bool // env assignment: reads <name>.Env, so it preserves
		hasVar  bool
	}
	var events []event
	claimed := map[ast.Node]bool{}

	record := func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Env" || i >= len(x.Rhs) {
					continue
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				events = append(events, event{pos: x.Pos(), kind: 1, name: id.Name,
					scrub:   containsScrub(x.Rhs[i], scrubbers),
					extends: referencesEnvOf(x.Rhs[i], id.Name)})
			}
			for i, rhs := range x.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || isSpawn(call) == "" {
					continue
				}
				claimed[call] = true
				name := ""
				if i < len(x.Lhs) {
					if id, ok := x.Lhs[i].(*ast.Ident); ok {
						name = id.Name
					}
				}
				events = append(events, event{pos: call.Pos(), kind: 0, name: name,
					cmd: commandLiteral(call, isSpawn(call)), hasVar: name != ""})
			}
		case *ast.ValueSpec:
			// `var cmd = exec.Command(...)` — a DeclStmt, not an AssignStmt. The
			// previous walk reported these as "spawns with no handle", which was a
			// loud but wrong diagnosis.
			for i, v := range x.Values {
				call, ok := v.(*ast.CallExpr)
				if !ok || isSpawn(call) == "" {
					continue
				}
				claimed[call] = true
				name := ""
				if i < len(x.Names) {
					name = x.Names[i].Name
				}
				events = append(events, event{pos: call.Pos(), kind: 0, name: name,
					cmd: commandLiteral(call, isSpawn(call)), hasVar: name != ""})
			}
		}
		return true
	}
	ast.Inspect(body, record)

	// Unclaimed spawns keep no handle at all (chained .Run(), passed inline).
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || claimed[call] {
			return true
		}
		if kind := isSpawn(call); kind != "" {
			events = append(events, event{pos: call.Pos(), kind: 0, cmd: commandLiteral(call, kind)})
		}
		return true
	})
	// Composite-literal construction: &exec.Cmd{Path: ..., Env: ...}. Invisible to
	// isSpawn, so an unscrubbed spawn written this way shipped with zero signal.
	ast.Inspect(body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isCmdLit(cl) {
			return true
		}
		scrubbed := false
		for _, el := range cl.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Env" && containsScrub(kv.Value, scrubbers) {
				scrubbed = true
			}
		}
		events = append(events, event{pos: cl.Pos(), kind: 0, name: "<exec.Cmd literal>",
			hasVar: true, scrub: scrubbed})
		return true
	})

	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	var out []Spawn
	for i, e := range events {
		if e.kind != 0 {
			continue
		}
		s := Spawn{File: rel, Line: fset.Position(e.pos).Line, Command: e.cmd,
			Var: e.name, Func: fname}
		if e.name == "<exec.Cmd literal>" {
			// A composite literal carries its own Env field; there is no later
			// assignment to look for.
			s.Scrubbed = e.scrub
			out = append(out, s)
			continue
		}
		if e.hasVar {
			for _, later := range events[i+1:] {
				if later.name != e.name {
					continue
				}
				if later.kind == 0 {
					break // the variable is rebound; anything after belongs to that spawn
				}
				if later.extends {
					continue // append preserves whatever it already was
				}
				s.Scrubbed = later.scrub
				break
			}
		}
		out = append(out, s)
	}
	return out
}

// isCmdLit reports whether a composite literal constructs an exec.Cmd.
func isCmdLit(cl *ast.CompositeLit) bool {
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Cmd" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "exec"
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
			if bodyReturnsScrub(fn.Body) {
				out[dir][fn.Name.Name] = true
			}
		}
	}
	return out, nil
}

// bodyReturnsScrub reports whether a function RETURNS a value derived from
// childenv.Scrub.
//
// Returning, not merely mentioning: the first version credited any helper whose
// body contained the call anywhere, so a helper that scrubbed a throwaway value
// and returned os.Environ() would have laundered every call site that used it
// (review round 3). The credit has to follow the value that actually reaches
// cmd.Env, and the return expression is the closest structural proxy available
// without full type-checked dataflow.
func bodyReturnsScrub(body *ast.BlockStmt) bool {
	// Minimal local dataflow: which locals hold a Scrub-derived value. The real
	// helper is `env := childenv.Scrub(ambient); env = append(env, pins...);
	// return env`, so a check that demanded the literal call IN the return
	// expression would reject it — while a check that accepted a mere mention
	// anywhere would accept `_ = childenv.Scrub(x); return os.Environ()`.
	// Tracking assignment-to-local is the smallest rule that separates those two.
	derived := map[string]bool{}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		if found {
			return false
		}
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				id, ok := lhs.(*ast.Ident)
				if !ok || i >= len(x.Rhs) {
					continue
				}
				if directScrub(x.Rhs[i]) {
					derived[id.Name] = true
					continue
				}
				// `env = append(env, …)` keeps whatever env already was.
				if referencesIdent(x.Rhs[i], id.Name) && derived[id.Name] {
					continue
				}
				derived[id.Name] = false
			}
		case *ast.ReturnStmt:
			for _, r := range x.Results {
				if directScrub(r) {
					found = true
					return false
				}
				if id, ok := r.(*ast.Ident); ok && derived[id.Name] {
					found = true
					return false
				}
			}
		}
		return true
	})
	return found
}

// referencesIdent reports whether an expression reads the named identifier.
func referencesIdent(e ast.Expr, name string) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// directScrub reports whether an expression contains a literal childenv.Scrub
// call. No indirection is followed: this is the base case the one admitted level
// of helper indirection is measured against.
func directScrub(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
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
