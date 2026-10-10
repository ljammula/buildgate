package sandbox

import (
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// The name fold and the matching of a path against the table of instruction
// paths (review_instructions.go): the table indexed by first folded component,
// and the one-pass classification of a path into the outermost entry covering
// it, the spelling a mask of it has, or the proper prefix of a fixed path it
// ends in.

// foldName is the one fold behind every match and collision check: NFKD,
// then Unicode full case folding (so a sharp s folds to "ss"), then NFKD again
// because folding can leave a composed rune. It over-matches (safe).
func foldName(s string) string {
	ascii := true
	for i := 0; i < len(s) && ascii; i++ {
		ascii = s[i] < 0x80
	}
	if ascii {
		return strings.ToLower(s) // the full case fold of ASCII is its lower case
	}
	return norm.NFKD.String(cases.Fold().String(norm.NFKD.String(s)))
}

var foldedGit = foldName(".git")

type fixedPath struct{ canon, fold []string }

func foldComponents(parts []string) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = foldName(p)
	}
	return out
}

var (
	reviewFixedPaths = func() (out []fixedPath) {
		for _, e := range append(append([]string(nil), reviewInstructionDirs...), reviewInstructionFiles...) {
			parts := strings.Split(e, "/")
			out = append(out, fixedPath{parts, foldComponents(parts)})
		}
		return out
	}()
	// tableByFirst lists the table by first folded component, so a path
	// component that starts no entry costs one lookup: the base name of that
	// fold (its table spelling, "" for none) and the fixed paths it begins.
	tableByFirst = func() map[string]tableStart {
		m := map[string]tableStart{}
		for _, f := range reviewFixedPaths {
			e := m[f.fold[0]]
			e.fixed = append(e.fixed, f)
			m[f.fold[0]] = e
		}
		for _, name := range reviewInstructionBaseNames {
			e := m[foldName(name)]
			e.name = name
			m[foldName(name)] = e
		}
		return m
	}()
)

type tableStart struct {
	name  string
	fixed []fixedPath
}

func joinSlash(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// foldsAt reports whether want is the components of fold from index i on.
func foldsAt(fold []string, i int, want []string) bool {
	if len(fold)-i < len(want) {
		return false
	}
	for k, w := range want {
		if fold[i+k] != w {
			return false
		}
	}
	return true
}

// scanFold classifies a path in one pass over its components. n is the number
// of leading components forming the outermost table entry covering it, canon
// the spelling a mask of it has: the path's own directories above the entry,
// then the table's spelling of the entry. An entry (a fixed path or a base
// name) matches at any depth; the outermost is the match that ends first, so
// a/.claude/b/.github/instructions/x is covered by a/.claude. When nothing
// matches, lead is the number of leading components that end in a proper
// prefix of a fixed path (pkg/.github of pkg/.github/workflows/ci.yml is 2),
// the longest when there are several: a link there would redirect a fixed
// path. The work is the path's components plus, for each that starts an entry,
// the entries it starts.
func scanFold(parts, fold []string) (n int, canon string, lead int) {
	for i := 0; i < len(fold) && (n == 0 || i+1 < n); i++ {
		start, ok := tableByFirst[fold[i]]
		if !ok {
			continue
		}
		if start.name != "" {
			// No match starting here or later ends before this one.
			return i + 1, joinSlash(strings.Join(parts[:i], "/"), start.name), 0
		}
		for _, f := range start.fixed {
			if end := i + len(f.fold); (n == 0 || end < n) && foldsAt(fold, i, f.fold) {
				n, canon = end, joinSlash(strings.Join(parts[:i], "/"), strings.Join(f.canon, "/"))
			}
			for k := 1; n == 0 && k < len(f.fold) && foldsAt(fold, i, f.fold[:k]); k++ {
				lead = max(lead, i+k)
			}
		}
	}
	if n > 0 {
		lead = 0
	}
	return n, canon, lead
}

// matchInstructionPath reports whether parts is at or under a table entry.
func matchInstructionPath(parts []string) (n int, canon string, ok bool) {
	n, canon, _ = scanFold(parts, foldComponents(parts))
	return n, canon, n > 0
}

// instrPath is a slash-separated path classified against the table.
type instrPath struct {
	parts, fold []string
	n           int    // components of the outermost table match; 0 for none
	canon       string // the mask spelling of them
	lead        int    // when n == 0: leading components that end in a proper prefix of a fixed path
}

func classify(p string) instrPath {
	parts := strings.Split(p, "/")
	ip := instrPath{parts: parts, fold: foldComponents(parts)}
	ip.n, ip.canon, ip.lead = scanFold(parts, ip.fold)
	return ip
}

// relevant: the path is at or under a table entry, or ends exactly in a proper
// prefix of a fixed path (a file or link that could redirect one).
func (ip instrPath) relevant() bool { return ip.n > 0 || (ip.lead > 0 && ip.lead == len(ip.parts)) }
