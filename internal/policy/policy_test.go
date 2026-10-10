package policy

import (
	"slices"
	"strings"
	"testing"

	"buildgate/internal/run"
)

const wellFormedTicket = `This is an existing repo. Read ` + "`ARCHITECTURE.md`" + `, ` + "`PROGRESS.md`" + `, and ` + "`spec/contract.md`" + ` before changing anything, and preserve all existing functionality and passing tests -- this is an extension, not a rewrite.

## Goal

Add the first useful product slice.

## Required changes

1. Add the domain behavior.
2. Add the endpoint and screen.

Update ` + "`ARCHITECTURE.md`" + ` and append a ` + "`PROGRESS.md`" + ` entry before finishing.

## Verification

` + "`make verify`" + ` must pass. Confirm ` + "`make verify-full`" + ` still passes.

## Commit

Commit once both pass. Commit message must be exactly: ` + "`ticket(002): product-slice`" + `
`

func TestCanonicalVerifyPassesWhenBuildAndVerifySucceed(t *testing.T) {
	cmd := []string{"sh", "-c", "make verify"}
	got := CanonicalVerify(cmd, 0, 0, 1234, "abc123")

	if got.Check != "canonical_verify" || !slices.Equal(got.Command, cmd) || !got.Passed || got.ExitCode != 0 || got.DurationMs != 1234 || got.LogSHA256 != "abc123" {
		t.Errorf("CanonicalVerify() = %+v, want passing gate with supplied evidence", got)
	}
}

func TestCanonicalVerifyFailsWhenBuildFails(t *testing.T) {
	got := CanonicalVerify([]string{"make", "verify"}, 1, 0, 10, "abc123")

	if got.Passed {
		t.Errorf("CanonicalVerify() Passed = true, want false when build fails")
	}
}

func TestCanonicalVerifyFailsWhenVerifyFails(t *testing.T) {
	got := CanonicalVerify([]string{"make", "verify"}, 0, 2, 10, "abc123")

	if got.Passed {
		t.Errorf("CanonicalVerify() Passed = true, want false when verification fails")
	}
	if got.ExitCode != 2 {
		t.Errorf("CanonicalVerify() ExitCode = %d, want 2", got.ExitCode)
	}
}

func TestDiffScopeViolationsNoneWithinAllowedList(t *testing.T) {
	gate, got := DiffScope([]string{"a.go", "a_test.go"}, []string{"a.go", "a_test.go", "b.go"})
	if len(got) != 0 {
		t.Errorf("got %v, want no violations", got)
	}
	if gate.Check != "diff_scope" || !gate.Passed {
		t.Errorf("DiffScope() gate = %+v, want passing diff_scope gate", gate)
	}
}

