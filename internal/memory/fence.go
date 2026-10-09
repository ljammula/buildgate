package memory

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// The fenced block a memory change may edit, and nothing else in the file.
const (
	BeginMarker    = "<!-- buildgate:memory:begin v1 -->"
	EndMarker      = "<!-- buildgate:memory:end -->"
	SectionHeading = "## Working in this repository"
)

// ErrFence wraps every refusal to read an AGENTS.md whose fence is absent in
// part, doubled, damaged or ambiguous: such a file is never edited.
var ErrFence = errors.New("memory: AGENTS.md fence is ambiguous")

// Section is a file split around its fenced block.
type Section struct {
	Present bool
	Before  string   // file bytes before the begin marker line, verbatim
	After   string   // file bytes after the end marker line, verbatim
	Lines   []string // the "- ..." lines of the block, verbatim, in order
}

func fenceErr(why string) error { return errors.Join(ErrFence, errors.New(why)) }

// listLine reports whether l is a line the block may hold: "- " then some
// text, on one line, with no marker, NUL or invalid UTF-8.
func listLine(l string) bool {
	if !strings.HasPrefix(l, "- ") || strings.TrimSpace(l[2:]) == "" {
		return false
	}
	if strings.ContainsAny(l, "\r\n\x00") || !utf8.ValidString(l) {
		return false
	}
	return !strings.Contains(l, BeginMarker) && !strings.Contains(l, EndMarker)
}

// markerLine returns the byte offset of the line that is exactly marker
// (ending in "\n" or the end of the file) and the offset just past it.
func markerLine(file, marker string) (start, next int, ok bool) {
	for pos := 0; pos < len(file); {
		end := strings.IndexByte(file[pos:], '\n')
		line, adv := file[pos:], len(file)-pos
		if end >= 0 {
			line, adv = file[pos:pos+end], end+1
		}
		if line == marker {
			return pos, pos + adv, true
		}
		pos += adv
	}
	return 0, 0, false
}

// openCodeFence reports whether text leaves a Markdown code fence open.
func openCodeFence(text string) bool {
	open := false
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimLeft(l, " ")
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			open = !open
		}
	}
	return open
}

// splitMarkers locates the one begin and one end marker line.
func splitMarkers(file string) (bStart, bNext, eStart, eNext int, err error) {
	if strings.Count(file, BeginMarker) != 1 || strings.Count(file, EndMarker) != 1 {
		return 0, 0, 0, 0, fenceErr("need exactly one begin and one end marker")
	}
	bStart, bNext, okB := markerLine(file, BeginMarker)
	eStart, eNext, okE := markerLine(file, EndMarker)
	switch {
	case !okB || !okE:
		return 0, 0, 0, 0, fenceErr("a marker is not alone at column 0 on its line")
	case eStart < bNext:
		return 0, 0, 0, 0, fenceErr("end marker before begin marker")
	case openCodeFence(file[:bStart]):
		return 0, 0, 0, 0, fenceErr("markers sit inside a code fence")
	}
	return bStart, bNext, eStart, eNext, nil
}

// blockLines reads the block between the markers: blank lines, the heading,
// then blank lines and "- " lines.
func blockLines(block string) ([]string, error) {
	if strings.ContainsRune(block, '\r') {
		return nil, fenceErr("a carriage return inside the block")
	}
	rows := strings.Split(block, "\n")
	if rows[len(rows)-1] == "" { // the block ends in a newline
		rows = rows[:len(rows)-1]
	}
	i := 0
	for i < len(rows) && rows[i] == "" {
		i++
	}
	if i >= len(rows) || rows[i] != SectionHeading {
		return nil, fenceErr("the block does not open with the section heading")
	}
	lines := []string{}
	for _, r := range rows[i+1:] {
		switch {
		case r == "":
		case listLine(r):
			lines = append(lines, r)
		default:
			return nil, fenceErr("a block line is neither blank nor a single \"- \" line")
		}
	}
	return lines, nil
}

// ParseSection splits file around its fenced block. A file with no marker at
// all has no section; anything else short of one clean block is ErrFence.
func ParseSection(file []byte) (Section, error) {
	if !utf8.Valid(file) || strings.IndexByte(string(file), 0) >= 0 {
		return Section{}, fenceErr("not UTF-8 text without NUL")
	}
	text := string(file)
	if !strings.Contains(text, BeginMarker) && !strings.Contains(text, EndMarker) {
		return Section{Before: text}, nil
	}
	bStart, bNext, eStart, eNext, err := splitMarkers(text)
	if err != nil {
		return Section{}, err
	}
	lines, err := blockLines(text[bNext:eStart])
	if err != nil {
		return Section{}, err
	}
	return Section{Present: true, Before: text[:bStart], After: text[eNext:], Lines: lines}, nil
}

// separator is what Render puts between the text before a new section and its
// begin marker: a newline so the marker starts a line, and for a section that
// is appended a blank line.
func (s Section) separator() string {
	b := s.Before
	if b == "" {
		return ""
	}
	sep := ""
	if !strings.HasSuffix(b, "\n") {
		sep = "\n"
	}
	if !s.Present && b+sep != "\n" && !strings.HasSuffix(b+sep, "\n\n") {
		sep += "\n"
	}
	return sep
}

// Render writes Before, the block with lines, and After. A line that could
// not be read back (a marker, a newline, no "- " start) is dropped. Before
// and After are written verbatim except that a new section is separated from
// the text before it by a blank line.
func (s Section) Render(lines []string) []byte {
	var b strings.Builder
	b.WriteString(s.Before)
	b.WriteString(s.separator())
	b.WriteString(BeginMarker + "\n" + SectionHeading + "\n\n")
	for _, l := range lines {
		if listLine(l) {
			b.WriteString(l + "\n")
		}
	}
	b.WriteString(EndMarker + "\n")
	b.WriteString(s.After)
	return []byte(b.String())
}
