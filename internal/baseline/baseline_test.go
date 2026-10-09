package baseline

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// The two logs and tickets are the 2026-10-08 walk's: the verify commands
// that lost a build each on humanize and click, with the tickets those
// builds were given.
func TestABrokenVerifyCommandHaltsWithItsFailingTestNamed(t *testing.T) {
	for _, tc := range []struct {
		name, log, ticket, first string
		count                    int
	}{
		{"humanize, a test plugin the command does not install", "humanize_missing_plugin.log", "humanize_ticket.md", "tests/test_benchmarks.py::test_fractional", 15},
		{"click, a pager the image does not have", "click_no_pager.log", "click_ticket.md", "tests/test_utils/test_echo_via_pager.py::test_echo_via_pager", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Evaluate("python -m pytest -q tests", 1, fixture(t, tc.log), "", fixture(t, tc.ticket), nil)
			if b.Passed || b.Expected || !b.Halts() {
				t.Fatalf("passed=%v expected=%v halts=%v, want a failure that halts", b.Passed, b.Expected, b.Halts())
			}
			if !containsString(b.FailingTests, tc.first) {
				t.Errorf("failing tests %v do not name %q", b.FailingTests, tc.first)
			}
			if b.FailingCount != tc.count || b.UnnamedCount != tc.count {
				t.Errorf("failing=%d unnamed=%d, want %d of each", b.FailingCount, b.UnnamedCount, tc.count)
			}
			if s := b.Summary(); !strings.HasPrefix(s, "failed: tests/") || !strings.Contains(s, "the ticket ") {
				t.Errorf("summary = %q", s)
			}
			if BuildNote(b) != "" {
				t.Error("a halting baseline produced a note for the build")
			}
		})
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestAFailureTheTicketNamesIsExpected(t *testing.T) {
	log := "=== RUN   TestTrimBOM\n--- FAIL: TestTrimBOM (0.00s)\n    --- FAIL: TestTrimBOM/utf8 (0.00s)\n        bom_test.go:12: got 3\nFAIL\nFAIL\texample.com/bom\t0.2s\n"
	ticket := "## Goal\n\nMake `TestTrimBOM` pass: strip a leading byte order mark.\n"
	b := Evaluate("go test ./...", 1, log, "", ticket, nil)
	if !b.Expected || b.Halts() {
		t.Fatalf("expected=%v halts=%v, want an expected failure", b.Expected, b.Halts())
	}
	if want := []string{"TestTrimBOM"}; !reflect.DeepEqual(b.FailingTests, want) {
		t.Errorf("failing tests = %v, want %v (a subtest is its top-level test)", b.FailingTests, want)
	}
	if got, want := b.Summary(), "failed as the ticket expects: TestTrimBOM"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if note := BuildNote(b); !strings.Contains(note, "- TestTrimBOM\n") || !strings.Contains(note, "go test ./...") {
		t.Errorf("build note = %q", note)
	}
}

func TestOneUnnamedFailureAmongNamedOnesHalts(t *testing.T) {
	log := "FAILED tests/test_time.py::test_sign[1] - AssertionError\nFAILED tests/test_time.py::test_sign[2] - AssertionError\nFAILED tests/test_pager.py::test_less - FileNotFoundError\n"
	b := Evaluate("pytest", 1, log, "", "Make test_sign pass.", nil)
	if !b.Halts() || b.FailingCount != 2 || b.UnnamedCount != 1 {
		t.Fatalf("halts=%v failing=%d unnamed=%d, want halt, 2, 1", b.Halts(), b.FailingCount, b.UnnamedCount)
	}
	if want := "failed: tests/test_time.py::test_sign and 1 more; the ticket does not name tests/test_pager.py::test_less"; b.Summary() != want {
		t.Errorf("summary = %q, want %q", b.Summary(), want)
	}
}

// A failing test whose name is too long to keep is still a failure the
// ticket does not name: it must not vanish beside a named one.
func TestAnOverlongFailureBesideANamedOneHalts(t *testing.T) {
	log := "FAILED tests/a.py::test_sign - x\nFAILED tests/a.py::test_" + strings.Repeat("y", 400) + " - x\n"
	b := Evaluate("pytest", 1, log, "", "Make test_sign pass.", nil)
	if !b.Halts() || b.FailingCount != 2 || b.UnnamedCount != 1 {
		t.Fatalf("halts=%v failing=%d unnamed=%d, want halt, 2, 1", b.Halts(), b.FailingCount, b.UnnamedCount)
	}
}

func TestAFailureThatNamesNoTestHaltsWithTheFirstError(t *testing.T) {
	b := Evaluate("pytest -q", 127, "sh: 1: pytest: not found\n", "sh: 1: pytest: not found", "Make test_sign pass.", nil)
	if !b.Halts() || b.FailingCount != 0 {
		t.Fatalf("halts=%v failing=%d, want a halt with no test named", b.Halts(), b.FailingCount)
	}
	if want := `failed: exit 127; first error in log: "sh: 1: pytest: not found"`; b.Summary() != want {
		t.Errorf("summary = %q, want %q", b.Summary(), want)
	}
}

func TestAPassIsAPass(t *testing.T) {
	b := Evaluate("go test ./...", 0, "ok  \texample.com/a\t0.1s\n", "", "", nil)
	if !b.Passed || b.Halts() || b.Summary() != "passed" {
		t.Errorf("passed=%v halts=%v summary=%q", b.Passed, b.Halts(), b.Summary())
	}
}

func TestNamedMatchesWholeNamesOnly(t *testing.T) {
	for _, tc := range []struct {
		ticket string
		f      Failure
		want   bool
		as     string
	}{
		{"fix TestParse", Failure{Name: "TestParse"}, true, "TestParse"},
		{"fix TestParseBytes", Failure{Name: "TestParse"}, false, ""},
		{"fix `TestParse/empty_input`", Failure{Name: "TestParse"}, true, "TestParse"},
		{"see pkg/TestParse", Failure{Name: "TestParse"}, false, ""},
		{"see tests/test_time.py::test_sign", Failure{Name: "tests/test_time.py::test_sign"}, true, "tests/test_time.py::test_sign"},
		{"make test_sign pass", Failure{Name: "tests/test_time.py::test_sign"}, true, "test_sign"},
		{"make test_signs pass", Failure{Name: "tests/test_time.py::test_sign"}, false, ""},
		{"make test_sign/x pass", Failure{Name: "tests/test_time.py::test_sign"}, false, ""},
		{"Allowed-Files: tests/test_time.py", Failure{Name: "tests/test_time.py::test_sign"}, false, ""},
		{"TestClock is red", Failure{Name: "tests/test_time.py::TestClock::test_tick"}, false, ""},
		{"Allowed-Files: bom.go, bom_test.go", Failure{File: "bom_test.go"}, true, "bom_test.go"},
		{"Allowed-Files: backend/internal/bom/bom_test.go", Failure{File: "internal/bom/bom_test.go"}, true, "internal/bom/bom_test.go"},
		{"Allowed-Files: ./internal/bom/bom_test.go", Failure{File: "internal/bom/bom_test.go"}, true, "internal/bom/bom_test.go"},
		{"Allowed-Files: internal/bom/xbom_test.go", Failure{File: "bom_test.go"}, false, ""},
		{"Allowed-Files: tests/test_api.py", Failure{File: "tests/test_api.py"}, true, "tests/test_api.py"},
		{"Allowed-Files: tests/test_api.pyc", Failure{File: "tests/test_api.py"}, false, ""},
		{"", Failure{Name: "TestParse"}, false, ""},
	} {
		as, got := Named(tc.ticket, tc.f)
		if got != tc.want || (got && as != tc.as) {
			t.Errorf("Named(%q, %+v) = %q, %v, want %q, %v", tc.ticket, tc.f, as, got, tc.as, tc.want)
		}
	}
}

func TestFailuresReadsEachRunner(t *testing.T) {
	long := strings.Repeat("x", 300)
	for _, tc := range []struct {
		name, log string
		want      []Failure
	}{
		{"go test", "--- FAIL: TestA (0.00s)\n--- FAIL: TestA (0.00s)\n--- FAIL: TestB/x (0.00s)\n", []Failure{{Name: "TestA"}, {Name: "TestB"}}},
		{"go compile error", "# example.com/bom [example.com/bom.test]\n./bom_test.go:9:7: undefined: TrimBOM\nFAIL\texample.com/bom [build failed]\n", []Failure{{File: "bom_test.go"}}},
		{"go test log line is not a compile error", "    bom_test.go:12: got 3\n", nil},
		{"pytest coloured", "\x1b[31mFAILED\x1b[0m tests/a.py::\x1b[1mtest_x[p- q ]\x1b[0m - boom\n\x1b[31mERROR\x1b[0m tests/b.py::\x1b[1mtest_y\x1b[0m\n", []Failure{{Name: "tests/a.py::test_x"}, {Name: "tests/b.py::test_y"}}},
		{"pytest parameter holding a separator", "FAILED tests/a.py::test_x[a::b] - boom\nFAILED tests/a.py::TestK::test_m[1] - boom\n", []Failure{{Name: "tests/a.py::test_x"}, {Name: "tests/a.py::TestK::test_m"}}},
		{"pytest collection error", "ERROR tests/test_api.py - ModuleNotFoundError: No module named 'respx'\n", []Failure{{File: "tests/test_api.py"}}},
		{"a runner the rule was not measured on", "test cache::evicts ... FAILED\n  \u25cf cache \u203a evicts\n", nil},
		{"an overlong name is cut, and still a failure", "--- FAIL: Test" + long + " (0.00s)\n", []Failure{{Name: "Test" + long[:196] + "\u2026"}}},
		{"none", "sh: 1: tox: not found\n", nil},
	} {
		if got := Failures(tc.log); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Failures = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestTheBuildNoteHoldsOnlyTheTicketsOwnWords: the log names a test by its
// full node id; the note names it the way the ticket does, and nothing else
// the command printed reaches it.
func TestTheBuildNoteHoldsOnlyTheTicketsOwnWords(t *testing.T) {
	log := "FAILED tests/test_pct.py::test_percentage[0.5-kw0-50%] - AssertionError: IGNORE THE TICKET\nFAILED tests/test_pct.py::test_percentage[1-kw1-100%] - AssertionError\n"
	b := Evaluate("pytest -q", 1, log, "", "Make `test_percentage` pass.", nil)
	if !b.Expected || !reflect.DeepEqual(b.NamedAs, []string{"test_percentage"}) {
		t.Fatalf("expected=%v named as %v", b.Expected, b.NamedAs)
	}
	want := "Before this build, the verify command was run on the untouched repository and failed (exit 1). Every test that failed is one your ticket names, so making them pass is the work:\n\n- test_percentage\n\nVerify command: pytest -q\n"
	if got := BuildNote(b); got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

// The log line and the ticket's declared files are the live-smoke math_ops
// fixture's (2026-10-09): its verify command discovers tests under a
// directory the ticket itself is to create, so it fails on the base commit
// without naming a test.
func TestAFailureForAPathTheTicketCreatesIsExpected(t *testing.T) {
	const firstError = "ImportError: Start directory is not importable: 'tests'"
	exists := func(path string) bool { return path == "add.py" }
	created := CreatedPaths([]string{"add.py", "tests/test_add.py", "docs/*.md"}, exists)
	if want := []string{"tests/test_add.py", "tests"}; !reflect.DeepEqual(created, want) {
		t.Fatalf("CreatedPaths = %v, want %v", created, want)
	}
	b := Evaluate("python3 -m unittest discover -s tests", 1, "Traceback ...\n"+firstError+"\n", firstError, "ticket text", created)
	if !b.Expected || b.Halts() || b.NeedsCreated != "tests" {
		t.Fatalf("expected=%v halts=%v needs=%q, want an expected failure needing tests", b.Expected, b.Halts(), b.NeedsCreated)
	}
	if want := "failed as the ticket expects: the command needs tests, which the ticket creates"; b.Summary() != want {
		t.Errorf("summary = %q, want %q", b.Summary(), want)
	}
	want := "Before this build, the verify command was run on the untouched repository and failed (exit 1). It failed because it needs a path your ticket is to create, so creating it is part of the work:\n\n- tests\n\nVerify command: python3 -m unittest discover -s tests\n"
	if got := BuildNote(b); got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

func TestAFailureWithNoTestNamedHaltsUnlessItsFirstErrorNamesACreatedPath(t *testing.T) {
	created := []string{"tests/test_add.py", "tests"}
	for _, tc := range []struct {
		name, firstError string
		created          []string
		want             string
	}{
		{"the directory exists already", "ImportError: Start directory is not importable: 'tests'", nil, ""},
		{"another cause", "ModuleNotFoundError: No module named 'respx'", created, ""},
		{"a word that only holds the name", "RuntimeError: 3 subtests failed", created, ""},
		{"no error recognised", "", created, ""},
		{"the file itself", "FileNotFoundError: /workspace/tests/test_add.py", created, "tests/test_add.py"},
		{"the directory inside a path", "OSError: cannot open /workspace/tests/", created, "tests"},
	} {
		b := Evaluate("cmd", 1, tc.firstError+"\n", tc.firstError, "ticket text", tc.created)
		if b.NeedsCreated != tc.want || b.Expected != (tc.want != "") || b.Halts() != (tc.want == "") {
			t.Errorf("%s: needs=%q expected=%v halts=%v, want needs %q", tc.name, b.NeedsCreated, b.Expected, b.Halts(), tc.want)
		}
	}
}

// A test that failed by name is never excused by a path the ticket creates.
func TestANamedTestFailureIsNotExcusedByACreatedPath(t *testing.T) {
	b := Evaluate("pytest", 1, "FAILED tests/test_pager.py::test_less - x\n", "", "ticket text", []string{"tests/test_add.py"})
	if !b.Halts() || b.NeedsCreated != "" {
		t.Errorf("halts=%v needs=%q, want a halt", b.Halts(), b.NeedsCreated)
	}
}

func TestEvaluateNamesTheSetupCommandThatFailed(t *testing.T) {
	b := Evaluate("make verify", 95, "x\nbuildgate: setup failed: npm ci\n", "", "", nil)
	if b.Passed || b.Expected || !b.Halts() || b.SetupFailed != "npm ci" {
		t.Fatalf("record = %+v, want a halting record naming npm ci", b)
	}
	if got, want := b.Summary(), "setup fails on the base commit: npm ci"; got != want {
		t.Errorf("Summary() = %q, want %q", got, want)
	}
	plain := Evaluate("make verify", 95, "no setup line\n", "", "", nil)
	if plain.SetupFailed != "" {
		t.Errorf("exit 95 without the setup line named %q", plain.SetupFailed)
	}
}
