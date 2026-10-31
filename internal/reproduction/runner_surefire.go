package reproduction

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// surefireXML runs a JUnit test class with Maven Surefire and reads the
// class's TEST-<class>.xml report. The per-test timeout is JUnit Jupiter's
// default timeout (JUnit 5.9 or later) in its separate-thread mode, so a test
// that never checks for an interrupt (a busy loop) still reports a
// TimeoutException when it passes; an older JUnit 5 ignores the thread mode
// and only interrupts, and a JUnit 4 test has no per-test timeout: both are
// then cut only by the launch's cap, which reads as a timeout of every test
// (CapExceeded).
type surefireXML struct{}

// surefireCompileExit is the launch's exit status when `mvn test-compile`,
// run on its own before any test, fails and its output names the
// reproduction's own source file: the reproduction does not compile. Maven
// itself exits 0 or 1, and a test that ends its JVM makes Surefire fail with
// 1, so only the script's own compile step returns it.
const surefireCompileExit = 86

// surefireBuildExit is the launch's exit status when `mvn test-compile`
// fails and its output does not name the reproduction's file: the module did
// not build for another reason (a dependency it could not fetch, a main
// source that does not compile, a plugin error). It is no compile verdict on
// the reproduction, and reads as missing.
const surefireBuildExit = 87

func (surefireXML) ID() string { return "surefire-xml" }

const surefireSourceRoot = "src/test/java/"

// Applies: Surefire's default includes under a module's src/test/java, in a
// project directory with a pom.xml.
func (surefireXML) Applies(p string, m Markers) bool {
	return strings.Contains("/"+p, "/"+surefireSourceRoot) &&
		baseMatches(p, "Test*.java", "*Test.java", "*Tests.java", "*TestCase.java") &&
		m.Has("pom.xml")
}

var (
	javaBlockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	javaLineComment  = regexp.MustCompile(`//[^\n]*`)
	javaString       = regexp.MustCompile(`"(?:[^"\\\n]|\\.)*"`)
	javaTextBlock    = regexp.MustCompile(`(?s)""".*?"""`)
	// A test annotation, other annotations, modifiers, void, the name.
	javaTestMethod = regexp.MustCompile(`@(?:[\w.]+\.)?(?:Test|ParameterizedTest|RepeatedTest)\b(?:\s*\([^)]*\))?` +
		`(?:\s+@[\w.]+(?:\s*\([^)]*\))?)*\s+(?:(?:public|protected|private|static|final|synchronized)\s+)*void\s+(\w+)\s*\(`)
)

// javaCode is the source with comments and string literals blanked, so an
// annotation inside either is not read. Every newline is kept, so a position
// in it is on the same line as in the source.
func javaCode(content []byte) string {
	keepLines := func(s string) string { return strings.Repeat("\n", strings.Count(s, "\n")) }
	s := javaTextBlock.ReplaceAllStringFunc(string(content), func(m string) string { return `""` + keepLines(m) })
	s = javaBlockComment.ReplaceAllStringFunc(s, func(m string) string { return " " + keepLines(m) })
	s = javaString.ReplaceAllString(s, `""`)
	return javaLineComment.ReplaceAllString(s, "")
}

// Tests are the void methods annotated @Test, @ParameterizedTest or
// @RepeatedTest, by name, each spanning its annotation to the closing brace
// of its body (a method with no body, ended by a semicolon, spans to it).
func (surefireXML) Tests(_ string, content []byte) ([]TestSpan, error) {
	code := javaCode(content)
	var spans []TestSpan
	for _, m := range javaTestMethod.FindAllStringSubmatchIndex(code, -1) {
		spans = append(spans, TestSpan{
			ID:    code[m[2]:m[3]],
			Start: 1 + strings.Count(code[:m[0]], "\n"),
			End:   1 + strings.Count(code[:javaBodyEnd(code, m[1])], "\n"),
		})
	}
	return uniqueSpans(spans), nil
}

// javaBodyEnd is the offset of the character that ends the method whose
// parameter list opened just before from: its body's closing brace, or the
// semicolon of a method with no body. code has no comments or strings; a
// character literal is skipped. At the end of code when nothing closes.
func javaBodyEnd(code string, from int) int {
	depth, parens := 0, 1
	for i := from; i < len(code); i++ {
		switch c := code[i]; {
		case c == '\'':
			if end := strings.IndexByte(code[i+1:], '\''); end >= 0 && end <= 3 {
				i += end + 1
				if code[i-1] == '\\' && i+1 < len(code) && code[i+1] == '\'' {
					i++
				}
			}
		case parens > 0 && c == '(':
			parens++
		case parens > 0 && c == ')':
			parens--
		case parens > 0:
		case c == ';' && depth == 0:
			return i
		case c == '{':
			depth++
		case c == '}':
			if depth--; depth == 0 {
				return i
			}
		}
	}
	return len(code) - 1
}

