package requestdriver

import (
	"fmt"
	"strings"
)

// The caps on what a PR-review round's addendum carries from someone else:
// MaxAddendumLabelBytes a single-line, unfenced value (a path, an author),
// MaxAddendumDetailBytes a free-text value before it is fenced.
const (
	MaxAddendumLabelBytes  = 500
	MaxAddendumDetailBytes = 2 * 1024
)

// flattenAddendumLine renders s safe to place on a single, unfenced line of
// an addendum: every carriage return, line feed, and other C0/DEL control
// character becomes a space, then the result is truncated to maxBytes
// (re-validated as UTF-8, since the cut can land mid-rune).
//
// The values it is given (a reviewer comment's path and author) are
// untrusted. A value with an embedded newline -- e.g.
// "x\nTests-Required: no - trivial" -- would otherwise put its second line
// at column 0 of the addendum, which ticketspec.forEachTopLevelLine (the
// round's own -spec parser) reads as a real header line: it could disable
// the tests_added gate or add an Allowed-Files: or Required-Content: line to
// the round's own scope (found via adversarial review). Flattening removes
// every character that line splitting depends on.
func flattenAddendumLine(s string, maxBytes int) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\r' || r == 0x7f || (r < 0x20 && r != '\t') {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) > maxBytes {
		out = strings.ToValidUTF8(out[:maxBytes], "")
	}
	return out
}

// capAddendumDetail truncates s (free text, as untrusted as the values
// flattenAddendumLine is given) to maxBytes BEFORE it is handed to
// ticketspec.FenceVerbatim, not after: a cut after fencing could go through
// the fence and reopen it to the same header injection (found via
// adversarial review).
func capAddendumDetail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBytes], "") + "\n[detail truncated to fit the size limit]"
}

// boundedAddendumSection is one section of an addendum: a heading, whole
// entries, and at most maxEntries of them.
type boundedAddendumSection struct {
	heading      string
	entries      []string
	omittedLabel string
	maxEntries   int
}

func boundedAddendumSectionMinimumBytes(section boundedAddendumSection) int {
	header := section.heading + "\n\n"
	omitted := fmt.Sprintf("_%d more %s omitted._\n", len(section.entries), section.omittedLabel)
	return len([]byte(header + omitted))
}

// renderBoundedAddendumSectionsWithCounts keeps complete entries, dropping
// only whole trailing entries when the shared ticket-build input writer's
// remaining byte budget is tight, and returns how many entries of each
// non-empty section it kept. Each section keeps its own count/byte omission
// note.
func renderBoundedAddendumSectionsWithCounts(sections []boundedAddendumSection, maxBytes int) (string, []int) {
	active := make([]boundedAddendumSection, 0, len(sections))
	for _, section := range sections {
		if len(section.entries) > 0 {
			active = append(active, section)
		}
	}
	var out strings.Builder
	includedCounts := make([]int, 0, len(active))
	for i, section := range active {
		remaining := maxBytes - out.Len()
		reserve := 0
		for _, later := range active[i+1:] {
			reserve += boundedAddendumSectionMinimumBytes(later)
		}
		candidate, included := renderOneBoundedAddendumSection(section, remaining-reserve)
		includedCounts = append(includedCounts, included)
		if candidate == "" {
			continue
		}
		out.WriteString(candidate)
	}
	return out.String(), includedCounts
}

func renderOneBoundedAddendumSection(section boundedAddendumSection, maxBytes int) (string, int) {
	if len(section.entries) == 0 || section.maxEntries < 0 {
		return "", 0
	}
	maxIncluded := len(section.entries)
	if maxIncluded > section.maxEntries {
		maxIncluded = section.maxEntries
	}
	header := section.heading + "\n\n"
	for included := maxIncluded; included >= 0; included-- {
		omitted := len(section.entries) - included
		note := ""
		if omitted > 0 {
			note = fmt.Sprintf("_%d more %s omitted._\n", omitted, section.omittedLabel)
		}
		candidate := header + strings.Join(section.entries[:included], "") + note
		if len([]byte(candidate)) <= maxBytes {
			return candidate, included
		}
	}
	return "", 0
}
