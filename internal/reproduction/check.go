package reproduction

import (
	"bytes"
	"errors"
	"path"
	"strings"

	"buildgate/internal/policy"
	"buildgate/internal/sandbox"
)

// Finding is the part of one review finding the shape rules read: the spec
// line it cites and the reproduction it carries.
type Finding struct {
	SpecLine int    // 1-based line number in the approved spec
	SpecText string // the cited text
	Path     string // the reproduction's repository-relative path
	Content  []byte
}

// Judged is what the shape rules compare a reproduction with, all read by the
// caller from the approved spec and the commit the review judged.
type Judged struct {
	Spec         []byte   // the approved spec.md, checked against its approved hash
	TestPatterns []string // .factory.yml test_patterns; empty means policy.DefaultTestPatterns
	Tracked      []string // every path tracked in the judged commit
	Protected    []string // the run's release protected paths
	Root         RootFacts
}

// Accepted is a reproduction that passed every shape rule.
type Accepted struct {
	Path     string
	Runner   string
	TestIDs  []string
	SpecLine int
	SpecText string // the cited line, trimmed
}

// Checked is one finding's result: Accepted, or the shape rule that refused
// it (advice).
type Checked struct {
	Accepted *Accepted
	Refused  string
}

// Each refusal is a fixed sentence: it names the rule, never the review's text.
var (
	errSpecLine     = errors.New("the cited spec line is missing or not the approved spec's line")
	errContent      = errors.New("the reproduction is empty, binary or over the size cap")
	errPathShape    = errors.New("the reproduction path is not a clean relative path")
	errNotTest      = errors.New("the reproduction path does not match the test patterns")
	errReserved     = errors.New("the reproduction path is under .factory/ or .git")
	errProtected    = errors.New("the reproduction path is a protected path")
	errInstruction  = errors.New("the reproduction path is an instruction path")
	errTracked      = errors.New("the reproduction path is tracked in the judged commit")
	errNoRunner     = errors.New("no reproduction runner applies to the path")
	errTestIDs      = errors.New("the reproduction names no test, or too many")
	errTooMany      = errors.New("the review carries more reproductions than the cap")
	errDuplicate    = errors.New("the review carries this reproduction path twice")
	errNoReproduced = errors.New("the finding carries no reproduction")
)

// CheckAll applies Check to each finding of one review, in order. Only an
// accepted reproduction counts toward MaxReproductions and takes its path
// (folded): past the cap, and for a path an earlier accepted one took, the
// finding is refused; a refused one crowds out nothing.
func CheckAll(findings []Finding, j Judged) []Checked {
	out := make([]Checked, len(findings))
	seen := map[string]bool{}
	held := 0
	for i, f := range findings {
		if f.Path == "" {
			out[i] = Checked{Refused: errNoReproduced.Error()}
			continue
		}
		fold := sandbox.FoldPath(f.Path)
		if seen[fold] {
			out[i] = Checked{Refused: errDuplicate.Error()}
			continue
		}
		a, err := Check(f, j)
		switch {
		case err != nil:
			out[i] = Checked{Refused: err.Error()}
		case held >= MaxReproductions:
			out[i] = Checked{Refused: errTooMany.Error()}
		default:
			seen[fold] = true
			held++
			out[i] = Checked{Accepted: &a}
		}
	}
	return out
}

// IsCleanRelative reports whether p is a reproduction path's shape: relative,
// clean, no "." or ".." component, no control or backslash character, and no
// leading '-' or ':'.
func IsCleanRelative(p string) bool { return cleanRelative(p) }

// Check applies SC-022's shape rules to one finding's reproduction. A refusal
// makes the finding advice; it is never a verdict on the run.
func Check(f Finding, j Judged) (Accepted, error) {
	line, ok := citedLine(j.Spec, f.SpecLine, f.SpecText)
	if !ok {
		return Accepted{}, errSpecLine
	}
	if len(f.Content) == 0 || len(f.Content) > MaxContentBytes || bytes.IndexByte(f.Content, 0) >= 0 {
		return Accepted{}, errContent
	}
	if err := checkPath(f.Path, j); err != nil {
		return Accepted{}, err
	}
	r, ok := For(f.Path, j.Root)
	if !ok {
		return Accepted{}, errNoRunner
	}
	ids, err := r.TestIDs(f.Path, f.Content)
	if err != nil || len(ids) == 0 || len(ids) > MaxTestIDs {
		return Accepted{}, errTestIDs
	}
	return Accepted{Path: f.Path, Runner: r.ID(), TestIDs: ids, SpecLine: f.SpecLine, SpecText: line}, nil
}

// citedLine returns the spec's line n, trimmed, when text is byte-equal to it
// after trimming whitespace. A blank line is never a citation.
func citedLine(spec []byte, n int, text string) (string, bool) {
	lines := strings.Split(string(spec), "\n")
	if n < 1 || n > len(lines) {
		return "", false
	}
	line := strings.TrimSpace(lines[n-1])
	return line, line != "" && line == strings.TrimSpace(text)
}

// checkPath applies the path rules, in order.
func checkPath(p string, j Judged) error {
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
