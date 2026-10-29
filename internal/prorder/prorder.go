// Package prorder decides which pull request each changed path belongs to
// under a repository's `.factory.yml` pr_order: an ordered list of path
// groups, each a list of patterns in the syntax of allowed_paths. A changed
// path joins the first group with a pattern it matches, and a path no group
// matches joins one more group after the listed ones. The package reads
// paths only, never a file's content or any prose.
package prorder

import (
	"fmt"
	"sort"
	"strings"

	"buildgate/internal/policy"
)

// Order is a pr_order: group i holds the patterns of group i+1 of the file.
type Order [][]string

// Index is the 0-based group of file: the first group with a matching
// pattern, or len(o) for a file no group matches.
func (o Order) Index(file string) int {
	for i, patterns := range o {
		if policy.PathAllowed(file, patterns) {
			return i
		}
	}
	return len(o)
}

// Groups splits changed into the files of each group the change touches, in
// group order, each group's files sorted and without repeats. A group the
// change does not touch has no entry.
func (o Order) Groups(changed []string) [][]string {
	byGroup := map[int]map[string]bool{}
	for _, file := range changed {
		i := o.Index(file)
		if byGroup[i] == nil {
			byGroup[i] = map[string]bool{}
		}
		byGroup[i][file] = true
	}
	var order []int
	for i := range byGroup {
		order = append(order, i)
	}
	sort.Ints(order)
	groups := make([][]string, 0, len(order))
	for _, i := range order {
		files := make([]string, 0, len(byGroup[i]))
		for file := range byGroup[i] {
			files = append(files, file)
		}
		sort.Strings(files)
		groups = append(groups, files)
	}
	return groups
}

// Touched is the 0-based indexes of the groups changed touches, ascending.
func (o Order) Touched(changed []string) []int {
	seen := map[int]bool{}
	var touched []int
	for _, file := range changed {
		if i := o.Index(file); !seen[i] {
			seen[i] = true
			touched = append(touched, i)
		}
	}
	sort.Ints(touched)
	return touched
}

// Label names group i (0-based) for a person: its number and patterns, or
// the files no group lists.
func (o Order) Label(i int) string {
	if i < 0 || i >= len(o) {
		return "the files no pr_order group lists"
	}
	return fmt.Sprintf("pr_order group %d (%s)", i+1, strings.Join(o[i], ", "))
}

// MixedReason says why changed may not be one pull request: it touches more
// than one group, each named with one of its files. "" when it touches at
// most one.
func (o Order) MixedReason(changed []string) string {
	touched := o.Touched(changed)
	if len(touched) < 2 {
		return ""
	}
	first := map[int]string{}
	for _, file := range changed {
		i := o.Index(file)
		if cur, ok := first[i]; !ok || file < cur {
			first[i] = file
		}
	}
	parts := make([]string, 0, len(touched))
	for _, i := range touched {
		parts = append(parts, fmt.Sprintf("%s in %s", first[i], o.Label(i)))
	}
	return "the change touches more than one pr_order group: " + strings.Join(parts, "; ")
}

// OutsideReason says why changed leaves group only (0-based): the first file
// of another group, named with that group. "" when every file is in only.
func (o Order) OutsideReason(only int, changed []string) string {
	files := append([]string(nil), changed...)
	sort.Strings(files)
	for _, file := range files {
		if i := o.Index(file); i != only {
			return fmt.Sprintf("%s belongs to %s, but this pull request is limited to %s", file, o.Label(i), o.Label(only))
		}
	}
	return ""
}

// Part is one carved pull request: the 0-based group it holds and the paths
// of the change that go with it.
type Part struct {
	Group int
	Files []string
}

// Carve splits changed into one part per group it touches, in group order.
// A harness byproduct path (policy.ExcludeHarnessByproducts) decides no group
// and goes with the last part, so that the parts together hold every changed
// path. A change that touches no group has no part.
func (o Order) Carve(changed []string) []Part {
	kept := policy.ExcludeHarnessByproducts(changed)
	isKept := make(map[string]bool, len(kept))
	for _, file := range kept {
		isKept[file] = true
	}
	var byproducts []string
	for _, file := range changed {
		if !isKept[file] {
			byproducts = append(byproducts, file)
		}
	}
	files := o.Groups(kept)
	parts := make([]Part, 0, len(files))
	for _, group := range files {
		parts = append(parts, Part{Group: o.Index(group[0]), Files: group})
	}
	if len(parts) > 0 && len(byproducts) > 0 {
		last := &parts[len(parts)-1]
		last.Files = append(last.Files, byproducts...)
		sort.Strings(last.Files)
	}
	return parts
}
