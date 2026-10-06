package request

import (
	"strings"
	"testing"
)

const validTicket = `Verify-Command: make verify
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
- 2

## Out of scope

Nothing else.
`

const validSpecTwoCriteria = `# Spec

## Problem

Something is broken.

## Scope

Just this service.

## Non-goals

Not that.

## Affected services and packages

internal/foo

## Acceptance criteria

1. It works.
2. It doesn't break anything else.

## Risks

None.

## Open questions

None.
`

func TestValidateTicketPlanAcceptsAValidTicket(t *testing.T) {
	if err := ValidateTicketPlan(validTicket); err != nil {
		t.Errorf("ValidateTicketPlan(valid) = %v, want nil", err)
	}
}

func TestValidateTicketPlanRejectsMissingSubsection(t *testing.T) {
	broken := `## Goal

Fix the thing.

## Plan

### Files to touch

- internal/foo/foo.go

### Steps

1. Fix it.

### Acceptance criteria covered

- 1

## Out of scope

Nothing else.
`
	err := ValidateTicketPlan(broken)
	if err == nil {
		t.Fatal("ValidateTicketPlan(missing ### Tests to add) = nil, want error")
	}
}

func TestValidateTicketPlanRejectsEmptySubsection(t *testing.T) {
	broken := `## Goal

Fix the thing.

## Plan

### Files to touch

### Steps

1. Fix it.

### Tests to add

- internal/foo/foo_test.go

### Acceptance criteria covered

- 1

## Out of scope

Nothing else.
`
	err := ValidateTicketPlan(broken)
	if err == nil {
		t.Fatal("ValidateTicketPlan(empty ### Files to touch) = nil, want error")
	}
}

func TestValidateTicketPlanRejectsUnparseableCriteria(t *testing.T) {
	broken := `## Goal

Fix the thing.

## Plan

### Files to touch

- internal/foo/foo.go

### Steps

1. Fix it.

### Tests to add

- internal/foo/foo_test.go

### Acceptance criteria covered

Covers everything, basically.

## Out of scope

Nothing else.
`
	err := ValidateTicketPlan(broken)
	if err == nil {
		t.Fatal("ValidateTicketPlan(unparseable criteria) = nil, want error")
	}
}

func TestValidateTicketPlanRejectsOutOfOrderHeadings(t *testing.T) {
	broken := `## Plan

### Files to touch

- internal/foo/foo.go

### Steps

1. Fix it.

### Tests to add

- internal/foo/foo_test.go

### Acceptance criteria covered

- 1

## Goal

Fix the thing.

## Out of scope

Nothing else.
`
	if err := ValidateTicketPlan(broken); err == nil {
		t.Fatal("ValidateTicketPlan(out-of-order headings) = nil, want error")
	}
}

func TestValidatePlanCoverageAcceptsFullCoverage(t *testing.T) {
	if err := ValidatePlanCoverage(2, []string{validTicket}); err != nil {
		t.Errorf("ValidatePlanCoverage(fully covered) = %v, want nil", err)
	}
}

func TestValidatePlanCoverageReportsUnclaimedCriteria(t *testing.T) {
	onlyClaimsOne := `## Goal

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
	err := ValidatePlanCoverage(3, []string{onlyClaimsOne})
	if err == nil {
		t.Fatal("ValidatePlanCoverage(missing 2 and 3) = nil, want error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected a non-empty error naming the unclaimed criteria")
	}
}

func TestValidatePlanCoverageSplitAcrossTwoTickets(t *testing.T) {
	claimsOne := `## Goal

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
	claimsTwo := `## Goal

Fix the other thing.

## Plan

### Files to touch

- internal/bar/bar.go

### Steps

1. Fix it.

### Tests to add

- internal/bar/bar_test.go

### Acceptance criteria covered

- 2

## Out of scope

Nothing else.
`
	if err := ValidatePlanCoverage(2, []string{claimsOne, claimsTwo}); err != nil {
		t.Errorf("ValidatePlanCoverage(split across two tickets) = %v, want nil", err)
	}
}

func TestSpecAcceptanceCriteriaCount(t *testing.T) {
	count, err := SpecAcceptanceCriteriaCount(validSpecTwoCriteria)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteriaCount: %v", err)
	}
	if count != 2 {
		t.Errorf("SpecAcceptanceCriteriaCount = %d, want 2", count)
	}
}

