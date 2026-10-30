package reproduction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// minitestJSON runs a Ruby test file under Minitest with the embedded
// bg_repro_reporter, which runs each test under the per-test timeout and
// writes a JSON report; it loads the file itself, so a load or syntax error
// is reported as one. The timeout is Ruby's Timeout.timeout, raised in the
// test's thread at the VM's next interrupt check: a loop in Ruby code is
// interrupted; one inside a C call that never checks, or code that rescues
// the exception, is cut only by the launch's cap, which reads as a timeout
// of every test (CapExceeded).
type minitestJSON struct{}

const minitestReporter = "bg_repro_reporter.rb"

func (minitestJSON) ID() string { return "minitest-json" }

func (minitestJSON) Applies(p string, root RootFacts) bool {
	return baseMatches(p, "*_test.rb", "test_*.rb") && root.Has("Gemfile")
}

var (
	rbClass      = regexp.MustCompile(`^\s*class\s+((?:\w+::)*)(\w+)\b`)
	rbDefTest    = regexp.MustCompile(`^\s*def\s+(test_\w+[?!]?)`)
	rbTestDecl   = regexp.MustCompile(`^\s*test\s+(?:"([^"\\#]*)"|'([^'\\]*)')\s*(do\b|\{)`)
	rbEndlessDef = regexp.MustCompile(`^def\s+[^(\s]+(?:\([^)]*\))?\s*=`)
	rbSpaces     = regexp.MustCompile(`\s+`)
)

// Tests are "Class#test_name": def test_* methods, and ActiveSupport's
// test "name" do blocks (named test_ plus the name with spaces as
// underscores), under the last class opened above them (its last name
// component). A test spans from its line to the closing end (or brace) at its
// own indentation; one that closes on its own line spans that line.
func (minitestJSON) Tests(_ string, content []byte) ([]TestSpan, error) {
	var spans []TestSpan
	class := ""
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		if m := rbClass.FindStringSubmatch(line); m != nil {
			class = m[2]
			continue
		}
		name, brace := "", false
		if m := rbDefTest.FindStringSubmatch(line); m != nil {
			name = m[1]
		} else if m := rbTestDecl.FindStringSubmatch(line); m != nil {
			name = "test_" + rbSpaces.ReplaceAllString(m[1]+m[2], "_")
			brace = m[3] == "{"
		}
		if name != "" && class != "" {
			spans = append(spans, TestSpan{ID: class + "#" + name, Start: i + 1, End: rbBlockEnd(lines, i, brace)})
		}
	}
	return uniqueSpans(spans), nil
}

// rbBlockEnd is the 1-based line that closes the block opened on lines[i]: the
// first later line at the opening line's indentation that starts with end (or
// } for a brace block), the opening line itself when it closes there (a
// trailing end or }, or an endless def), and the last line when none does.
func rbBlockEnd(lines []string, i int, brace bool) int {
	closer := "end"
	if brace {
		closer = "}"
	}
	open := lines[i]
	trimmed := strings.TrimSpace(open)
	if strings.HasSuffix(trimmed, " end") || strings.HasSuffix(trimmed, ";end") || strings.HasSuffix(trimmed, "}") ||
		rbEndlessDef.MatchString(trimmed) {
		return i + 1
	}
	indent := len(open) - len(strings.TrimLeft(open, " \t"))
	for j := i + 1; j < len(lines); j++ {
		l := lines[j]
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if len(l)-len(strings.TrimLeft(l, " \t")) == indent && (t == closer || strings.HasPrefix(t, closer+" ") || strings.HasPrefix(t, closer+".")) {
			return j + 1
		}
	}
	return len(lines)
}

func (minitestJSON) Command(inv Invocation) Command {
	script := fmt.Sprintf("BG_REPRO_REPORT=%s BG_REPRO_TEST_TIMEOUT=%d ruby -Itest -Ilib -I%s -rbg_repro_reporter -e 'BgRepro.load_test(ARGV[0])' %s%s",
		shellQuote(inv.ReportPath), int(PerTestTimeout.Seconds()), shellQuote(inv.HelperDir), shellQuote(inv.Path), minitestNameFilter(inv.TestIDs))
	return Command{Script: script, ReportPath: inv.ReportPath, Helpers: helper(minitestReporter), Timeout: launchTimeout(inv.TestIDs)}
}

// minitestNameFilter is minitest's -n option over the invocation's method
// names (the part of each ID after the class), each quoted and anchored. A
// same-named test of another class runs too and is not judged. No IDs, no
// option.
func minitestNameFilter(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	names := make([]string, len(ids))
	for i, id := range ids {
		_, name, _ := strings.Cut(id, "#")
		names[i] = regexp.QuoteMeta(name)
	}
	return " -n " + shellQuote("/^("+strings.Join(names, "|")+")$/")
}

type minitestReport struct {
	LoadError *string `json:"load_error"`
	Tests     []struct {
		ID     string `json:"id"`
		Result string `json:"result"`
	} `json:"tests"`
}

// Classify: a load_error is a compile; a report id's class is cut to its
// last name component, as TestIDs names it.
func (minitestJSON) Classify(_ string, report []byte, _ int, ids []string) (map[string]Kind, Outcome) {
	var r minitestReport
	if len(bytes.TrimSpace(report)) == 0 || json.Unmarshal(report, &r) != nil {
		return allKind(ids, KindMissing), OutcomeMissing
	}
	if r.LoadError != nil {
		return allKind(ids, KindCompile), OutcomeCompile
	}
	kinds := map[string]Kind{}
	for _, t := range r.Tests {
		class, name, ok := strings.Cut(t.ID, "#")
		if !ok {
			continue
		}
		if i := strings.LastIndex(class, "::"); i >= 0 {
			class = class[i+2:]
		}
		merge(kinds, class+"#"+name, minitestKind(t.Result))
	}
	return fromKinds(kinds, ids)
}

func minitestKind(result string) Kind {
	switch k := Kind(result); k {
	case KindPass, KindFail, KindError, KindTimeout, KindSkipped:
		return k
	}
	return KindMissing
}