func TestDiffScopeViolationsFindsOutOfScopeFile(t *testing.T) {
	gate, got := DiffScope([]string{"a.go", "unexpected.go"}, []string{"a.go"})
	want := []string{"unexpected.go"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if gate.Passed {
		t.Errorf("DiffScope() Passed = true, want false")
	}
}

func TestDiffScopeViolationsEmptyChangedIsFine(t *testing.T) {
	gate, got := DiffScope(nil, []string{"a.go"})
	if len(got) != 0 {
		t.Errorf("got %v, want no violations for an empty changed-file list", got)
	}
	if !gate.Passed {
		t.Errorf("DiffScope() Passed = false, want true")
	}
}

// TestExcludeHarnessByproductsFiltersKnownArtifacts pins the fix for the
// real false-quarantine cause identified in the plan's 2026-08-28 Opus
// review: the harness's own housekeeping (a .gitignore line for its own
// artifacts, BUILD_REPORT.md, BUILD_EVIDENCE.json, .pi-build-session/)
// must never count against a ticket's declared scope, even when the
// ticket's Allowed-Files says nothing about them.
func TestExcludeHarnessByproductsFiltersKnownArtifacts(t *testing.T) {
	changed := []string{
		"a.go",
		".gitignore",
		"BUILD_REPORT.md",
		"BUILD_EVIDENCE.json",
		".pi-build-session/notes.txt",
		".pi-build-round-state.json",
		".pi-conformity-session/x.jsonl",
		".pi-code-review-session/y.jsonl",
		".pi-combined-review-session/z.jsonl",
	}
	got := ExcludeHarnessByproducts(changed)
	want := []string{"a.go"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestDiffScopeDoesNotExemptHarnessByproducts is the regression test for
// a real cross-package bug found by codex review (2026-08-28, round 1):
// an earlier version of this fix baked the harness-byproduct exemption
// directly into diffScopeViolations/DiffScope, which
// internal/release.MergePolicyCheck also calls — with the opposite
// semantics of a *protected*-path membership test ("passes precisely
// when the changed file is protected"). That made every changed
// .gitignore/BUILD_REPORT.md/etc. silently count as "protected"
// regardless of the operator's own configured ProtectedPaths, denying an
// otherwise-clean release for a reason it never actually configured.
// DiffScope itself must stay a neutral matcher; only EvaluateRun's
// ticket-scope call filters through ExcludeHarnessByproducts first.
func TestDiffScopeDoesNotExemptHarnessByproducts(t *testing.T) {
	gate, got := DiffScope([]string{".gitignore"}, nil)
	want := []string{".gitignore"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v: DiffScope must not special-case harness byproducts", got, want)
	}
	if gate.Passed {
		t.Errorf("DiffScope() Passed = true, want false")
	}
}

// TestDiffScopeViolationsMatchesDirectoryPrefixPattern pins the
// Opus-recommended fix for tool-generated files a ticket can't enumerate
// by name (e.g. Flutter's gen-l10n output): an Allowed-Files entry ending
// in "/" matches that directory and everything under it.
func TestDiffScopeViolationsMatchesDirectoryPrefixPattern(t *testing.T) {
	changed := []string{"app/lib/l10n/app_localizations_en.dart", "app/lib/l10n/app_localizations.dart"}
	gate, got := DiffScope(changed, []string{"app/lib/l10n/"})
	if len(got) != 0 {
		t.Errorf("got violations %v, want none: directory-prefix pattern should match", got)
	}
	if !gate.Passed {
		t.Errorf("DiffScope() Passed = false, want true")
	}
}

// TestDiffScopeViolationsMatchesGlobPattern pins the Opus-recommended fix
// for a known filename pattern within a single directory.
func TestDiffScopeViolationsMatchesGlobPattern(t *testing.T) {
	gate, got := DiffScope(
		[]string{"app/lib/l10n/app_localizations_en.dart"},
		[]string{"app/lib/l10n/app_localizations*.dart"},
	)
	if len(got) != 0 {
		t.Errorf("got violations %v, want none: glob pattern should match", got)
	}
	if !gate.Passed {
		t.Errorf("DiffScope() Passed = false, want true")
	}
}

// TestDiffScopeViolationsGlobDoesNotCrossDirectories pins that a glob
// pattern uses path.Match semantics (no "/" crossing) so it can't
// accidentally admit a file in an unrelated, deeper directory.
func TestDiffScopeViolationsGlobDoesNotCrossDirectories(t *testing.T) {
	gate, got := DiffScope(
		[]string{"app/lib/l10n/sub/app_localizations_en.dart"},
		[]string{"app/lib/l10n/app_localizations*.dart"},
	)
	want := []string{"app/lib/l10n/sub/app_localizations_en.dart"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v: a glob must not match across a directory boundary", got, want)
	}
	if gate.Passed {
		t.Errorf("DiffScope() Passed = true, want false")
	}
}

func TestRequiredFilesChangedAllPresent(t *testing.T) {
	gate, missing := RequiredFilesChanged([]string{"a.go", "a_test.go", "b.go"}, []string{"a.go", "a_test.go"})
	if len(missing) != 0 {
		t.Errorf("got %v, want no missing files", missing)
	}
	if gate.Check != "required_files_changed" || !gate.Passed {
		t.Errorf("RequiredFilesChanged() gate = %+v, want passing required_files_changed gate", gate)
	}
}

func TestRequiredFilesChangedFindsMissingFile(t *testing.T) {
	gate, missing := RequiredFilesChanged([]string{"a_test.go"}, []string{"a.go", "a_test.go"})
	want := []string{"a.go"}
	if !slices.Equal(missing, want) {
		t.Errorf("got %v, want %v", missing, want)
	}
	if gate.Passed {
		t.Errorf("RequiredFilesChanged() Passed = true, want false")
	}
}

func TestRequiredFilesChangedEmptyChangedFailsWhenFilesRequired(t *testing.T) {
	gate, missing := RequiredFilesChanged(nil, []string{"a.go"})
	want := []string{"a.go"}
	if !slices.Equal(missing, want) {
		t.Errorf("got %v, want %v", missing, want)
	}
	if gate.Passed {
		t.Errorf("RequiredFilesChanged() Passed = true, want false for an empty changed-file list with required files declared")
	}
}

func TestRequiredFilesPreDirtyNoneDirty(t *testing.T) {
	got := RequiredFilesPreDirty([]string{"unrelated.go"}, []string{"a.go", "a_test.go"})
	if len(got) != 0 {
		t.Errorf("got %v, want none dirty", got)
	}
}

func TestRequiredFilesPreDirtyFindsPreExistingDirt(t *testing.T) {
	got := RequiredFilesPreDirty([]string{"a.go", "unrelated.go"}, []string{"a.go", "a_test.go"})
	want := []string{"a.go"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestRequiredContentPresentAllNew(t *testing.T) {
	base := map[string]string{"a.dart": "class Widget {}\n"}
	final := map[string]string{"a.dart": "class Widget {\n  Key('quick-time-chips')\n}\n"}
	gate, missing := RequiredContentPresent(base, final, []string{"Key('quick-time-chips')"})
	if len(missing) != 0 {
		t.Errorf("got %v, want none missing", missing)
	}
	if gate.Check != "required_content_present" || !gate.Passed {
		t.Errorf("RequiredContentPresent() gate = %+v, want passing", gate)
	}
}

func TestRequiredContentPresentMissingFromFinal(t *testing.T) {
	base := map[string]string{"a.dart": "class Widget {}\n"}
	final := map[string]string{"a.dart": "class Widget {\n  // cosmetic change only\n}\n"}
	gate, missing := RequiredContentPresent(base, final, []string{"Key('quick-time-chips')"})
	want := []string{"Key('quick-time-chips')"}
	if !slices.Equal(missing, want) {
		t.Errorf("got %v, want %v", missing, want)
	}
	if gate.Passed {
		t.Error("RequiredContentPresent() Passed = true, want false")
	}
}

func TestRequiredContentPresentAlreadyInBaseDoesNotCount(t *testing.T) {
	base := map[string]string{"a.dart": "class Widget {\n  Key('quick-time-chips')\n}\n"}
	final := map[string]string{"a.dart": "class Widget {\n  Key('quick-time-chips')\n  // untouched otherwise\n}\n"}
	gate, missing := RequiredContentPresent(base, final, []string{"Key('quick-time-chips')"})
	want := []string{"Key('quick-time-chips')"}
	if !slices.Equal(missing, want) {
		t.Errorf("got %v, want %v — content already present at base isn't new evidence of this run's change", missing, want)
	}
	if gate.Passed {
		t.Error("RequiredContentPresent() Passed = true, want false")
	}
}

func TestRequiredContentPresentMatchesAcrossFiles(t *testing.T) {
	base := map[string]string{"a.dart": "", "a_test.dart": ""}
	final := map[string]string{"a.dart": "no marker here\n", "a_test.dart": "func TestQuickTimeChips() {}\n"}
	gate, missing := RequiredContentPresent(base, final, []string{"func TestQuickTimeChips("})
	if len(missing) != 0 {
		t.Errorf("got %v, want none missing — the string only needs to appear in one of the required files", missing)
	}
	if !gate.Passed {
		t.Error("RequiredContentPresent() Passed = false, want true")
	}
}

// TestRequiredContentPresentDoesNotMaskAcrossFiles is the regression test
// for a real finding from review: the marker is newly added to file B,
// but that same marker already existed in file A at base — concatenating
// every file into one blob before matching let A's pre-existing content
// mask B's genuinely new addition. Checked per-file, A's pre-existing
// marker must not affect B's independent pass.
func TestRequiredContentPresentDoesNotMaskAcrossFiles(t *testing.T) {
	base := map[string]string{"a.dart": "Key('already-here')\n", "b.dart": ""}
	final := map[string]string{"a.dart": "Key('already-here')\n", "b.dart": "Key('already-here')\n"}
	gate, missing := RequiredContentPresent(base, final, []string{"Key('already-here')"})
	if len(missing) != 0 {
		t.Errorf("got %v, want none missing — b.dart newly added the marker, independent of a.dart already having it", missing)
	}
	if !gate.Passed {
		t.Error("RequiredContentPresent() Passed = false, want true")
	}
}

// TestRequiredContentPresentDoesNotFabricateSeamMatch is the regression
// test for the other half of the same finding: concatenating file
// content can fabricate a match at the seam between two files that
// exists in neither file individually. "abc"+"def" naively concatenates
// to "abcdef", which contains "cd" — a substring split across the seam —
// even though no single file's content does.
func TestRequiredContentPresentDoesNotFabricateSeamMatch(t *testing.T) {
	base := map[string]string{"a.dart": "", "b.dart": ""}
	final := map[string]string{"a.dart": "xxxabc", "b.dart": "defxxx"}
	gate, missing := RequiredContentPresent(base, final, []string{"abcdef"})
	want := []string{"abcdef"}
	if !slices.Equal(missing, want) {
		t.Errorf("got %v, want %v — no single file actually contains the marker", missing, want)
	}
	if gate.Passed {
		t.Error("RequiredContentPresent() Passed = true, want false — a seam artifact must not count")
	}
}

func TestRequiredFilesPreDirtyEmptyInitialIsFine(t *testing.T) {
	got := RequiredFilesPreDirty(nil, []string{"a.go"})
	if len(got) != 0 {
		t.Errorf("got %v, want none dirty for a clean starting workspace", got)
	}
}

func TestEvaluateRunAcceptsWhenOnlyCanonicalVerifyDeclared(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "make verify"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		// tests_added always runs -- opted out here since this
		// fixture isn't exercising it, matching every other test in this
		// file that declares no ChangedFiles.
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if !result.Accepted {
		t.Errorf("Accepted = false, want true; FailedChecks = %v", result.FailedChecks)
	}
	wantChecks := []string{"canonical_verify", "tests_added"}
	if len(result.GateResults) != len(wantChecks) {
		t.Errorf("GateResults = %+v, want exactly canonical_verify and tests_added (no other keys declared)", result.GateResults)
	}
}

func TestEvaluateRunRejectsOnFailingCanonicalVerify(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:       []string{"sh", "-c", "make verify"},
		BuildExitCode:       0,
		VerifyExitCode:      1,
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false")
	}
	if !slices.Equal(result.FailedChecks, []string{"canonical_verify"}) {
		t.Errorf("FailedChecks = %v, want [canonical_verify]", result.FailedChecks)
	}
}

func TestEvaluateRunRunsAllDeclaredGatesAndAggregatesFailures(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:             []string{"sh", "-c", "true"},
		BuildExitCode:             0,
		VerifyExitCode:            0,
		ChangedFiles:              []string{"a.go", "out-of-scope.txt"},
		AllowedFiles:              []string{"a.go"},
		RequiredChangedFiles:      []string{"a.go", "b.go"},
		RequiredContent:           []string{"marker"},
		RequiredContentBaseFiles:  map[string]string{"a.go": "", "b.go": ""},
		RequiredContentFinalFiles: map[string]string{"a.go": "cosmetic change only", "b.go": ""},
		TestsRequiredOptOut:       "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false")
	}
	wantFailed := []string{"diff_scope", "required_files_changed", "required_content_present"}
	if !slices.Equal(result.FailedChecks, wantFailed) {
		t.Errorf("FailedChecks = %v, want %v", result.FailedChecks, wantFailed)
	}
	if !slices.Equal(result.DiffScopeViolations, []string{"out-of-scope.txt"}) {
		t.Errorf("DiffScopeViolations = %v, want [out-of-scope.txt]", result.DiffScopeViolations)
	}
	if !slices.Equal(result.MissingRequiredFiles, []string{"b.go"}) {
		t.Errorf("MissingRequiredFiles = %v, want [b.go]", result.MissingRequiredFiles)
	}
	if !slices.Equal(result.MissingRequiredContent, []string{"marker"}) {
		t.Errorf("MissingRequiredContent = %v, want [marker]", result.MissingRequiredContent)
	}
	if len(result.GateResults) != 5 {
		t.Fatalf("GateResults = %+v, want 5 (one per declared check, plus tests_added which always runs)", result.GateResults)
	}
	wantChecks := []string{"canonical_verify", "diff_scope", "required_files_changed", "required_content_present", "tests_added"}
	for i, want := range wantChecks {
		if result.GateResults[i].Check != want {
			t.Errorf("GateResults[%d].Check = %q, want %q", i, result.GateResults[i].Check, want)
		}
	}
}

func TestEvaluateRunAcceptsWhenAllDeclaredGatesPass(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:             []string{"sh", "-c", "true"},
		BuildExitCode:             0,
		VerifyExitCode:            0,
		ChangedFiles:              []string{"a.go", "a_test.go"},
		AllowedFiles:              []string{"a.go", "a_test.go"},
		RequiredChangedFiles:      []string{"a.go"},
		RequiredContent:           []string{"marker"},
		RequiredContentBaseFiles:  map[string]string{"a.go": ""},
		RequiredContentFinalFiles: map[string]string{"a.go": "marker"},
		FullSuiteCommand:          []string{"sh", "-c", "true"},
		FullSuiteExitCode:         0,
	})
	if !result.Accepted {
		t.Errorf("Accepted = false, want true; FailedChecks = %v", result.FailedChecks)
	}
	if len(result.FailedChecks) != 0 {
		t.Errorf("FailedChecks = %v, want none", result.FailedChecks)
	}
}

