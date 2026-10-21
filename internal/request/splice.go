package request

import (
	"fmt"
	"strings"
)

// sectionSpan is one skeleton section of a document: the bytes from its
// heading line to the line before the next skeleton heading that follows it
// (or to the end of the document).
type sectionSpan struct {
	heading    string
	start, end int
}

// skeletonSections locates every heading of headings in content the way
// ValidateSpecSkeleton and ValidateTicketPlan do (a line whose trimmed text is
// the heading, searched in order) and returns the section each one opens, in
// heading order. A heading content lacks is absent from the result and does
// not move the search, so its neighbours are still found. Offsets are byte
// offsets into content; a section ends where the next found heading's line
// begins, so the sections and the text before the first one tile content
// exactly.
func skeletonSections(content string, headings []string) []sectionSpan {
	lines := strings.SplitAfter(content, "\n")
	starts := make([]int, len(lines))
	offset := 0
	for i, line := range lines {
		starts[i] = offset
		offset += len(line)
	}
	var spans []sectionSpan
	searchFrom := 0
	for _, heading := range headings {
		for j := searchFrom; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == heading {
				spans = append(spans, sectionSpan{heading: heading, start: starts[j]})
				searchFrom = j + 1
				break
			}
		}
	}
	for i := range spans {
		spans[i].end = len(content)
		if i+1 < len(spans) {
			spans[i].end = spans[i+1].start
		}
	}
	return spans
}

// SectionText returns the text of the skeleton section heading opens in
// content (its heading line included), and false when content has no such
// heading.
func SectionText(content string, headings []string, heading string) (string, bool) {
	for _, s := range skeletonSections(content, headings) {
		if s.heading == heading {
			return content[s.start:s.end], true
		}
	}
	return "", false
}

// SpliceSections returns current with the sections named in take replaced by
// the same sections of drafted. headings is the document's skeleton (the
// required headings, in order). Every byte outside the taken sections is
// copied from current unchanged: the text before the first heading, the
// sections nobody commented on, line endings, a missing final newline. That
// is the guarantee a scoped redraft rests on: the drafting model writes a
// whole document, and buildgate, not the model, decides which of it lands.
//
// It refuses (a structural check, so it may) when a section to take is
// missing from current or from drafted.
func SpliceSections(current, drafted string, headings []string, take []string) (string, error) {
	replace := make(map[string]string, len(take))
	for _, heading := range take {
		if _, ok := SectionText(current, headings, heading); !ok {
			return "", fmt.Errorf("the reviewed document has no %q section, which the review commented on", heading)
		}
		text, ok := SectionText(drafted, headings, heading)
		if !ok {
			return "", fmt.Errorf("the redraft has no %q section, which the review commented on", heading)
		}
		replace[heading] = text
	}
	spans := skeletonSections(current, headings)
	if len(spans) == 0 {
		return current, nil
	}
	var out strings.Builder
	out.WriteString(current[:spans[0].start])
	for i, s := range spans {
		text, ok := replace[s.heading]
		if !ok {
			out.WriteString(current[s.start:s.end])
			continue
		}
		out.WriteString(text)
		// A drafted section that ends the draft may lack its final newline;
		// the next section of current must still start on a line of its own.
		if i+1 < len(spans) && !strings.HasSuffix(text, "\n") {
			out.WriteString("\n")
		}
	}
	return out.String(), nil
}
