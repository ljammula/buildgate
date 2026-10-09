package memory

import (
	"errors"
	"fmt"
)

// Budget bounds the section: the number of "- " lines and their total bytes
// (each line counts one newline). A zero field takes its default.
type Budget struct{ Lines, Chars int }

const (
	DefaultBudgetLines = 40
	DefaultBudgetChars = 3000
	// MaxChangesPerRequest is how many lines one memory request may add and
	// remove in total.
	MaxChangesPerRequest = 5
)

// ErrApply wraps every refusal of Apply.
var ErrApply = errors.New("memory: change refused")

// The reasons Apply refuses a change.
const (
	ApplyOverLines   = "lines"
	ApplyOverChars   = "chars"
	ApplyOverChanges = "changes"
	ApplyMissing     = "missing"
	ApplyUnreadable  = "unreadable"
	ApplyBothWays    = "added_and_removed"
)

// ApplyError says why Apply refused: What is one of the Apply* reasons, Have
// and Limit the two numbers compared (zero for a refusal about one line), and
// Line the line concerned, clipped.
type ApplyError struct {
	What        string
	Have, Limit int
	Line        string
}

func (e *ApplyError) Error() string {
	switch e.What {
	case ApplyOverLines:
		return fmt.Sprintf("%v: the section would hold %d lines, over its budget of %d", ErrApply, e.Have, e.Limit)
	case ApplyOverChars:
		return fmt.Sprintf("%v: the section would hold %d characters, over its budget of %d", ErrApply, e.Have, e.Limit)
	case ApplyOverChanges:
		return fmt.Sprintf("%v: %d lines added and removed, over the limit of %d for one request", ErrApply, e.Have, e.Limit)
	case ApplyMissing:
		return fmt.Sprintf("%v: the line to remove is not in the section (%q)", ErrApply, e.Line)
	case ApplyBothWays:
		return fmt.Sprintf("%v: a line is both added and removed (%q)", ErrApply, e.Line)
	}
	return fmt.Sprintf("%v: a line to add is not a single \"- \" line (%q)", ErrApply, e.Line)
}

func (e *ApplyError) Unwrap() error { return ErrApply }

// OrDefault fills a zero field with its default.
func (b Budget) OrDefault() Budget {
	if b.Lines <= 0 {
		b.Lines = DefaultBudgetLines
	}
	if b.Chars <= 0 {
		b.Chars = DefaultBudgetChars
	}
	return b
}

// Used is what lines take of a budget: their number and their bytes, one
// newline each.
func Used(lines []string) (count, chars int) {
	for _, l := range lines {
		chars += len(l) + 1
	}
	return len(lines), chars
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func dedupe(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if !contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// Apply returns current without the remove lines and with the add lines
// appended. Each remove line must be in current exactly as written (every
// copy of it goes); an add line already in current is not a change. Every
// other line of current is kept verbatim and in order. It refuses, with an
// *ApplyError naming the numbers, a change of more than maxChanges lines or a
// result that adds a line and is over the budget's lines or characters. It
// never chooses a line to drop: making room is the caller's remove list.
func Apply(current []string, add, remove []string, budget Budget, maxChanges int) ([]string, error) {
	b := budget.OrDefault()
	remove = dedupe(remove)
	for _, l := range remove {
		if !contains(current, l) {
			return nil, &ApplyError{What: ApplyMissing, Line: clip(l)}
		}
	}
	var adding []string
	for _, l := range dedupe(add) {
		switch {
		case !listLine(l):
			return nil, &ApplyError{What: ApplyUnreadable, Line: clip(l)}
		case contains(remove, l):
			return nil, &ApplyError{What: ApplyBothWays, Line: clip(l)}
		case !contains(current, l):
			adding = append(adding, l)
		}
	}
	if n := len(adding) + len(remove); n > maxChanges {
		return nil, &ApplyError{What: ApplyOverChanges, Have: n, Limit: maxChanges}
	}
	lines := make([]string, 0, len(current)+len(adding))
	for _, l := range current {
		if !contains(remove, l) {
			lines = append(lines, l)
		}
	}
	lines = append(lines, adding...)
	count, chars := Used(lines)
	switch {
	case len(adding) == 0: // a removal alone never needs room
	case count > b.Lines:
		return nil, &ApplyError{What: ApplyOverLines, Have: count, Limit: b.Lines}
	case chars > b.Chars:
		return nil, &ApplyError{What: ApplyOverChars, Have: chars, Limit: b.Chars}
	}
	return lines, nil
}