// TestEvaluateRunExemptsHarnessByproductsFromDiffScope is the end-to-end
// version of TestExcludeHarnessByproductsFiltersKnownArtifacts: a real
// ticket-scope evaluation must accept a run whose only "out of scope"
// changes are the harness's own byproducts, without the ticket having to
// declare them in Allowed-Files.
func TestEvaluateRunExemptsHarnessByproductsFromDiffScope(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:       []string{"sh", "-c", "true"},
		BuildExitCode:       0,
		VerifyExitCode:      0,
		ChangedFiles:        []string{"a.go", ".gitignore", "BUILD_REPORT.md"},
		AllowedFiles:        []string{"a.go"},
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if !result.Accepted {
		t.Errorf("Accepted = false, want true; FailedChecks = %v, DiffScopeViolations = %v", result.FailedChecks, result.DiffScopeViolations)
	}
}

func TestTestsAddedPassesWithADefaultPatternMatch(t *testing.T) {
	got := TestsAdded([]string{"internal/foo/bar.go", "internal/foo/bar_test.go"}, nil, "")
	if !got.Passed {
		t.Errorf("TestsAdded() Passed = false, want true when a *_test.go file changed")
	}
	if got.Check != "tests_added" {
		t.Errorf("Check = %q, want tests_added", got.Check)
	}
}

func TestTestsAddedFailsWithNoTestFileChanged(t *testing.T) {
	got := TestsAdded([]string{"internal/foo/bar.go"}, nil, "")
	if got.Passed {
		t.Error("TestsAdded() Passed = true, want false when no changed file matches a test pattern")
	}
}

func TestTestsAddedHonorsCustomPatterns(t *testing.T) {
	got := TestsAdded([]string{"e2e/checkout.feature"}, []string{"*.feature"}, "")
	if !got.Passed {
		t.Error("TestsAdded() Passed = false, want true against a custom test_patterns match")
	}
	// The same changed file does not satisfy any default pattern, proving
	// the custom pattern -- not a default -- is what matched.
	if got2 := TestsAdded([]string{"e2e/checkout.feature"}, nil, ""); got2.Passed {
		t.Error("TestsAdded() with default patterns unexpectedly passed against a non-test-convention file")
	}
}

// TestTestsAddedDefaultPatternsCoverMultipleLanguages pins
// DefaultTestPatterns' own multi-language coverage (found live: a Python
// repo's tests/test_product_lab.py change failed tests_added outright
// against the old Go-only *_test.go default) -- one representative file
// per covered ecosystem.
func TestTestsAddedDefaultPatternsCoverMultipleLanguages(t *testing.T) {
	for _, changed := range []string{
		"internal/foo/bar_test.go",
		"tests/test_product_lab.py",
		"tests/product_lab_test.py",
		"src/app.test.js",
		"src/app.test.ts",
		"src/App.test.tsx",
		"src/app.spec.js",
		"src/app.spec.ts",
		"spec/widget_spec.rb",
		"src/com/acme/WidgetTest.java",
		"WidgetTests.cs",
		"src/widget_test.rs",
	} {
		t.Run(changed, func(t *testing.T) {
			got := TestsAdded([]string{changed}, nil, "")
			if !got.Passed {
				t.Errorf("TestsAdded([]string{%q}, nil, \"\") Passed = false, want true", changed)
			}
		})
	}
}

func TestTestsAddedCustomPatternMatchesFullPathNotJustBasename(t *testing.T) {
	got := TestsAdded([]string{"test/integration/foo.go"}, []string{"test/*/*.go"}, "")
	if !got.Passed {
		t.Error("TestsAdded() Passed = false, want true when the full path matches a custom pattern")
	}
}

func TestTestsAddedOptOutPassesRegardlessOfChangedFiles(t *testing.T) {
	got := TestsAdded([]string{"docs/README.md"}, nil, "docs-only change")
	if !got.Passed {
		t.Error("TestsAdded() Passed = false, want true when the ticket declared an opt-out reason")
	}
}

// TestTicketTestsAddedFeasibleCatchesTheLiveCase pins the exact shape found
// live (a Flutter + Go app repo run 3, 2026-09-28): a ticket's Allowed-Files/
// Required-Changed-Files named only one concrete, non-test Go file, no
// Tests-Required opt-out declared -- structurally unable to ever pass
// tests_added, yet the plan reached plan_review and a build ran before
// quarantining on it.
func TestTicketTestsAddedFeasibleCatchesTheLiveCase(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"backend/internal/handler/dispatch_mux.go"}, nil, "")
	if got {
		t.Error("TicketTestsAddedFeasible() = true, want false for Allowed-Files containing only a concrete non-test file with no opt-out")
	}
}

func TestTicketTestsAddedFeasibleTrueWithATestFileInAllowedFiles(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"internal/foo/bar.go", "internal/foo/bar_test.go"}, nil, "")
	if !got {
		t.Error("TicketTestsAddedFeasible() = false, want true when Allowed-Files includes a file matching a test pattern")
	}
}

func TestTicketTestsAddedFeasibleTrueWithOptOutReason(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"backend/internal/handler/dispatch_mux.go"}, nil, "route exercised by ticket 002's dispatch test")
	if !got {
		t.Error("TicketTestsAddedFeasible() = false, want true when the ticket declares a Tests-Required opt-out reason")
	}
}

func TestTicketTestsAddedFeasibleTrueWithDirectoryAllowedFilesEntry(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"backend/internal/handler/"}, nil, "")
	if !got {
		t.Error("TicketTestsAddedFeasible() = false, want true when Allowed-Files declares a whole directory that could contain a test file")
	}
}

func TestTicketTestsAddedFeasibleTrueWithGlobAllowedFilesEntry(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"backend/internal/handler/*.go"}, nil, "")
	if !got {
		t.Error("TicketTestsAddedFeasible() = false, want true when Allowed-Files declares a glob that could match a test file")
	}
}

func TestTicketTestsAddedFeasibleFalseWithMultipleConcreteNonTestFiles(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"backend/internal/handler/dispatch_mux.go", "backend/internal/handler/router.go"}, nil, "")
	if got {
		t.Error("TicketTestsAddedFeasible() = true, want false when every Allowed-Files entry is a concrete non-test file")
	}
}

func TestTicketTestsAddedFeasibleHonorsCustomPatterns(t *testing.T) {
	got := TicketTestsAddedFeasible([]string{"e2e/checkout.feature"}, []string{"*.feature"}, "")
	if !got {
		t.Error("TicketTestsAddedFeasible() = false, want true against a custom test_patterns match")
	}
}

