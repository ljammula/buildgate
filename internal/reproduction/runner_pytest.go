package reproduction

import (
	"fmt"
	"regexp"
	"strings"
)

// pytestJUnit runs a Python test file with pytest and reads its JUnit XML
// report. The embedded bg_repro_timeout plugin sets an alarm for each test
// phase; its BgReproTimeout exception names a timeout in the report.
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

// TestIDs are pytest's default collection from the file's text: top-level
// test* functions as "name", and test* methods of top-level Test* classes as
// "Class::name". Lines inside triple-quoted strings are skipped.
func (pytestJUnit) TestIDs(_ string, content []byte) ([]string, error) {
	var ids []string
	class := ""
	inString := false
	for _, line := range strings.Split(string(content), "\n") {
		quoted := inString
		inString = togglesTripleQuote(line, inString)
		if quoted {
			continue
		}
		ids, class = pyLine(line, class, ids)
	}
	return uniqueIDs(ids), nil
}

// togglesTripleQuote reports whether a triple-quoted string is open after
// the line, given whether one was open before it.
func togglesTripleQuote(line string, open bool) bool {
	n := strings.Count(line, `"""`) + strings.Count(line, `'''`)
	return open != (n%2 == 1)
}

// pyLine reads one line outside a string: a top-level line ends the current
// class, and may open a Test* class or name a test.
func pyLine(line, class string, ids []string) ([]string, string) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return ids, class
	}
	if line[0] != ' ' && line[0] != '\t' {
		if strings.HasPrefix(line, "@") {
			return ids, class // a decorator keeps the class open for the next line
		}
		class = ""
		if m := pyClass.FindStringSubmatch(line); m != nil {
			return ids, m[1]
		}
		if m := pyTopTest.FindStringSubmatch(line); m != nil {
			ids = append(ids, m[1])
		}
		return ids, class
	}
	if m := pyClassTest.FindStringSubmatch(line); m != nil && class != "" && methodIndent(line) {
		ids = append(ids, class+"::"+m[1])
	}
	return ids, class
}

// methodIndent: one level of indentation (four spaces or one tab), the
// class body itself rather than a nested definition.
func methodIndent(line string) bool {
	return (strings.HasPrefix(line, "    ") && !strings.HasPrefix(line, "     ")) ||
		(strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, "\t\t"))
}

func (pytestJUnit) Command(inv Invocation) Command {
	script := fmt.Sprintf(`PYTHONPATH=%s"${PYTHONPATH:+:$PYTHONPATH}" BG_REPRO_TEST_TIMEOUT=%d python3 -m pytest -p no:cacheprovider -o addopts= -p bg_repro_timeout --junitxml=%s -q %s`,
		shellQuote(inv.HelperDir), int(PerTestTimeout.Seconds()), shellQuote(inv.ReportPath), shellQuote(inv.Path))
	return Command{Script: script, ReportPath: inv.ReportPath, Helpers: helper(pytestTimeoutPlugin), Timeout: launchTimeout(inv.TestIDs)}
}

// Classify: exit 2 to 5 (interrupted, internal error, usage error, nothing
// collected) or a collection error case is a compile; otherwise each case's
// failure, error or skip, merged per ID over its parameter sets.
func (pytestJUnit) Classify(_ string, report []byte, exit int, ids []string) (map[string]Kind, Outcome) {
	cases, ok := readJUnit(report)
	if (exit >= 2 && exit <= 5) || (ok && pyCollectionError(cases)) {
		return allKind(ids, KindCompile), OutcomeCompile
	}
	if !ok {
		return allKind(ids, KindMissing), OutcomeMissing
	}
	kinds := map[string]Kind{}
	for _, c := range cases {
		merge(kinds, pyCaseID(c), pyCaseKind(c))
	}
	return fromKinds(kinds, ids)
}

// pyCollectionError: pytest records a module that failed to collect as a
// case with no class name and an <error>.
func pyCollectionError(cases []junitCase) bool {
	for _, c := range cases {
		if c.ClassName == "" && c.Error != nil {
			return true
		}
	}
	return false
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
