package claims

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hostExecutionFuncs are internal/runner's functions that run a command on
// the host. Tests use them as a stand-in for a sandboxed launch; no
// production path may, because every build, verify, gate and request job
// runs in the sandbox (containment-matrix.md's Credentials row).
var hostExecutionFuncs = map[string]bool{
	"Run":                   true,
	"RunWithRetries":        true,
	"RunWithRetriesChecked": true,
}

// TestNoProductionCodeRunsAJobOnTheHost fails when a non-test Go file outside
// internal/runner names one of internal/runner's host-execution functions,
// as a call or as a value, or dot-imports the package. Structural: it reads selector expressions on the
// file's own import name for the package.
func TestNoProductionCodeRunsAJobOnTheHost(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	for _, rel := range trackedGoFiles(t, root) {
		if strings.HasSuffix(rel, "_test.go") || filepath.Dir(rel) == "internal/runner" {
			continue
		}
		for _, use := range hostExecutionUses(t, filepath.Join(root, rel)) {
			t.Errorf("%s:%d names runner.%s: a job runs in the sandbox only; keep the host runner to tests", rel, use.line, use.name)
		}
	}
}

type hostExecutionUse struct {
	name string
	line int
}

func hostExecutionUses(t *testing.T, path string) []hostExecutionUse {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	local := ""
	for _, imp := range file.Imports {
		if p, _ := strconv.Unquote(imp.Path.Value); p == "buildgate/internal/runner" {
			local = "runner"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}
	if local == "" || local == "_" {
		return nil
	}
	if local == "." {
		return []hostExecutionUse{{name: "* (dot import)", line: 1}}
	}
	var uses []hostExecutionUse
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == local && hostExecutionFuncs[sel.Sel.Name] {
			uses = append(uses, hostExecutionUse{name: sel.Sel.Name, line: fset.Position(sel.Pos()).Line})
		}
		return true
	})
	return uses
}
