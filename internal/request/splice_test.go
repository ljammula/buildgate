package request

import (
	"strings"
	"testing"
)

// TestSpliceSectionsRefusesADraftThatDoesNotBoundTheSection: the redraft's
// section ends where the skeleton says, so a renamed or dropped next heading,
// or a heading the skeleton lacks inside it, is refused instead of letting the
// section swallow its neighbour.
func TestSpliceSectionsRefusesADraftThatDoesNotBoundTheSection(t *testing.T) {
	current := joinSections(scopedSpecSections)
	good := rewriteEverySection(scopedSpecSections)
	heads := SpecSkeletonHeadings()
	for name, drafted := range map[string]string{
		"next heading renamed": strings.Replace(good, "## Non-goals", "## Not goals", 1),
		"next heading dropped": strings.Replace(good, "## Non-goals\n\nREWRITTEN ## Non-goals\n\n", "", 1),
		"heading appended":     strings.Replace(good, "## Non-goals", "## Rollout\n\nnew\n\n## Non-goals", 1),
	} {
		if _, err := SpliceSections(current, drafted, heads, []string{"## Scope"}); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
	// A deeper heading inside the section is the section's own text.
	deeper := strings.Replace(good, "REWRITTEN ## Scope\n", "REWRITTEN ## Scope\n\n### Detail\n\nmore\n", 1)
	got, err := SpliceSections(current, deeper, heads, []string{"## Scope"})
	if err != nil || !strings.Contains(got, "### Detail\n\nmore\n") {
		t.Errorf("a ### heading inside a taken section: %v, %q", err, got)
	}
}

// TestSpliceSectionsCutsTheLastSectionAtAHeadingOfItsLevel: the last
// skeleton section ends at the next heading of its level or higher, in the
// draft and in the reviewed file, so text after it is neither taken nor lost.
func TestSpliceSectionsCutsTheLastSectionAtAHeadingOfItsLevel(t *testing.T) {
	heads := SpecSkeletonHeadings()
	tail := "## Appendix\n\nhand-written notes.\n"
	current := joinSections(scopedSpecSections) + tail
	drafted := rewriteEverySection(scopedSpecSections) + "## Extra\n\nmodel-invented.\n"
	got, err := SpliceSections(current, drafted, heads, []string{"## Open questions"})
	if err != nil {
		t.Fatal(err)
	}
	want := joinSections(scopedSpecSections[:7]) + "## Open questions\n\nREWRITTEN ## Open questions\n\n" + tail
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestSpliceSectionsIgnoresHeadingsInCodeFencesAndRefusesAmbiguity: a skeleton
// heading line inside a fenced block is not a heading, and where one is there
// the splice refuses, since the plan checks would read that line as a heading.
func TestSpliceSectionsIgnoresHeadingsInCodeFencesAndRefusesAmbiguity(t *testing.T) {
	heads := SpecSkeletonHeadings()
	fence := "```"
	fencedOther := strings.Replace(joinSections(scopedSpecSections), "The refund path.\n", fence+"\n## Mentioned\n"+fence+"\nThe refund path.\n", 1)
	if _, err := SpliceSections(fencedOther, rewriteEverySection(scopedSpecSections), heads, []string{"## Risks"}); err != nil {
		t.Errorf("a non-skeleton heading in a fence: %v", err)
	}
	fenced := strings.Replace(joinSections(scopedSpecSections), "The refund path.\n", fence+"\n## Risks\n"+fence+"\nThe refund path.\n", 1)
	if _, err := SpliceSections(fenced, rewriteEverySection(scopedSpecSections), heads, []string{"## Problem"}); err == nil {
		t.Error("a skeleton heading inside a fence of the reviewed file: want a refusal")
	}
	if _, err := SpliceSections(joinSections(scopedSpecSections), fenced, heads, []string{"## Problem"}); err == nil {
		t.Error("a skeleton heading inside a fence of the redraft: want a refusal")
	}
	if text, ok := SectionText(fenced, heads, "## Risks"); !ok || strings.Contains(text, fence) {
		t.Errorf("SectionText must find the real heading, not the fenced one: %q", text)
	}
}
