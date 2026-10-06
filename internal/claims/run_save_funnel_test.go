// Package claims — this file makes M4-K1's funnel invariant ("every write
// of a run record goes through internal/run.Run.Persist") a tested
// property. run.Run.Save itself is unexported-equivalent outside
// internal/run in spirit, if not in Go's own visibility rules: calling it
// directly bypasses run.Run.RecordEvent's durable events.db append, the
// exact gap M4-K1 closed (~11 production call sites once did this; see
// run.Run.Persist's own doc comment).
//
// This is a parser-based heuristic, not a type-checked one:
// golang.org/x/tools/go/packages (which would give real type information)
// is not a dependency of this module, and adding one for a single guard
// test is a heavier cost than the imprecision below. See
// runRunTypedIdentifiers' own doc comment for exactly what it does and
// does not catch.
package claims

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// runSaveFunnelAllowlist names production call sites permitted to call
// run.Run.Save directly, as "<path relative to repo root>:<line>". Empty
// today: every known production bypass was converted to Persist by M4-K1.
// A future legitimate exception (there should rarely be one -- Persist
// exists precisely so callers never need to reach for Save directly)
// gets added here with a comment explaining why Persist doesn't fit,
// not by silently weakening the check below.
var runSaveFunnelAllowlist = map[string]bool{}

// runRunTypedIdentifiers scans one AST subtree (a top-level function's
// body, or a package-level func-literal's body) and returns the set of
// identifier names that are, somewhere in it, either:
//
//   - a function or function-literal parameter declared as *run.Run, or
//   - the target of a ":="/"=" assignment whose right-hand side is a call
//     to run.Load(...).
//
// Scoped to one top-level declaration, not the whole file: an earlier
// version of this check scanned the entire file at once and false-
// positived on internal/api/server.go, where the identifier "loaded" is
// *run.Run in one HTTP handler and *request.Request in a different one
// -- exactly the class of name reuse a real type checker would resolve
// for free. Per-top-level-declaration scoping fixes that case. It is
// still not fully block-scoped WITHIN one function: a name is flagged
// for the rest of that function (including sibling branches and later
// shadowing) once seen as *run.Run anywhere inside it, which
// over-approximates in the direction of catching more, not fewer,
// potential violations. That remaining imprecision is only unsound if a
// single function reuses one identifier name for both a *run.Run and an
// unrelated type with its own .Save method -- checked by hand for every
// function this test currently scans (none do). A file/function that
// introduces such a collision in the future would need a real
// type-checked replacement for this test, not a patch to the heuristic.
func runRunTypedIdentifiers(node ast.Node) map[string]bool {
	names := map[string]bool{}
	isRunRunStarExpr := func(expr ast.Expr) bool {
		star, ok := expr.(*ast.StarExpr)
		if !ok {
			return false
		}
		sel, ok := star.X.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		return ok && pkgIdent.Name == "run" && sel.Sel.Name == "Run"
	}
	collectParams := func(ft *ast.FuncType) {
		if ft == nil || ft.Params == nil {
			return
		}
		for _, field := range ft.Params.List {
			if !isRunRunStarExpr(field.Type) {
				continue
			}
			for _, n := range field.Names {
				names[n.Name] = true
			}
		}
	}
	isRunLoadCall := func(expr ast.Expr) bool {
		call, ok := expr.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkgIdent, ok := sel.X.(*ast.Ident)
		return ok && pkgIdent.Name == "run" && sel.Sel.Name == "Load"
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.FuncDecl:
			collectParams(node.Type)
		case *ast.FuncLit:
			collectParams(node.Type)
		case *ast.AssignStmt:
			if len(node.Rhs) == 1 && isRunLoadCall(node.Rhs[0]) {
				if id, ok := node.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
					names[id.Name] = true
				}
			}
		}
		return true
	})
	return names
}

// findRunSaveCalls returns every "<ident>.Save(...)" call site under node
// where ident's name is in runVars, as source line numbers.
func findRunSaveCalls(fset *token.FileSet, node ast.Node, runVars map[string]bool) []int {
	var lines []int
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Save" {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || !runVars[id.Name] {
			return true
		}
		lines = append(lines, fset.Position(sel.Pos()).Line)
		return true
	})
	return lines
}

// TestNoDirectRunSaveOutsideRunPackage enforces M4-K1's funnel: no
// production code outside internal/run may call run.Run.Save directly --
// every write of a run record goes through run.Run.Persist instead (Save
// plus the durable events.db RecordEvent append Persist adds). See this
// file's own doc comment for the heuristic's scope and limits, and
// runSaveFunnelAllowlist for the (currently empty) list of permitted
// exceptions.
func TestNoDirectRunSaveOutsideRunPackage(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	fset := token.NewFileSet()
	var violations []string

	for _, base := range []string{"cmd", "internal"} {
		walkRoot := filepath.Join(root, base)
		err := filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				name := d.Name()
				if path != walkRoot && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
					return filepath.SkipDir
				}
				// internal/run itself is the funnel's home: Persist,
				// RecordEvent, and every legitimate internal Save call
				// (Persist's own, plus WithLock-adjacent helpers) live
				// here and are exempt by design, not by omission.
				if filepath.Base(path) == "run" && filepath.Dir(path) == filepath.Join(root, "internal") {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || isTestFile(path) {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			// Scoped per top-level declaration (function, or a
			// package-level `var x = func(...) {...}`), not the whole
			// file -- see runRunTypedIdentifiers' own doc comment for
			// why: a name like "loaded" can legitimately be *run.Run in
			// one top-level function and *request.Request in another,
			// within the same file.
			for _, decl := range file.Decls {
				var scanNode ast.Node
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body == nil {
						continue
					}
					// The whole declaration, not just Body: a top-level
					// function's own *run.Run parameter (e.g.
					// flaggedConformityVerdicts(runRecord *run.Run) in
					// request_driver.go) lives in d.Type, not d.Body.
					scanNode = d
				case *ast.GenDecl:
					scanNode = d
				default:
					continue
				}
				runVars := runRunTypedIdentifiers(scanNode)
				if len(runVars) == 0 {
					continue
				}
				for _, line := range findRunSaveCalls(fset, scanNode, runVars) {
					loc := rel + ":" + strconv.Itoa(line)
					if runSaveFunnelAllowlist[loc] {
						continue
					}
					violations = append(violations, loc)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", walkRoot, err)
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found *run.Run.Save(...) called directly outside internal/run (must go through run.Run.Persist instead, or be added to runSaveFunnelAllowlist with a reason):\n%s", strings.Join(violations, "\n"))
	}
}
