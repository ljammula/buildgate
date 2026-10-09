// Package baseline judges the run of a ticket's verify command on the base
// commit, before the build's first round: which tests it reported failing,
// and whether the ticket names every one of them.
//
// The rule (Evaluate): a baseline that fails stops the run before any model
// call unless the failure is the ticket's own work: the ticket names every
// failing test, or, when no test failed by name, the command's first error
// names a path the ticket declares and the base commit does not have (a
// test directory the ticket is to create). The final canonical
// verify is the same command in the same kind of sandbox, so a build is
// accepted only if it turns every baseline failure green; a failure the
// ticket does not ask for (a test that needs a program the image lacks, a
// plugin the command does not install) is one no build of that ticket can
// fix, and a build spent on it is lost.
package baseline

import (
	"regexp"
	"sort"
	"strconv"
	"strings"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/sanitize"
)

// Failure is one failing test of a verify log. Name is the test as the
// runner printed it, without its parameters; File is set instead for a
// failure that has no test name (a file that did not compile or import).
type Failure struct {
	Name string
	File string
}

// label is the failure as an operator reads it.
func (f Failure) label() string {
	if f.Name != "" {
		return f.Name
	}
	return f.File
}

// failurePattern is one runner's failing-test line. The first capture group
// is the test's name, or a file path when file is set.
type failurePattern struct {
	re   *regexp.Regexp
	file bool
}

// failurePatterns covers go test and pytest, the two runners the rule was
// measured on, plus a Go compile error and a pytest collection error,
// which name a file and no test. Any other runner reports no Failure: its
// baseline failure names nothing, so Evaluate cannot find it in a ticket and
// the run halts.
var failurePatterns = []failurePattern{
	{re: regexp.MustCompile(`(?m)^\s*--- FAIL: (\S+)`)},
	{re: regexp.MustCompile(`(?m)^(?:FAILED|ERROR) ([^\s\[]+::[^\s\[]+)`)},
	{re: regexp.MustCompile(`(?m)^ERROR (\S+\.py)(?:\s|$)`), file: true},
	{re: regexp.MustCompile(`(?m)^(?:\./)?(\S+\.go):\d+:\d+: `), file: true},
}

// maxNameLen bounds one failing test's name: a log line is the repository's
// own output, and the name reaches status lines and the run record.
const maxNameLen = 200

// Failures lists the distinct failing tests log reports, in the order it
// first names them. A Go subtest is reported as its top-level test: that is
// the name a ticket would use.
func Failures(log string) []Failure {
	content := sanitize.Text(log)
	type found struct {
		pos int
		f   Failure
	}
	var all []found
	for _, p := range failurePatterns {
		for _, loc := range p.re.FindAllStringSubmatchIndex(content, -1) {
			text := sanitize.Line(content[loc[2]:loc[3]])
			if text == "" {
				continue
			}
			if len(text) > maxNameLen {
				// Still a failure, and one no ticket names: the cut name
				// ends in a character no test name holds.
				text = strings.ToValidUTF8(text[:maxNameLen], "") + "\u2026"
			}
			f := Failure{Name: text}
			if p.file {
				f = Failure{File: text}
			} else if strings.HasPrefix(text, "Test") || strings.HasPrefix(text, "Example") || strings.HasPrefix(text, "Fuzz") {
				f.Name, _, _ = strings.Cut(text, "/")
			}
			all = append(all, found{pos: loc[0], f: f})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].pos < all[j].pos })
	seen := map[Failure]bool{}
	var out []Failure
	for _, a := range all {
		if !seen[a.f] {
			seen[a.f] = true
			out = append(out, a.f)
		}
	}
	return out
}

