package request

import (
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
