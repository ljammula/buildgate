package request

import (
	"fmt"
	"strings"

	"buildgate/internal/ticketspec"
)

// sectionSpan is one skeleton section of a document: the bytes from its
// heading line to the line before the next skeleton heading that follows it.
// The last section ends at the first later heading line of its own level or a
// higher one, or at the end of the document.
type sectionSpan struct {
	heading    string
	start, end int
	// startLine is the index of the heading line; endLine the index of the
	// first line after the section.
	startLine, endLine int
}

// parsedDoc is a document read for its skeleton sections.
type parsedDoc struct {
	lines       []string
	starts      []int
	spans       []sectionSpan
	occurrences map[string]int
	// fenced is the skeleton heading text found on a line inside a code fence:
	// ValidateSpecSkeleton and ValidateTicketPlan do not know about fences, so
	// where this is set the headings they find may differ from the ones read
	// here, and a splice is refused.
	fenced string
}

// fenceOpen returns the fence character and length a line opens, or 0.
func fenceOpen(line string) (byte, int) {
	return ticketspec.FenceMarker(strings.TrimSpace(line))
}

// fenceCloses reports whether line closes a fence of ch opened with n.
func fenceCloses(line string, ch byte, n int) bool {
	return ticketspec.ClosesFence(strings.TrimSpace(line), ch, n)
}

// parseSkeleton locates every heading of headings in content the way
// ValidateSpecSkeleton and ValidateTicketPlan do (a line whose trimmed text is
// the heading, searched in order), except that a line inside a fenced code
// block is not a heading. A heading content lacks is absent from the spans and
// does not move the search, so its neighbours are still found.
func parseSkeleton(content string, headings []string) parsedDoc {
	d := parsedDoc{lines: strings.SplitAfter(content, "\n"), occurrences: map[string]int{}}
	d.starts = make([]int, len(d.lines))
	inFence := make([]bool, len(d.lines))
	offset := 0
	var fenceCh byte
	fenceLen := 0
	for i, line := range d.lines {
		d.starts[i] = offset
		offset += len(line)
		if fenceCh != 0 {
			inFence[i] = true
			if fenceCloses(line, fenceCh, fenceLen) {
				fenceCh, fenceLen = 0, 0
			}
			continue
		}
		if ch, n := fenceOpen(line); ch != 0 {
			fenceCh, fenceLen = ch, n
			inFence[i] = true
		}
	}
	isSkeleton := make(map[string]bool, len(headings))
	for _, h := range headings {
		isSkeleton[h] = true
	}
	for i, line := range d.lines {
		if inFence[i] && isSkeleton[strings.TrimSpace(line)] {
			d.fenced = strings.TrimSpace(line)
		}
		if !inFence[i] && isSkeleton[strings.TrimSpace(line)] {
			d.occurrences[strings.TrimSpace(line)]++
		}
	}
	searchFrom := 0
	for _, heading := range headings {
		for j := searchFrom; j < len(d.lines); j++ {
			if !inFence[j] && strings.TrimSpace(d.lines[j]) == heading {
				d.spans = append(d.spans, sectionSpan{heading: heading, start: d.starts[j], startLine: j})
				searchFrom = j + 1
				break
			}
		}
	}
	for i := range d.spans {
		s := &d.spans[i]
		s.end, s.endLine = len(content), len(d.lines)
		if i+1 < len(d.spans) {
			s.end, s.endLine = d.spans[i+1].start, d.spans[i+1].startLine
			continue
		}
		level := MarkdownHeadingLevel(d.lines[s.startLine])
		for j := s.startLine + 1; j < len(d.lines); j++ {
			if l := MarkdownHeadingLevel(d.lines[j]); !inFence[j] && l > 0 && l <= level {
				s.end, s.endLine = d.starts[j], j
				break
			}
		}
	}
	return d
}

func (d parsedDoc) span(heading string) (sectionSpan, bool) {
	for _, s := range d.spans {
		if s.heading == heading {
			return s, true
		}
	}
	return sectionSpan{}, false
}

func (d parsedDoc) count(heading string) int {
	return d.occurrences[heading]
}

