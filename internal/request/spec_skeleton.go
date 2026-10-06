package request

import (
	"fmt"
	"strings"
)

// requiredSpecHeadings is the fixed skeleton agent/pi/scripts/draft_spec.py
// instructs the model to produce, in order:
// a spec, deliberately not a plan -- no file lists, no steps. Decomposing
// into tickets with a plan is a later pass.
var requiredSpecHeadings = []string{
	"# Spec",
	"## Problem",
	"## Scope",
	"## Non-goals",
	"## Affected services and packages",
	"## Acceptance criteria",
	"## Risks",
	"## Open questions",
}

// acceptanceCriteriaHeading names which entry of requiredSpecHeadings
// ValidateSpecSkeleton also requires non-empty content under.
const acceptanceCriteriaHeading = "## Acceptance criteria"

// ValidateSpecSkeleton checks that content contains every heading in
// requiredSpecHeadings, in order, and that the "## Acceptance criteria"
// section has at least one non-blank line of content underneath it
// before the next heading (or end of document). It is a pure function --
// no I/O, no model calls -- so the request driver can call it against a
// drafted spec.md before ever advancing the request out of
// spec_drafting, exactly the same "agent output is evidence, the factory
// decides" split policy.EvaluateRunInput's own gates already use for a
// build.
//
// Headings are matched by exact, trimmed line equality (a heading line's
// only content, after trimming surrounding whitespace, must be the
// heading text itself) -- a model that writes "## Acceptance Criteria"
// (different case) or folds a heading into running prose fails this
// check, deliberately: the driver's whole point is a skeleton the
// operator can rely on being present verbatim, not something a human (or
// a later plan-drafting pass) has to fuzzy-match.
//
// A heading present but out of order (e.g. "## Scope" appearing before
// "## Problem") is reported as the earlier-in-order heading missing, not
// as "out of order": this function's own sequential search only looks
// forward from the last heading it found, so an out-of-order heading is
// never found for the earlier requirement -- see the returned error's
// own wording.
func ValidateSpecSkeleton(content string) error {
	lines := strings.Split(content, "\n")
	positions := make([]int, len(requiredSpecHeadings))
	searchFrom := 0
	for i, heading := range requiredSpecHeadings {
		idx := -1
		for j := searchFrom; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == heading {
				idx = j
				break
			}
		}
		if idx == -1 {
			return fmt.Errorf("spec is missing required heading %q, or it appears out of order (expected headings in order: %s)", heading, strings.Join(requiredSpecHeadings, ", "))
		}
		positions[i] = idx
		searchFrom = idx + 1
	}

	acceptanceIndex := -1
	for i, heading := range requiredSpecHeadings {
		if heading == acceptanceCriteriaHeading {
			acceptanceIndex = i
			break
		}
	}
	sectionStart := positions[acceptanceIndex] + 1
	sectionEnd := len(lines)
	if acceptanceIndex+1 < len(positions) {
		sectionEnd = positions[acceptanceIndex+1]
	}
	for _, line := range lines[sectionStart:sectionEnd] {
		if strings.TrimSpace(line) != "" {
			return nil
		}
	}
	return fmt.Errorf("spec's %q section has no content", acceptanceCriteriaHeading)
}
