package reproduction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// vitestJSON runs a JavaScript or TypeScript test file with vitest and reads
// its JSON reporter's output. The per-test timeout is vitest's own
// --testTimeout.
type vitestJSON struct{}

func (vitestJSON) ID() string { return "vitest-json" }

// Applies: vitest's default include, *.test.* and *.spec.* with a JS or TS
// extension, in a repository with vitest at its root.
func (vitestJSON) Applies(p string, root RootFacts) bool {
	var globs []string
	for _, kind := range []string{"test", "spec"} {
		for _, ext := range []string{"js", "jsx", "ts", "tsx", "mjs", "cjs", "mts", "cts"} {
			globs = append(globs, "*."+kind+"."+ext)
		}
	}
	return baseMatches(p, globs...) && root.hasVitest()
}

// vitestIDSep joins a test's describe titles and its own title into its ID.
const vitestIDSep = " > "

// TestIDs reads the file's describe/suite and it/test calls whose first
// argument is a plain string literal, outside comments and strings. A test
// inside describes is "outer > inner > title". A call through .each or .for
// has no static title and is not an ID.
func (vitestJSON) TestIDs(_ string, content []byte) ([]string, error) {
	s := jsScanner{src: string(content)}
	var ids []string
	for {
		call, ok := s.nextCall()
		if !ok {
			break
		}
		if call.suite {
			s.suites = append(s.suites, jsSuite{title: call.title, depth: call.depth})
			continue
		}
		ids = append(ids, s.qualified(call.title))
	}
	return uniqueIDs(ids), nil
}

type jsSuite struct {
	title string
	depth int // paren depth inside the describe call
}

type jsCall struct {
	title string
	suite bool
	depth int
}

// jsScanner walks JavaScript source, skipping comments and string bodies
// and tracking parenthesis depth, so a describe's body ends where its call
// closes.
type jsScanner struct {
	src    string
	i      int
	depth  int
	suites []jsSuite
}

func (s *jsScanner) qualified(title string) string {
	parts := make([]string, 0, len(s.suites)+1)
	for _, su := range s.suites {
		parts = append(parts, su.title)
	}
	return strings.Join(append(parts, title), vitestIDSep)
}

// nextCall advances to the next test or suite call with a literal title.
func (s *jsScanner) nextCall() (jsCall, bool) {
	for s.i < len(s.src) {
		c := s.src[s.i]
		switch {
		case s.skipCommentOrString():
		case c == '(':
			s.depth++
			s.i++
		case c == ')':
			s.closeParen()
		case isIdentStart(c):
			if call, ok := s.identifier(); ok {
				return call, true
			}
		default:
			s.i++
		}
	}
	return jsCall{}, false
}

func (s *jsScanner) closeParen() {
	s.i++
	for len(s.suites) > 0 && s.suites[len(s.suites)-1].depth == s.depth {
		s.suites = s.suites[:len(s.suites)-1]
	}
	if s.depth > 0 {
		s.depth--
	}
}

// skipCommentOrString skips a comment or a whole string at the cursor.
func (s *jsScanner) skipCommentOrString() bool {
	rest := s.src[s.i:]
	switch {
	case strings.HasPrefix(rest, "//"):
		s.i += indexOrEnd(rest, "\n")
	case strings.HasPrefix(rest, "/*"):
		s.i += 2 + indexOrEnd(rest[2:], "*/") + 2
	case rest[0] == '"' || rest[0] == '\'' || rest[0] == '`':
		_, n := jsString(rest)
		s.i += n
	default:
		return false
	}
	s.i = min(s.i, len(s.src))
	return true
}

func indexOrEnd(s, sep string) int {
	if i := strings.Index(s, sep); i >= 0 {
		return i
	}
	return len(s)
}

// jsString reads the string literal at the start of s: its value when it is
// plain (no escape, no ${}), and its length.
func jsString(s string) (string, int) {
	q := s[0]
	plain := true
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			plain = false
			i++
		case q == '`' && strings.HasPrefix(s[i:], "${"):
			plain = false
		case s[i] == q:
			if !plain {
				return "", i + 1
			}
			return s[1:i], i + 1
		}
	}
	return "", len(s)
}

func isIdentStart(c byte) bool {
	return c == '_' || c == '$' || (c|0x20 >= 'a' && c|0x20 <= 'z')
}

