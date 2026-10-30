package reproduction

import (
	"fmt"
	"regexp"
	"strings"
)

// pytestJUnit runs a Python test file with pytest and reads its JUnit XML
// report. The embedded bg_repro_timeout plugin sets an alarm for each test
// phase; its BgReproTimeout exception names a timeout in the report. Python
// runs the alarm's handler in the main thread between bytecodes, so a loop
// in Python code is interrupted; one inside a C call that never returns to
// the interpreter, or code that catches the exception, is cut only by the
// launch's cap, which reads as a timeout of every test (CapExceeded).
type pytestJUnit struct{}

const pytestTimeoutPlugin = "bg_repro_timeout.py"

func (pytestJUnit) ID() string { return "pytest-junitxml" }

func (pytestJUnit) Applies(p string, root RootFacts) bool {
	return baseMatches(p, "test_*.py", "*_test.py") &&
		root.Has("pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "pytest.ini", "tox.ini")
}

var (
	pyTopTest   = regexp.MustCompile(`^(?:async\s+)?def\s+(test\w*)\s*\(`)
	pyClass     = regexp.MustCompile(`^class\s+(Test\w*)\s*[:(]`)
	pyClassTest = regexp.MustCompile(`^[ \t]+(?:async\s+)?def\s+(test\w*)\s*\(`)
)

// Tests are pytest's default collection from the file's text: top-level
// test* functions as "name", and test* methods of top-level Test* classes as
// "Class::name". Lines inside triple-quoted strings are skipped. A test spans
// from its def line to the last line indented deeper than it (blank and
// comment lines do not extend it).
func (pytestJUnit) Tests(_ string, content []byte) ([]TestSpan, error) {
	var spans []TestSpan
	class := ""
	open := -1 // index in spans of the test whose block is open
	openIndent := 0
	inString := false
	for i, line := range strings.Split(string(content), "\n") {
		no := i + 1
		quoted := inString
		inString = togglesTripleQuote(line, inString)
		if quoted {
			if open >= 0 {
				spans[open].End = no
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if open >= 0 && indent <= openIndent {
			open = -1
		}
		if open >= 0 {
			spans[open].End = no
			continue
		}
		var id string
		id, class = pyLine(line, class)
		if id != "" {
			spans = append(spans, TestSpan{ID: id, Start: no, End: no})
			open, openIndent = len(spans)-1, indent
		}
		if inString && open >= 0 {
			spans[open].End = no // the def line opens a string that runs on
		}
	}
	return uniqueSpans(spans), nil
}

// togglesTripleQuote reports whether a triple-quoted string is open after
// the line, given whether one was open before it.
func togglesTripleQuote(line string, open bool) bool {
	n := strings.Count(line, `"""`) + strings.Count(line, `'''`)
	return open != (n%2 == 1)
}

// pyLine reads one line outside a string: a top-level line ends the current
// class, and may open a Test* class or name a test (its ID, "" for none).
func pyLine(line, class string) (id, newClass string) {
	if line[0] != ' ' && line[0] != '\t' {
		if strings.HasPrefix(line, "@") {
			return "", class // a decorator keeps the class open for the next line
		}
		if m := pyClass.FindStringSubmatch(line); m != nil {
			return "", m[1]
		}
		if m := pyTopTest.FindStringSubmatch(line); m != nil {
			return m[1], ""
		}
		return "", ""
	}
	if m := pyClassTest.FindStringSubmatch(line); m != nil && class != "" && methodIndent(line) {
		return class + "::" + m[1], class
	}
	return "", class
}

// methodIndent: one level of indentation (four spaces or one tab), the
// class body itself rather than a nested definition.
func methodIndent(line string) bool {
	return (strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ")) ||
		(strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "\t\t"))
}

// pytestInterpreter picks the repository's own Python, which is where its
// setup commands installed pytest: an active $VIRTUAL_ENV, then a `.venv` or
// `venv` directory at the worktree root, else python3 on PATH. It tests
// paths only; it reads no file.
const pytestInterpreter = `BG_PY=python3; if [ -n "${VIRTUAL_ENV:-}" ] && [ -x "$VIRTUAL_ENV/bin/python" ]; then BG_PY="$VIRTUAL_ENV/bin/python"; elif [ -x .venv/bin/python ]; then BG_PY=.venv/bin/python; elif [ -x venv/bin/python ]; then BG_PY=venv/bin/python; fi; `

func (pytestJUnit) Command(inv Invocation) Command {
	script := pytestInterpreter + fmt.Sprintf(`PYTHONPATH=%s"${PYTHONPATH:+:$PYTHONPATH}" BG_REPRO_TEST_TIMEOUT=%d "$BG_PY" -m pytest -p no:cacheprovider -o addopts= -p bg_repro_timeout --junitxml=%s -q %s`,
		shellQuote(inv.HelperDir), int(PerTestTimeout.Seconds()), shellQuote(inv.ReportPath), pytestNodeIDs(inv))
	return Command{Script: script, ReportPath: inv.ReportPath, Helpers: helper(pytestTimeoutPlugin), Timeout: launchTimeout(inv.TestIDs)}
}

// pytestNodeIDs are the invocation's tests as pytest node IDs, so a launch
// runs those tests and not the rest of the file; with none, the file.
func pytestNodeIDs(inv Invocation) string {
	if len(inv.TestIDs) == 0 {
		return shellQuote(inv.Path)
	}
	words := make([]string, len(inv.TestIDs))
	for i, id := range inv.TestIDs {
		words[i] = shellQuote(inv.Path + "::" + id)
	}
	return strings.Join(words, " ")
}

// pyModuleError is Python's error for a module it cannot find.
var pyModuleError = regexp.MustCompile(`ModuleNotFoundError`)

// Classify: a collection error case whose text names the reproduction's own
// file and is no ModuleNotFoundError (compileFailure) is a compile: a name
// the reproduction imports that the module does not have ("cannot import
// name"), an attribute it reads at import (AttributeError), a syntax error.
// A module that cannot be found, whatever it is, any other collection
// error, and exit 2 to 5 with no such case (interrupted, internal error,
// usage error such as a conftest that does not import, nothing collected:
// pytest writes no report for 3 and 4) are missing. Otherwise each case's
// failure, error or skip, merged per ID over its parameter sets.
func (pytestJUnit) Classify(p string, report []byte, exit int, ids []string) (map[string]Kind, Outcome) {
	cases, ok := readJUnit(report)
	collection := pyCollectionErrors(cases)
	for _, c := range collection {
		if compileFailure(c.Error.Message+"\n"+c.Error.Body, p, pyModuleError) {
			return allKind(ids, KindCompile), OutcomeCompile
		}
	}
	if !ok || len(collection) > 0 || (exit >= 2 && exit <= 5) {
		return allKind(ids, KindMissing), OutcomeMissing
	}
	kinds := map[string]Kind{}
	for _, c := range cases {
		merge(kinds, pyCaseID(c), pyCaseKind(c))
	}
	return fromKinds(kinds, ids)
}

// pyCollectionErrors: pytest records a module that failed to collect as a
// case with no class name and an <error>.
func pyCollectionErrors(cases []junitCase) []junitCase {
	var out []junitCase
	for _, c := range cases {
		if c.ClassName == "" && c.Error != nil {
			out = append(out, c)
		}
	}
	return out
}

// pyCaseID maps a case to its ID: the parameter suffix of the name is
// dropped, and a class name after the module's dotted path is the class.
// The module is the last component of the class name that starts with
// "test" or ends in "_test"; classes follow it.
func pyCaseID(c junitCase) string {
	name, _, _ := strings.Cut(c.Name, "[")
	parts := strings.Split(c.ClassName, ".")
	for i := len(parts) - 1; i >= 0; i-- {
		if baseMatches(parts[i]+".py", "test_*.py", "*_test.py") {
			if classes := parts[i+1:]; len(classes) > 0 {
				return strings.Join(classes, "::") + "::" + name
			}
			return name
		}
	}
	return name
}

func pyCaseKind(c junitCase) Kind {
	switch {
	case c.Failure != nil:
		return pyProblemKind(c.Failure, KindFail)
	case c.Error != nil:
		return pyProblemKind(c.Error, KindError)
	case c.Skipped != nil:
		return KindSkipped
	}
	return KindPass
}

// pyProblemKind: the plugin's timeout; for a failure, an assertion is a fail
// and any other exception an error.
func pyProblemKind(p *junitProblem, def Kind) Kind {
	if strings.Contains(p.Message, "BgReproTimeout") {
		return KindTimeout
	}
	if def == KindError {
		return KindError
	}
	if strings.HasPrefix(p.Message, "assert") || strings.Contains(p.Message, "AssertionError") ||
		strings.HasSuffix(strings.TrimSpace(p.Body), ": AssertionError") {
		return KindFail
	}
	return KindError
}
