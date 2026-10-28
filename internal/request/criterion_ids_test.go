package request

import (
	"slices"
	"strings"
	"testing"
)

// specWithCriteria returns a structurally valid spec whose criteria section
// is body.
func specWithCriteria(body string) string {
	return "# Spec\n\n## Problem\n\nx\n\n## Scope\n\nx\n\n## Non-goals\n\nx\n\n## Affected services and packages\n\nx\n\n## Acceptance criteria\n\n" +
		body + "\n\n## Risks\n\nx\n\n## Open questions\n\nx\n"
}

func TestValidateSpecCriteriaRequiresIDsOnADraftedSpec(t *testing.T) {
	t.Parallel()
	plain := specWithCriteria("1. It works.\n2. It also works.")
	if err := ValidateSpecCriteria(plain, true); err == nil || !strings.Contains(err.Error(), "AC-N") {
		t.Fatalf("a drafted spec without IDs: err = %v", err)
	}
	labelled := specWithCriteria("1. AC-1: When a, the system shall b.\n2. AC-2: When c, the system shall d.\n   continued on a second line.")
	if err := ValidateSpecCriteria(labelled, true); err != nil {
		t.Fatalf("a labelled spec: %v", err)
	}
	if err := ValidateSpecCriteria(specWithCriteria("Just prose."), true); err == nil {
		t.Fatal("a drafted spec with no numbered list was accepted")
	}
}

func TestValidateSpecCriteriaRefusesAnIDThatIsNotItsNumber(t *testing.T) {
	t.Parallel()
	err := ValidateSpecCriteria(specWithCriteria("1. AC-1: x.\n2. AC-3: y."), true)
	if err == nil || !strings.Contains(err.Error(), "criterion 2 is labelled AC-3") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateSpecCriteriaRefusesANumberingGap(t *testing.T) {
	t.Parallel()
	for _, require := range []bool{true, false} {
		err := ValidateSpecCriteria(specWithCriteria("1. AC-1: x.\n3. AC-3: y."), require)
		if err == nil || !strings.Contains(err.Error(), "numbered 1..2 in order") {
			t.Fatalf("requireIDs=%v: err = %v", require, err)
		}
	}
}

func TestValidateSpecCriteriaRefusesMixedIDs(t *testing.T) {
	t.Parallel()
	for _, require := range []bool{true, false} {
		err := ValidateSpecCriteria(specWithCriteria("1. AC-1: x.\n2. y."), require)
		if err == nil || !strings.Contains(err.Error(), "mix labelled and unlabelled") {
			t.Fatalf("requireIDs=%v: err = %v", require, err)
		}
	}
}

func TestValidateSpecCriteriaAcceptsALegacySpec(t *testing.T) {
	t.Parallel()
	if err := ValidateSpecCriteria(specWithCriteria("1. It works.\n2. It also works."), false); err != nil {
		t.Fatalf("a spec without IDs, not required: %v", err)
	}
	if err := ValidateSpecCriteria(specWithCriteria("No list at all."), false); err != nil {
		t.Fatalf("a spec with no numbered list, not required: %v", err)
	}
}

func TestValidateHandedOverSpecAcceptsAnOperatorShapedSpecAndChecksNamedNumbering(t *testing.T) {
	t.Parallel()
	operatorSpec := "# Feature\n\n## Scope\n\n- It supports the requested mode.\n"
	if err := ValidateHandedOverSpec(operatorSpec); err != nil {
		t.Fatalf("operator-shaped handed-over spec: %v", err)
	}
	if err := ValidateHandedOverSpec(specWithCriteria("1. first\n3. gap")); err == nil || !strings.Contains(err.Error(), "numbered 1..2 in order") {
		t.Fatalf("numbered handed-over spec: err = %v", err)
	}
}

func TestSpecReviewNotesNameACriterionNotInEARSForm(t *testing.T) {
	t.Parallel()
	spec := specWithCriteria("1. AC-1: When a request arrives, the system shall answer it.\n" +
		"2. AC-2: Answers are fast.\n" +
		"3. AC-3: The system shall log each request.\n" +
		"4. AC-4: While idle, the system shall sleep.")
	got := strings.Join(SpecReviewNotes(spec), "\n")
	if want := `criterion 2 is not in the "When ..., the system shall ..." form`; got != want {
		t.Errorf("notes = %q, want %q", got, want)
	}
	if notes := SpecReviewNotes(specWithCriteria("1. Answers are fast.")); len(notes) != 0 {
		t.Errorf("a spec without IDs gets no wording note: %q", notes)
	}
}

func TestSpecReviewNotesDescribeDerivedCriteriaWithoutIDNotes(t *testing.T) {
	t.Parallel()
	spec := "# Feature\n\n- The title item.\n\n## Wanted behaviour\n\n- AC-7: The first behaviour.\n\n## Interface\n\n* The interface behaviour.\n\n## Notes\n\n- Not a criterion.\n"
	want := []string{"Criteria 1-3 were derived from the list items of 3 sections."}
	if got := SpecReviewNotes(spec); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("notes = %q, want %q", got, want)
	}
	oneSection := "# Feature\n\n## Wanted behaviour\n\n- keep this\n"
	if got := SpecReviewNotes(oneSection); strings.Join(got, "\n") != "Criteria 1-1 were derived from the list items of 1 section." {
		t.Errorf("one-section notes = %q, want the singular form", got)
	}
}

