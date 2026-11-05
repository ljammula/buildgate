// Package claims — this file makes "a dependency is passed in, never reached
// through a package variable" a tested property of `make verify`: no function
// body assigns a package-level variable, its own package's or another
// buildgate package's. A test that swaps a package variable for a fake, or
// production code that keeps per-run state in one, shares that state with
// every other test and run in the process; a parameter or a struct field does
// not. It parses the source directly (go/parser), as complexity_test.go does.
package claims

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// packageGlobalsBaselineFile lists the assignments to package-level
// variables from inside a function that remained when the rule became a
// test, one "<count> <file> <function> <variable>" per line: mostly the
// timeouts and limits a test shrinks. A listed line may shrink or go, never
// grow; an assignment not listed fails the test.
const packageGlobalsBaselineFile = "testdata/package_globals_baseline.txt"

// updatePackageGlobalsBaselineEnv, set to 1, rewrites the baseline from the
// current source. Its diff must show only removed lines, lower counts, or a
// moved or renamed function: a new line or a higher count is a dependency to
// pass in instead.
const updatePackageGlobalsBaselineEnv = "CLAIMS_UPDATE_PACKAGE_GLOBALS_BASELINE"

// globalsFile is one parsed tracked file and the package it belongs to.
type globalsFile struct {
	path    string // relative to the repository root
	pkg     string // "<dir> <package name>": an external test package is its own
	src     *ast.File
	topVars map[*ast.ValueSpec]bool
}

// packageVars maps "<dir> <package name>" to its package-level var names.
type packageVars map[string]map[string]bool

// collectGlobalsFiles parses every tracked, non-generated .go file with
// object resolution, so an identifier declared in the same file resolves to
// its declaration and a local shadowing one is told apart from the package's.
func collectGlobalsFiles(t *testing.T, root string, files []string) ([]globalsFile, packageVars) {
	t.Helper()
	fset := token.NewFileSet()
	vars := packageVars{}
	var parsed []globalsFile
	for _, file := range files {
		src, err := parser.ParseFile(fset, filepath.Join(root, file), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if ast.IsGenerated(src) {
			continue
		}
		gf := globalsFile{path: file, pkg: path.Dir(file) + " " + src.Name.Name, src: src, topVars: map[*ast.ValueSpec]bool{}}
		if vars[gf.pkg] == nil {
			vars[gf.pkg] = map[string]bool{}
		}
		for _, decl := range src.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				gf.topVars[vs] = true
				for _, name := range vs.Names {
					vars[gf.pkg][name.Name] = true
				}
			}
		}
		parsed = append(parsed, gf)
	}
	return parsed, vars
}

// importedPackages maps each local name src imports a buildgate package under
// to that package's "<dir> <package name>" key in vars.
func importedPackages(src *ast.File, vars packageVars) map[string]string {
	byDir := map[string]string{}
	for key := range vars {
		dir, name, _ := strings.Cut(key, " ")
		if !strings.HasSuffix(name, "_test") {
			byDir[dir] = key
		}
	}
	imported := map[string]string{}
	for _, spec := range src.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil || (importPath != modulePath && !strings.HasPrefix(importPath, modulePath+"/")) {
			continue
		}
		dir := strings.TrimPrefix(strings.TrimPrefix(importPath, modulePath), "/")
		if dir == "" {
			dir = "."
		}
		key, ok := byDir[dir]
		if !ok {
			continue
		}
		local := strings.TrimPrefix(key, dir+" ")
		if spec.Name != nil {
			local = spec.Name.Name
		}
		imported[local] = key
	}
	return imported
}

// assignedPackageVar names the package-level variable lhs writes ("pkg.Name"
// for another package's, "Name" for the file's own), or "" when lhs writes a
// local, a parameter, a field of one, or nothing that is a package variable.
func (gf globalsFile) assignedPackageVar(lhs ast.Expr, vars packageVars, imported map[string]string) string {
	for {
		switch e := lhs.(type) {
		case *ast.SelectorExpr:
			if id, ok := e.X.(*ast.Ident); ok && id.Obj == nil {
				if key, ok := imported[id.Name]; ok {
					if vars[key][e.Sel.Name] {
						return id.Name + "." + e.Sel.Name
					}
					return ""
				}
			}
			lhs = e.X
		case *ast.IndexExpr:
			lhs = e.X
		case *ast.StarExpr:
			lhs = e.X
		case *ast.UnaryExpr:
			if e.Op != token.AND {
				return ""
			}
			lhs = e.X
		case *ast.ParenExpr:
			lhs = e.X
		case *ast.Ident:
			if e.Name == "_" {
				return ""
			}
			if e.Obj != nil {
				if vs, ok := e.Obj.Decl.(*ast.ValueSpec); ok && gf.topVars[vs] {
					return e.Name
				}
				return ""
			}
			if vars[gf.pkg][e.Name] {
				return e.Name
			}
			return ""
		default:
			return ""
		}
	}
}

// isOnceBody reports whether call hands a function literal to the Do method
// of a value whose name says it is a once (sync.Once.Do: once.Do, s.initOnce.Do):
// a lazy initialisation that runs once per process, allowed. Any other Do (a
// retry helper, say) is scanned like any other call.
func isOnceBody(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Do" || len(call.Args) != 1 {
		return false
	}
	if _, ok := call.Args[0].(*ast.FuncLit); !ok {
		return false
	}
	var recv string
	switch x := sel.X.(type) {
	case *ast.Ident:
		recv = x.Name
	case *ast.SelectorExpr:
		recv = x.Sel.Name
	}
	return strings.Contains(strings.ToLower(recv), "once")
}

