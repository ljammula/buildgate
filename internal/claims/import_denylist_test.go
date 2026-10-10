// Package claims — this file keeps two dependency rules from AGENTS.md's
// "Module boundaries" section true by test: fakes are hand-written, and a
// package starts running host processes only by a reviewed decision.
package claims

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// mockFrameworks are the import-path prefixes of generated or reflective mock
// libraries. A fake here is a small hand-written struct in a _test.go file.
var mockFrameworks = []string{
	"github.com/golang/mock",
	"go.uber.org/mock",
	"github.com/stretchr/testify/mock",
	"github.com/vektra/mockery",
	"github.com/matryer/moq",
	"github.com/maxbrunsfeld/counterfeiter",
	"github.com/gojuno/minimock",
}

func isMockFramework(importPath string) bool {
	for _, prefix := range mockFrameworks {
		if importPath == prefix || strings.HasPrefix(importPath, prefix+"/") {
			return true
		}
	}
	return false
}

func TestNoMockFrameworkIsImported(t *testing.T) {
	t.Parallel()
	if !isMockFramework("go.uber.org/mock/gomock") || isMockFramework("go.uber.org/mockery") {
		t.Fatal("isMockFramework does not match by import-path prefix; the scan below proves nothing")
	}
	root := findRepoRoot(t)
	fset := token.NewFileSet()
	for _, file := range trackedGoFiles(t, root) {
		src, err := parser.ParseFile(fset, filepath.Join(root, file), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, imp := range src.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("%s: import path %s: %v", file, imp.Path.Value, err)
			}
			if isMockFramework(importPath) {
				t.Errorf("%s imports the mock framework %s: write a small fake struct in a _test.go file instead", file, importPath)
			}
		}
	}
}

// processRunningPackages are the packages whose non-test code imports os/exec.
// A new entry is a new place the module reaches the host: add it only with
// the boundary interface its callers use to fake it (AGENTS.md, "Rules for a
// new boundary").
var processRunningPackages = map[string]bool{
	"buildgate/cmd/bg-forward":           true,
	"buildgate/cmd/factoryd":             true,
	"buildgate/internal/daemonheartbeat": true,
	"buildgate/internal/forge":           true,
	"buildgate/internal/hostcontrol":     true,
	"buildgate/internal/notify":          true,
	"buildgate/internal/openshell":       true,
	"buildgate/internal/oraclecanary":    true,
	"buildgate/internal/oraclecommit":    true,
	"buildgate/internal/projectconfig":   true,
	"buildgate/internal/release":         true,
	"buildgate/internal/requestdriver":   true,
	"buildgate/internal/requestsubmit":   true,
	"buildgate/internal/runner":          true,
	"buildgate/internal/sandbox":         true,
	"buildgate/internal/testfixture":     true,
	"buildgate/internal/workflow":        true,
	"buildgate/internal/workspace":       true,

	// Test fixtures: git in the test's own temp repository, as testfixture.
	"buildgate/internal/requestdriver/requestdrivertest": true,
}

func TestOnlyListedPackagesRunProcesses(t *testing.T) {
	t.Parallel()
	g := buildModuleImportGraph(t, findRepoRoot(t))
	for pkg, imports := range g {
		if imports["os/exec"] && !processRunningPackages[pkg] {
			t.Errorf("%s imports os/exec but is not in processRunningPackages: run the process through an interface its caller declares, then list the package", pkg)
		}
	}
	for pkg := range processRunningPackages {
		if !g[pkg]["os/exec"] {
			t.Errorf("%s is in processRunningPackages but no longer imports os/exec: remove it from the list", pkg)
		}
	}
}