func TestExactAcceptanceCriteriaBulletListIsDerivedButDraftedListsMustBeNumbered(t *testing.T) {
	t.Parallel()
	spec := specWithCriteria("- first\n- second")
	if err := ValidateHandedOverSpec(spec); err != nil {
		t.Fatalf("handed-over bullet list: %v", err)
	}
	if err := ValidateSpecCriteria(spec, true); err == nil || !strings.Contains(err.Error(), "must be numbered (1. …)") {
		t.Fatalf("drafted bullet list: err = %v", err)
	}
}

func TestExactAcceptanceCriteriaUsesTheNumberedParserWhenStylesAreMixed(t *testing.T) {
	t.Parallel()
	if got, err := SpecAcceptanceCriteria(specWithCriteria("- first\n1. second")); err != nil || !slices.Equal(got, []string{"1. second"}) {
		t.Fatalf("bullet before numbered: criteria = %q, err = %v", got, err)
	}
	if got, err := SpecAcceptanceCriteria(specWithCriteria("1. first\n- continuation")); err != nil || !slices.Equal(got, []string{"1. first - continuation"}) {
		t.Fatalf("bullet after numbered: criteria = %q, err = %v", got, err)
	}
	if err := ValidateHandedOverSpec(specWithCriteria("- first\n1. second")); err != nil {
		t.Fatalf("mixed exact criteria should be accepted by the numbered parser: %v", err)
	}
}

func TestTicketCoveredCriteriaAcceptsACIDs(t *testing.T) {
	t.Parallel()
	ticket := notesTicket("001.spec.md", []string{"a.go"}, 1)
	content := strings.Replace(ticket.Content, "- 1\n", "- AC-1\n- AC-3\n- 2\n", 1)
	got, err := TicketCoveredCriteria(content)
	if err != nil || len(got) != 3 || got[0] != 1 || got[1] != 3 || got[2] != 2 {
		t.Fatalf("covered = %v, err = %v", got, err)
	}
	if err := ValidateTicketPlan(content); err != nil {
		t.Fatalf("ValidateTicketPlan: %v", err)
	}
}

// citingTicket is notesTicket with its Tests to add naming tests.
func citingTicket(name string, tests string, criteria ...int) PlanTicket {
	ticket := notesTicket(name, []string{"a.go"}, criteria...)
	ticket.Content = strings.Replace(ticket.Content, "- one\n", tests, 1)
	return ticket
}

func TestPlanReviewNotesNameACriterionWithNoTest(t *testing.T) {
	t.Parallel()
	criteria := []string{"1. AC-1: When a, the system shall b.", "2. AC-2: When c, the system shall d.", "3. AC-3: When e, the system shall f."}
	tickets := []PlanTicket{
		citingTicket("001.spec.md", "- a_test.go: covers the b case (AC-1)\n", 1, 2),
		citingTicket("002.spec.md", "- c_test.go: covers AC-30 only\n", 3),
	}
	got := PlanReviewNotes(criteria, tickets, "", 0)
	want := []string{
		"criterion 2 has no test: no covering ticket's Tests to add cites AC-2",
		"criterion 3 has no test: no covering ticket's Tests to add cites AC-3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("notes = %q, want %q", got, want)
	}
}

func TestNoTestNoteOnlyForASpecWithIDs(t *testing.T) {
	t.Parallel()
	tickets := []PlanTicket{citingTicket("001.spec.md", "- a_test.go\n", 1, 2)}
	if got := PlanReviewNotes([]string{"1. It works.", "2. It also works."}, tickets, "", 0); len(got) != 0 {
		t.Errorf("a spec without IDs: notes = %q", got)
	}
	if got := PlanReviewNotes([]string{"1. AC-1: It works.", "2. It also works."}, tickets, "", 0); len(got) != 0 {
		t.Errorf("a spec with some IDs: notes = %q", got)
	}
}

func TestValidateSpecCriteriaRefusesALeadingZeroID(t *testing.T) {
	t.Parallel()
	err := ValidateSpecCriteria(specWithCriteria("1. AC-01: x.\n2. AC-2: y."), true)
	if err == nil || !strings.Contains(err.Error(), "without leading zeros (AC-1)") {
		t.Fatalf("err = %v", err)
	}
}

func TestValidateSpecCriteriaAcceptsALabelWithItsTextOnTheNextLines(t *testing.T) {
	t.Parallel()
	spec := specWithCriteria("1. AC-1:\n   When a, the system shall b.\n2. AC-2: When c, the system shall d.")
	if err := ValidateSpecCriteria(spec, true); err != nil {
		t.Fatalf("a label with its text below: %v", err)
	}
	if err := ValidateSpecCriteria(specWithCriteria("1. AC-1:\n2. AC-2: y."), true); err == nil || !strings.Contains(err.Error(), "no text") {
		t.Fatalf("a label with no text: err = %v", err)
	}
}

// TestAFencedNumberedLineIsNotACriterion: a numbered line inside a code fence
// continues the criterion it is in.
func TestAFencedNumberedLineIsNotACriterion(t *testing.T) {
	t.Parallel()
	for _, fence := range []string{"```", "~~~"} {
		spec := specWithCriteria("1. AC-1: When a, the system shall print:\n" + fence + "\n2. not a criterion\n3. nor this\n" + fence + "\n2. AC-2: When c, the system shall d.")
		if err := ValidateSpecCriteria(spec, true); err != nil {
			t.Errorf("%s: %v", fence, err)
		}
		criteria, err := SpecAcceptanceCriteria(spec)
		if err != nil || len(criteria) != 2 {
			t.Errorf("%s: criteria = %d (%v), want 2", fence, len(criteria), err)
		}
	}
}