// packageVarAssignments maps "<file> <function> <variable>" to the number of
// statements in that function that assign the package variable. An init
// function, and a function literal handed to a Do method, are not counted:
// both run once per process.
func packageVarAssignments(files []globalsFile, vars packageVars) map[string]int {
	found := map[string]int{}
	for _, gf := range files {
		imported := importedPackages(gf.src, vars)
		for _, decl := range gf.src.Decls {
			name := "(package var initializer)"
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Recv == nil && fn.Name.Name == "init" {
					continue
				}
				name = functionName(fn)
			}
			record := func(lhs ast.Expr) {
				if v := gf.assignedPackageVar(lhs, vars, imported); v != "" {
					found[gf.path+" "+name+" "+v]++
				}
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if isOnceBody(n) {
						return false
					}
				case *ast.AssignStmt:
					if n.Tok != token.DEFINE {
						for _, lhs := range n.Lhs {
							record(lhs)
						}
					}
				case *ast.IncDecStmt:
					record(n.X)
				case *ast.RangeStmt:
					if n.Tok == token.ASSIGN {
						for _, lhs := range []ast.Expr{n.Key, n.Value} {
							if lhs != nil {
								record(lhs)
							}
						}
					}
				}
				return true
			})
		}
	}
	return found
}

func readPackageGlobalsBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(packageGlobalsBaselineFile)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	baseline := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		count, key, ok := strings.Cut(line, " ")
		n, err := strconv.Atoi(count)
		if !ok || err != nil {
			t.Fatalf("baseline line %q is not \"<count> <file> <function> <variable>\"", line)
		}
		baseline[key] = n
	}
	return baseline
}

func writePackageGlobalsBaseline(t *testing.T, found map[string]int) {
	t.Helper()
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(found)) {
		fmt.Fprintf(&b, "%d %s\n", found[key], key)
	}
	if err := os.WriteFile(packageGlobalsBaselineFile, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
}

func TestNoFunctionAssignsAPackageVariable(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	files := trackedGoFiles(t, root)
	if len(files) < minTrackedGoFiles {
		t.Fatalf("only %d tracked .go files found, want at least %d: the file listing is broken and the rule would pass vacuously", len(files), minTrackedGoFiles)
	}
	parsed, vars := collectGlobalsFiles(t, root, files)
	found := packageVarAssignments(parsed, vars)

	if os.Getenv(updatePackageGlobalsBaselineEnv) == "1" {
		writePackageGlobalsBaseline(t, found)
		return
	}

	baseline := readPackageGlobalsBaseline(t)
	for _, key := range slices.Sorted(maps.Keys(found)) {
		allowed, listed := baseline[key]
		switch {
		case !listed:
			t.Errorf("%s: a function assigns a package variable (%d time(s)); pass the dependency in as a parameter or a struct field instead (AGENTS.md, Module boundaries)", key, found[key])
		case found[key] > allowed:
			t.Errorf("%s: assigned %d times, up from %d in %s; pass the dependency in instead of adding to it", key, found[key], allowed, packageGlobalsBaselineFile)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(baseline)) {
		if found[key] < baseline[key] {
			t.Errorf("%s is listed %d time(s) in %s but is now assigned %d time(s): regenerate the baseline with %s=1 go test ./internal/claims -run %s", key, baseline[key], packageGlobalsBaselineFile, found[key], updatePackageGlobalsBaselineEnv, t.Name())
		}
	}
}

// TestPackageVarAssignmentsSeesOnlyPackageVariables proves the scan on a
// fixture: a package variable assigned in a function, directly, through a
// field, by ++, through *&, as a range variable, inside a non-once Do, and
// through another package's selector counts; a local that shadows it, a :=,
// a once.Do body and an init function do not.
func TestPackageVarAssignmentsSeesOnlyPackageVariables(t *testing.T) {
	const fixture = `package p

import other "buildgate/internal/other"

var counter int
var settings struct{ name string }
var once onceType

func init() { counter = 1 }

func f() {
	counter = 2
	counter++
	*&counter = 5
	for counter = range []int{1} {
	}
	retry.Do(func() { counter = 6 })
	once.Do(func() { counter = 7 })
	settings.name = "x"
	other.Hook = nil
	counter := 3
	counter = 4
	_ = counter
}
`
	const otherSrc = "package other\n\nvar Hook func()\n"
	fset := token.NewFileSet()
	src, err := parser.ParseFile(fset, "p.go", fixture, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherFile, err := parser.ParseFile(fset, "other.go", otherSrc, 0)
	if err != nil {
		t.Fatal(err)
	}
	topVars := map[*ast.ValueSpec]bool{}
	for _, decl := range src.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
			for _, spec := range gen.Specs {
				topVars[spec.(*ast.ValueSpec)] = true
			}
		}
	}
	files := []globalsFile{
		{path: "p/p.go", pkg: "p p", src: src, topVars: topVars},
		{path: "internal/other/other.go", pkg: "internal/other other", src: otherFile, topVars: map[*ast.ValueSpec]bool{}},
	}
	vars := packageVars{"p p": {"counter": true, "settings": true}, "internal/other other": {"Hook": true}}
	got := packageVarAssignments(files, vars)
	want := map[string]int{
		"p/p.go f counter":    5,
		"p/p.go f settings":   1,
		"p/p.go f other.Hook": 1,
	}
	if !maps.Equal(got, want) {
		t.Errorf("assignments = %v, want %v", got, want)
	}
}
