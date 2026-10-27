package policy

import (
	"slices"
	"testing"
)

func scopeInput(scope Scope, changed ...string) EvaluateRunInput {
	return EvaluateRunInput{
		VerifyCommand:       []string{"sh", "-c", "true"},
		ChangedFiles:        changed,
		AllowedFiles:        scope.AllowedFiles,
		ScopeUnapproved:     scope.Unapproved,
		AllowedPaths:        scope.AllowedPaths,
		TestsRequiredOptOut: "not exercising tests_red_on_base in this fixture",
	}
}

func TestEvaluateRunRefusesAFileOutsideAnApprovedList(t *testing.T) {
	res := EvaluateRun(scopeInput(Scope{AllowedFiles: []string{"a.go"}}, "a.go", "b.go"))
	if res.Accepted || !slices.Equal(res.DiffScopeViolations, []string{"b.go"}) || !slices.Contains(res.FailedChecks, "diff_scope") {
		t.Errorf("approved list: accepted=%v violations=%v failed=%v, want diff_scope to refuse b.go", res.Accepted, res.DiffScopeViolations, res.FailedChecks)
	}
}

func TestEvaluateRunNotesAFileOutsideAnUnapprovedListAndRefusesNothing(t *testing.T) {
	res := EvaluateRun(scopeInput(Scope{AllowedFiles: []string{"a.go"}, Unapproved: true}, "a.go", "b.go", ".gitignore", "BUILD_REPORT.md"))
	if !res.Accepted || len(res.FailedChecks) != 0 || len(res.DiffScopeViolations) != 0 {
		t.Fatalf("unapproved list: accepted=%v failed=%v violations=%v, want nothing refused", res.Accepted, res.FailedChecks, res.DiffScopeViolations)
	}
	if g, ok := gateByName(res, "diff_scope"); !ok || !g.Passed {
		t.Errorf("diff_scope gate = %+v (ran %v), want a passing gate", g, ok)
	}
	if want := []string{"changed outside the plan's Allowed-Files: b.go"}; !slices.Equal(res.Notes, want) {
		t.Errorf("notes = %q, want %q (harness by-products are never noted)", res.Notes, want)
	}
}

func TestEvaluateRunRefusesAFileOutsideTheRepositoryPathsWhateverTheTicketSays(t *testing.T) {
	changed := []string{"a.go", "docs/readme.md", "vendor/x.go", "ci/build.sh"}
	for _, tc := range []struct {
		unapproved bool
		want       []string
	}{
		{false, []string{"docs/readme.md", "ci/build.sh"}},
		{true, []string{"ci/build.sh"}},
	} {
		res := EvaluateRun(scopeInput(Scope{AllowedFiles: []string{"a.go", "vendor/x.go"}, Unapproved: tc.unapproved, AllowedPaths: []string{"*.go", "docs/", "vendor/"}}, changed...))
		if res.Accepted || !slices.Equal(res.DiffScopeViolations, tc.want) {
			t.Errorf("unapproved=%v: accepted=%v violations=%v, want %v refused", tc.unapproved, res.Accepted, res.DiffScopeViolations, tc.want)
		}
		if tc.unapproved && !slices.Equal(res.Notes, []string{ScopeNotePrefix + "docs/readme.md"}) {
			t.Errorf("notes = %q, want only the file the repository allows and the unapproved list does not", res.Notes)
		}
	}
}

func TestEvaluateRunRepositoryPathsApplyWithoutAnAllowedFilesList(t *testing.T) {
	res := EvaluateRun(scopeInput(Scope{AllowedPaths: []string{"src/"}}, "src/a.go", "other/b.go"))
	if res.Accepted || !slices.Equal(res.DiffScopeViolations, []string{"other/b.go"}) {
		t.Errorf("accepted=%v violations=%v, want other/b.go refused", res.Accepted, res.DiffScopeViolations)
	}
	if res := EvaluateRun(scopeInput(Scope{}, "anything.go")); !res.Accepted {
		t.Errorf("no scope at all must refuse nothing: %v", res.FailedChecks)
	}
}

func TestEvaluateRunRepositoryPathsStillExcludeByproductsAndOracles(t *testing.T) {
	in := scopeInput(Scope{AllowedPaths: []string{"internal/mood/"}}, "internal/mood/mood.go", ".gitignore", "BUILD_REPORT.md", oraclePath, ".buildgate/oracles.json")
	in.Oracles = oracleEvidence()
	if res := EvaluateRun(in); !res.Accepted {
		t.Errorf("by-products and hash-verified oracles must not fail allowed_paths: failed=%v violations=%v", res.FailedChecks, res.DiffScopeViolations)
	}
	in.Oracles = nil
	if res := EvaluateRun(in); res.Accepted || !slices.Contains(res.DiffScopeViolations, ".buildgate/oracles.json") {
		t.Errorf("without oracle evidence .buildgate/oracles.json is an ordinary change outside allowed_paths: %v", res.DiffScopeViolations)
	}
}

func TestPathAllowedIsAnchoredAtTheRepositoryRoot(t *testing.T) {
	for _, tc := range []struct {
		pattern, file string
		want          bool
	}{
		{"src/", "src/a.go", true},
		{"src/", "src/deep/a.go", true},
		{"src/", "x/src/a.go", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/a.md", false},
		{"docs/*.md", "docs/a.md", true},
		{"docs/*.md", "docs/x/a.md", false},
		{"a.go", "a.go", true},
		{"a.go", "x/a.go", false},
	} {
		if got := PathAllowed(tc.file, []string{tc.pattern}); got != tc.want {
			t.Errorf("PathAllowed(%q, %q) = %v, want %v", tc.file, tc.pattern, got, tc.want)
		}
	}
}