func TestTicketTestsAddedFeasibleTrueWithEmptyAllowedFiles(t *testing.T) {
	// writeAndValidateDraftedTickets rejects a ticket with no Allowed-Files
	// at all as its own, separate "missing Allowed-Files" error -- this
	// function must not also flag it, or the two errors would race for
	// which one a caller reports first.
	got := TicketTestsAddedFeasible(nil, nil, "")
	if !got {
		t.Error("TicketTestsAddedFeasible() = false, want true for an empty Allowed-Files list")
	}
}

func TestEvaluateRunTestsAddedGateFailsAcceptedRun(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		ChangedFiles:   []string{"a.go"},
	})
	if result.Accepted {
		t.Error("Accepted = true, want false when tests_added has no matching changed file and no opt-out")
	}
	if !slices.Equal(result.FailedChecks, []string{"tests_added"}) {
		t.Errorf("FailedChecks = %v, want [tests_added]", result.FailedChecks)
	}
}

func TestEvaluateRunTestsAddedGatePassesWithMatchingTestFile(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		ChangedFiles:   []string{"a.go", "a_test.go"},
	})
	if !result.Accepted {
		t.Errorf("Accepted = false, want true; FailedChecks = %v", result.FailedChecks)
	}
}

func TestEvaluateRunTestsAddedGatePassesViaOptOutEvenWithoutRunningPatterns(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:       []string{"sh", "-c", "true"},
		BuildExitCode:       0,
		VerifyExitCode:      0,
		ChangedFiles:        []string{"docs/README.md"},
		TestsRequiredOptOut: "docs-only change",
	})
	if !result.Accepted {
		t.Errorf("Accepted = false, want true; FailedChecks = %v", result.FailedChecks)
	}
	for _, g := range result.GateResults {
		if g.Check == "tests_added" && !g.Passed {
			t.Error("tests_added gate should have passed via the declared opt-out")
		}
	}
}

func TestFullSuiteVerifyPassesOnZeroExit(t *testing.T) {
	cmd := []string{"sh", "-c", "make verify-full"}
	got := FullSuiteVerify(cmd, 0, 5678, "def456")

	if got.Check != "full_suite_verify" || !slices.Equal(got.Command, cmd) || !got.Passed || got.ExitCode != 0 || got.DurationMs != 5678 || got.LogSHA256 != "def456" {
		t.Errorf("FullSuiteVerify() = %+v, want passing gate with supplied evidence", got)
	}
}

func TestFullSuiteVerifyFailsOnNonzeroExit(t *testing.T) {
	got := FullSuiteVerify([]string{"make", "verify-full"}, 1, 10, "def456")

	if got.Passed {
		t.Error("FullSuiteVerify() Passed = true, want false when the full suite fails")
	}
}

func TestEvaluateRunSkipsFullSuiteVerifyWhenNotDeclared(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
	})
	for _, g := range result.GateResults {
		if g.Check == "full_suite_verify" {
			t.Errorf("GateResults = %+v, want no full_suite_verify gate when FullSuiteCommand is nil", result.GateResults)
		}
	}
}

// TestEvaluateRunQuarantinesOnFullSuiteRegression is the case gap 3 of the
// plan's 2026-08-28 readiness review named: a slice's own targeted
// Verify-Command passes, but it silently broke another, already-accepted
// slice's test. full_suite_verify is the only gate that can catch this,
// and its failure must still quarantine the run even though every
// per-ticket gate passed.
func TestEvaluateRunQuarantinesOnFullSuiteRegression(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:       []string{"sh", "-c", "true"},
		BuildExitCode:       0,
		VerifyExitCode:      0,
		ChangedFiles:        []string{"a.go"},
		AllowedFiles:        []string{"a.go"},
		FullSuiteCommand:    []string{"sh", "-c", "make verify-full"},
		FullSuiteExitCode:   1,
		FullSuiteDurationMs: 42,
		FullSuiteLogSHA256:  "def456",
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false when full_suite_verify fails")
	}
	if !slices.Equal(result.FailedChecks, []string{"full_suite_verify"}) {
		t.Errorf("FailedChecks = %v, want [full_suite_verify]", result.FailedChecks)
	}
	wantChecks := []string{"canonical_verify", "diff_scope", "tests_added", "full_suite_verify"}
	if len(result.GateResults) != len(wantChecks) {
		t.Fatalf("GateResults = %+v, want %d gates", result.GateResults, len(wantChecks))
	}
	for i, want := range wantChecks {
		if result.GateResults[i].Check != want {
			t.Errorf("GateResults[%d].Check = %q, want %q", i, result.GateResults[i].Check, want)
		}
	}
}

func TestSpecConformityPassesOnZeroExit(t *testing.T) {
	cmd := []string{"python3", "conformity_review.py"}
	got := SpecConformity(cmd, 0, 1234, "abc123")

	if got.Check != "spec_conformity" || !slices.Equal(got.Command, cmd) || !got.Passed || got.ExitCode != 0 || got.DurationMs != 1234 || got.LogSHA256 != "abc123" {
		t.Errorf("SpecConformity() = %+v, want passing gate with supplied evidence", got)
	}
}

func TestSpecConformityFailsOnNonzeroExit(t *testing.T) {
	got := SpecConformity([]string{"python3", "conformity_review.py"}, 1, 10, "abc123")

	if got.Passed {
		t.Error("SpecConformity() Passed = true, want false on a nonzero exit")
	}
}

func TestEvaluateRunSkipsSpecConformityWhenNotDeclared(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
	})
	for _, g := range result.GateResults {
		if g.Check == "spec_conformity" {
			t.Errorf("GateResults = %+v, want no spec_conformity gate when SpecConformityCommand is nil", result.GateResults)
		}
	}
}

// TestEvaluateRunQuarantinesOnSpecConformityFailure covers the two-phase
// design's own reason for existing: canonical_verify (and every other
// gate) can pass cleanly while the separate, ticket-declared
// spec-conformity review still fails -- that must still quarantine the
// run.
func TestEvaluateRunQuarantinesOnSpecConformityFailure(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:            []string{"sh", "-c", "true"},
		BuildExitCode:            0,
		VerifyExitCode:           0,
		ChangedFiles:             []string{"a.go"},
		AllowedFiles:             []string{"a.go"},
		SpecConformityCommand:    []string{"python3", "conformity_review.py"},
		SpecConformityExitCode:   1,
		SpecConformityDurationMs: 42,
		SpecConformityLogSHA256:  "abc123",
		TestsRequiredOptOut:      "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false when spec_conformity fails")
	}
	if !slices.Equal(result.FailedChecks, []string{"spec_conformity"}) {
		t.Errorf("FailedChecks = %v, want [spec_conformity]", result.FailedChecks)
	}
	wantChecks := []string{"canonical_verify", "diff_scope", "tests_added", "spec_conformity"}
	if len(result.GateResults) != len(wantChecks) {
		t.Fatalf("GateResults = %+v, want %d gates", result.GateResults, len(wantChecks))
	}
	for i, want := range wantChecks {
		if result.GateResults[i].Check != want {
			t.Errorf("GateResults[%d].Check = %q, want %q", i, result.GateResults[i].Check, want)
		}
	}
}

func TestCodeReviewPassesOnZeroExit(t *testing.T) {
	cmd := []string{"python3", "code_review.py"}
	got := CodeReview(cmd, 0, 1234, "abc123")

	if got.Check != "code_review" || !slices.Equal(got.Command, cmd) || !got.Passed || got.ExitCode != 0 || got.DurationMs != 1234 || got.LogSHA256 != "abc123" {
		t.Errorf("CodeReview() = %+v, want passing gate with supplied evidence", got)
	}
}

func TestCodeReviewFailsOnNonzeroExit(t *testing.T) {
	got := CodeReview([]string{"python3", "code_review.py"}, 1, 10, "abc123")

	if got.Passed {
		t.Error("CodeReview() Passed = true, want false on a nonzero exit")
	}
}