func TestSpecAcceptanceCriteriaCountRejectsMissingSection(t *testing.T) {
	_, err := SpecAcceptanceCriteriaCount("# Spec\n\n## Problem\n\nx\n")
	if err == nil {
		t.Fatal("SpecAcceptanceCriteriaCount(no criteria section) = nil, want error")
	}
}

// TestSpecAcceptanceCriteriaFoldsContinuationLines is a regression for the
// 2026-09-17 multi-repo validation finding: a criterion spanning more than
// one raw line -- a wrapped sentence, or a lettered sub-list under a
// numbered item -- used to be silently truncated to just its first line,
// dropping the rest instead of folding it into the criterion's own text.
func TestSpecAcceptanceCriteriaFoldsContinuationLines(t *testing.T) {
	spec := `# Spec

## Problem

x

## Scope

x

## Non-goals

x

## Affected services and packages

internal/foo

## Acceptance criteria

1. The test suite includes, at minimum, these cases:
   a. A positive case.
   b. A negative case.
2. It works, spanning
   a wrapped second line.

## Risks

None.

## Open questions

None.
`
	got, err := SpecAcceptanceCriteria(spec)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria: %v", err)
	}
	want := []string{
		"1. The test suite includes, at minimum, these cases: a. A positive case. b. A negative case.",
		"2. It works, spanning a wrapped second line.",
	}
	if len(got) != len(want) {
		t.Fatalf("SpecAcceptanceCriteria = %d criteria, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SpecAcceptanceCriteria[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestSpecAcceptanceCriteriaFoldsNumberedSubList is a regression for the
// adversarial-review finding on this same branch: a numbered (not
// lettered) sub-list nested under a criterion also matches
// specCriterionItemRE once its indentation is trimmed away, so it must
// still fold into its parent criterion, not split into bogus top-level
// criteria of its own.
func TestSpecAcceptanceCriteriaFoldsNumberedSubList(t *testing.T) {
	spec := `# Spec

## Problem

x

## Scope

x

## Non-goals

x

## Affected services and packages

internal/foo

## Acceptance criteria

1. It works.
2. The API rejects a request missing any of:
   1) an auth header
   2) a content-type
   3) an idempotency key

## Risks

None.

## Open questions

None.
`
	got, err := SpecAcceptanceCriteria(spec)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria: %v", err)
	}
	want := []string{
		"1. It works.",
		"2. The API rejects a request missing any of: 1) an auth header 2) a content-type 3) an idempotency key",
	}
	if len(got) != len(want) {
		t.Fatalf("SpecAcceptanceCriteria = %d criteria, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SpecAcceptanceCriteria[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStripCommitMessageCriteriaRemovesAndRenumbers(t *testing.T) {
	spec := `# Spec

## Problem

x

## Scope

x

## Non-goals

x

## Affected services and packages

internal/foo

## Acceptance criteria

1. It works.
2. The commit message subject line begins with ` + "`ticket(foo):`" + `.
3. It doesn't break anything else.

## Risks

None.

## Open questions

None.
`
	got, removed := StripCommitMessageCriteria(spec)
	if removed != 1 {
		t.Fatalf("StripCommitMessageCriteria removed = %d, want 1", removed)
	}
	criteria, err := SpecAcceptanceCriteria(got)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria(stripped): %v", err)
	}
	want := []string{"1. It works.", "2. It doesn't break anything else."}
	if len(criteria) != len(want) {
		t.Fatalf("SpecAcceptanceCriteria(stripped) = %d criteria, want %d: %v", len(criteria), len(want), criteria)
	}
	for i := range want {
		if criteria[i] != want[i] {
			t.Errorf("SpecAcceptanceCriteria(stripped)[%d] = %q, want %q", i, criteria[i], want[i])
		}
	}
	// Every other section must survive verbatim.
	for _, marker := range []string{"## Problem", "## Risks", "## Open questions", "internal/foo"} {
		if !strings.Contains(got, marker) {
			t.Errorf("StripCommitMessageCriteria dropped unrelated content %q", marker)
		}
	}
}

// TestStripCommitMessageCriteriaPreservesSurvivingMultiLineFormatting is a
// regression for an adversarial-review finding on this same branch: an
// earlier version rebuilt every surviving criterion from its own folded
// (always-one-line) text, flattening a criterion's own wrapped/lettered
// sub-list formatting even though it was never touched.
func TestStripCommitMessageCriteriaPreservesSurvivingMultiLineFormatting(t *testing.T) {
	spec := `# Spec

## Problem

x

## Scope

x

## Non-goals

x

## Affected services and packages

internal/foo

## Acceptance criteria

1. The test suite includes, at minimum, these cases:
   a. A positive case.
   b. A negative case.
2. The commit subject line begins with ` + "`ticket(foo):`" + `.

## Risks

None.

## Open questions

None.
`
	got, removed := StripCommitMessageCriteria(spec)
	if removed != 1 {
		t.Fatalf("StripCommitMessageCriteria removed = %d, want 1", removed)
	}
	want := "1. The test suite includes, at minimum, these cases:\n   a. A positive case.\n   b. A negative case.\n"
	if !strings.Contains(got, want) {
		t.Errorf("StripCommitMessageCriteria flattened the surviving criterion's own formatting -- got:\n%s\nwant it to contain:\n%s", got, want)
	}
}

// TestStripCommitMessageCriteriaRecordsRemovedTextVisibly is a
// regression for an adversarial-review finding on this same branch:
// commitMessageCriterionRE is a heuristic and can occasionally match a
// real, unrelated criterion -- silently deleting matched text with no
// trace would leave a human approving spec_review unaware anything was
// removed. The removed text must survive somewhere in the persisted
// document, and must not corrupt the criteria section itself when
// re-parsed.
func TestStripCommitMessageCriteriaRecordsRemovedTextVisibly(t *testing.T) {
	spec := `# Spec

## Problem

x

## Scope

x

## Non-goals

x

## Affected services and packages

internal/foo

## Acceptance criteria

1. It works.
2. The commit subject line begins with ` + "`ticket(foo):`" + `.

## Risks

None.

## Open questions

None.
`
	got, removed := StripCommitMessageCriteria(spec)
	if removed != 1 {
		t.Fatalf("StripCommitMessageCriteria removed = %d, want 1", removed)
	}
	if !strings.Contains(got, "commit subject line begins with") {
		t.Errorf("removed criterion's own text does not survive anywhere in the persisted document: %s", got)
	}
	// Re-parsing the criteria section must still see exactly the
	// surviving criterion -- the removal note must not have been swept
	// into it as a continuation line.
	criteria, err := SpecAcceptanceCriteria(got)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria(stripped): %v", err)
	}
	if len(criteria) != 1 || criteria[0] != "1. It works." {
		t.Errorf("SpecAcceptanceCriteria(stripped) = %v, want exactly [\"1. It works.\"] -- the removal note must not corrupt the criteria section", criteria)
	}
}

// TestStripCommitMessageCriteriaCatchesNonAdjacentPhrasings is a
// regression for an adversarial-review finding on this same branch: the
// original matcher only fired when "commit"/"commit's" was directly
// adjacent to "message"/"subject", missing common real phrasings with an
// intervening word (and the exact wording an earlier live-test finding
// on this same session used).
func TestStripCommitMessageCriteriaCatchesNonAdjacentPhrasings(t *testing.T) {
	cases := []string{
		"1. The commit for this work has a subject line beginning with `ticket`.",
		"1. The commit log message references the ticket ID.",
		"1. Each commit's log message follows Conventional Commits.",
		"1. All commits include messages referencing the ticket ID.",
		"1. Commit subjects follow the imperative mood.",
	}
	for _, criterion := range cases {
		spec := "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\ninternal/foo\n\n## Acceptance criteria\n\n" +
			criterion + "\n2. Something else entirely.\n\n## Risks\n\nNone.\n\n## Open questions\n\nNone.\n"
		_, removed := StripCommitMessageCriteria(spec)
		if removed != 1 {
			t.Errorf("StripCommitMessageCriteria(%q) removed = %d, want 1", criterion, removed)
		}
	}
}

func TestStripCommitMessageCriteriaNoopWhenNoneMatch(t *testing.T) {
	got, removed := StripCommitMessageCriteria(validSpecTwoCriteria)
	if removed != 0 {
		t.Fatalf("StripCommitMessageCriteria removed = %d, want 0", removed)
	}
	if got != validSpecTwoCriteria {
		t.Error("StripCommitMessageCriteria must return the input unchanged when nothing matches")
	}
}

func TestStripCommitMessageCriteriaNoopWithoutSection(t *testing.T) {
	spec := "# Spec\n\n## Problem\n\nx\n"
	got, removed := StripCommitMessageCriteria(spec)
	if removed != 0 || got != spec {
		t.Error("StripCommitMessageCriteria must no-op when the spec has no Acceptance criteria section")
	}
}