// Named reports whether ticket names f, and the words it does so with. A
// test is named by its full name or by its last component (the function of
// a pytest node id); a Go test also by one of its subtests
// ("TestParse/empty"). A failure with no test name is named by its file's
// path as the log printed it, or by a longer path that ends with it (the
// log's path is relative to the directory the command ran in).
func Named(ticket string, f Failure) (as string, ok bool) {
	if f.File != "" {
		return f.File, containsWord(ticket, f.File, pathSuffix)
	}
	if !strings.Contains(f.Name, "::") {
		return f.Name, containsWord(ticket, f.Name, subtestPrefix)
	}
	if containsWord(ticket, f.Name, whole) {
		return f.Name, true
	}
	last := f.Name[strings.LastIndex(f.Name, "::")+2:]
	return last, containsWord(ticket, last, whole)
}

// boundary says what may stand next to a word for it to count as named.
type boundary int

const (
	// whole: no identifier or path character on either side.
	whole boundary = iota
	// subtestPrefix: as whole, and a "/" may follow (a Go subtest's name).
	subtestPrefix
	// pathSuffix: as whole, and a "/" may precede (a longer path).
	pathSuffix
	// pathPart: a "/" may stand on either side (a directory inside a path).
	pathPart
)

// containsWord reports whether text holds word standing alone as b allows,
// so "TestParse" does not name "TestParseBytes" and "test_time.py" does not
// name "tests/test_time.py::test_sign".
func containsWord(text, word string, b boundary) bool {
	if word == "" {
		return false
	}
	for start := 0; ; {
		i := strings.Index(text[start:], word)
		if i < 0 {
			return false
		}
		i += start
		end := i + len(word)
		before := i == 0 || !wordByte(text[i-1]) || ((b == pathSuffix || b == pathPart) && text[i-1] == '/')
		after := end == len(text) || !wordByte(text[end]) || ((b == subtestPrefix || b == pathPart) && text[end] == '/')
		if before && after {
			return true
		}
		start = i + 1
	}
}

func wordByte(b byte) bool {
	return b == '_' || b == '/' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
}

// EvaluateWithSetup is Evaluate for a command that ran after the repository's
// setup commands (`.factory.yml` setup:): an exit of run.SetupFailedExitCode
// whose log names a setup command is a setup failure, not a verify failure.
// A command that ran no setup is judged by Evaluate alone, whatever it exits.
func EvaluateWithSetup(command string, exitCode int, log, firstError, ticket string, created []string) *run.BaselineVerify {
	if exitCode == run.SetupFailedExitCode {
		if setup := run.SetupFailedCommand(log); setup != "" {
			return &run.BaselineVerify{Command: command, ExitCode: exitCode, SetupFailed: sanitize.Line(setup)}
		}
	}
	return Evaluate(command, exitCode, log, firstError, ticket, created)
}

// Evaluate judges a verify command's run on the base commit: exitCode and
// log are the command's, firstError the log's first recognised error line
// (used only when no test was named), ticket the text of the ticket the
// run builds, and created the paths the ticket declares that the base
// commit does not have (CreatedPaths).
func Evaluate(command string, exitCode int, log, firstError, ticket string, created []string) *run.BaselineVerify {
	b := &run.BaselineVerify{Command: command, ExitCode: exitCode, Passed: exitCode == 0}
	if b.Passed {
		return b
	}
	failures := Failures(log)
	b.FailingCount = len(failures)
	for _, f := range failures {
		if len(b.FailingTests) < run.BaselineVerifyMaxNamed {
			b.FailingTests = append(b.FailingTests, f.label())
		}
		if as, ok := Named(ticket, f); ok {
			if len(b.NamedAs) < run.BaselineVerifyMaxNamed && !contains(b.NamedAs, as) {
				b.NamedAs = append(b.NamedAs, as)
			}
			continue
		}
		b.UnnamedCount++
		if len(b.Unnamed) < run.BaselineVerifyMaxNamed {
			b.Unnamed = append(b.Unnamed, f.label())
		}
	}
	if b.FailingCount == 0 {
		b.FirstError = sanitize.Line(firstError)
		// No test failed by name. The failure is still the ticket's own
		// work when the command's first error names something the ticket
		// is to create: a test directory that is not there yet.
		if path := createdPathNamedBy(b.FirstError, created); path != "" {
			b.NeedsCreated = path
			b.NamedAs = []string{path}
			b.Expected = true
		}
		return b
	}
	b.Expected = b.UnnamedCount == 0
	return b
}

