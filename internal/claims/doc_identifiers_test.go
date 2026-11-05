package claims

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// identifierDocs are the documents whose backticked code identifiers must
// name a symbol this repository defines.
var identifierDocs = []string{"safety-contract.md", "CLAIMS.md", "containment-matrix.md", "AGENTS.md"}

// identifierAllowlist holds the backticked identifiers in identifierDocs
// that are code but are defined outside this repository, each with its
// reason.
var identifierAllowlist = map[string]string{
	"GetVersion":       "Temporal Go SDK workflow.GetVersion, the version marker AGENTS.md's activity rule names",
	"sessionStorage":   "the browser's Web Storage API, where the console keeps its token",
	"localStorage":     "the browser's Web Storage API, which the console does not use for a token",
	"isRequired":       "GitHub GraphQL API field of a check on a pull request",
	"thinkingLevelMap": "a key of pi's models.json, which quickstart writes",
	"RuntimeDefault":   "the container runtime's default seccomp profile",
}

var (
	// inlineCodeRE matches one inline code span: a run of backticks, its
	// content, and the same run again.
	inlineCodeRE = regexp.MustCompile("(`+)([^`]+?)(?:`+)")
	// plainIdentRE is one Go/TS/Python identifier.
	plainIdentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	tsDeclRE     = regexp.MustCompile(`\b(?:function\*?|const|let|var|class|interface|type|enum)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	pyDeclRE     = regexp.MustCompile(`(?m)^\s*(?:async\s+)?(?:def|class)\s+([A-Za-z_][A-Za-z0-9_]*)|^([A-Za-z_][A-Za-z0-9_]*)\s*[:=]`)
	shDeclRE     = regexp.MustCompile(`(?m)^\s*(?:function\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*\(\)`)
)

// hasInnerCapital reports whether name has an upper-case letter after its
// first character and a lower-case letter anywhere: lowerCamelCase or
// PascalCase of two words or more. A single capitalised word, an
// ALL_CAPS name and a snake_case key do not.
func hasInnerCapital(name string) bool {
	lower, inner := false, false
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z' && i > 0:
			inner = true
		}
	}
	return lower && inner
}

// docIdentifier returns the symbol name a backticked span asks to resolve,
// or "" when the span is not shaped like a code identifier of this
// repository. A span is checked when it is one identifier with an inner
// capital (lowerCamelCase, PascalCase of two words or more), or dotted
// identifiers (`pkg.Name`, `Type.Method`, `pkg.Type.Field`) whose last
// part starts upper-case or has an inner capital and whose first part is
// not an external Go package, each part two characters or more (so a
// version pattern like vX.Y.Z is not one); a trailing "()" is ignored. Anything with a
// space, slash, hyphen, file extension or other punctuation is prose, a
// path, a flag or a command, and a TestXxx, BenchmarkXxx or FuzzXxx name is
// left to TestClaimsReferenceRealTests.
func docIdentifier(span string, externalPkgs map[string]bool) string {
	span = strings.TrimSuffix(strings.TrimSpace(span), "()")
	parts := strings.Split(span, ".")
	for _, part := range parts {
		if !plainIdentRE.MatchString(part) || (len(parts) > 1 && len(part) < 2) {
			return ""
		}
	}
	last := parts[len(parts)-1]
	if len(parts) == 1 {
		if !hasInnerCapital(last) {
			return ""
		}
	} else {
		if externalPkgs[parts[0]] {
			return ""
		}
		startsUpper := last[0] >= 'A' && last[0] <= 'Z'
		if !startsUpper && !hasInnerCapital(last) {
			return ""
		}
	}
	for _, prefix := range []string{"Test", "Benchmark", "Fuzz"} {
		if rest, ok := strings.CutPrefix(last, prefix); ok && rest != "" && rest[0] >= 'A' && rest[0] <= 'Z' {
			return ""
		}
	}
	return last
}

// trackedSourceFiles lists the tracked Go, Python, TypeScript and shell
// files under root, leaving out testdata directories and files deleted from
// the working tree.
func trackedSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.go", "*.py", "*.ts", "*.tsx", "*.mjs", "*.sh").Output()
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

// goSymbols adds to symbols every name src declares (functions, methods,
// types, variables, constants, struct fields and interface methods) and to
// external the name src uses for each package outside this module.
func goSymbols(src *ast.File, symbols, external map[string]bool) {
	for _, imp := range src.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "buildgate" || strings.HasPrefix(path, "buildgate/") {
			continue
		}
		name := filepath.Base(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		external[name] = true
	}
	ast.Inspect(src, func(n ast.Node) bool {
		switch d := n.(type) {
		case *ast.FuncDecl:
			symbols[d.Name.Name] = true
		case *ast.TypeSpec:
			symbols[d.Name.Name] = true
		case *ast.ValueSpec:
			for _, name := range d.Names {
				symbols[name.Name] = true
			}
		case *ast.Field:
			for _, name := range d.Names {
				symbols[name.Name] = true
			}
		}
		return true
	})
}

// repositorySymbols returns every symbol the tracked source files define,
// and the names of the external Go packages they import (less any that is
// also one of this module's own package names).
func repositorySymbols(t *testing.T, root string) (symbols, external map[string]bool) {
	t.Helper()
	symbols, external = map[string]bool{}, map[string]bool{}
	own := map[string]bool{}
	fset := token.NewFileSet()
	for _, rel := range trackedSourceFiles(t, root) {
		content, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		var re *regexp.Regexp
		switch filepath.Ext(rel) {
		case ".go":
			src, err := parser.ParseFile(fset, rel, content, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", rel, err)
			}
			own[src.Name.Name] = true
			goSymbols(src, symbols, external)
			continue
		case ".py":
			re = pyDeclRE
		case ".sh":
			re = shDeclRE
		default:
			re = tsDeclRE
		}
		for _, m := range re.FindAllSubmatch(content, -1) {
			for _, group := range m[1:] {
				if len(group) > 0 {
					symbols[string(group)] = true
				}
			}
		}
	}
	for name := range own {
		delete(external, name)
	}
	return symbols, external
}

// unresolvedDocIdentifiers returns "doc:line: `span`" for every backticked
// identifier of content (outside fenced code blocks) that names no symbol,
// and records in allowed each identifierAllowlist entry it meets.
func unresolvedDocIdentifiers(doc string, content []byte, symbols, external, allowed map[string]bool) []string {
	var bad []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	fenced := false
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(text), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		for _, m := range inlineCodeRE.FindAllStringSubmatch(text, -1) {
			name := docIdentifier(m[2], external)
			if name == "" || symbols[name] {
				continue
			}
			if _, ok := identifierAllowlist[name]; ok {
				allowed[name] = true
				continue
			}
			bad = append(bad, fmt.Sprintf("%s:%d: `%s`", doc, line, strings.TrimSpace(m[2])))
		}
	}
	return bad
}

// TestDocIdentifiersNameRealSymbols checks that every backticked code
// identifier in safety-contract.md, CLAIMS.md, containment-matrix.md and
// AGENTS.md names a symbol a tracked Go, Python, TypeScript or shell file of
// this repository defines (a func, method, type, var, const, struct field or
// interface method; a Python def, class or module-level name; a TypeScript
// declaration; a shell function), so a rename or deletion cannot leave these
// documents citing code that is gone. docIdentifier says which spans count
// as identifiers; identifierAllowlist holds the few defined outside the
// repository.
func TestDocIdentifiersNameRealSymbols(t *testing.T) {
	t.Parallel()
	root := findRepoRoot(t)
	symbols, external := repositorySymbols(t, root)
	var bad []string
	allowed := map[string]bool{}
	for _, doc := range identifierDocs {
		content, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		bad = append(bad, unresolvedDocIdentifiers(doc, content, symbols, external, allowed)...)
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s names no symbol defined in a tracked Go, Python, TypeScript or shell file: name the real one, or add it to identifierAllowlist with its reason if it is defined outside this repository", b)
	}
	for _, name := range sortedKeys(boolKeys(identifierAllowlist)) {
		switch {
		case symbols[name]:
			t.Errorf("identifierAllowlist entry %q is defined in this repository: remove it from the list", name)
		case !allowed[name]:
			t.Errorf("identifierAllowlist entry %q is cited by none of %v: remove it from the list", name, identifierDocs)
		}
	}
}

// boolKeys returns m's keys as a set.
func boolKeys(m map[string]string) map[string]bool {
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return set
}

// TestDocIdentifierShape pins which backticked spans the check reads.
func TestDocIdentifierShape(t *testing.T) {
	t.Parallel()
	external := map[string]bool{"os": true}
	cases := map[string]string{
		"concludeRound":                  "concludeRound",
		"Request.StackedOnAnotherTicket": "StackedOnAnotherTicket",
		"request.PlanReviewNotes":        "PlanReviewNotes",
		"release.Decision.Allowed":       "Allowed",
		"dp.sandbox.runtime()":           "",
		"RunWorkflow()":                  "RunWorkflow",
		"os.Stdout":                      "",
		"Persist":                        "",
		"FACTORYD_AUTOSTART":             "",
		"max_review_rounds":              "",
		"run.json":                       "",
		"-on-branch":                     "",
		"internal/run/run.go":            "",
		"draft -> product_policy_check":  "",
		"TestNoPrivateNotesReferences":   "",
		"SC-001":                         "",
	}
	for span, want := range cases {
		if got := docIdentifier(span, external); got != want {
			t.Errorf("docIdentifier(%q) = %q, want %q", span, got, want)
		}
	}
}