func TestEvaluateRunSkipsCodeReviewWhenNotDeclared(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
	})
	for _, g := range result.GateResults {
		if g.Check == "code_review" {
			t.Errorf("GateResults = %+v, want no code_review gate when CodeReviewCommand is nil", result.GateResults)
		}
	}
}

// TestEvaluateRunQuarantinesOnCodeReviewFailure mirrors
// TestEvaluateRunQuarantinesOnSpecConformityFailure for the standalone
// code-review gate: every other gate can pass cleanly while the
// separate code-review pass still fails (a blocking high-severity
// finding under -code-review-policy=required) -- that must still
// quarantine the run.
func TestEvaluateRunQuarantinesOnCodeReviewFailure(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:        []string{"sh", "-c", "true"},
		BuildExitCode:        0,
		VerifyExitCode:       0,
		ChangedFiles:         []string{"a.go"},
		AllowedFiles:         []string{"a.go"},
		CodeReviewCommand:    []string{"python3", "code_review.py"},
		CodeReviewExitCode:   1,
		CodeReviewDurationMs: 42,
		CodeReviewLogSHA256:  "abc123",
		TestsRequiredOptOut:  "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false when code_review fails")
	}
	if !slices.Equal(result.FailedChecks, []string{"code_review"}) {
		t.Errorf("FailedChecks = %v, want [code_review]", result.FailedChecks)
	}
	wantChecks := []string{"canonical_verify", "diff_scope", "tests_added", "code_review"}
	if len(result.GateResults) != len(wantChecks) {
		t.Fatalf("GateResults = %+v, want %d gates", result.GateResults, len(wantChecks))
	}
	for i, want := range wantChecks {
		if result.GateResults[i].Check != want {
			t.Errorf("GateResults[%d].Check = %q, want %q", i, result.GateResults[i].Check, want)
		}
	}
}

// TestEvaluateRunSkipsUnconfiguredNamedGates is the named gates' version
// of TestEvaluateRunSkipsFullSuiteVerifyWhenNotDeclared: a gate absent from
// NamedGates never appears in GateResults at all -- not present-and-
// passed, since "not configured" is a distinct state the PR body must be
// able to render.
func TestEvaluateRunSkipsUnconfiguredNamedGates(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
	})
	for _, g := range result.GateResults {
		if g.Check == "lint" || g.Check == "security_audit" || g.Check == "unit_tests" || g.Check == "integration_tests" {
			t.Errorf("GateResults = %+v, want no named gates when NamedGates is empty", result.GateResults)
		}
	}
}

// TestEvaluateRunQuarantinesOnFailingSecurityAudit is the named gates'
// "Done when" case: a failing security_command quarantines the run
// with "security_audit" as the cause, the same way full_suite_verify's
// own failure does.
func TestEvaluateRunQuarantinesOnFailingSecurityAudit(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		NamedGates: []NamedGateInput{
			{Check: "lint", Command: []string{"sh", "-c", "golangci-lint run ./..."}, ExitCode: 0, DurationMs: 10, LogSHA256: "abc"},
			{Check: "security_audit", Command: []string{"sh", "-c", "govulncheck ./..."}, ExitCode: 1, DurationMs: 20, LogSHA256: "def"},
		},
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false when security_audit fails")
	}
	if !slices.Equal(result.FailedChecks, []string{"security_audit"}) {
		t.Errorf("FailedChecks = %v, want [security_audit]", result.FailedChecks)
	}
	wantChecks := []string{"canonical_verify", "tests_added", "lint", "security_audit"}
	if len(result.GateResults) != len(wantChecks) {
		t.Fatalf("GateResults = %+v, want %d gates", result.GateResults, len(wantChecks))
	}
	for i, want := range wantChecks {
		if result.GateResults[i].Check != want {
			t.Errorf("GateResults[%d].Check = %q, want %q", i, result.GateResults[i].Check, want)
		}
	}
	if !result.GateResults[2].Passed {
		t.Errorf("lint gate should have passed: %+v", result.GateResults[2])
	}
	if result.GateResults[3].Passed {
		t.Errorf("security_audit gate should have failed: %+v", result.GateResults[3])
	}
}

// TestEvaluateRunQuarantinesOnFailingReferenceOracle is the
// reference_oracle counterpart to
// TestEvaluateRunQuarantinesOnFailingSecurityAudit, added alongside the
// gate itself rather than relying on the generic NamedGate mechanism's
// other coverage to stand in for it: a failing reference-oracle command
// must quarantine the run the same way any other failing named gate does, and this pins that
// specifically by name rather than only by analogy. Confirmed live
// (2026-09-15): three real sandboxed builds against a real local model
// all passed this gate legitimately; this test is the fast, deterministic
// regression guard for the failure path those live runs didn't happen to
// exercise.
func TestEvaluateRunQuarantinesOnFailingReferenceOracle(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		NamedGates: []NamedGateInput{
			{Check: "reference_oracle", Command: []string{"sh", "-c", "go test ./verify/..."}, ExitCode: 1, DurationMs: 15, LogSHA256: "ghi"},
		},
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	if result.Accepted {
		t.Error("Accepted = true, want false when reference_oracle fails")
	}
	if !slices.Equal(result.FailedChecks, []string{"reference_oracle"}) {
		t.Errorf("FailedChecks = %v, want [reference_oracle]", result.FailedChecks)
	}
	wantChecks := []string{"canonical_verify", "tests_added", "reference_oracle"}
	if len(result.GateResults) != len(wantChecks) {
		t.Fatalf("GateResults = %+v, want %d gates", result.GateResults, len(wantChecks))
	}
	for i, want := range wantChecks {
		if result.GateResults[i].Check != want {
			t.Errorf("GateResults[%d].Check = %q, want %q", i, result.GateResults[i].Check, want)
		}
	}
	if result.GateResults[2].Passed {
		t.Errorf("reference_oracle gate should have failed: %+v", result.GateResults[2])
	}
}

// TestEvaluateRunCarriesReferenceOracleSHA256Through is the regression
// test for the SC-012 follow-up (PR #151 review, round 2): the oracle
// content hash cmd/factoryd computes must survive EvaluateRun/NamedGate
// unchanged into the persisted GateResult, the same way LogSHA256 already
// does -- policy.NamedGate copies it through verbatim rather than
// recomputing or dropping it.
func TestEvaluateRunCarriesReferenceOracleSHA256Through(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		NamedGates: []NamedGateInput{
			{Check: "reference_oracle", Command: []string{"sh", "-c", "go test ./verify/..."}, ExitCode: 0, DurationMs: 15, LogSHA256: "ghi", ReferenceOracleSHA256: "deadbeef"},
		},
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	var found bool
	for _, g := range result.GateResults {
		if g.Check != "reference_oracle" {
			continue
		}
		found = true
		if g.ReferenceOracleSHA256 != "deadbeef" {
			t.Errorf("ReferenceOracleSHA256 = %q, want %q to survive unchanged", g.ReferenceOracleSHA256, "deadbeef")
		}
	}
	if !found {
		t.Fatal("no reference_oracle GateResult found")
	}
}

// TestEvaluateRunLeavesReferenceOracleSHA256EmptyForOtherGates confirms
// the field stays empty for every gate that isn't reference_oracle, so a
// reviewer reading GateResults never mistakes an unrelated gate's zero
// value for "no oracle content was hashed" vs. "this gate isn't the
// oracle gate at all".
func TestEvaluateRunLeavesReferenceOracleSHA256EmptyForOtherGates(t *testing.T) {
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand:  []string{"sh", "-c", "true"},
		BuildExitCode:  0,
		VerifyExitCode: 0,
		NamedGates: []NamedGateInput{
			{Check: "lint", Command: []string{"sh", "-c", "golangci-lint run ./..."}, ExitCode: 0, DurationMs: 10, LogSHA256: "abc"},
		},
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	})
	for _, g := range result.GateResults {
		if g.Check == "lint" && g.ReferenceOracleSHA256 != "" {
			t.Errorf("lint gate's ReferenceOracleSHA256 = %q, want empty", g.ReferenceOracleSHA256)
		}
	}
}

func TestProductSpecFrozenPassesFrozenStatus(t *testing.T) {
	passed, reason := ProductSpecFrozen("STATUS: FROZEN -- reviewed 2026-08-26T12:00:00Z\n\n# Spec\n")
	if !passed || reason != "" {
		t.Errorf("ProductSpecFrozen() = %v, %q, want true with no reason", passed, reason)
	}
}

