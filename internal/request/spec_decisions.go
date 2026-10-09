package request

import (
	"errors"
	"regexp"
	"strings"
)

// The marker openDecisionItem looks for, "[NEEDS DECISION]", is what
// agent/pi/scripts/draft_spec.prompt.md has the spec draft write on a choice
// it leaves to the operator.

// openQuestionsHeading is the spec section those items are written under.
const openQuestionsHeading = "## Open questions"

// ErrOpenDecisions is wrapped by Approve's error when spec.md still asks the
// operator for a decision. The answer goes back through Reject, whose
// redraft writes it into every affected section, or the operator settles
// the item by editing the spec: an approved spec never carries an
// unanswered choice into planning.
var ErrOpenDecisions = errors.New("the spec has decisions left for you")

// openDecisionMaxRunes bounds one item in Approve's refusal.
const openDecisionMaxRunes = 200

// openDecisionItem matches a line that opens a decision item: the marker
// first on the line, after any list, sub-heading, quote or emphasis markup.
// A sentence that only mentions the marker ("No [NEEDS DECISION] items
// remain", "Resolved: ...") is not an item.
var openDecisionItem = regexp.MustCompile("^(?:#{3,6}[ \\t]+|>[ \\t]*)?(?:[-*+][ \\t]+|\\d+[.)][ \\t]+)?[*_`]*\\[NEEDS DECISION\\]")

// OpenDecisions returns the first line of each "[NEEDS DECISION]" item in
// content's "## Open questions" section, in order. The section runs from
// that heading to the next first- or second-level heading outside a fenced
// block: an item written as a sub-heading ("### 1. [NEEDS DECISION] ...")
// stays in it. A marker anywhere else (quoted in Scope, say) is not an item.
func OpenDecisions(content string) []string {
	var items []string
	inSection, inFence := false, false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(trimmed, "# ") || strings.HasPrefix(trimmed, "## ") {
			inSection = trimmed == openQuestionsHeading
			continue
		}
		if !inSection || !openDecisionItem.MatchString(trimmed) {
			continue
		}
		if runes := []rune(trimmed); len(runes) > openDecisionMaxRunes {
			trimmed = string(runes[:openDecisionMaxRunes]) + "..."
		}
		items = append(items, trimmed)
	}
	return items
}