func isIdentByte(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// identifier reads a member chain at the cursor (describe.skip, it.only) and,
// when it is a test or suite call with a literal title, returns it with the
// cursor past the title.
func (s *jsScanner) identifier() (jsCall, bool) {
	start := s.i
	for s.i < len(s.src) && (isIdentByte(s.src[s.i]) || s.src[s.i] == '.') {
		s.i++
	}
	chain := strings.Split(s.src[start:s.i], ".")
	if start > 0 && s.src[start-1] == '.' {
		return jsCall{}, false // a property of something else
	}
	suite, ok := jsCallee(chain)
	if !ok {
		return jsCall{}, false
	}
	j := skipSpace(s.src, s.i)
	if j >= len(s.src) || s.src[j] != '(' {
		return jsCall{}, false
	}
	k := skipSpace(s.src, j+1)
	if k >= len(s.src) || !strings.ContainsRune(`"'`+"`", rune(s.src[k])) {
		return jsCall{}, false
	}
	title, n := jsString(s.src[k:])
	s.depth++
	s.i = k + n
	if title == "" {
		return jsCall{}, false
	}
	return jsCall{title: title, suite: suite, depth: s.depth}, true
}

// jsCallee: describe/suite (a suite) or it/test (a test), with modifiers
// other than each and for.
func jsCallee(chain []string) (suite, ok bool) {
	for _, m := range chain[1:] {
		if m == "each" || m == "for" {
			return false, false
		}
	}
	switch chain[0] {
	case "describe", "suite":
		return true, true
	case "it", "test":
		return false, true
	}
	return false, false
}

func skipSpace(s string, i int) int {
	for i < len(s) && strings.ContainsRune(" \t\r\n", rune(s[i])) {
		i++
	}
	return i
}

func (vitestJSON) Command(inv Invocation) Command {
	script := fmt.Sprintf("npx --no-install vitest run %s --reporter=json --outputFile=%s --testTimeout=%d",
		shellQuote(inv.Path), shellQuote(inv.ReportPath), PerTestTimeout.Milliseconds())
	return Command{Script: script, ReportPath: inv.ReportPath, Timeout: launchTimeout(inv.TestIDs)}
}

// vitestReport is the part of vitest's JSON reporter output read here.
type vitestReport struct {
	TestResults []struct {
		Name             string `json:"name"`
		Status           string `json:"status"`
		Message          string `json:"message"`
		AssertionResults []struct {
			AncestorTitles  []string `json:"ancestorTitles"`
			Title           string   `json:"title"`
			Status          string   `json:"status"`
			Duration        float64  `json:"duration"`
			FailureMessages []string `json:"failureMessages"`
		} `json:"assertionResults"`
	} `json:"testResults"`
}

// Classify reads the results of the reproduction's own file only: vitest's
// file argument is a filter that also matches a longer path ending in it, and
// the report names files by absolute path, so the file is the shortest name
// ending in "/"+p. That file failing with no test results did not compile or
// import.
func (vitestJSON) Classify(p string, report []byte, _ int, ids []string) (map[string]Kind, Outcome) {
	var r vitestReport
	if len(bytes.TrimSpace(report)) == 0 || json.Unmarshal(report, &r) != nil {
		return allKind(ids, KindMissing), OutcomeMissing
	}
	file := -1
	for i, f := range r.TestResults {
		if (f.Name == p || strings.HasSuffix(f.Name, "/"+p)) && (file < 0 || len(f.Name) < len(r.TestResults[file].Name)) {
			file = i
		}
	}
	if file < 0 {
		return allKind(ids, KindMissing), OutcomeMissing
	}
	f := r.TestResults[file]
	if f.Status == "failed" && len(f.AssertionResults) == 0 {
		return allKind(ids, KindCompile), OutcomeCompile
	}
	kinds := map[string]Kind{}
	for _, a := range f.AssertionResults {
		id := strings.Join(append(append([]string(nil), a.AncestorTitles...), a.Title), vitestIDSep)
		merge(kinds, id, vitestKind(a.Status, a.FailureMessages, a.Duration))
	}
	return fromKinds(kinds, ids)
}

// vitestKind: a failure whose message is vitest's timeout is a timeout. Vitest
// 3 reports a test timeout's stack as "Error: STACK_TRACE_ERROR" without the
// "timed out" text, so that, and a duration at the timeout, count too. An
// AssertionError is a fail; any other failure an error.
func vitestKind(status string, messages []string, durationMs float64) Kind {
	switch status {
	case "passed":
		return KindPass
	case "skipped", "pending", "todo", "disabled":
		return KindSkipped
	case "failed":
	default:
		return KindMissing
	}
	first := ""
	if len(messages) > 0 {
		first = messages[0]
	}
	switch {
	case strings.Contains(first, "Test timed out in") || strings.HasPrefix(first, "Error: STACK_TRACE_ERROR") ||
		durationMs >= float64(PerTestTimeout.Milliseconds()):
		return KindTimeout
	case strings.HasPrefix(first, "AssertionError"):
		return KindFail
	}
	return KindError
}
