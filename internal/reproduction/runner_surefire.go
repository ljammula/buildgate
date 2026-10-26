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
// repository with a root pom.xml.
func (surefireXML) Applies(p string, root RootFacts) bool {
	return strings.Contains("/"+p, "/"+surefireSourceRoot) &&
		baseMatches(p, "Test*.java", "*Test.java", "*Tests.java", "*TestCase.java") &&
		root.Has("pom.xml")
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
// annotation inside either is not read.
func javaCode(content []byte) string {
	s := javaTextBlock.ReplaceAllString(string(content), `""`)
	s = javaBlockComment.ReplaceAllString(s, " ")
	s = javaString.ReplaceAllString(s, `""`)
	return javaLineComment.ReplaceAllString(s, "")
}

// TestIDs are the void methods annotated @Test, @ParameterizedTest or
// @RepeatedTest, by name.
func (surefireXML) TestIDs(_ string, content []byte) ([]string, error) {
	var ids []string
	for _, m := range javaTestMethod.FindAllStringSubmatch(javaCode(content), -1) {
		ids = append(ids, m[1])
	}
	return uniqueIDs(ids), nil
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
	i := strings.Index("/"+inv.Path, "/"+surefireSourceRoot)
	module := inv.Path[:i]
	fqcn := strings.ReplaceAll(strings.TrimSuffix(inv.Path[i+len(surefireSourceRoot):], ".java"), "/", ".")
	class := path.Base(strings.ReplaceAll(fqcn, ".", "/"))
	reports := module + "target/surefire-reports"
	report := reports + "/TEST-" + fqcn + ".xml"
	pom := shellQuote(module + "pom.xml")
	script := fmt.Sprintf(`rm -rf -- %s || exit 125; log=$(mktemp) || exit 125; mvn -B -q -f %s test-compile >"$log" 2>&1; rc=$?; cat "$log"; `+
		`if [ "$rc" -ne 0 ]; then grep -qF -- %s "$log" && exit %d; exit %d; fi; `+
		"mvn -B -q -f %s -Dtest=%s -Dsurefire.failIfNoSpecifiedTests=false -Djunit.jupiter.execution.timeout.default=%ds -Djunit.jupiter.execution.timeout.thread.mode.default=SEPARATE_THREAD test; rc=$?; cp %s %s 2>/dev/null; exit $rc",
		shellQuote(reports), pom, shellQuote("/"+inv.Path), surefireCompileExit, surefireBuildExit,
		pom, shellQuote(class), int(PerTestTimeout.Seconds()), shellQuote(report), shellQuote(inv.ReportPath))
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