func TestProductSpecFrozenRejectsDraftStatus(t *testing.T) {
	passed, reason := ProductSpecFrozen("STATUS: DRAFT -- pending human review\n\n# Spec\n")
	if passed || !strings.Contains(reason, "DRAFT") {
		t.Errorf("ProductSpecFrozen() = %v, %q, want a DRAFT reason", passed, reason)
	}
}

func TestProductSpecFrozenRejectsMissingStatus(t *testing.T) {
	passed, reason := ProductSpecFrozen("# Spec\n")
	if passed || !strings.Contains(reason, "no STATUS") {
		t.Errorf("ProductSpecFrozen() = %v, %q, want a missing STATUS reason", passed, reason)
	}
}

func TestTicketStructureAcceptsWellFormedTicket(t *testing.T) {
	passed, reasons := TicketStructure(wellFormedTicket, 2)
	if !passed || len(reasons) != 0 {
		t.Errorf("TicketStructure() = %v, %v, want true with no reasons", passed, reasons)
	}
}

func TestTicketStructureRejectsMissingGoal(t *testing.T) {
	assertTicketReason(t, strings.Replace(wellFormedTicket, "## Goal", "Goal", 1), 2, "## Goal")
}

func TestTicketStructureRejectsMissingRequiredChanges(t *testing.T) {
	assertTicketReason(t, strings.Replace(wellFormedTicket, "## Required changes", "Required changes", 1), 2, "## Required changes")
}

func TestTicketStructureRejectsMissingStateFilesInstruction(t *testing.T) {
	ticket := strings.Replace(wellFormedTicket, "Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.", "Update the state files before finishing.", 1)
	assertTicketReason(t, ticket, 2, "ARCHITECTURE.md/PROGRESS.md")
}

func TestTicketStructureRejectsStateFilesInstructionOutsideRequiredChanges(t *testing.T) {
	const instruction = "Update `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing."
	ticket := strings.Replace(wellFormedTicket, instruction+"\n\n## Verification", "## Verification\n\n"+instruction, 1)
	assertTicketReason(t, ticket, 2, "ARCHITECTURE.md/PROGRESS.md")
}

func TestTicketStructureRejectsMissingVerification(t *testing.T) {
	assertTicketReason(t, strings.Replace(wellFormedTicket, "## Verification", "Verification", 1), 2, "## Verification")
}

func TestTicketStructureRejectsMissingMakeVerify(t *testing.T) {
	assertTicketReason(t, strings.Replace(wellFormedTicket, "`make verify`", "`make check`", 1), 2, "make verify")
}

func TestTicketStructureRejectsMissingCommit(t *testing.T) {
	assertTicketReason(t, strings.Replace(wellFormedTicket, "## Commit", "Commit", 1), 2, "## Commit")
}

func TestTicketStructureRequiresContextAfterTicketOne(t *testing.T) {
	ticket := strings.Replace(wellFormedTicket, "This is an existing repo.", "", 1)
	passed, reasons := TicketStructure(ticket, 1)
	if !passed || len(reasons) != 0 {
		t.Errorf("TicketStructure(ticket 1) = %v, %v, want true with no context line", passed, reasons)
	}
	assertTicketReason(t, ticket, 2, "This is an existing repo.")
}

func TestTicketStructureRejectsContextAfterGoal(t *testing.T) {
	ticket := strings.Replace(wellFormedTicket, "This is an existing repo.", "", 1)
	ticket = strings.Replace(ticket, "Add the first useful product slice.", "Add the first useful product slice. This is an existing repo.", 1)
	assertTicketReason(t, ticket, 2, "pre-Goal context line")
}

// TestTicketStructureRejectsFencedExampleOnly is the ticket-scope regression
// for the fenced-code exclusion ArchitectureStructure/ProgramDesignStructure
// already had: a ticket whose only well-formed structure lives inside a
// fenced example must not pass on the example's content.
func TestTicketStructureRejectsFencedExampleOnly(t *testing.T) {
	ticket := "This is an existing repo.\n\nHere is the template ticket format:\n\n```markdown\n" + wellFormedTicket + "\n```\n"
	passed, reasons := TicketStructure(ticket, 2)
	if passed {
		t.Fatalf("TicketStructure(fenced example) Passed = true, want false; reasons = %v", reasons)
	}
}

// TestTicketStructureAcceptsFencedVerificationCommand is the regression for
// a codex finding on the fenced-example fix above: fencing the actual
// verification command under a real (non-fenced) ## Verification heading —
// ordinary, common ticket-writing style — must still pass, not be treated
// as if it were a fake fenced template.
func TestTicketStructureAcceptsFencedVerificationCommand(t *testing.T) {
	ticket := strings.Replace(wellFormedTicket,
		"`make verify` must pass. Confirm `make verify-full` still passes.",
		"Run:\n\n```sh\nmake verify\n```\n",
		1)
	passed, reasons := TicketStructure(ticket, 2)
	if !passed {
		t.Fatalf("TicketStructure(fenced verification command) = false, %v; want true", reasons)
	}
}

func TestTicketStructureRejectsHeadingsOutOfOrder(t *testing.T) {
	ticket := strings.Replace(wellFormedTicket, "## Goal", "## temporary", 1)
	ticket = strings.Replace(ticket, "## Required changes", "## Goal", 1)
	ticket = strings.Replace(ticket, "## temporary", "## Required changes", 1)
	assertTicketReason(t, ticket, 2, "required order")
}

func TestTicketStructureReportsEveryFailure(t *testing.T) {
	passed, reasons := TicketStructure("", 2)
	if passed {
		t.Fatal("TicketStructure() Passed = true, want false")
	}
	for _, want := range []string{
		"## Goal",
		"## Required changes",
		"ARCHITECTURE.md/PROGRESS.md",
		"## Verification",
		"make verify",
		"## Commit",
		"This is an existing repo.",
	} {
		if !containsReason(reasons, want) {
			t.Errorf("TicketStructure() reasons = %v, want one naming %q", reasons, want)
		}
	}
}

const wellFormedArchitecture = `# Calculator Pilot — Architecture

## Repo layout

- backend/ contains the API.
- app/ contains the Flutter client.

## Verification

- make verify runs the checks.

## Known deviations

- Integration tests are not present yet.
`

const wellFormedProgramDesign = `# Calculator Pilot — API contract

## 1. Conventions

- JSON fields use snake_case.

## 2. Endpoint: add

The endpoint accepts two numbers and returns their sum.
`

const wellFormedBudgetProgramDesign = `# Budget Pilot — API contract

## Error envelope

Every error has a stable code and message.

## Data shapes

The contract defines the account shape.

## Endpoints

### POST /api/account

Creates the account.
`

func TestArchitectureStructureAcceptsPiArtifact(t *testing.T) {
	passed, reasons := ArchitectureStructure(wellFormedArchitecture, nil)
	if !passed || len(reasons) != 0 {
		t.Errorf("ArchitectureStructure() = %v, %v, want true with no reasons", passed, reasons)
	}
}

func TestArchitectureStructureRejectsMissingSections(t *testing.T) {
	for _, section := range []string{"Repo layout", "Verification", "Known deviations"} {
		t.Run(section, func(t *testing.T) {
			artifact := strings.Replace(wellFormedArchitecture, "## "+section, "# "+section, 1)
			passed, reasons := ArchitectureStructure(artifact, nil)
			if passed || !containsReason(reasons, section) {
				t.Errorf("ArchitectureStructure() = %v, %v, want a reason naming %q", passed, reasons, section)
			}
		})
	}
}

func TestArchitectureStructureRejectsSectionsOutOfOrder(t *testing.T) {
	artifact := strings.Replace(wellFormedArchitecture, "## Repo layout", "## temporary", 1)
	artifact = strings.Replace(artifact, "## Verification", "## Repo layout", 1)
	artifact = strings.Replace(artifact, "## temporary", "## Verification", 1)
	passed, reasons := ArchitectureStructure(artifact, nil)
	if passed || !containsReason(reasons, "required order") {
		t.Errorf("ArchitectureStructure() = %v, %v, want an ordering reason", passed, reasons)
	}
}

