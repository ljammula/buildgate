package request

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// templateHeadingsInOrder reports the first of headings that the prompt
// template does not carry as a line of its own, searching forward from the
// previous one -- the same exact, trimmed, in-order match
// ValidateSpecSkeleton and ValidateTicketPlan apply to the model's output.
func templateHeadingsInOrder(t *testing.T, template string, headings []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "agent", "pi", "scripts", template))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	from := 0
	for _, heading := range headings {
		found := false
		for i := from; i < len(lines); i++ {
			if strings.TrimSpace(lines[i]) == heading {
				from, found = i+1, true
				break
			}
		}
		if !found {
			t.Errorf("%s does not ask for heading %q (in validator order); a draft written from it fails validation", template, heading)
			return
		}
	}
}

// A prompt template is one half of a contract: it tells the model which
// headings to write, and the validator rejects a draft without them. These
// two tests fail when an edit to a template drops, renames or reorders a
// heading its validator requires.
func TestSpecPromptTemplateAsksForEveryRequiredSpecHeading(t *testing.T) {
	templateHeadingsInOrder(t, "draft_spec.prompt.md", requiredSpecHeadings)
}

func TestPlanPromptTemplateAsksForEveryRequiredTicketHeading(t *testing.T) {
	templateHeadingsInOrder(t, "plan_tickets.prompt.md", requiredTicketHeadings)
}
