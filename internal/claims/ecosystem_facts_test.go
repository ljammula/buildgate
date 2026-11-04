package claims

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"buildgate/internal/languageprofile"
)

// ecosystemBaselineFile lists, one "<file> <fact>" per line, the ecosystem
// facts (languageprofile.Facts) that a file outside internal/languageprofile
// still spells as a string literal of its own. A line may only go: a new pair
// is a fact to read from the profiles instead. Lines starting with # are
// comments, for a pair that is not a fact about a target repository (the
// module's own go.mod) and stays.
const ecosystemBaselineFile = "testdata/ecosystem_baseline.txt"

// updateEcosystemBaselineEnv rewrites the baseline from the tree, keeping its
// comment lines. It only removes lines: a pair the tree has that the baseline
// does not still fails, so a new one can only be added by an edit a reviewer
// sees.
const updateEcosystemBaselineEnv = "CLAIMS_UPDATE_ECOSYSTEM_BASELINE"

// ecosystemFactLiterals maps "<file> <fact>" for every string literal in src
// that is exactly one of facts.
func ecosystemFactLiterals(file string, src *ast.File, facts map[string]bool) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(src, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if value, err := strconv.Unquote(lit.Value); err == nil && facts[value] {
			found[file+" "+value] = true
		}
		return true
	})
	return found
}

// readEcosystemBaseline returns the baseline's pairs and its comment lines,
// the latter kept as written for a rewrite.
func readEcosystemBaseline(t *testing.T) (pairs map[string]bool, comments []string) {
	t.Helper()
	data, err := os.ReadFile(ecosystemBaselineFile)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	pairs = map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.TrimSpace(line) == "":
		case strings.HasPrefix(line, "#"):
			comments = append(comments, line)
		case strings.Count(line, " ") != 1:
			t.Fatalf("%s: malformed line %q, want \"<file> <fact>\"", ecosystemBaselineFile, line)
		default:
			pairs[line] = true
		}
	}
	return pairs, comments
}

// TestEcosystemFactsLiveInLanguageProfile keeps buildgate's knowledge of
// language ecosystems in one home: a file outside internal/languageprofile
// that spells a profile's marker, version source, manifest or test pattern as
// a string literal is listed in the baseline, and a new one fails. Reading
// the fact from languageprofile instead is the fix; the baseline only
// shrinks.
func TestEcosystemFactsLiveInLanguageProfile(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	facts := map[string]bool{}
	for _, fact := range languageprofile.Facts() {
		facts[fact] = true
	}
	files := trackedGoFiles(t, root)
	if len(files) < minTrackedGoFiles {
		t.Fatalf("only %d tracked .go files found, want at least %d: the file listing is broken and the check would pass vacuously", len(files), minTrackedGoFiles)
	}
	fset := token.NewFileSet()
	found := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "internal/languageprofile/") {
			continue
		}
		src, err := parser.ParseFile(fset, filepath.Join(root, file), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		if ast.IsGenerated(src) {
			continue
		}
		maps.Copy(found, ecosystemFactLiterals(file, src, facts))
	}

	baseline, comments := readEcosystemBaseline(t)
	if os.Getenv(updateEcosystemBaselineEnv) == "1" {
		var b strings.Builder
		for _, c := range comments {
			fmt.Fprintln(&b, c)
		}
		for _, pair := range slices.Sorted(maps.Keys(found)) {
			if baseline[pair] {
				fmt.Fprintln(&b, pair)
			}
		}
		if err := os.WriteFile(ecosystemBaselineFile, []byte(b.String()), 0o644); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
		baseline = found
	}
	for _, pair := range slices.Sorted(maps.Keys(found)) {
		if !baseline[pair] {
			file, fact, _ := strings.Cut(pair, " ")
			t.Errorf("%s spells the ecosystem fact %q itself: read it from internal/languageprofile instead", file, fact)
		}
	}
	for _, pair := range slices.Sorted(maps.Keys(baseline)) {
		if !found[pair] {
			t.Errorf("%s is listed in %s but no longer found: regenerate the baseline with %s=1 go test ./internal/claims -run %s", pair, ecosystemBaselineFile, updateEcosystemBaselineEnv, t.Name())
		}
	}
}

// TestEcosystemFactLiteralsFindsExactLiteralsOnly proves the scanner above
// on a fixture: an exact literal counts, the same name inside a sentence does
// not.
func TestEcosystemFactLiteralsFindsExactLiteralsOnly(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "x.go", `package x
var a = "go.mod"
var b = "edit go.mod first"
var c = `+"`*_test.go`"+`
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := ecosystemFactLiterals("x.go", src, map[string]bool{"go.mod": true, "*_test.go": true})
	if len(got) != 2 || !got["x.go go.mod"] || !got["x.go *_test.go"] {
		t.Fatalf("found %v, want the two exact literals only", got)
	}
}
