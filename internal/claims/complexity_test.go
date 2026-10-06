// Package claims — this file makes the function-complexity limit a tested
// property of `make verify`, so every agent and human hits it, not only an
// editor hook on one machine. It parses the source directly (go/parser),
// as imports_test.go does, instead of running an external linter.
package claims

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// maxComplexity is the most decision points (cyclomatic complexity) a
// function may have.
const maxComplexity = 25

// complexityBaselineFile lists the functions that were over maxComplexity when
// the limit became a test, one "<complexity> <file> <function>" per line.
// A listed function may shrink, never grow; a function not listed may not
// exceed the limit at all.
const complexityBaselineFile = "testdata/complexity_baseline.txt"

// updateComplexityBaselineEnv, set to 1, rewrites the baseline from the
// current source. Its diff must show only removed lines, lower numbers, or a
// moved or renamed function: a new line or a higher number is a function to
// split instead. The name is this test's own, so regenerating another
// package's goldens across ./... leaves the baseline alone.
const updateComplexityBaselineEnv = "CLAIMS_UPDATE_COMPLEXITY_BASELINE"

// minTrackedGoFiles is a floor on the files scanned: a listing bug that
// returned a fraction of the module would otherwise pass, or in update mode
// truncate the baseline. Measured 2026-10-05: 716 files.
const minTrackedGoFiles = 400

// cyclomaticComplexity counts fn's decision points the way gocyclo does: one
// for the function, one per if, for, range, non-default case, && and ||.
// A function literal counts toward the function that contains it.
func cyclomaticComplexity(fn *ast.FuncDecl) int {
	complexity := 1
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			complexity++
		case *ast.CaseClause:
			if n.List != nil {
				complexity++
			}
		case *ast.CommClause:
			if n.Comm != nil {
				complexity++
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				complexity++
			}
		}
		return true
	})
	return complexity
}

// functionName is fn's name, prefixed with its receiver type for a method:
// "(*Activities).runSandboxWithRetries".
func functionName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	return "(" + types.ExprString(fn.Recv.List[0].Type) + ")." + fn.Name.Name
}

// trackedGoFiles returns the module's own tracked .go files, relative to
// root. It leaves out a fixture under a testdata directory, which is not this
// module's source, and a tracked file deleted from the working tree, which
// git still lists until the deletion is staged.
func trackedGoFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var files []string
	for _, rel := range bytes.Split(bytes.TrimRight(out, "\x00"), []byte{0}) {
		path := string(rel)
		if path == "" || strings.HasPrefix(path, "testdata/") || strings.Contains(path, "/testdata/") {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, path)); os.IsNotExist(err) {
			continue
		}
		files = append(files, path)
	}
	return files
}

// functionsOverLimit maps "<file> <function>" to its complexity, for every
// function in src whose complexity exceeds maxComplexity.
func functionsOverLimit(file string, src *ast.File) map[string]int {
	over := map[string]int{}
	for _, decl := range src.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		key := file + " " + functionName(fn)
		if c := cyclomaticComplexity(fn); c > maxComplexity && c > over[key] {
			over[key] = c
		}
	}
	return over
}

func readComplexityBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(complexityBaselineFile)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	baseline := map[string]int{}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		number, key, ok := strings.Cut(line, " ")
		complexity, err := strconv.Atoi(number)
		if !ok || err != nil {
			t.Fatalf("%s: malformed line %q, want \"<complexity> <file> <function>\"", complexityBaselineFile, line)
		}
		baseline[key] = complexity
	}
	return baseline
}

func writeComplexityBaseline(t *testing.T, over map[string]int) {
	t.Helper()
	var b strings.Builder
	for _, key := range slices.Sorted(maps.Keys(over)) {
		fmt.Fprintf(&b, "%d %s\n", over[key], key)
	}
	if err := os.MkdirAll(filepath.Dir(complexityBaselineFile), 0o755); err != nil {
		t.Fatalf("create baseline directory: %v", err)
	}
	if err := os.WriteFile(complexityBaselineFile, []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
}

func TestFunctionComplexityStaysWithinLimit(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	fset := token.NewFileSet()
	over := map[string]int{}
	files := trackedGoFiles(t, root)
	if len(files) < minTrackedGoFiles {
		t.Fatalf("only %d tracked .go files found, want at least %d: the file listing is broken and the limit would pass vacuously", len(files), minTrackedGoFiles)
	}
	for _, file := range files {
		src, err := parser.ParseFile(fset, filepath.Join(root, file), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if ast.IsGenerated(src) {
			continue
		}
		maps.Copy(over, functionsOverLimit(file, src))
	}

	if os.Getenv(updateComplexityBaselineEnv) == "1" {
		writeComplexityBaseline(t, over)
		return
	}

	baseline := readComplexityBaseline(t)
	for _, key := range slices.Sorted(maps.Keys(over)) {
		allowed, listed := baseline[key]
		switch {
		case !listed:
			t.Errorf("%s has complexity %d, over the limit of %d: split it by responsibility", key, over[key], maxComplexity)
		case over[key] > allowed:
			t.Errorf("%s grew from complexity %d to %d and was already over the limit of %d: split it instead of adding to it", key, allowed, over[key], maxComplexity)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(baseline)) {
		if over[key] < baseline[key] {
			t.Errorf("%s is listed at complexity %d in %s but is now lower, moved or gone: regenerate the baseline with %s=1 go test ./internal/claims -run %s", key, baseline[key], complexityBaselineFile, updateComplexityBaselineEnv, t.Name())
		}
	}
}

// TestCyclomaticComplexityCountsDecisionPoints proves the counter above on
// source whose complexity is known by hand: the repo's own functions passing
// says nothing about whether the counter would notice one that should fail.
func TestCyclomaticComplexityCountsDecisionPoints(t *testing.T) {
	t.Parallel()
	const source = `package p

type T struct{}

func plain() {}

func (t *T) branches(a, b bool, xs []int, ch chan int) int {
	if a && b || len(xs) > 0 { // if, &&, ||
		return 1
	}
	for i := 0; i < 3; i++ { // for
	}
	for range xs { // range
	}
	switch {
	case a: // case
	default:
	}
	select {
	case <-ch: // comm case
	default:
	}
	func() {
		if b { // counts toward branches
		}
	}()
	return 0
}
`
	src, err := parser.ParseFile(token.NewFileSet(), "p.go", source, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]int{"plain": 1, "(*T).branches": 9}
	for _, decl := range src.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		name := functionName(fn)
		if got := cyclomaticComplexity(fn); got != want[name] {
			t.Errorf("complexity of %s = %d, want %d", name, got, want[name])
		}
		delete(want, name)
	}
	if len(want) > 0 {
		t.Errorf("functions not found in the test source: %v", want)
	}
}