func TestArchitectureStructureReportsEveryMissingSection(t *testing.T) {
	passed, reasons := ArchitectureStructure("", nil)
	if passed {
		t.Fatal("ArchitectureStructure() Passed = true, want false")
	}
	for _, want := range []string{"Repo layout", "Verification", "Known deviations"} {
		if !containsReason(reasons, want) {
			t.Errorf("ArchitectureStructure() reasons = %v, want one naming %q", reasons, want)
		}
	}
}

func TestArchitectureStructureIgnoresFencedAndIndentedExamples(t *testing.T) {
	artifact := "# Architecture\n\n```markdown\n## Repo layout\n## Verification\n## Known deviations\n```\n\n    ## Repo layout\n    ## Verification\n    ## Known deviations\n"
	passed, reasons := ArchitectureStructure(artifact, nil)
	if passed || len(reasons) != 3 {
		t.Fatalf("ArchitectureStructure() = %v, %v, want all three real sections missing", passed, reasons)
	}
}

// TestArchitectureStructureAcceptsCustomRequiredSections is the regression
// test for a real brownfield-onboarding gap found 2026-09-08: an existing
// repo's own real ARCHITECTURE.md, written organically over real
// development, essentially never already uses goal_pilot.py's own exact
// three heading names -- forcing every onboarded repo to rename real
// sections of its own doc to satisfy this check does not scale once
// multiple teams' repos are in scope. requiredSections lets an operator
// declare the repo's own actual heading names instead.
func TestArchitectureStructureAcceptsCustomRequiredSections(t *testing.T) {
	artifact := "# example-app architecture\n\n## System overview\n\nGo backend, Flutter frontend.\n\n## Running tests\n\n`make verify`.\n\n## Open issues\n\nNone tracked here.\n"
	passed, reasons := ArchitectureStructure(artifact, []string{"System overview", "Running tests", "Open issues"})
	if !passed || len(reasons) != 0 {
		t.Errorf("ArchitectureStructure() with custom sections = %v, %v, want true with no reasons", passed, reasons)
	}
	// The same content still fails against the default section names --
	// requiredSections is a real override, not a fallback that gets tried
	// in addition to the default.
	passed, reasons = ArchitectureStructure(artifact, nil)
	if passed || len(reasons) != 3 {
		t.Errorf("ArchitectureStructure() with default sections = %v, %v, want all three missing", passed, reasons)
	}
}

// TestDefaultArchitectureRequiredSectionsReturnsAFreshSliceEachCall guards
// against a real footgun found via an adversarial pass, not a failing
// test (2026-09-08): an earlier version of this exposed the default as a
// package-level `var []string` rather than a function. A caller building a
// custom list by copying that var and mutating one entry in place (a
// natural thing to try -- "start from the default, change one name") would
// have silently corrupted the same backing array every other caller in the
// process reads as "the default" from then on, including a concurrent
// preflight check for a completely different run. This proves mutating one
// call's returned slice never affects a later call's.
func TestDefaultArchitectureRequiredSectionsReturnsAFreshSliceEachCall(t *testing.T) {
	first := DefaultArchitectureRequiredSections()
	first[0] = "corrupted"
	second := DefaultArchitectureRequiredSections()
	if second[0] != "Repo layout" {
		t.Fatalf("DefaultArchitectureRequiredSections() = %v after mutating an earlier call's result, want the real default untouched", second)
	}
}

func TestAcceptanceSuiteWiredPassesWithNoSlicesDrafted(t *testing.T) {
	passed, reasons := AcceptanceSuiteWired("verify:\n\tgo test ./...\n", nil)
	if !passed || len(reasons) != 0 {
		t.Errorf("AcceptanceSuiteWired() = %v, %v, want true with no reasons when no slices are drafted", passed, reasons)
	}
}

func TestAcceptanceSuiteWiredPassesWhenEverySliceIsReferenced(t *testing.T) {
	makefile := "verify-full: verify\n\t(cd 'spec/acceptance/001' && go test ./...)\n\tpython3 -m unittest discover -s spec/acceptance/002\n"
	passed, reasons := AcceptanceSuiteWired(makefile, []string{"spec/acceptance/001", "spec/acceptance/002"})
	if !passed || len(reasons) != 0 {
		t.Errorf("AcceptanceSuiteWired() = %v, %v, want true with no reasons", passed, reasons)
	}
}

func TestAcceptanceSuiteWiredRejectsAnUnstagedSlice(t *testing.T) {
	// Regression: a calculator app repo's own history and a notes app repo ticket 001
	// (2026-09-06) both shipped real contract deviations that a drafted
	// but never-staged acceptance slice would have caught immediately.
	makefile := "verify-full: verify\n\t(cd 'spec/acceptance/001' && go test ./...)\n"
	passed, reasons := AcceptanceSuiteWired(makefile, []string{"spec/acceptance/001", "spec/acceptance/002"})
	if passed {
		t.Fatal("AcceptanceSuiteWired() Passed = true, want false for an unstaged slice")
	}
	if !containsReason(reasons, "spec/acceptance/002") {
		t.Errorf("AcceptanceSuiteWired() reasons = %v, want one naming the unstaged slice", reasons)
	}
	if containsReason(reasons, "spec/acceptance/001\"") {
		t.Errorf("AcceptanceSuiteWired() reasons = %v, want no complaint about the correctly-staged slice", reasons)
	}
}

func TestAcceptanceSuiteWiredReportsEveryUnstagedSlice(t *testing.T) {
	passed, reasons := AcceptanceSuiteWired("verify:\n\tgo test ./...\n", []string{"spec/acceptance/001", "spec/acceptance/002"})
	if passed || len(reasons) != 2 {
		t.Fatalf("AcceptanceSuiteWired() = %v, %v, want both unstaged slices reported", passed, reasons)
	}
}

func TestProgramDesignStructureAcceptsPiArtifact(t *testing.T) {
	for name, artifact := range map[string]string{
		"numbered conventions": wellFormedProgramDesign,
		"error envelope":       wellFormedBudgetProgramDesign,
	} {
		t.Run(name, func(t *testing.T) {
			passed, reasons := ProgramDesignStructure(artifact)
			if !passed || len(reasons) != 0 {
				t.Errorf("ProgramDesignStructure() = %v, %v, want true with no reasons", passed, reasons)
			}
		})
	}
}

func TestProgramDesignStructureReportsEveryMissingSection(t *testing.T) {
	passed, reasons := ProgramDesignStructure("")
	if passed {
		t.Fatal("ProgramDesignStructure() Passed = true, want false")
	}
	for _, want := range []string{"Conventions", "Endpoint"} {
		if !containsReason(reasons, want) {
			t.Errorf("ProgramDesignStructure() reasons = %v, want one naming %q", reasons, want)
		}
	}
}

func TestProgramDesignStructureRejectsEndpointBeforeConventions(t *testing.T) {
	artifact := strings.Replace(wellFormedProgramDesign, "## 1. Conventions", "## temporary", 1)
	artifact = strings.Replace(artifact, "## 2. Endpoint: add", "## 1. Conventions", 1)
	artifact = strings.Replace(artifact, "## temporary", "## 2. Endpoint: add", 1)
	passed, reasons := ProgramDesignStructure(artifact)
	if passed || !containsReason(reasons, "Conventions before Endpoint") {
		t.Errorf("ProgramDesignStructure() = %v, %v, want an ordering reason", passed, reasons)
	}
}

func TestProgramDesignStructureIgnoresFencedExamples(t *testing.T) {
	artifact := "# API contract\n\n```markdown\n## Conventions\n## Endpoint: fake\n```\n"
	passed, reasons := ProgramDesignStructure(artifact)
	if passed || len(reasons) != 2 {
		t.Fatalf("ProgramDesignStructure() = %v, %v, want both real sections missing", passed, reasons)
	}
}

func TestProgramDesignStructureRejectsHeadingThatMerelyMentionsEndpoint(t *testing.T) {
	artifact := "# API contract\n\n## Conventions\n\n## This endpoint requirement is intentionally absent\n"
	passed, reasons := ProgramDesignStructure(artifact)
	if passed || !containsReason(reasons, "missing an Endpoint section") {
		t.Fatalf("ProgramDesignStructure() = %v, %v, want a missing Endpoint reason", passed, reasons)
	}
}

