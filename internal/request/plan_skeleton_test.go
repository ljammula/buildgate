package request

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestSpecAcceptanceCriteriaCountRejectsASpecWithoutCriteria(t *testing.T) {
	_, err := SpecAcceptanceCriteriaCount("# Spec\n\n## Problem\n\nx\n")
	if err == nil {
		t.Fatal("SpecAcceptanceCriteriaCount(no criteria section) = nil, want error")
	}
}

func TestSpecAcceptanceCriteriaDerivesTheGoTomlExample(t *testing.T) {
	spec := "# go-toml: an encoder setting for multiline strings\n\n" +
		"## Problem\n\n- This problem bullet is not a criterion.\n\n" +
		"## Wanted behaviour\n\nThe prose before the first item is not a criterion.\n" +
		"- A new encoder setting chooses how string values are written, with three modes:\n" +
		"  - **default**: quote strings.\n  - **if newline**: use multiline strings.\n" +
		"- The setting applies to every string the encoder writes as a value: keys and nested values included.\n\n" +
		"## Interface\n\nIn package github.com/pelletier/go-toml/v2:\n" +
		"- `type MultilineStringMode int`, with the constants ...\n" +
		"- `func (enc *Encoder) SetMultilineStrings(mode MultilineStringMode) *Encoder`, chainable ...\n\n" +
		"Document the setting in the Encoder docs and in the README.\n\n" +
		"## Risks\n\n- This risk bullet is not a criterion.\n"
	want := []string{
		"1. A new encoder setting chooses how string values are written, with three modes: - **default**: quote strings. - **if newline**: use multiline strings.",
		"2. The setting applies to every string the encoder writes as a value: keys and nested values included.",
		"3. `type MultilineStringMode int`, with the constants ...",
		"4. `func (enc *Encoder) SetMultilineStrings(mode MultilineStringMode) *Encoder`, chainable ...",
	}
	got, err := SpecAcceptanceCriteria(spec)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("derived criteria = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("derived criteria[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSpecAcceptanceCriteriaDerivationIgnoresFencedListsAndSupportsNumberedLists(t *testing.T) {
	spec := `# Spec

## Scope

~~~text
- not a criterion
1. also not a criterion
~~~
1. The first real item.
2) The second real item.

## Problem

- ignored problem item
`
	got, err := SpecAcceptanceCriteria(spec)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria: %v", err)
	}
	want := []string{"1. The first real item.", "2. The second real item."}
	if len(got) != len(want) {
		t.Fatalf("derived criteria = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("derived criteria[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSpecAcceptanceCriteriaDerivationRejectsASpecWithNoLists(t *testing.T) {
	_, err := SpecAcceptanceCriteria("# Spec\n\n## Scope\n\nOnly prose.\n")
	if err == nil || !strings.Contains(err.Error(), "no list items to derive criteria from") {
		t.Fatalf("SpecAcceptanceCriteria(no lists) = %v, want a derivation error", err)
	}
}

func TestSpecAcceptanceCriteriaDerivesFromAMiscasedCriteriaHeadingAndTheTitle(t *testing.T) {
	got, err := SpecAcceptanceCriteria("# Title\n\n- Under the title.\n\n## Problem\n\n- Not a requirement.\n\n## Acceptance Criteria\n\n1. First.\n2. Second.\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"1. First.", "2. Second."}
	if !slices.Equal(got, want) {
		t.Errorf("SpecAcceptanceCriteria = %q, want %q", got, want)
	}
}

func TestSpecAcceptanceCriteriaKeepsSkeletonBehaviour(t *testing.T) {
	got, err := SpecAcceptanceCriteria(validSpecTwoCriteria)
	if err != nil {
		t.Fatalf("SpecAcceptanceCriteria(skeleton): %v", err)
	}
	want := []string{
		"1. It works.",
		"2. It doesn't break anything else.",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("SpecAcceptanceCriteria(skeleton) = %v, want %v", got, want)
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

func TestSpecAcceptanceCriteriaLargeHeadingDocumentIsLinearEnough(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 10_000; i++ {
		b.WriteString("## Heading ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	if b.Len() < 150_000 {
		b.WriteString(strings.Repeat("x", 150_000-b.Len()))
	}
	if b.Len() < 150_000 {
		t.Fatalf("test document is %d bytes, want at least 150000", b.Len())
	}
	started := time.Now()
	_, err := SpecAcceptanceCriteria(b.String())
	if err == nil {
		t.Fatal("document with no list items: want a derivation error")
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("parsing 150 KB with 10000 headings took %s", elapsed)
	}
}

func TestSpecAcceptanceCriteriaLargePreambleAndHeadingDocumentIsLinearEnough(t *testing.T) {
	var b strings.Builder
	b.WriteString(strings.Repeat("- preamble item\n", 15_000))
	for i := 0; i < 10_000; i++ {
		b.WriteString("## Heading ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	started := time.Now()
	criteria, err := SpecAcceptanceCriteria(b.String())
	if err != nil {
		t.Fatalf("large preamble: %v", err)
	}
	if len(criteria) != 15_000 {
		t.Fatalf("large preamble criteria = %d, want 15000", len(criteria))
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("parsing 15000-item preamble and 10000 headings took %s", elapsed)
	}
}
