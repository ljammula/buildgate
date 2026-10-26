package reproduction

import (
	"strings"
	"testing"
)

const testSpec = "# Spec\n\n## Acceptance\n\n  - `Parse(\"a=1\")` returns a map with key `a`.  \n- Errors name the line.\n"

func goodFinding() Finding {
	return Finding{
		SpecLine: 5,
		SpecText: "- `Parse(\"a=1\")` returns a map with key `a`.",
		Path:     "internal/parse/repro_parse_test.go",
		Content:  []byte("package parse\n\nimport \"testing\"\n\nfunc TestReproParse(t *testing.T) {}\n"),
	}
}

func goodJudged() Judged {
	return Judged{
		Spec:      []byte(testSpec),
		Tracked:   []string{"go.mod", "internal/parse/parse.go", "internal/parse/parse_test.go", "vendorlink"},
		Protected: []string{"internal/secret/", "Makefile"},
		Root:      RootFacts{"go.mod": []byte("module x\n")},
	}
}

func TestReproductionShapeAcceptsAWellFormedReproduction(t *testing.T) {
	a, err := Check(goodFinding(), goodJudged())
	if err != nil {
		t.Fatal(err)
	}
	if a.Runner != "go-test-json" || len(a.TestIDs) != 1 || a.TestIDs[0] != "TestReproParse" ||
		a.SpecText != "- `Parse(\"a=1\")` returns a map with key `a`." || a.SpecLine != 5 {
		t.Errorf("Accepted = %+v", a)
	}
}

func TestReproductionShapeSpecCitation(t *testing.T) {
	for name, mutate := range map[string]func(*Finding){
		"line zero":          func(f *Finding) { f.SpecLine = 0 },
		"past the end":       func(f *Finding) { f.SpecLine = 99 },
		"wrong line":         func(f *Finding) { f.SpecLine = 6 },
		"one byte differs":   func(f *Finding) { f.SpecText = strings.Replace(f.SpecText, "map", "Map", 1) },
		"a prefix only":      func(f *Finding) { f.SpecText = "- `Parse(\"a=1\")` returns" },
		"blank line cited":   func(f *Finding) { f.SpecLine, f.SpecText = 2, "" },
		"no citation at all": func(f *Finding) { f.SpecLine, f.SpecText = 0, "" },
	} {
		f := goodFinding()
		mutate(&f)
		if _, err := Check(f, goodJudged()); err != errSpecLine {
			t.Errorf("%s: err = %v, want %v", name, err, errSpecLine)
		}
	}
	// Surrounding whitespace on either side is trimmed.
	f := goodFinding()
	f.SpecText = "\t" + f.SpecText + "  "
	if _, err := Check(f, goodJudged()); err != nil {
		t.Errorf("trimmed citation refused: %v", err)
	}
}

func TestReproductionShapeContentCap(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":    nil,
		"over cap": []byte("package parse\n" + strings.Repeat("/", MaxContentBytes)),
		"binary":   []byte("package parse\x00"),
	} {
		f := goodFinding()
		f.Content = content
		if _, err := Check(f, goodJudged()); err != errContent {
			t.Errorf("%s: err = %v, want %v", name, err, errContent)
		}
	}
}

func TestReproductionShapePathRules(t *testing.T) {
	cases := map[string]struct {
		path string
		want error
	}{
		"absolute":                  {"/internal/parse/x_test.go", errPathShape},
		"dot-dot":                   {"internal/../x_test.go", errPathShape},
		"unclean":                   {"internal//parse/x_test.go", errPathShape},
		"dot component":             {"./x_test.go", errPathShape},
		"backslash":                 {`internal\x_test.go`, errPathShape},
		"control":                   {"internal/x\n_test.go", errPathShape},
		"leading dash":              {"-x_test.go", errPathShape},
		"pathspec magic":            {":x_test.go", errPathShape},
		"not a test file":           {"internal/parse/repro.go", errNotTest},
		"under .factory":            {".factory/check_test.go", errReserved},
		"under .Factory folded":     {".Factory/check_test.go", errReserved},
		"under .git":                {"a/.git/x_test.go", errReserved},
		"protected dir":             {"internal/secret/x_test.go", errProtected},
		"protected dir case-folded": {"internal/SECRET/x_test.go", errProtected},
		"instruction path":          {".claude/skills/x_test.go", errInstruction},
		"tracked":                   {"internal/parse/parse_test.go", errTracked},
		"tracked case-folded":       {"Internal/Parse/PARSE_test.go", errTracked},
		"under a tracked file":      {"vendorlink/x_test.go", errTracked},
	}
	for name, c := range cases {
		f := goodFinding()
		f.Path = c.path
		if _, err := Check(f, goodJudged()); err != c.want {
			t.Errorf("%s (%q): err = %v, want %v", name, c.path, err, c.want)
		}
	}
}