// SectionText returns the text of the skeleton section heading opens in
// content (its heading line included), and false when content has no such
// heading.
func SectionText(content string, headings []string, heading string) (string, bool) {
	s, ok := parseSkeleton(content, headings).span(heading)
	if !ok {
		return "", false
	}
	return content[s.start:s.end], true
}

// SectionTexts returns every section with the requested heading, in document
// order. It is used for anchors because an operator-shaped spec may repeat a
// heading and a quoted passage can be under any of those occurrences.
func SectionTexts(content string, headings []string, heading string) []string {
	d := parseSkeleton(content, headings)
	var texts []string
	for _, s := range d.spans {
		if s.heading == heading {
			texts = append(texts, content[s.start:s.end])
		}
	}
	return texts
}

// draftedSection returns the text to take for heading from a redraft, or an
// error when the redraft's shape cannot be trusted to bound it: the next
// skeleton heading must follow it (so the section ends where the skeleton says
// it does), and no other heading of its level or a higher one may sit between
// them (a section the model added would be swallowed into this one).
func draftedSection(d parsedDoc, content string, headings []string, heading string) (string, error) {
	s, ok := d.span(heading)
	if !ok {
		return "", fmt.Errorf("the redraft has no %q section, which the review commented on", heading)
	}
	at := -1
	for i, h := range headings {
		if h == heading {
			at = i
		}
	}
	if at+1 < len(headings) {
		next := headings[at+1]
		n, found := d.span(next)
		if !found {
			return "", fmt.Errorf("the redraft has no %q section after %q, so %q cannot be bounded", next, heading, heading)
		}
		level := MarkdownHeadingLevel(d.lines[s.startLine])
		for j := s.startLine + 1; j < n.startLine; j++ {
			if l := MarkdownHeadingLevel(d.lines[j]); l > 0 && l <= level {
				return "", fmt.Errorf("the redraft has a heading %q inside %q that the skeleton does not have", strings.TrimSpace(d.lines[j]), heading)
			}
		}
	}
	return content[s.start:s.end], nil
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
// missing from current or from drafted, when the redraft does not bound a
// taken section the way the skeleton does (draftedSection), or when a skeleton
// heading appears inside a code fence in either document.
func SpliceSections(current, drafted string, headings []string, take []string) (string, error) {
	cur, dra := parseSkeleton(current, headings), parseSkeleton(drafted, headings)
	for _, d := range []struct {
		name string
		doc  parsedDoc
	}{{"reviewed document", cur}, {"redraft", dra}} {
		if d.doc.fenced != "" {
			return "", fmt.Errorf("the %s has the line %q inside a code block, so its sections cannot be told apart safely", d.name, d.doc.fenced)
		}
	}
	replace := make(map[string]string, len(take))
	for _, heading := range take {
		if cur.count(heading) > 1 || dra.count(heading) > 1 {
			return "", ErrAmbiguousSection
		}
		if _, ok := cur.span(heading); !ok {
			return "", fmt.Errorf("the reviewed document has no %q section, which the review commented on", heading)
		}
		text, err := draftedSection(dra, drafted, headings, heading)
		if err != nil {
			return "", err
		}
		replace[heading] = text
	}
	if len(cur.spans) == 0 {
		return current, nil
	}
	var out strings.Builder
	out.WriteString(current[:cur.spans[0].start])
	for i, s := range cur.spans {
		text, ok := replace[s.heading]
		if !ok {
			out.WriteString(current[s.start:s.end])
			continue
		}
		out.WriteString(text)
		// A drafted section that ends the draft may lack its final newline;
		// whatever follows in current must still start on a line of its own.
		if (i+1 < len(cur.spans) || s.end < len(current)) && !strings.HasSuffix(text, "\n") {
			out.WriteString("\n")
		}
	}
	last := cur.spans[len(cur.spans)-1]
	out.WriteString(current[last.end:])
	return out.String(), nil
}

// ErrAmbiguousSection means a scoped heading is repeated in either document.
// Text-based section replacement cannot safely choose one occurrence, so the
// caller must use its existing whole-document redraft path.
var ErrAmbiguousSection = fmt.Errorf("a scoped section heading occurs more than once")
