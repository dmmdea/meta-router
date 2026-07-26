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
	// Conditional marks a spawn whose only scrub sits in a deeper block — an
	// if/else arm or a loop body — so it does not run on every path to the spawn.
	Conditional bool
}

// nonLaneCommands are argv[0] literals that cannot be a model lane, so spawning
// them cannot convert a subscription dispatch into metered spend.
//
// This is keyed on the SPAWN's own literal command, not on the containing file's
// contents. The previous B13 excused any FILE mentioning "taskkill" as a
// "process-control helper" — and claudelane, codexlane and locallane all kill a
// timed-out child, so the three most important lane spawn sites were exempt and
// deleting their scrub still passed (mutation-tested 2026-07-25).
// Deliberately SHORT. The first version also exempted shells and package managers
// (`powershell`, `bash`, `sh`, `cmd`, `npx`, `npm`, `go`, …), every one of which
// can start a model lane — `powershell -c claude -p …` is one exempt spawn away
// from an unscrubbed dispatch, which contradicts the invariant this list serves
// (review round 4). Only programs that cannot themselves become a lane stay, and
// each is here because a real unscrubbed spawn in this tree needs it: `git` for
// the replay's worktree plumbing, the kill family for timeout handling.
var nonLaneCommands = map[string]bool{
	"git":      true,
	"taskkill": true, "kill": true, "tasklist": true,
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
		execName := execAlias(file)
		total := countSpawns(file, execName)
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
			ss := spawnsInScope(fset, body, name, rel, scrubbers[filepath.Dir(rel)], execName)
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
func countSpawns(n ast.Node, execName string) int {
	c := 0
	ast.Inspect(n, func(m ast.Node) bool {
		switch x := m.(type) {
		case *ast.CallExpr:
			if isSpawn(x, execName) != "" {
				c++
			}
		case *ast.CompositeLit:
			if isCmdLit(x, execName) {
				c++
			}
		}
		return true
	})
	return c
}

// execAlias returns the local name a file binds "os/exec" to, or "" when the
// file does not import it. Hardcoding "exec" made `import ex "os/exec"` invisible
// to BOTH the resolver and the accounting tripwire meant to catch under-counting
// (review round 4) — an aliased import is legal Go and would have shipped silently.
func execAlias(file *ast.File) string {
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"os/exec"` {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "exec"
	}
	return ""
}

// isSpawn returns "Command", "CommandContext" or "" for a call expression.
func isSpawn(call *ast.CallExpr, execName string) string {
	if execName == "" {
		return ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != execName {
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
func spawnsInScope(fset *token.FileSet, body *ast.BlockStmt, fname, rel string, scrubbers map[string]bool, execName string) []Spawn {
	type event struct {
		pos     token.Pos
		kind    int // 0 = spawn, 1 = env assignment
		name    string
		cmd     string
		scrub   bool // env assignment: derives from Scrub
		extends bool // env assignment: reads <name>.Env, so it preserves
		hasVar  bool
		depth   int // block nesting relative to the function body
	}
	var events []event
	claimed := map[ast.Node]bool{}

	depth := 0
	var record func(n ast.Node) bool
	record = func(n ast.Node) bool {
		if _, ok := n.(*ast.BlockStmt); ok {
			depth++
			for _, c := range childNodes(n) {
				record(c)
			}
			depth--
			return false
		}
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
					extends: referencesEnvOf(x.Rhs[i], id.Name), depth: depth})
			}
			for i, rhs := range x.Rhs {
				call, ok := rhs.(*ast.CallExpr)
				if !ok || isSpawn(call, execName) == "" {
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
					cmd: commandLiteral(call, isSpawn(call, execName)), hasVar: name != "", depth: depth})
			}
		case *ast.ValueSpec:
			// `var cmd = exec.Command(...)` — a DeclStmt, not an AssignStmt. The
			// previous walk reported these as "spawns with no handle", which was a
			// loud but wrong diagnosis.
			for i, v := range x.Values {
				call, ok := v.(*ast.CallExpr)
				if !ok || isSpawn(call, execName) == "" {
					continue
				}
				claimed[call] = true
				name := ""
				if i < len(x.Names) {
					name = x.Names[i].Name
				}
				events = append(events, event{pos: call.Pos(), kind: 0, name: name,
					cmd: commandLiteral(call, isSpawn(call, execName)), hasVar: name != "", depth: depth})
			}
		}
		for _, c := range childNodes(n) {
			record(c)
		}
		return false
	}
	for _, st := range body.List {
		record(st)
	}

	// Unclaimed spawns keep no handle at all (chained .Run(), passed inline).
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || claimed[call] {
			return true
		}
		if kind := isSpawn(call, execName); kind != "" {
			events = append(events, event{pos: call.Pos(), kind: 0, cmd: commandLiteral(call, kind)})
		}
		return true
	})
	// Composite-literal construction: &exec.Cmd{Path: ..., Env: ...}. Invisible to
	// isSpawn, so an unscrubbed spawn written this way shipped with zero signal.
	ast.Inspect(body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isCmdLit(cl, execName) {
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
				// LAST non-extends assignment wins — deliberately no break here.
				// Breaking on the FIRST one regressed a capability round 2 had:
				// `cmd.Env = childenv.Scrub(...)` followed by `cmd.Env = os.Environ()`
				// scored as scrubbed, and every doc comment in this file said the
				// opposite of what the code did (review round 4). Pinned by
				// TestSpawnScrubOrdering.
				s.Scrubbed = later.scrub
				// A scrub that only happens on SOME paths is not a scrub. An
				// assignment nested deeper than the spawn's own block is
				// conditional (an if/else arm, a loop body), so it cannot be
				// credited unconditionally: `if len(req.Env) > 0 { cmd.Env = … }`
				// leaves cmd.Env nil — full ambient inheritance — on the default
				// path, and statement order alone cannot see that.
				if later.depth > e.depth {
					s.Scrubbed = false
					s.Conditional = true
				}
			}
		}
		out = append(out, s)
	}
	return out
}

// isCmdLit reports whether a composite literal constructs an exec.Cmd.
func isCmdLit(cl *ast.CompositeLit, execName string) bool {
	if execName == "" {
		return false
	}
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Cmd" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == execName
}

// containsScrub reports whether an expression's BASE value is the scrub result —
// either childenv.Scrub directly, or a same-package helper that returns it.
//
// The base, not "appears anywhere": the previous version walked the whole
// expression, so `cmd.Env = append(os.Environ(), childenv.Scrub(req.Env)...)`
// was credited even though the ambient environment is the base and the scrub only
// contributes the pins (review round 4). That is the same mention-vs-dataflow
// defect fixed one level up in the helper credit, and it has to hold here too.
//
// append(X, …) recurses into X, because appending to a scrubbed base keeps it
// scrubbed — that is the shape codexlane and mr-scorecard legitimately use.
func containsScrub(e ast.Expr, scrubbers map[string]bool) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	if id, ok := call.Fun.(*ast.Ident); ok {
		if id.Name == "append" {
			if len(call.Args) == 0 {
				return false
			}
			return containsScrub(call.Args[0], scrubbers)
		}
		return scrubbers[id.Name]
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "childenv" && sel.Sel.Name == "Scrub" {
			return true
		}
	}
	return false
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
	returns, scrubReturns := 0, 0
	ast.Inspect(body, func(n ast.Node) bool {
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
			if len(x.Results) == 0 {
				return true
			}
			r := x.Results[0]
			// A nil first result is an error path, not an environment.
			if id, ok := r.(*ast.Ident); ok && id.Name == "nil" {
				return true
			}
			returns++
			if directScrub(r) {
				scrubReturns++
				return false
			}
			if id, ok := r.(*ast.Ident); ok && derived[id.Name] {
				scrubReturns++
			}
		}
		return true
	})
	// EVERY environment-returning path must carry the scrub. Existential credit —
	// "some return is scrubbed" — let a helper with a second, unscrubbed return
	// launder every call site (review round 4).
	return returns > 0 && returns == scrubReturns
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

// childNodes returns a node's immediate children, so a walk can maintain its own
// block-nesting depth (ast.Inspect gives no depth signal and its callback cannot
// tell an if-body assignment from a top-level one).
func childNodes(n ast.Node) []ast.Node {
	var out []ast.Node
	first := true
	ast.Inspect(n, func(c ast.Node) bool {
		if first {
			first = false
			return true
		}
		if c != nil {
			out = append(out, c)
		}
		return false
	})
	return out
}
