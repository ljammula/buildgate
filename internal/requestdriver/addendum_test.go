package requestdriver

import (
	"strings"
	"testing"
)

func TestRenderBoundedAddendumSectionsKeepsOmissionNotesAtMinimumSize(t *testing.T) {
	sections := []boundedAddendumSection{
		{
			heading:      "\n## Reviewer comments to address",
			entries:      []string{"a reviewer comment that is too large for the remaining budget\n"},
			omittedLabel: "reviewer comments",
			maxEntries:   20,
		},
		{
			heading:      "\n## Findings",
			entries:      []string{"a finding that is too large for the remaining budget\n"},
			omittedLabel: "findings",
			maxEntries:   5,
		},
	}
	maxBytes := boundedAddendumSectionMinimumBytes(sections[0]) + boundedAddendumSectionMinimumBytes(sections[1])
	got, _ := renderBoundedAddendumSectionsWithCounts(sections, maxBytes)
	for _, want := range []string{
		"## Reviewer comments to address",
		"_1 more reviewer comments omitted._",
		"## Findings",
		"_1 more findings omitted._",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered addendum = %q, want it to contain %q", got, want)
		}
	}
}
