package claims

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/policy"
)

// retiredGateLiterals are the check ids, flags and .factory.yml keys of the
// four fixed command gates, which no longer exist.
var retiredGateLiterals = map[string]bool{
	"lint": true, "security_audit": true, "unit_tests": true, "integration_tests": true,
	"lint-command": true, "security-command": true, "unit-test-command": true, "integration-test-command": true,
	"lint_command": true, "security_command": true, "unit_test_command": true, "integration_test_command": true,
}

// TestTheCommandGatesAreTheVerifyFullSuiteAndTheRepositorysOwn enforces the
// claim that no built-in lint, security_audit, unit_tests or
// integration_tests gate, flag or .factory.yml key exists: the checks every
// run records are canonical_verify, diff_scope, tests_red_on_base and
// full_suite_verify, plus the repository's own `repo-<id>` gates; and no
// non-test Go code outside the policy's gate table names one of the retired
// ids, flags or keys. A repository that wants a lint check lists it under
// `gates:` (TestLoadRefusesTheRetiredFixedGateKeys is the refusal of the
// keys).
func TestTheCommandGatesAreTheVerifyFullSuiteAndTheRepositorysOwn(t *testing.T) {
	want := []string{"canonical_verify", "diff_scope", "tests_red_on_base", "full_suite_verify"}
	if strings.Join(policy.AllGateChecks, ",") != strings.Join(want, ",") {
		t.Errorf("policy.AllGateChecks = %v, want %v", policy.AllGateChecks, want)
	}
	if got := policy.RepoGateChecks(map[string]string{"repo-b": "x", "repo-a": "y", "lint": "z"}); strings.Join(got, ",") != "repo-a,repo-b" {
		t.Errorf("RepoGateChecks = %v, want only the repository's gates, sorted", got)
	}
	root := findRepoRoot(t)
	fset := token.NewFileSet()
	scanned := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); strings.HasPrefix(name, ".") || name == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) != ".go" || isTestFile(path) {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil && retiredGateLiterals[s] {
						t.Errorf("%s: string %s names a retired fixed command gate", rel, lit.Value)
					}
				}
				if id, ok := n.(*ast.Ident); ok && (id.Name == "CommandGate" || id.Name == "CommandGates" || id.Name == "CommandGateIDs") {
					t.Errorf("%s: identifier %s names the retired command-gate table", rel, id.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", top, err)
		}
	}
	if scanned < 200 {
		t.Fatalf("scanned only %d Go files: the walk is broken, which would make this test vacuous", scanned)
	}
}
