package memory

import "sort"

// Budget bounds the section: the number of "- " lines and their total bytes
// (each line counts one newline). A zero field takes its default.
type Budget struct{ Lines, Chars int }

const (
	DefaultBudgetLines = 40
	DefaultBudgetChars = 3000
)

// Candidate is a proposed addition or removal of one line.
type Candidate struct {
	Line   string
	Remove bool
}

// KnownLine is what the store knows about a line the section holds, keyed by
// exact line text. A current line absent from the map is a human line.
type KnownLine struct {
	RetireProposed  bool
	LastConfirmedAt string
}

// FitResult is the outcome of Fit. Lines is the section after the change.
type FitResult struct {
	Lines      []string
	Added      []string
	Removed    []string
	NotFitting []string
}

func (b Budget) orDefault() Budget {
	if b.Lines <= 0 {
		b.Lines = DefaultBudgetLines
	}
	if b.Chars <= 0 {
		b.Chars = DefaultBudgetChars
	}
	return b
}

func chars(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

func fits(lines []string, extra string, b Budget) bool {
	return len(lines)+1 <= b.Lines && chars(lines)+len(extra)+1 <= b.Chars
}

// victims lists the indices of removable lines in the order to remove them:
// retire-proposed first, then oldest confirmation (empty is oldest), ties by
// position. Lines in keep (added in this call) are never listed.
func victims(lines []string, known map[string]KnownLine, keep map[string]bool) []int {
	var idx []int
	for i, l := range lines {
		if _, ok := known[l]; ok && !keep[l] {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ka, kb := known[lines[idx[a]]], known[lines[idx[b]]]
		if ka.RetireProposed != kb.RetireProposed {
			return ka.RetireProposed
		}
		return ka.LastConfirmedAt < kb.LastConfirmedAt
	})
	return idx
}

func without(lines []string, drop map[int]bool) []string {
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if !drop[i] {
			out = append(out, l)
		}
	}
	return out
}

// planRoom returns the indices to remove so that extra fits, or false when no
// removal order within the change allowance makes it fit.
func planRoom(lines []string, known map[string]KnownLine, keep map[string]bool, extra string, b Budget, allowance int) ([]int, bool) {
	if fits(lines, extra, b) {
		return nil, true
	}
	var chosen []int
	drop := map[int]bool{}
	for _, i := range victims(lines, known, keep) {
		if len(chosen)+2 > allowance {
			return nil, false
		}
		chosen = append(chosen, i)
		drop[i] = true
		if fits(without(lines, drop), extra, b) {
			return chosen, true
		}
	}
	return nil, false
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// retireProposedLines lists, in position order, the known retire-proposed
// lines that fit in the change allowance.
func retireProposedLines(lines []string, known map[string]KnownLine, allowance int) []string {
	var out []string
	for _, l := range lines {
		if len(out) >= allowance {
			break
		}
		if k, ok := known[l]; ok && k.RetireProposed && !contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// Fit changes the section's lines within the budget and at most maxChanges
// additions plus removals (zero or less: none). Retire-proposed known lines
// are removed first. An addition that needs room evicts known lines (retire-
// proposed, then least recently confirmed); a human line is never removed.
// What cannot be added is returned in NotFitting. The result is deterministic.
func Fit(current []string, known map[string]KnownLine, add []string, budget Budget, maxChanges int) FitResult {
	b := budget.orDefault()
	res := FitResult{}
	lines := append([]string(nil), current...)
	res.Removed = retireProposedLines(lines, known, maxChanges)
	drop := map[int]bool{}
	for i, l := range lines {
		if contains(res.Removed, l) {
			drop[i] = true
		}
	}
	lines = without(lines, drop)
	changes := len(res.Removed)
	keep := map[string]bool{}
	for _, l := range add {
		if contains(current, l) || contains(res.Added, l) || contains(res.NotFitting, l) {
			continue
		}
		plan, ok := planRoom(lines, known, keep, l, b, maxChanges-changes)
		if !ok || !listLine(l) || changes >= maxChanges {
			res.NotFitting = append(res.NotFitting, l)
			continue
		}
		drop = map[int]bool{}
		for _, i := range plan {
			res.Removed = append(res.Removed, lines[i])
			drop[i] = true
		}
		lines = append(without(lines, drop), l)
		res.Added = append(res.Added, l)
		keep[l] = true
		changes += len(plan) + 1
	}
	res.Lines = lines
	return res
}
