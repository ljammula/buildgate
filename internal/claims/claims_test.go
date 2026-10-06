// Package claims enforces one thing: every test name listed as
// enforcing a claim in CLAIMS.md actually exists as a Go test somewhere
// in this repo. It does not and cannot check that the test still
// faithfully covers the claim — that stays a human/review judgment. This
// is deliberately not a doc-parser for the plan markdown itself; CLAIMS.md
// is maintained by hand, and only its test-existence claims are checked
// mechanically (per review edit 6: go test -list re-enters the Go
// toolchain from inside a test process, so this greps source instead).
package claims

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var (
	claimsTestNameRE = regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")
	testFuncRE       = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
)

func TestEveryClaimedTestExists(t *testing.T) {
	t.Parallel()
	repoRoot := findRepoRoot(t)

	claimed := extractClaimedTestNames(t, filepath.Join(repoRoot, "CLAIMS.md"))
	if len(claimed) == 0 {
		t.Fatal("found zero claimed test names in CLAIMS.md — did its table format change?")
	}

	existing := collectTestFuncNames(t, repoRoot)

	for _, name := range claimed {
		if !existing[name] {
			t.Errorf("CLAIMS.md claims %s enforces a claim, but no such test exists in the repo", name)
		}
	}
}

func extractClaimedTestNames(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CLAIMS.md: %v", err)
	}
	matches := claimsTestNameRE.FindAllStringSubmatch(string(b), -1)
	seen := map[string]bool{}
	var names []string
	for _, m := range matches {
		if !seen[m[1]] {
			seen[m[1]] = true
			names = append(names, m[1])
		}
	}
	return names
}

func collectTestFuncNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || !isTestFile(path) {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range testFuncRE.FindAllStringSubmatch(string(b), -1) {
			names[m[1]] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for test files: %v", err)
	}
	return names
}

func isTestFile(path string) bool {
	base := filepath.Base(path)
	return len(base) > len("_test.go") && base[len(base)-len("_test.go"):] == "_test.go"
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find repo root (no go.mod found)")
		}
		dir = parent
	}
}
