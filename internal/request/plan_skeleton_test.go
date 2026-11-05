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

// headingDocument is n "## Heading i" lines padded to at least 15 bytes a
// heading, with no list item.
func headingDocument(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("## Heading ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	if b.Len() < 15*n {
		b.WriteString(strings.Repeat("x", 15*n-b.Len()))
	}
	return b.String()
}

// preambleAndHeadingDocument is 3n/2 preamble items followed by n headings.
func preambleAndHeadingDocument(n int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat("- preamble item\n", n*3/2))
	for i := 0; i < n; i++ {
		b.WriteString("## Heading ")
		b.WriteString(strconv.Itoa(i))
		b.WriteByte('\n')
	}
	return b.String()
}

// assertParseScalesLinearly fails when parsing a document four times the
// size takes more than eight times as long (linear is about 4, quadratic
// about 16). It compares the best of five runs at each size, so it holds on a
// slow or loaded machine where a fixed time limit does not.
func assertParseScalesLinearly(t *testing.T, doc func(n int) string, n int, parse func(string)) {
	t.Helper()
	best := func(text string) time.Duration {
		fastest := time.Duration(1<<63 - 1)
		for range 5 {
			started := time.Now()
			parse(text)
			fastest = min(fastest, time.Since(started))
		}
		return max(fastest, time.Microsecond)
	}
	small, large := best(doc(n)), best(doc(4*n))
	if large > 8*small {
		t.Fatalf("parsing %d took %s, %d took %s: %.1fx for 4x the input, want at most 8x", n, small, 4*n, large, float64(large)/float64(small))
	}
}

func TestSpecAcceptanceCriteriaLargeHeadingDocumentIsLinearEnough(t *testing.T) {
	started := time.Now()
	if _, err := SpecAcceptanceCriteria(headingDocument(10_000)); err == nil {
		t.Fatal("document with no list items: want a derivation error")
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("parsing 10000 headings took %s", elapsed)
	}
	assertParseScalesLinearly(t, headingDocument, 5_000, func(text string) { _, _ = SpecAcceptanceCriteria(text) })
}

func TestSpecAcceptanceCriteriaLargePreambleAndHeadingDocumentIsLinearEnough(t *testing.T) {
	started := time.Now()
	criteria, err := SpecAcceptanceCriteria(preambleAndHeadingDocument(10_000))
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("parsing a 15000-item preamble and 10000 headings took %s", elapsed)
	}
	if err != nil {
		t.Fatalf("large preamble: %v", err)
	}
	if len(criteria) != 15_000 {
		t.Fatalf("large preamble criteria = %d, want 15000", len(criteria))
	}
	assertParseScalesLinearly(t, preambleAndHeadingDocument, 5_000, func(text string) { _, _ = SpecAcceptanceCriteria(text) })
}

// The factory's spec ticket may be a spec handed over in its own shape, not
// the drafted skeleton: SpecTicketStructure accepts it, while
// RequestTicketStructure, which judges content of unknown origin, does not.
func TestSpecTicketStructureAcceptsAHandedOverSpec(t *testing.T) {
	handedOver := "# Spec\n\n## Goal\n\ng\n\n## Wanted behaviour\n\n- one\n- two\n"
	if ok, reasons := SpecTicketStructure(handedOver); !ok {
		t.Errorf("a handed-over spec was refused as the spec ticket: %v", reasons)
	}
	if ok, _ := RequestTicketStructure(handedOver); ok {
		t.Error("RequestTicketStructure accepted a handed-over spec of unknown origin")
	}
	if ok, reasons := SpecTicketStructure(validTicket); !ok {
		t.Errorf("a ticket was refused: %v", reasons)
	}
}

// A request ticket is the shape the planning job drafts or the approved spec's
// own skeleton (the one ticket the factory writes for a request submitted
// without a plan); anything else is refused with the ticket shape's reasons.
func TestRequestTicketStructureAcceptsATicketAndTheSpecForm(t *testing.T) {
	if ok, reasons := RequestTicketStructure(validTicket); !ok {
		t.Errorf("a drafted ticket was refused: %v", reasons)
	}
	spec := "# Spec\n\n## Problem\n\np\n\n## Scope\n\ns\n\n## Non-goals\n\nn\n\n## Affected services and packages\n\na\n\n## Acceptance criteria\n\n1. one\n\n## Risks\n\nr\n\n## Open questions\n\nNone.\n"
	if ok, reasons := RequestTicketStructure(spec); !ok {
		t.Errorf("a spec in spec form was refused: %v", reasons)
	}
	if ok, reasons := RequestTicketStructure("## Goal\n\nonly a goal\n"); ok || len(reasons) == 0 {
		t.Errorf("a ticket off both forms was accepted: %v", reasons)
	}
}
