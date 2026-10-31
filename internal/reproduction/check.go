package reproduction

import (
	"errors"
	"path"
	"strings"

	"buildgate/internal/policy"
	"buildgate/internal/sandbox"
)

// Judged is what the path rules compare a held test file with, all read by the
// caller from the commit that was judged.
type Judged struct {
	TestPatterns []string // .factory.yml test_patterns; empty means policy.DefaultTestPatterns
	Tracked      []string // every path tracked in the judged commit
	Protected    []string // the run's release protected paths
}

// Accepted is a test file that passed every shape rule.
type Accepted struct {
	Path     string
	Runner   string
	Dir      string // the project directory the runner runs in (For)
	TestIDs  []string
	SpecLine int
	SpecText string // the cited line, trimmed
}

// IsCleanRelative reports whether p is a reproduction path's shape: relative,
// clean, no "." or ".." component, no control or backslash character, and no
// leading '-' or ':'.
func IsCleanRelative(p string) bool { return cleanRelative(p) }

// Each refusal is a fixed sentence naming the rule, never a file's text.
var (
	errPathShape   = errors.New("the path is not a clean relative path")
	errNotTest     = errors.New("the path does not match the test patterns")
	errReserved    = errors.New("the path is under .factory/ or .git")
	errProtected   = errors.New("the path is a protected path")
	errInstruction = errors.New("the path is an instruction path")
	errTracked     = errors.New("the path is tracked in the judged commit")
	errNoRunner    = errors.New("no reproduction runner applies to the path")
)

// CheckPath applies SC-022's path rules to a new test file's path, in order:
// a clean relative path; matching the test patterns; not under .factory/ or
// .git; not a release protected path; not an instruction path; not equal,
// under case folding, to a path tracked in the judged commit (or under one).
// A refusal is a fixed sentence naming the rule.
func CheckPath(p string, j Judged) error {
	if !cleanRelative(p) {
		return errPathShape
	}
	patterns := j.TestPatterns
	if len(patterns) == 0 {
		patterns = policy.DefaultTestPatterns
	}
	if !policy.IsTestFile(p, patterns) {
		return errNotTest
	}
	fold := sandbox.FoldPath(p)
	if reserved(fold) {
		return errReserved
	}
	if protected(p, j.Protected) {
		return errProtected
	}
	if sandbox.IsInstructionPath(p) {
		return errInstruction
	}
	if trackedAt(fold, j.Tracked) {
		return errTracked
	}
	return nil
}

// cleanRelative: relative, clean, no "." or ".." component, no control or
// backslash character, and no leading '-' or ':' a command line or a git
// pathspec would read as an option or magic.
func cleanRelative(p string) bool {
	if p == "" || p[0] == '/' || p[0] == '-' || p[0] == ':' || path.Clean(p) != p {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." || seg == "" {
			return false
		}
	}
	return true
}

// reserved: the folded path is under .factory/ (the repository's gate
// scripts) or has a .git component.
func reserved(fold string) bool {
	if strings.HasPrefix(fold, ".factory/") {
		return true
	}
	for _, seg := range strings.Split(fold, "/") {
		if seg == ".git" {
			return true
		}
	}
	return false
}

// protected matches the release protected paths as the release check does,
// and again case-folded (over-matching is safe).
func protected(p string, prot []string) bool {
	if policy.PathCoveredByAllowed(p, prot) {
		return true
	}
	lower := make([]string, len(prot))
	for i, s := range prot {
		lower[i] = strings.ToLower(s)
	}
	return policy.PathCoveredByAllowed(strings.ToLower(p), lower)
}

// trackedAt reports whether the folded path, or a directory above it, is a
// tracked path of the judged commit under the same fold: a new path never
// aliases a tracked file, and never sits under a tracked file, link or
// submodule.
func trackedAt(fold string, tracked []string) bool {
	set := make(map[string]bool, len(tracked))
	for _, t := range tracked {
		set[sandbox.FoldPath(t)] = true
	}
	for p := fold; p != "." && p != "/" && p != ""; p = path.Dir(p) {
		if set[p] {
			return true
		}
	}
	return false
}
