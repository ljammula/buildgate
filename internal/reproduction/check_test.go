package reproduction

import "testing"

func goodJudged() Judged {
	return Judged{
		Tracked:   []string{"go.mod", "internal/parse/parse.go", "internal/parse/parse_test.go", "vendorlink"},
		Protected: []string{"internal/secret/", "Makefile"},
	}
}

func TestCheckPathAcceptsANewTestFile(t *testing.T) {
	if err := CheckPath("internal/parse/edge_parse_test.go", goodJudged()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPathRules(t *testing.T) {
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
		if err := CheckPath(c.path, goodJudged()); err != c.want {
			t.Errorf("%s (%q): err = %v, want %v", name, c.path, err, c.want)
		}
	}
}

func TestCheckPathTestPatternsAreTheRepositorysOrTheDefaults(t *testing.T) {
	j := goodJudged()
	j.TestPatterns = []string{"*_spec_test.go"}
	if err := CheckPath("internal/parse/edge_parse_test.go", j); err != errNotTest {
		t.Errorf("repository patterns not applied: %v", err)
	}
	j.TestPatterns = []string{"__tests__/"}
	if err := CheckPath("src/__tests__/parse.test.ts", j); err != nil {
		t.Errorf("directory pattern refused: %v", err)
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
