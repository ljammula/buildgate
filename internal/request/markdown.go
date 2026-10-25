package request

import (
	"regexp"
	"strings"

	"buildgate/internal/ticketspec"
)

var yamlFrontMatterKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]+:`)

// FrontMatterBodyOffset returns the first line after a leading YAML
// front-matter block, or zero when the document does not begin with a closed
// block. The opening marker may have a leading UTF-8 BOM. To avoid treating a
// thematic break as metadata, the first non-blank line after the marker must
// look like a YAML key before a later --- or ... closes the block.
func FrontMatterBodyOffset(lines []string) int {
	if len(lines) == 0 || strings.TrimSpace(strings.TrimPrefix(lines[0], "\uFEFF")) != "---" {
		return 0
	}
	keyFound := false
	for i := 1; i < len(lines); i++ {
		marker := strings.TrimSpace(lines[i])
		if !keyFound {
			if marker == "" {
				continue
			}
			if !yamlFrontMatterKeyRE.MatchString(marker) {
				return 0
			}
			keyFound = true
			continue
		}
		if marker == "---" || marker == "..." {
			return i + 1
		}
	}
	return 0
}

// MarkdownHeadingLevel returns the level of an unindented ATX heading, or
// zero for every other line. Only the six Markdown heading levels count.
func MarkdownHeadingLevel(raw string) int {
	if raw == "" || raw != strings.TrimLeft(raw, " \t") || raw[0] != '#' {
		return 0
	}
	level := 0
	for level < len(raw) && raw[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(raw) || (raw[level] != ' ' && raw[level] != '\t') {
		return 0
	}
	return level
}

// NormalizeSpecHeading is the shared structural heading comparison. It
// ignores surrounding space and one trailing colon, and folds ASCII letters
// without changing non-ASCII characters.
func NormalizeSpecHeading(raw string) string {
	text := strings.TrimSpace(raw)
	if level := MarkdownHeadingLevel(text); level > 0 {
		text = strings.TrimSpace(text[level:])
	}
	text = strings.TrimSpace(strings.TrimSuffix(text, ":"))
	return asciiLower(text)
}

// HeadingLabel returns a heading's displayed text, retaining a trailing
// colon when the author wrote one.
func HeadingLabel(raw string) string {
	text := strings.TrimSpace(raw)
	if level := MarkdownHeadingLevel(text); level > 0 {
		return strings.TrimSpace(text[level:])
	}
	return text
}

func asciiLower(text string) string {
	bytes := []byte(text)
	for i, c := range bytes {
		if c >= 'A' && c <= 'Z' {
			bytes[i] = c + ('a' - 'A')
		}
	}
	return string(bytes)
}

// markdownLineMask marks lines that are structural content rather than
// document text: leading front matter, fenced blocks, and HTML comment
// blocks. It is computed once per document and shared by all request-parser
// readers so a heading or list cannot be found by one reader and hidden by
// another.
func markdownLineMask(lines []string) []bool {
	blocked := make([]bool, len(lines))
	bodyOffset := FrontMatterBodyOffset(lines)
	for i := 0; i < bodyOffset && i < len(blocked); i++ {
		blocked[i] = true
	}

	var fenceChar byte
	fenceLen := 0
	inHTMLComment := false
	for i, raw := range lines {
		if i < bodyOffset {
			continue
		}
		trimmed := strings.TrimSpace(raw)
		if fenceChar != 0 {
			blocked[i] = true
			if ticketspec.ClosesFence(trimmed, fenceChar, fenceLen) {
				fenceChar, fenceLen = 0, 0
			}
			continue
		}
		if inHTMLComment {
			blocked[i] = true
			if strings.Contains(raw, "-->") {
				inHTMLComment = false
			}
			continue
		}
		commentText := strings.TrimLeft(raw, " ")
		indent := len(raw) - len(commentText)
		if indent <= 3 && strings.HasPrefix(commentText, "<!--") {
			blocked[i] = true
			if !strings.Contains(raw, "-->") {
				inHTMLComment = true
			}
			continue
		}

		if ch, n := ticketspec.FenceMarker(trimmed); ch != 0 {
			blocked[i] = true
			fenceChar, fenceLen = ch, n
		}
	}
	return blocked
}

func lineBlocked(mask []bool, index int) bool {
	return index >= 0 && index < len(mask) && mask[index]
}
