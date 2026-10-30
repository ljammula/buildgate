package reproduction

import (
	"regexp"
	"strconv"
)

// TestSpan is one test of a file, as a runner's host parse reads it: its ID
// and the lines it spans, from 1 and inclusive.
type TestSpan struct {
	ID         string
	Start, End int
}

// LineRange is a run of lines of a file, from 1 and inclusive.
type LineRange struct{ Start, End int }

// LineChanges is where a change touched a file, on the file's result side:
// the ranges of lines it added or rewrote, and the lines after which it only
// removed lines (0 for the top of the file).
type LineChanges struct {
	Added        []LineRange
	DeletedAfter []int
}

var hunkHeader = regexp.MustCompile(`(?m)^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParseHunks reads the hunk headers of a zero-context unified diff of one
// file (git diff -U0) into the line changes on its new side.
func ParseHunks(diff []byte) LineChanges {
	var c LineChanges
	for _, m := range hunkHeader.FindAllSubmatch(diff, -1) {
		start, _ := strconv.Atoi(string(m[1]))
		count := 1
		if len(m[2]) > 0 {
			count, _ = strconv.Atoi(string(m[2]))
		}
		if count == 0 {
			c.DeletedAfter = append(c.DeletedAfter, start)
			continue
		}
		c.Added = append(c.Added, LineRange{Start: start, End: start + count - 1})
	}
	return c
}

// touches reports whether the change added or rewrote a line of the span, or
// removed lines from inside it (between two of its lines).
func (c LineChanges) touches(s TestSpan) bool {
	for _, r := range c.Added {
		if r.Start <= s.End && r.End >= s.Start {
			return true
		}
	}
	for _, after := range c.DeletedAfter {
		if after >= s.Start && after < s.End {
			return true
		}
	}
	return false
}