// CreatedPaths lists what a ticket is to create: each file it declares
// (declared: its Allowed-Files and Required-Changed-Files, patterns left
// out) that exists reports false for, and each directory above such a file
// that exists reports false for too. Paths are slash-separated and relative
// to the repository root.
func CreatedPaths(declared []string, exists func(path string) bool) []string {
	seen := map[string]bool{}
	var created []string
	for _, file := range declared {
		file = strings.TrimPrefix(strings.TrimSpace(file), "./")
		if file == "" || strings.ContainsAny(file, "*?[") || exists(file) {
			continue
		}
		for path := file; path != "." && path != "/" && path != ""; path = parentDir(path) {
			if exists(path) {
				break
			}
			if !seen[path] {
				seen[path] = true
				created = append(created, path)
			}
		}
	}
	return created
}

func parentDir(path string) string {
	i := strings.LastIndex(path, "/")
	if i < 0 {
		return ""
	}
	return path[:i]
}

// createdPathNamedBy is the longest of created that firstError names as a
// path or part of one, "" when it names none.
func createdPathNamedBy(firstError string, created []string) string {
	best := ""
	for _, path := range created {
		if len(path) > len(best) && containsWord(firstError, path, pathPart) {
			best = path
		}
	}
	return best
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// BuildNote is what a build is told about a baseline that failed as its
// ticket expects: the tests to turn green, each in the words the ticket
// itself uses for it (BaselineVerify.NamedAs). Nothing the verify command
// printed is copied into the note: beside the factory's own sentences it
// holds only text the ticket already gives the build, and the operator's
// verify command. "" for any other result.
func BuildNote(b *run.BaselineVerify) string {
	if b == nil || !b.Expected {
		return ""
	}
	var s strings.Builder
	s.WriteString("Before this build, the verify command was run on the untouched repository and failed (exit ")
	s.WriteString(strconv.Itoa(b.ExitCode))
	if b.NeedsCreated != "" {
		s.WriteString("). It failed because it needs a path your ticket is to create, so creating it is part of the work:\n\n")
	} else {
		s.WriteString("). Every test that failed is one your ticket names, so making them pass is the work:\n\n")
	}
	for _, name := range b.NamedAs {
		s.WriteString("- " + name + "\n")
	}
	s.WriteString("\nVerify command: " + b.Command + "\n")
	return s.String()
}

// OutOfScope is what the diff_scope gate would flag among left, the paths a
// command left in the workspace: the call policy.EvaluateRun makes for a
// run's changed files (ExcludeHarnessByproducts, then DiffScope), and under
// the same condition, a ticket that declares Allowed-Files (non-nil).
// A ticket without them has no such gate and nothing is out of scope. Sorted,
// without duplicates.
func OutOfScope(left, allowed []string) []string {
	if allowed == nil {
		return nil
	}
	_, violations := policy.DiffScope(policy.ExcludeHarnessByproducts(left), allowed)
	seen := make(map[string]bool, len(violations))
	var out []string
	for _, v := range violations {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// NoteLeftovers records on b the paths of left that OutOfScope flags for
// allowed: at most run.BaselineVerifyMaxNamed of them, each as a clean line
// of at most maxNameLen bytes (a path is the repository's own output), and
// how many there were.
func NoteLeftovers(b *run.BaselineVerify, left, allowed []string) {
	out := OutOfScope(left, allowed)
	b.LeftOutOfScopeCount = len(out)
	for _, p := range out {
		if len(b.LeftOutOfScope) == run.BaselineVerifyMaxNamed {
			break
		}
		p = sanitize.Line(p)
		if len(p) > maxNameLen {
			p = p[:maxNameLen]
		}
		if p != "" {
			b.LeftOutOfScope = append(b.LeftOutOfScope, p)
		}
	}
}