func TestProgramDesignStructureAcceptsHTTPMethodHeadingWithoutLiteralEndpointWord(t *testing.T) {
	// Regression for a real, live-found gap (2026-08-30): a genuine
	// /contract-plan run for calculator-pilot-v2 produced a contract
	// naming its routes with headings like "`GET /` -- HTML form page"
	// under a "## Web server" parent, never using the literal word
	// "Endpoint" anywhere -- and the mandatory factoryd project-bootstrap
	// preflight rejected this real, fully-built, fully-tested app over
	// that heading-vocabulary mismatch alone. This is the exact shape
	// (trimmed), not a synthetic minimal example.
	artifact := "# Calculator Pilot -- Wire & Interface Contract\n\n" +
		"## Conventions\n\nSome conventions.\n\n" +
		"## Web server (`webapp.py`)\n\nStdlib `http.server`.\n\n" +
		"### `GET /` -- HTML form page\n\n- Status `200`.\n\n" +
		"### `POST /calculate` -- shared handler\n\nBranches on Content-Type.\n\n" +
		"### Other routes\n\n| Request | Status |\n"
	passed, reasons := ProgramDesignStructure(artifact)
	if !passed || len(reasons) != 0 {
		t.Errorf("ProgramDesignStructure() = %v, %v, want true with no reasons for a real HTTP-method-heading contract", passed, reasons)
	}
}

func TestProgramDesignStructureStillRejectsPathLikeProseNotAHeading(t *testing.T) {
	// The HTTP-method-heading allowance above must stay heading-scoped,
	// not fire on a "GET /" mention inside a paragraph.
	artifact := "# API contract\n\n## Conventions\n\nSee `GET /accounts` in the routing table below.\n\n## Not an endpoint section\n\nNothing here names a route as its own heading.\n"
	passed, reasons := ProgramDesignStructure(artifact)
	if passed || !containsReason(reasons, "missing an Endpoint section") {
		t.Fatalf("ProgramDesignStructure() = %v, %v, want a missing Endpoint reason", passed, reasons)
	}
}

func TestProgramDesignStructureAcceptsEveryStandardHTTPMethodHeading(t *testing.T) {
	// Regression for a real codex review finding on the fix above: the
	// first version of httpEndpointHeadingRE only listed the methods the
	// one real contract on hand happened to use, missing TRACE and
	// CONNECT -- a contract documenting a real TRACE or CONNECT route
	// would otherwise still fail this check for the same reason the fix
	// exists to correct.
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT"} {
		t.Run(method, func(t *testing.T) {
			artifact := "# API contract\n\n## Conventions\n\n### `" + method + " /debug`\n\nSome description.\n"
			passed, reasons := ProgramDesignStructure(artifact)
			if !passed || len(reasons) != 0 {
				t.Errorf("ProgramDesignStructure() = %v, %v, want true with no reasons for a %s heading", passed, reasons, method)
			}
		})
	}
}

func assertTicketReason(t *testing.T, ticket string, ticketNumber int, want string) {
	t.Helper()
	passed, reasons := TicketStructure(ticket, ticketNumber)
	if passed {
		t.Fatalf("TicketStructure() Passed = true, want false")
	}
	if !containsReason(reasons, want) {
		t.Errorf("TicketStructure() reasons = %v, want one naming %q", reasons, want)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.Contains(reason, want) {
			return true
		}
	}
	return false
}

const wellFormedBrownfieldTicket = `Verify-Command: make verify
Allowed-Files: internal/foo/foo.go, internal/foo/foo_test.go
Required-Changed-Files: internal/foo/foo.go

## Goal

Fix the thing.

## Plan

### Files to touch

- internal/foo/foo.go

### Steps

1. Fix it.

### Tests to add

- internal/foo/foo_test.go

### Acceptance criteria covered

- 1

## Out of scope

Nothing else.
`

func TestTicketStructureBrownfieldAcceptsWellFormedTicket(t *testing.T) {
	passed, reasons := TicketStructureBrownfield(wellFormedBrownfieldTicket)
	if !passed || len(reasons) != 0 {
		t.Errorf("TicketStructureBrownfield() = %v, %v, want true with no reasons", passed, reasons)
	}
}

func TestTicketStructureBrownfieldRejectsMissingHeaderKey(t *testing.T) {
	missingAllowedFiles := strings.Replace(wellFormedBrownfieldTicket, "Allowed-Files: internal/foo/foo.go, internal/foo/foo_test.go\n", "", 1)
	passed, reasons := TicketStructureBrownfield(missingAllowedFiles)
	if passed {
		t.Fatal("TicketStructureBrownfield(missing Allowed-Files:) Passed = true, want false")
	}
	if !containsReason(reasons, "Allowed-Files:") {
		t.Errorf("reasons = %v, want one naming %q", reasons, "Allowed-Files:")
	}
}

func TestTicketStructureBrownfieldRejectsMissingSection(t *testing.T) {
	missingOutOfScope := strings.Replace(wellFormedBrownfieldTicket, "## Out of scope\n\nNothing else.\n", "", 1)
	passed, reasons := TicketStructureBrownfield(missingOutOfScope)
	if passed {
		t.Fatal("TicketStructureBrownfield(missing ## Out of scope) Passed = true, want false")
	}
	if !containsReason(reasons, "Out of scope") {
		t.Errorf("reasons = %v, want one naming %q", reasons, "Out of scope")
	}
}

func TestTicketStructureBrownfieldRejectsFencedHeaderKeyOnly(t *testing.T) {
	fencedOnly := "```\nVerify-Command: make verify\nAllowed-Files: a.go\nRequired-Changed-Files: a.go\n```\n\n" +
		strings.Replace(wellFormedBrownfieldTicket, "Verify-Command: make verify\nAllowed-Files: internal/foo/foo.go, internal/foo/foo_test.go\nRequired-Changed-Files: internal/foo/foo.go\n\n", "", 1)
	passed, reasons := TicketStructureBrownfield(fencedOnly)
	if passed {
		t.Fatal("TicketStructureBrownfield(header keys only inside a fenced block) Passed = true, want false")
	}
	if !containsReason(reasons, "Verify-Command:") {
		t.Errorf("reasons = %v, want one naming %q", reasons, "Verify-Command:")
	}
}

func TestTicketStructureBrownfieldDoesNotAlterGreenfieldBehaviour(t *testing.T) {
	passed, reasons := TicketStructure(wellFormedTicket, 2)
	if !passed || len(reasons) != 0 {
		t.Errorf("TicketStructure() = %v, %v, want true with no reasons (greenfield behaviour must be unchanged)", passed, reasons)
	}
}

// A named gate's rerun on the base commit is carried to its gate result and
// decides nothing: the gate fails on its own exit code alone.
func TestEvaluateRunCarriesANamedGatesBaseCheckWithoutJudgingByIt(t *testing.T) {
	passesOnBase := &run.GateBaseCheck{Outcome: run.GateBasePasses, BaseSHA: "abc"}
	result := EvaluateRun(EvaluateRunInput{
		VerifyCommand: []string{"sh", "-c", "true"},
		NamedGates: []NamedGateInput{
			{Check: "lint", Command: []string{"sh", "-c", "lint"}, ExitCode: 1, BaseCheck: passesOnBase},
			{Check: "unit_tests", Command: []string{"sh", "-c", "test"}, ExitCode: 0},
		},
	})
	if result.Accepted || !slices.Contains(result.FailedChecks, "lint") || slices.Contains(result.FailedChecks, "unit_tests") {
		t.Fatalf("accepted = %v, failed checks = %v; want lint failed: a passing rerun must not pass the gate", result.Accepted, result.FailedChecks)
	}
	for _, g := range result.GateResults {
		switch g.Check {
		case "lint":
			if g.Passed || g.BaseCheck != passesOnBase {
				t.Errorf("lint = %+v, want failed with its base check", g)
			}
		case "unit_tests":
			if !g.Passed || g.BaseCheck != nil {
				t.Errorf("unit_tests = %+v, want passed with no base check", g)
			}
		}
	}
}
