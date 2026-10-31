package reproduction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// vitestJSON runs a JavaScript or TypeScript test file with vitest and reads
// its JSON reporter's output. The per-test timeout is vitest's own
// --testTimeout, a timer on the test's event loop: a synchronous loop that
// never yields blocks the timer, and a test may raise its own timeout, so
// such a test is cut only by the launch's cap, which reads as a timeout of
// every test (CapExceeded).
type vitestJSON struct{}

func (vitestJSON) ID() string { return "vitest-json" }

// Applies: vitest's default include, *.test.* and *.spec.* with a JS or TS
// extension, in a project directory that declares vitest.
func (vitestJSON) Applies(p string, m Markers) bool {
	var globs []string
	for _, kind := range []string{"test", "spec"} {
		for _, ext := range []string{"js", "jsx", "ts", "tsx", "mjs", "cjs", "mts", "cts"} {
			globs = append(globs, "*."+kind+"."+ext)
		}
	}
	return baseMatches(p, globs...) && m.hasVitest()
}

// vitestIDSep joins a test's describe titles and its own title into its ID.
const vitestIDSep = " > "

// Tests reads the file's describe/suite and it/test calls whose first
// argument is a plain string literal, outside comments and strings. A test
// inside describes is "outer > inner > title". A call through .each or .for
// has no static title and is not an ID, and neither is a test inside a suite
// made through one: vitest names it by the suite's title for each row, which
// the file does not hold. A test spans from the line of its call to the line
// of its closing parenthesis.
func (vitestJSON) Tests(_ string, content []byte) ([]TestSpan, error) {
	s := jsScanner{src: string(content)}
	for {
		call, ok := s.nextCall()
		if !ok {
			break
		}
		open := jsOpen{title: call.title, suite: call.suite, depth: call.depth, span: -1}
		if !call.suite && !s.inTableSuite() {
			s.spans = append(s.spans, TestSpan{ID: s.qualified(call.title), Start: s.line(call.at), End: s.line(len(s.src))})
			open.span = len(s.spans) - 1
		}
		s.open = append(s.open, open)
	}
	return uniqueSpans(s.spans), nil
}

// jsOpen is a describe or test call whose parenthesis is still open: a
// suite's title joins the IDs inside it; a test's span (an index in
// jsScanner.spans, -1 for a suite) ends where its call closes.
type jsOpen struct {
	title string
	suite bool
	table bool // a suite called through .each or .for: no static title
	depth int  // paren depth inside the call
	span  int
}

type jsCall struct {
	title string
	suite bool
	depth int
	at    int // offset of the callee
}

// jsScanner walks JavaScript source, skipping comments and string bodies
// and tracking parenthesis depth, so a call's body ends where its call
// closes.
type jsScanner struct {
	src   string
	i     int
	depth int
	open  []jsOpen
	spans []TestSpan
}

// line is the 1-based line of the offset.
func (s *jsScanner) line(off int) int { return 1 + strings.Count(s.src[:min(off, len(s.src))], "\n") }

// inTableSuite reports whether the cursor is inside a suite called through
// .each or .for.
func (s *jsScanner) inTableSuite() bool {
	for _, o := range s.open {
		if o.table {
			return true
		}
	}
	return false
}

func (s *jsScanner) qualified(title string) string {
	var parts []string
	for _, o := range s.open {
		if o.suite {
			parts = append(parts, o.title)
		}
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
	at := s.i
	s.i++
	for len(s.open) > 0 && s.open[len(s.open)-1].depth == s.depth {
		if top := s.open[len(s.open)-1]; top.span >= 0 {
			s.spans[top.span].End = s.line(at)
		}
		s.open = s.open[:len(s.open)-1]
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
		s.tableSuite(chain)
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
	return jsCall{title: title, suite: suite, depth: s.depth, at: start}, true
}

// tableSuite opens, for describe.each(rows)(title, fn) or
// describe.each`rows`(title, fn) at the cursor (also .for), the call that
// holds the suite's body, so the tests inside it are known to have no static
// name. The cursor ends inside that call, past its opening parenthesis.
func (s *jsScanner) tableSuite(chain []string) {
	if (chain[0] != "describe" && chain[0] != "suite") || !slices.ContainsFunc(chain[1:], func(m string) bool { return m == "each" || m == "for" }) {
		return
	}
	j := skipSpace(s.src, s.i)
	switch {
	case j < len(s.src) && s.src[j] == '`':
		_, n := jsString(s.src[j:])
		j += n
	case j < len(s.src) && s.src[j] == '(':
		end, ok := s.matchingParen(j)
		if !ok {
			return
		}
		j = end + 1
	default:
		return
	}
	j = skipSpace(s.src, j)
	if j >= len(s.src) || s.src[j] != '(' {
		return
	}
	s.depth++
	s.i = j + 1
	s.open = append(s.open, jsOpen{suite: true, table: true, depth: s.depth, span: -1})
}

// matchingParen is the offset of the parenthesis that closes the one at
// open, skipping comments and strings; false when the source ends first.
func (s *jsScanner) matchingParen(open int) (int, bool) {
	sub := jsScanner{src: s.src, i: open + 1}
	for depth := 1; sub.i < len(sub.src); {
		switch c := sub.src[sub.i]; {
		case sub.skipCommentOrString():
		case c == '(':
			depth++
			sub.i++
		case c == ')':
			if depth--; depth == 0 {
				return sub.i, true
			}
			sub.i++
		default:
			sub.i++
		}
	}
	return 0, false
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
	script := inv.inDir(fmt.Sprintf("npx --no-install vitest run %s --reporter=json --outputFile=%s --testTimeout=%d%s",
		shellQuote(inv.rel()), shellQuote(inv.ReportPath), PerTestTimeout.Milliseconds(), vitestNamePattern(inv.TestIDs)))
	return Command{Script: script, ReportPath: inv.ReportPath, Timeout: launchTimeout(inv.TestIDs)}
}

// vitestNamePattern is the -t option that selects the invocation's tests by
// full name: vitest matches the regexp against the describe titles and the
// test's title joined by spaces, so each ID's parts are quoted, joined by a
// space and anchored at the end (and at a word start, since the file's own
// name may precede the first title). A test that matches only because its name
// ends the same way runs too and is not judged. No IDs, no option.
func vitestNamePattern(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	alts := make([]string, len(ids))
	for i, id := range ids {
		parts := strings.Split(id, vitestIDSep)
		for j, p := range parts {
			parts[j] = regexp.QuoteMeta(p)
		}
		alts[i] = strings.Join(parts, " ")
	}
	return " -t " + shellQuote("(?:^|\\s)(?:"+strings.Join(alts, "|")+")$")
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
