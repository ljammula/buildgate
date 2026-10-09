package memory

import (
	"errors"
	"strings"
	"unicode"
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

// controlRune reports a character a section line may not hold: an ASCII or Latin-1
// control character or DEL (an escape sequence starts with one), a format
// character (direction marks and overrides, zero-width characters, a BOM) or
// a line or paragraph separator.
func controlRune(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}

// listLine reports whether l is a line the block may hold: "- " then some
// text, on one line, with no marker, control or format character, or invalid
// UTF-8.
func listLine(l string) bool {
	if !strings.HasPrefix(l, "- ") || strings.TrimSpace(l[2:]) == "" {
		return false
	}
	if strings.IndexFunc(l, controlRune) >= 0 || !utf8.ValidString(l) {
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

// fenceRun returns the fence character and the length of the run of it that
// opens line after its leading spaces: three or more "`" or "~". rest is the
// line after the run.
func fenceRun(line string) (ch byte, n int, rest string) {
	t := strings.TrimLeft(line, " ")
	if t == "" || t[0] != '`' && t[0] != '~' {
		return 0, 0, ""
	}
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 {
		return 0, 0, ""
	}
	return t[0], n, t[n:]
}

// openCodeFence reports whether text leaves a Markdown code fence open. A
// fence opens on a line that starts with three or more "`" or "~" (a backtick
// fence's line holds no other backtick) and closes on a later line that is a
// run of the same character, at least as long, and nothing else.
func openCodeFence(text string) bool {
	var open byte
	length := 0
	for _, l := range strings.Split(text, "\n") {
		ch, n, rest := fenceRun(l)
		switch {
		case n == 0:
		case open == 0:
			if ch == '~' || !strings.Contains(rest, "`") {
				open, length = ch, n
			}
		case ch == open && n >= length && strings.TrimSpace(rest) == "":
			open, length = 0, 0
		}
	}
	return open != 0
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
			return nil, fenceErr("a block line is neither blank nor a single \"- \" line without control characters")
		}
	}
	return lines, nil
}

// ParseSection splits file around its fenced block. A file with no marker at
// all has no section, unless it ends inside a code fence (a section appended
// there could not be read back); anything else short of one clean block is
// ErrFence. Whatever it accepts, Render's output for it parses again.
func ParseSection(file []byte) (Section, error) {
	if !utf8.Valid(file) || strings.IndexByte(string(file), 0) >= 0 {
		return Section{}, fenceErr("not UTF-8 text without NUL")
	}
	text := string(file)
	if !strings.Contains(text, BeginMarker) && !strings.Contains(text, EndMarker) {
		if openCodeFence(text) {
			return Section{}, fenceErr("the file ends inside a code fence: close the fence by hand")
		}
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
// not be read back (a marker, a newline, a control character, no "- " start)
// is dropped. Before
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
