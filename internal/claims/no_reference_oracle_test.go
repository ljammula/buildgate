package claims

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// retiredOracleLiterals are the flag names, request states, gate ids and
// outcomes of the reference-oracle path, which no longer exists.
var retiredOracleLiterals = map[string]bool{
	"draft-oracles":                     true,
	"no-commit-oracles":                 true,
	"reference-oracle-command":          true,
	"reference-oracle-dir":              true,
	"reference-oracle-mount-path":       true,
	"reference-oracle-in-loop-retry":    true,
	"reference_oracle":                  true,
	"reference_oracle_command":          true,
	"oracle_drafting":                   true,
	"oracle_review":                     true,
	"oracle_red_on_base":                true,
	"oracle_green_on_base":              true,
	"oracle_not_run_on_base":            true,
	"oracle_passes_on_base":             true,
	"CommitOraclesActivity":             true,
	"RunPostOracleCommitVerifyActivity": true,
}

// retiredOracleIdentifier matches the names the path's code was written under.
var retiredOracleIdentifier = regexp.MustCompile(`OracleDrafting|OracleReview|ReferenceOracle|CommitOracles|DraftOracles|OracleDraft|OracleGreenOnBase|OracleRedOnBase|OracleNotRun|OraclePassesOnBase|OracleCanary|OracleEvidence|PostOracleCommit`)

// TestNoReferenceOracleExists enforces the claim that the reference-oracle
// path is gone: no package by its names, and no string literal or identifier
// of its flags, request states, gate id, outcomes or steps in the module's
// non-test Go code. The request record's refusal of a state this release does
// not define is TestLoadRefusesARequestInAStateThisReleaseDoesNotDefine.
func TestNoReferenceOracleExists(t *testing.T) {
	root := findRepoRoot(t)
	for _, pkg := range []string{"oraclecanary", "oraclecommit", "oraclemanifest"} {
		if _, err := os.Stat(filepath.Join(root, "internal", pkg)); err == nil {
			t.Errorf("internal/%s exists: the reference oracle's package is retired", pkg)
		}
	}
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
				switch n := n.(type) {
				case *ast.BasicLit:
					if n.Kind == token.STRING {
						if s, err := strconv.Unquote(n.Value); err == nil && retiredOracleLiterals[s] {
							t.Errorf("%s: string %s names the retired reference-oracle path", rel, n.Value)
						}
					}
				case *ast.Ident:
					if retiredOracleIdentifier.MatchString(n.Name) {
						t.Errorf("%s: identifier %s names the retired reference-oracle path", rel, n.Name)
					}
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
