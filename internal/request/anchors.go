package request

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// RejectionAnchor is one note of a review rejection tied to a place in a
// reviewed file: the file, optionally a section heading in it, optionally a
// numbered item under that heading. A reviewer who writes "does not say
// which account" against acceptance criterion 2 hands the redraft the
// criterion, not a sentence it has to locate.
type RejectionAnchor struct {
	// Path is the request-relative file the note is about ("spec.md",
	// "tickets/001.spec.md", "oracle/RUN_COMMAND.txt").
	Path string `json:"path"`
	// Section is the heading the note is under, as written in the file
	// ("## Acceptance criteria"); empty for a note on the whole file.
	Section string `json:"section,omitempty"`
	// Item is the 1-based numbered item under Section; 0 for a note on the
	// section as a whole.
	Item int `json:"item,omitempty"`
	// Note is the reviewer's text, one line.
	Note string `json:"note"`
}

const (
	// maxRejectionAnchors bounds one rejection's anchored notes: a review
	// with more separate points than this is a redraft from scratch.
	maxRejectionAnchors = 50
	maxAnchorPathLen    = 200
	maxAnchorSectionLen = 200
	maxAnchorNoteLen    = 2000
	maxAnchorItem       = 9999
)

// anchorPathRE is the shape of a request-relative reviewed file: no
// whitespace, nothing a feedback line could be broken with.
var anchorPathRE = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// oneLine collapses every run of whitespace and control characters, line
// breaks included, to one space: an anchor is rendered as one list item of the feedback a drafting
// model reads, and a line break in it could open a heading of its own.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// NormalizeRejectionAnchors validates anchors and returns them with every
// field on one line. The API accepts them from any caller holding the write
// token, so each bound is checked here and not left to the console.
func NormalizeRejectionAnchors(anchors []RejectionAnchor) ([]RejectionAnchor, error) {
	if len(anchors) > maxRejectionAnchors {
		return nil, fmt.Errorf("a rejection carries at most %d anchored notes, got %d", maxRejectionAnchors, len(anchors))
	}
	out := make([]RejectionAnchor, 0, len(anchors))
	for i, a := range anchors {
		n := i + 1
		a.Section, a.Note = oneLine(a.Section), oneLine(a.Note)
		switch {
		case a.Path == "" || len(a.Path) > maxAnchorPathLen || !anchorPathRE.MatchString(a.Path):
			return nil, fmt.Errorf("anchored note %d: path must be a request-relative file name of at most %d characters", n, maxAnchorPathLen)
		case len(a.Section) > maxAnchorSectionLen:
			return nil, fmt.Errorf("anchored note %d: section is longer than %d bytes", n, maxAnchorSectionLen)
		case a.Item < 0 || a.Item > maxAnchorItem:
			return nil, fmt.Errorf("anchored note %d: item must be between 0 and %d", n, maxAnchorItem)
		case a.Item > 0 && a.Section == "":
			return nil, fmt.Errorf("anchored note %d: an item needs the section it is under", n)
		case a.Note == "":
			return nil, fmt.Errorf("anchored note %d: the note is empty", n)
		case len(a.Note) > maxAnchorNoteLen:
			return nil, fmt.Errorf("anchored note %d: the note is longer than %d bytes", n, maxAnchorNoteLen)
		}
		out = append(out, a)
	}
	return out, nil
}

// AnchoredReason is the reason text a rejection with anchored notes is
// recorded under: one list item per anchor naming its place, then the
// reviewer's free note. anchors must already be normalized. It is the same
// string everywhere a reason is read (history, the revision snapshot, the
// CLI, the stage feedback file), so the drafter is told the exact item and
// no reader needs to know anchors exist.
func AnchoredReason(anchors []RejectionAnchor, note string) string {
	if len(anchors) == 0 {
		return note
	}
	var b strings.Builder
	for _, a := range anchors {
		b.WriteString("- " + a.Path)
		if a.Section != "" {
			b.WriteString(", " + a.Section)
		}
		if a.Item > 0 {
			fmt.Fprintf(&b, ", number %d", a.Item)
		}
		b.WriteString(": " + a.Note + "\n")
	}
	if note != "" {
		b.WriteString("\n" + note)
	}
	return strings.TrimRight(b.String(), "\n")
}