func TestReproductionShapeTestPatternsAreTheRepositorysOrTheDefaults(t *testing.T) {
	j := goodJudged()
	j.TestPatterns = []string{"*_spec_test.go"}
	if _, err := Check(goodFinding(), j); err != errNotTest {
		t.Errorf("repository patterns not applied: %v", err)
	}
	// A directory pattern counts (contract: test_patterns, directory patterns included).
	j.TestPatterns = []string{"__tests__/"}
	j.Root = RootFacts{"package.json": []byte(`{"devDependencies":{"vitest":"3"}}`)}
	f := goodFinding()
	f.Path = "src/__tests__/parse.test.ts"
	f.Content = []byte("import { test } from 'vitest'\ntest('parses', () => {})\n")
	if _, err := Check(f, j); err != nil {
		t.Errorf("directory pattern refused: %v", err)
	}
}

func TestReproductionShapeNeedsARunnerAndTestIDs(t *testing.T) {
	j := goodJudged()
	j.Root = RootFacts{}
	if _, err := Check(goodFinding(), j); err != errNoRunner {
		t.Errorf("no runner: err = %v", err)
	}
	f := goodFinding()
	f.Content = []byte("package parse\n\nfunc helper() {}\n")
	if _, err := Check(f, goodJudged()); err != errTestIDs {
		t.Errorf("no test: err = %v", err)
	}
	var b strings.Builder
	b.WriteString("package parse\n\nimport \"testing\"\n\n")
	for i := 0; i <= MaxTestIDs; i++ {
		b.WriteString("func TestN" + string(rune('A'+i)) + "(t *testing.T) {}\n")
	}
	f.Content = []byte(b.String())
	if _, err := Check(f, goodJudged()); err != errTestIDs {
		t.Errorf("over %d tests: err = %v", MaxTestIDs, err)
	}
}

func TestReproductionShapeCapsAReviewsReproductions(t *testing.T) {
	var fs []Finding
	for _, p := range []string{"a/one_test.go", "a/ONE_test.go", "a/two_test.go", "a/three_test.go", "a/four_test.go"} {
		f := goodFinding()
		f.Path = p
		fs = append(fs, f)
	}
	fs = append(fs, Finding{SpecLine: 5})
	got := CheckAll(fs, goodJudged())
	want := []string{"", errDuplicate.Error(), "", "", errTooMany.Error(), errNoReproduced.Error()}
	for i, c := range got {
		if c.Refused != want[i] || (c.Refused == "") != (c.Accepted != nil) {
			t.Errorf("finding %d: %+v, want refused %q", i, c, want[i])
		}
	}
}

// A refused reproduction takes no place under MaxReproductions and no path:
// three refused ones before them crowd out none of the valid ones, and a
// refused path may be carried again by a valid one.
func TestReproductionShapeRefusedOnesDoNotCountTowardTheCap(t *testing.T) {
	var fs []Finding
	for _, p := range []string{"a/bad1_test.go", "a/bad2_test.go", "a/bad3_test.go"} {
		f := goodFinding()
		f.Path, f.SpecText = p, "not the spec's line"
		fs = append(fs, f)
	}
	for _, p := range []string{"a/one_test.go", "a/two_test.go", "a/bad1_test.go", "a/four_test.go"} {
		f := goodFinding()
		f.Path = p
		fs = append(fs, f)
	}
	got := CheckAll(fs, goodJudged())
	want := []string{errSpecLine.Error(), errSpecLine.Error(), errSpecLine.Error(), "", "", "", errTooMany.Error()}
	for i, c := range got {
		if c.Refused != want[i] || (c.Refused == "") != (c.Accepted != nil) {
			t.Errorf("finding %d: %+v, want refused %q", i, c, want[i])
		}
	}
}

func TestIsCleanRelative(t *testing.T) {
	for p, want := range map[string]bool{
		"a/b_test.go": true, "": false, "/a/b_test.go": false, "a/../b_test.go": false, "./a_test.go": false,
		"a//b_test.go": false, "-a_test.go": false, `a\b_test.go`: false, "a/\x01b_test.go": false,
	} {
		if got := IsCleanRelative(p); got != want {
			t.Errorf("IsCleanRelative(%q) = %v, want %v", p, got, want)
		}
	}
}