// surefireSelector is the -Dtest value that runs the invocation's methods of
// the class (Class#m1+m2), or the whole class when there are none.
func surefireSelector(class string, ids []string) string {
	if len(ids) == 0 {
		return class
	}
	return class + "#" + strings.Join(ids, "+")
}

// Command compiles the module's main and test sources on their own first
// (`mvn test-compile`, its output kept in a temporary file and then copied
// to the log); when that fails the script exits before any test runs:
// surefireCompileExit when the output names the reproduction's path (the
// factory-checked relative path, IsCleanRelative, after a "/", searched as a
// fixed string; javac names a file by its absolute path), surefireBuildExit
// otherwise. It then runs the one class in
// its module and copies its report out; the class's binary name is read
// from the path under src/test/java. The module's surefire-reports
// directory is deleted before Maven runs, so the report copied out is one
// this launch wrote: a commit that tracks a report there (a stale pass)
// leaves none, and the launch reads as missing.
func (surefireXML) Command(inv Invocation) Command {
	rel := inv.rel()
	i := strings.Index("/"+rel, "/"+surefireSourceRoot)
	module := rel[:i]
	fqcn := strings.ReplaceAll(strings.TrimSuffix(rel[i+len(surefireSourceRoot):], ".java"), "/", ".")
	class := path.Base(strings.ReplaceAll(fqcn, ".", "/"))
	reports := module + "target/surefire-reports"
	report := reports + "/TEST-" + fqcn + ".xml"
	pom := shellQuote(module + "pom.xml")
	script := inv.inDir(fmt.Sprintf(`rm -rf -- %s || exit 125; log=$(mktemp) || exit 125; mvn -B -q -f %s test-compile >"$log" 2>&1; rc=$?; cat "$log"; `+
		`if [ "$rc" -ne 0 ]; then grep -qF -- %s "$log" && exit %d; exit %d; fi; `+
		"mvn -B -q -f %s -Dtest=%s -Dsurefire.failIfNoSpecifiedTests=false -Djunit.jupiter.execution.timeout.default=%ds -Djunit.jupiter.execution.timeout.thread.mode.default=SEPARATE_THREAD test; rc=$?; cp %s %s 2>/dev/null; exit $rc",
		shellQuote(reports), pom, shellQuote("/"+rel), surefireCompileExit, surefireBuildExit,
		pom, shellQuote(surefireSelector(class, inv.TestIDs)), int(PerTestTimeout.Seconds()), shellQuote(report), shellQuote(inv.ReportPath)))
	return Command{Script: script, ReportPath: inv.ReportPath, Timeout: launchTimeout(inv.TestIDs)}
}

// Classify: no report after the compile step failed naming the
// reproduction (surefireCompileExit) is a compile error; any other launch
// with no report, one whose compile step failed without naming it
// (surefireBuildExit) included, is missing. A case's
// name is its method, with any "(...)" or "[...]" suffix dropped.
func (surefireXML) Classify(_ string, report []byte, exit int, ids []string) (map[string]Kind, Outcome) {
	cases, ok := readJUnit(report)
	if !ok {
		if report == nil && exit == surefireCompileExit {
			return allKind(ids, KindCompile), OutcomeCompile
		}
		return allKind(ids, KindMissing), OutcomeMissing
	}
	kinds := map[string]Kind{}
	for _, c := range cases {
		name, _, _ := strings.Cut(c.Name, "(")
		name, _, _ = strings.Cut(name, "[")
		merge(kinds, strings.TrimSpace(name), surefireKind(c))
	}
	return fromKinds(kinds, ids)
}

func surefireKind(c junitCase) Kind {
	switch {
	case c.Failure != nil && isJavaTimeout(c.Failure):
		return KindTimeout
	case c.Failure != nil:
		return KindFail
	case c.Error != nil && isJavaTimeout(c.Error):
		return KindTimeout
	case c.Error != nil:
		return KindError
	case c.Skipped != nil:
		return KindSkipped
	}
	return KindPass
}

func isJavaTimeout(p *junitProblem) bool {
	return strings.Contains(p.Type, "TimeoutException") || strings.Contains(p.Message, "timed out after")
}
