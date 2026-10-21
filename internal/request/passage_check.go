package request

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ticketAnchorPathRE is the shape of a ticket file a comment can be on.
var ticketAnchorPathRE = regexp.MustCompile(`^tickets/[A-Za-z0-9_-]+\.spec\.md$`)

// ErrBadAnchor is the refusal for a comment that names a place the document
// under review cannot have: another file, or a section the skeleton lacks. It
// is the caller's mistake (400), not text that moved (ErrPassageMoved, 409).
var ErrBadAnchor = errors.New("a comment names a place that is not part of the document")

// checkAnchorsAgainstFiles refuses a rejection whose comments are about text
// that is not in the file now: at spec_review each anchor must name spec.md,
// at plan_review a ticket file that exists, and a section it names must be a
// skeleton heading present in that file, an item no larger than the spec's
// criteria count, a quote a byte-for-byte substring of the anchored section
// (of the file when no section is named). A comment bound to text that has
// since been edited or redrafted would otherwise scope a redraft to the wrong
// place. Other states (oracle_review) have no such passages and pass. The
// failure wraps ErrBadAnchor (a place the document cannot have) or
// ErrPassageMoved (text that is gone) and names the comment by its number.
func checkAnchorsAgainstFiles(dataDir string, r *Request, anchors []RejectionAnchor) error {
	if r.State != StateSpecReview && r.State != StatePlanReview {
		return nil
	}
	files := map[string]string{}
	read := func(path string) (string, error) {
		if content, ok := files[path]; ok {
			return content, nil
		}
		b, err := os.ReadFile(filepath.Join(Dir(dataDir, r.ID), filepath.FromSlash(path)))
		if err != nil {
			return "", err
		}
		files[path] = string(b)
		return files[path], nil
	}
	for i, a := range anchors {
		if err := checkAnchor(r, a, read); err != nil {
			if errors.Is(err, ErrBadAnchor) {
				return fmt.Errorf("request %s: comment %d (%s): %w", r.ID, i+1, a.Path, err)
			}
			return fmt.Errorf("request %s: comment %d (%s): %v: read it again %w", r.ID, i+1, a.Path, err, ErrPassageMoved)
		}
	}
	return nil
}

func checkAnchor(r *Request, a RejectionAnchor, read func(string) (string, error)) error {
	isSpec := r.State == StateSpecReview
	if isSpec && a.Path != specFileName {
		return fmt.Errorf("a spec review comment is on spec.md: %w", ErrBadAnchor)
	}
	if !isSpec && !ticketAnchorPathRE.MatchString(a.Path) {
		return fmt.Errorf("a plan review comment is on a ticket file: %w", ErrBadAnchor)
	}
	if a.Section != "" && !slices.Contains(skeletonFor(a.Path), a.Section) {
		return fmt.Errorf("%q is not a section of %s: %w", a.Section, a.Path, ErrBadAnchor)
	}
	content, err := read(a.Path)
	if err != nil {
		return fmt.Errorf("the file is gone")
	}
	scope := content
	if a.Section != "" {
		text, ok := SectionText(content, skeletonFor(a.Path), a.Section)
		if !ok {
			return fmt.Errorf("the file has no %q section", a.Section)
		}
		scope = text
	}
	if a.Item > 0 && isSpec && a.Section == acceptanceCriteriaHeading {
		criteria, _ := SpecAcceptanceCriteria(content)
		if a.Item > len(criteria) {
			return fmt.Errorf("the spec has %d acceptance criteria, not %d", len(criteria), a.Item)
		}
	}
	if a.Quote != "" && !strings.Contains(scope, a.Quote) {
		return fmt.Errorf("the quoted passage is not in %s", sectionOrFile(a.Section))
	}
	return nil
}

func sectionOrFile(section string) string {
	if section == "" {
		return "the file"
	}
	return fmt.Sprintf("the %q section", section)
}
