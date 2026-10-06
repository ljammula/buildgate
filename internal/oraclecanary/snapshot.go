package oraclecanary

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Ecosystem names the test toolchain an oracle file belongs to.
type Ecosystem string

const (
	Go     Ecosystem = "go"
	Python Ecosystem = "python"
	JS     Ecosystem = "js"
	Dart   Ecosystem = "dart"
)

// oracleMetaFiles are the non-test files an oracle directory carries; they
// are copied into a canary snapshot unchanged.
var oracleMetaFiles = map[string]bool{
	"MANIFEST.json":   true,
	"RUN_COMMAND.txt": true,
}

var (
	pythonTestName = regexp.MustCompile(`^(test_oracle_.+|.+_oracle_test)\.py$`)
	jsTestName     = regexp.MustCompile(`\.oracle\.test\.(ts|tsx|js|jsx|mjs|cjs)$`)
	goBuildLine    = regexp.MustCompile(`^//(go:build|[ \t]*\+build)\b`)
)

// Classify reports the ecosystem of a test file by name. isTest is false for
// MANIFEST.json and RUN_COMMAND.txt. Any other name is unsupported: an
// oracle the canary cannot make fail must never be silently passed.
func Classify(name string) (eco Ecosystem, isTest bool, err error) {
	base := filepath.Base(name)
	switch {
	case oracleMetaFiles[base]:
		return "", false, nil
	case strings.HasSuffix(base, "_test.go"):
		return Go, true, nil
	case pythonTestName.MatchString(base):
		return Python, true, nil
	case jsTestName.MatchString(base):
		return JS, true, nil
	case strings.HasSuffix(base, "_test.dart"):
		return Dart, true, nil
	}
	return "", false, fmt.Errorf("oracle file %q is not a recognised oracle test (*_test.go, test_oracle_*.py, *_oracle_test.py, *.oracle.test.{ts,js}, *_test.dart): ecosystem unsupported, so no canary can be built", name)
}

// CanaryContent returns always-failing content for one oracle test file that
// still compiles/collects in its ecosystem. original is only consulted for Go,
// whose package clause must match the package the file is spliced into.
func CanaryContent(name string, original []byte, nonce string) ([]byte, error) {
	eco, isTest, err := Classify(name)
	if err != nil {
		return nil, err
	}
	if !isTest {
		return nil, fmt.Errorf("%q is not a test file", name)
	}
	switch eco {
	case Go:
		return goCanary(name, original, nonce)
	case Python:
		return pythonCanary(original, nonce), nil
	case JS:
		return jsCanary(original, nonce), nil
	case Dart:
		return dartCanary(original, nonce), nil
	}
	return nil, fmt.Errorf("unsupported ecosystem %q", eco)
}

// BuildSnapshot writes a canary copy of the oracle directory srcDir into
// dstDir: every test file keeps its name but is replaced by always-failing
// content; MANIFEST.json and RUN_COMMAND.txt are copied unchanged. It returns
// the oracle's single ecosystem, and errors on an unsupported file, a mix of
// ecosystems, or a directory with no tests.
func BuildSnapshot(srcDir, dstDir, nonce string) (Ecosystem, error) {
	if err := checkNonce(nonce); err != nil {
		return "", err
	}
	var eco Ecosystem
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dstDir, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("oracle entry %q is not a regular file", rel)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fileEco, isTest, err := Classify(rel)
		if err != nil {
			return err
		}
		if isTest {
			if eco != "" && eco != fileEco {
				return fmt.Errorf("oracle mixes ecosystems %s and %s (%q); one oracle directory must be one ecosystem", eco, fileEco, rel)
			}
			eco = fileEco
			if data, err = CanaryContent(rel, data, nonce); err != nil {
				return err
			}
		}
		return os.WriteFile(filepath.Join(dstDir, rel), data, 0o644)
	})
	if err != nil {
		return "", err
	}
	if eco == "" {
		return "", fmt.Errorf("oracle directory %q contains no test files", srcDir)
	}
	return eco, nil
}

// goCanary builds the always-failing Go canary for one oracle file. It keeps
// the SAME NAME for every top-level Test function of the original (except
// TestMain): operators' RUN_COMMANDs select tests by their real names (-run
// TestOracleLevel), and a canary named differently is selected by nothing, runs
// no tests, exits 0, and reads FALSE_PASS for a perfectly good command. The
// file is parsed with go/parser (a naive line match can pick a `package` or
// `func` inside a comment); any //go:build or // +build lines are carried over
// so the canary is excluded exactly when the real file is; and an oracle with
// no TestOracle* function is refused, because the proposed command selects
// tests with -run TestOracle and would otherwise run nothing and exit 0.
func goCanary(name string, original []byte, nonce string) ([]byte, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, original, parser.SkipObjectResolution)
	if err != nil || file.Name == nil {
		return nil, fmt.Errorf("%s: cannot parse the file; cannot build a Go canary: %v", name, err)
	}
	var names []string
	hasOracle := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isGoTestName(fn.Name.Name) {
			continue
		}
		names = append(names, fn.Name.Name)
		hasOracle = hasOracle || strings.HasPrefix(fn.Name.Name, "TestOracle")
	}
	if !hasOracle {
		return nil, fmt.Errorf("%s: no func TestOracle* found; the generated command selects tests with -run TestOracle, so this oracle would run nothing", name)
	}
	var constraints []string
	for _, line := range strings.Split(string(original), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			break
		}
		if goBuildLine.MatchString(trimmed) {
			constraints = append(constraints, trimmed)
		}
	}
	var b strings.Builder
	if len(constraints) > 0 {
		b.WriteString(strings.Join(constraints, "\n") + "\n\n")
	}
	fmt.Fprintf(&b, "package %s\n\nimport \"testing\"\n", file.Name.Name)
	for _, n := range names {
		fmt.Fprintf(&b, "\n// %s always fails: a RUN_COMMAND that really runs the oracle must fail on it.\nfunc %s(t *testing.T) {\n\tt.Fatal(%s)\n}\n", n, n, msgExpr(`"`, nonce))
	}
	return []byte(b.String()), nil
}

// isGoTestName reports whether name is a Go test function name (Test followed
// by a non-lowercase rune) other than TestMain.
func isGoTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok || name == "TestMain" {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}
