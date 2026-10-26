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
	rbClass    = regexp.MustCompile(`^\s*class\s+((?:\w+::)*)(\w+)\b`)
	rbDefTest  = regexp.MustCompile(`^\s*def\s+(test_\w+[?!]?)`)
	rbTestDecl = regexp.MustCompile(`^\s*test\s+(?:"([^"\\#]*)"|'([^'\\]*)')\s*(?:do\b|\{)`)
	rbSpaces   = regexp.MustCompile(`\s+`)
)

// TestIDs are "Class#test_name": def test_* methods, and ActiveSupport's
// test "name" do blocks (named test_ plus the name with spaces as
// underscores), under the last class opened above them (its last name
// component).
func (minitestJSON) TestIDs(_ string, content []byte) ([]string, error) {
	var ids []string
	class := ""
	for _, line := range strings.Split(string(content), "\n") {
		if m := rbClass.FindStringSubmatch(line); m != nil {
			class = m[2]
			continue
		}
		name := ""
		if m := rbDefTest.FindStringSubmatch(line); m != nil {
			name = m[1]
		} else if m := rbTestDecl.FindStringSubmatch(line); m != nil {
			name = "test_" + rbSpaces.ReplaceAllString(m[1]+m[2], "_")
		}
		if name != "" && class != "" {
			ids = append(ids, class+"#"+name)
		}
	}
	return uniqueIDs(ids), nil
}

func (minitestJSON) Command(inv Invocation) Command {
	script := fmt.Sprintf("BG_REPRO_REPORT=%s BG_REPRO_TEST_TIMEOUT=%d ruby -Itest -Ilib -I%s -rbg_repro_reporter -e 'BgRepro.load_test(ARGV[0])' %s",
		shellQuote(inv.ReportPath), int(PerTestTimeout.Seconds()), shellQuote(inv.HelperDir), shellQuote(inv.Path))
	return Command{Script: script, ReportPath: inv.ReportPath, Helpers: helper(minitestReporter), Timeout: launchTimeout(inv.TestIDs)}
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
