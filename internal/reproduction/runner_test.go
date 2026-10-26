package reproduction

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, runner, name string) []byte {
	t.Helper()
	if name == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join("testdata", runner, name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func runnerByID(t *testing.T, id string) Runner {
	t.Helper()
	for _, r := range Runners() {
		if r.ID() == id {
			return r
		}
	}
	t.Fatalf("no runner %s", id)
	return nil
}

type classifyCase struct {
	fixture string // "" for no report
	path    string
	exit    int
	id      string
	kind    Kind
	outcome Outcome
}

// TestRunnerClassifiesEveryOutcome reads each runner's recorded reports (the
// surefire-xml ones are hand-written; see its testdata README) for a pass, a
// failed assertion, an error, a timeout, a skip, a compile or collection
// error and a missing report.
func TestRunnerClassifiesEveryOutcome(t *testing.T) {
	tables := map[string][]classifyCase{
		"go-test-json": {
			{"pass.json", "p/repro_test.go", 0, "TestPass", KindPass, OutcomePassed},
			{"fail.json", "p/repro_test.go", 1, "TestFail", KindFail, OutcomeFailed},
			{"error.json", "p/repro_test.go", 1, "TestPanic", KindError, OutcomeFailed},
			{"timeout.json", "p/repro_test.go", 1, "TestHang", KindTimeout, OutcomeFailed},
			{"skipped.json", "p/repro_test.go", 0, "TestSkip", KindSkipped, OutcomeInconclusive},
			{"compile.json", "p/repro_test.go", 1, "TestPass", KindCompile, OutcomeCompile},
			// The same build failure judged for another file: it names
			// p/repro_test.go, not this one, so it is no compile verdict.
			{"compile.json", "p/other_test.go", 1, "TestPass", KindMissing, OutcomeMissing},
			// A package the reproduction's imports does not build, or a
			// module of the main code that cannot be fetched: missing. The
			// reproduction's own import that cannot be resolved names it.
			{"dependency-build-fail.json", "p/repro_test.go", 1, "TestPass", KindMissing, OutcomeMissing},
			{"setup-fail-dependency.json", "p/repro_test.go", 1, "TestPass", KindMissing, OutcomeMissing},
			{"setup-fail-own.json", "p/repro_test.go", 1, "TestPass", KindCompile, OutcomeCompile},
			{"", "p/repro_test.go", 1, "TestPass", KindMissing, OutcomeMissing},
			{"pass.json", "p/repro_test.go", 0, "TestNotInReport", KindMissing, OutcomeInconclusive},
		},
		"pytest-junitxml": {
			{"pass.xml", "tests/test_repro.py", 0, "test_pass", KindPass, OutcomePassed},
			{"fail.xml", "tests/test_repro.py", 1, "test_fail", KindFail, OutcomeFailed},
			{"error.xml", "tests/test_repro.py", 1, "test_error", KindError, OutcomeFailed},
			{"timeout.xml", "tests/test_repro.py", 1, "test_hang", KindTimeout, OutcomeFailed},
			{"skipped.xml", "tests/test_repro.py", 0, "test_skipped", KindSkipped, OutcomeInconclusive},
			{"compile.xml", "tests/test_broken.py", 2, "test_x", KindCompile, OutcomeCompile},
			{"compile.xml", "tests/test_broken.py", 1, "test_x", KindCompile, OutcomeCompile},
			// A collection error that names another file is no compile verdict.
			{"compile.xml", "tests/test_repro.py", 2, "test_pass", KindMissing, OutcomeMissing},
			// A package __init__ that does not import fails the
			// reproduction's own import, which names it.
			{"collect-package-init.xml", "tests/test_repro.py", 2, "test_pass", KindCompile, OutcomeCompile},
			// Internal and usage errors (a conftest that does not import)
			// write no report (pytest 9.1.1): missing.
			{"", "tests/test_repro.py", 3, "test_pass", KindMissing, OutcomeMissing},
			{"", "tests/test_repro.py", 4, "test_pass", KindMissing, OutcomeMissing},
			{"class.xml", "tests/test_repro.py", 1, "TestGroup::test_method_fails", KindFail, OutcomeFailed},
			{"param.xml", "tests/test_repro.py", 1, "test_param", KindFail, OutcomeFailed},
			{"", "tests/test_repro.py", 1, "test_pass", KindMissing, OutcomeMissing},
			{"", "tests/test_repro.py", 5, "test_pass", KindMissing, OutcomeMissing},
		},
		"vitest-json": {
			{"pass.json", "src/pass.test.js", 0, "add > sums two numbers", KindPass, OutcomePassed},
			{"fail.json", "src/fail.test.js", 1, "add > sums two numbers", KindFail, OutcomeFailed},
			{"error.json", "src/error.test.js", 1, "throws", KindError, OutcomeFailed},
			{"timeout.json", "src/timeout.test.js", 1, "hangs", KindTimeout, OutcomeFailed},
			{"skipped.json", "src/skipped.test.js", 0, "later", KindSkipped, OutcomeInconclusive},
			{"compile.json", "src/compile.test.js", 1, "add > sums", KindCompile, OutcomeCompile},
			{"import.json", "src/importfail.test.js", 1, "x", KindCompile, OutcomeCompile},
			{"", "src/pass.test.js", 1, "add > sums two numbers", KindMissing, OutcomeMissing},
			{"pass.json", "src/other.test.js", 0, "add > sums two numbers", KindMissing, OutcomeMissing},
		},
		"surefire-xml": {
			{"pass.xml", "src/test/java/com/example/AddTest.java", 0, "sumsTwoNumbers", KindPass, OutcomePassed},
			{"fail.xml", "src/test/java/com/example/AddTest.java", 1, "sumsTwoNumbers", KindFail, OutcomeFailed},
			{"error.xml", "src/test/java/com/example/AddTest.java", 1, "throwsOnOverflow", KindError, OutcomeFailed},
			{"timeout.xml", "src/test/java/com/example/AddTest.java", 1, "hangs", KindTimeout, OutcomeFailed},
			{"timeout-separate-thread.xml", "src/test/java/com/example/BusyTest.java", 1, "spins", KindTimeout, OutcomeFailed},
			{"", "src/test/java/com/example/NewApiTest.java", surefireCompileExit, "usesNewApi", KindCompile, OutcomeCompile},
			{"", "src/test/java/com/example/NewApiTest.java", surefireBuildExit, "usesNewApi", KindMissing, OutcomeMissing},
			{"skipped.xml", "src/test/java/com/example/AddTest.java", 0, "later", KindSkipped, OutcomeInconclusive},
			{"param.xml", "src/test/java/com/example/AddTest.java", 1, "sums", KindFail, OutcomeFailed},
			{"", "src/test/java/com/example/AddTest.java", 1, "sumsTwoNumbers", KindMissing, OutcomeMissing},
			{"", "src/test/java/com/example/AddTest.java", 0, "sumsTwoNumbers", KindMissing, OutcomeMissing},
		},
		"minitest-json": {
			{"pass.json", "test/pass_test.rb", 0, "AddTest#test_sums", KindPass, OutcomePassed},
			{"fail.json", "test/fail_test.rb", 1, "AddTest#test_sums", KindFail, OutcomeFailed},
			{"error.json", "test/error_test.rb", 1, "AddTest#test_raises", KindError, OutcomeFailed},
			{"timeout.json", "test/timeout_test.rb", 1, "AddTest#test_hangs", KindTimeout, OutcomeFailed},
			{"skipped.json", "test/skipped_test.rb", 0, "AddTest#test_later", KindSkipped, OutcomeInconclusive},
			{"compile.json", "test/compile_test.rb", 2, "AddTest#test_x", KindCompile, OutcomeCompile},
			{"load.json", "test/loadfail_test.rb", 2, "AddTest#test_x", KindCompile, OutcomeCompile},
			{"", "test/pass_test.rb", 1, "AddTest#test_sums", KindMissing, OutcomeMissing},
		},
	}
	if len(tables) != len(Runners()) {
		t.Fatalf("a table per runner: %d tables, %d runners", len(tables), len(Runners()))
	}
	for id, table := range tables {
		r := runnerByID(t, id)
		for _, c := range table {
			t.Run(id+"/"+c.fixture+"/"+c.id, func(t *testing.T) {
				kinds, outcome := r.Classify(c.path, fixture(t, id, c.fixture), c.exit, []string{c.id})
				if kinds[c.id] != c.kind || outcome != c.outcome {
					t.Errorf("Classify = %v %s, want %s %s", kinds, outcome, c.kind, c.outcome)
				}
			})
		}
	}
}

// TestRecordedSourcesYieldTheReportsIDs parses the source each report was
// recorded from: the IDs TestIDs reads are the ones Classify keys by.
func TestRecordedSourcesYieldTheReportsIDs(t *testing.T) {
	cases := []struct {
		runner, source, path string
		want                 []string
	}{
		{"go-test-json", "source.txt", "p/repro_test.go", []string{"TestPass", "TestFail", "TestPanic", "TestSkip", "TestHang"}},
		{"pytest-junitxml", "source.txt", "tests/test_repro.py", []string{"test_pass", "test_fail", "test_error", "test_skipped", "test_hang", "TestGroup::test_method_fails", "test_param"}},
		{"vitest-json", "source-pass.txt", "src/pass.test.js", []string{"add > sums two numbers"}},
		{"vitest-json", "source-error.txt", "src/error.test.js", []string{"throws"}},
		{"vitest-json", "source-skipped.txt", "src/skipped.test.js", []string{"later"}},
		{"surefire-xml", "source.txt", "src/test/java/com/example/AddTest.java", []string{"sumsTwoNumbers", "throwsOnOverflow", "hangs", "later", "sums"}},
		{"minitest-json", "source-timeout.txt", "test/timeout_test.rb", []string{"AddTest#test_hangs"}},
	}
	for _, c := range cases {
		ids, err := runnerByID(t, c.runner).TestIDs(c.path, fixture(t, c.runner, c.source))
		if err != nil || !reflect.DeepEqual(ids, c.want) {
			t.Errorf("%s %s: TestIDs = %q, %v; want %q", c.runner, c.source, ids, err, c.want)
		}
	}
}

func TestGoTestIDsAreTopLevelTestingTFunctions(t *testing.T) {
	src := `package p

import tt "testing"

func TestA(t *tt.T)          {}
func Test(t *tt.T)           {}
func Testlower(t *tt.T)      {}
func TestMain(m *tt.M)       {}
func TestB(b *tt.B)          {}
func TestC(t *tt.T) error    { return nil }
func (x) TestD(t *tt.T)      {}
func TestÉ(t *tt.T)          {}
func Test_e(t *tt.T)         {}
`
	ids, err := goTest{}.TestIDs("p/x_test.go", []byte(src))
	if err != nil || !reflect.DeepEqual(ids, []string{"TestA", "Test", "TestÉ", "Test_e"}) {
		t.Errorf("TestIDs = %q, %v", ids, err)
	}
	if _, err := (goTest{}).TestIDs("p/x_test.go", []byte("package p\nfunc TestA(")); err == nil {
		t.Error("a file that does not parse gives no IDs")
	}
}

func TestPytestIDsSkipStringsAndNestedDefinitions(t *testing.T) {
	src := "import pytest\n\n" +
		"DOC = \"\"\"\ndef test_in_docstring():\n    pass\n\"\"\"\n\n" +
		"@pytest.mark.parametrize('n', [1])\ndef test_top(n):\n    def test_inner():\n        pass\n\n" +
		"async def test_async():\n    pass\n\n" +
		"class TestThing:\n    def test_one(self):\n        pass\n\n    @pytest.mark.slow\n    def test_two(self):\n        pass\n\n" +
		"class Helper:\n    def test_not_collected(self):\n        pass\n\n" +
		"def helper():\n    pass\n"
	ids, _ := pytestJUnit{}.TestIDs("tests/test_x.py", []byte(src))
	want := []string{"test_top", "test_async", "TestThing::test_one", "TestThing::test_two"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("TestIDs = %q, want %q", ids, want)
	}
}

func TestVitestIDsFollowDescribeNesting(t *testing.T) {
	src := "import { describe, it, test, expect } from 'vitest'\n" +
		"// it('commented', () => {})\n" +
		"const s = \"test('in a string', () => {})\"\n" +
		"describe('outer', () => {\n" +
		"  describe.skip(\"inner\", () => {\n" +
		"    it('deep (with parens)', () => { expect(f(1)).toBe(2) })\n" +
		"  })\n" +
		"  test.only(`after inner`, async () => {})\n" +
		"  it.each([1, 2])('each %i', (n) => {})\n" +
		"  it(`templated ${x}`, () => {})\n" +
		"})\n" +
		"test('top', () => {})\n" +
		"obj.test('a method', () => {})\n"
	ids, _ := vitestJSON{}.TestIDs("src/a.test.ts", []byte(src))
	want := []string{"outer > inner > deep (with parens)", "outer > after inner", "top"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("TestIDs = %q, want %q", ids, want)
	}
}

func TestMinitestIDsNameTheirClass(t *testing.T) {
	src := "require \"test_helper\"\n\nmodule Shop\n  class CartTest < ActiveSupport::TestCase\n" +
		"    test \"adds an item\" do\n    end\n\n    def test_empty?\n    end\n\n    def helper\n    end\n  end\nend\n"
	ids, _ := minitestJSON{}.TestIDs("test/cart_test.rb", []byte(src))
	want := []string{"CartTest#test_adds_an_item", "CartTest#test_empty?"}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("TestIDs = %q, want %q", ids, want)
	}
	// A nested class name in the report folds to its last component.
	kinds, outcome := minitestJSON{}.Classify("test/cart_test.rb", []byte(`{"tests":[{"id":"Shop::CartTest#test_adds_an_item","result":"fail"}]}`), 1, want[:1])
	if kinds[want[0]] != KindFail || outcome != OutcomeFailed {
		t.Errorf("Classify = %v %s", kinds, outcome)
	}
}

func TestForChoosesExactlyOneRunnerByPathAndRoot(t *testing.T) {
	vitestPkg := RootFacts{"package.json": []byte(`{"devDependencies":{"vitest":"^3.2.4"}}`)}
	jestPkg := RootFacts{"package.json": []byte(`{"devDependencies":{"jest":"^29"}}`)}
	cases := []struct {
		path string
		root RootFacts
		want string
	}{
		{"internal/x/x_test.go", RootFacts{"go.mod": nil}, "go-test-json"},
		{"internal/x/x_test.go", RootFacts{}, ""},
		{"tests/test_x.py", RootFacts{"pyproject.toml": nil}, "pytest-junitxml"},
		{"pkg/x_test.py", RootFacts{"requirements.txt": nil}, "pytest-junitxml"},
		{"tests/x.py", RootFacts{"pyproject.toml": nil}, ""},
		{"src/a.test.ts", vitestPkg, "vitest-json"},
		{"src/a.spec.tsx", RootFacts{"vitest.config.ts": nil}, "vitest-json"},
		{"src/a.test.ts", jestPkg, ""},
		{"src/__tests__/a.ts", vitestPkg, ""},
		{"core/src/test/java/com/x/FooTest.java", RootFacts{"pom.xml": nil}, "surefire-xml"},
		{"src/main/java/com/x/FooTest.java", RootFacts{"pom.xml": nil}, ""},
		{"src/test/java/com/x/FooTest.java", RootFacts{"build.gradle": nil}, ""},
		{"test/models/cart_test.rb", RootFacts{"Gemfile": nil}, "minitest-json"},
		{"spec/cart_spec.rb", RootFacts{"Gemfile": nil}, ""},
		{"src/lib_test.rs", RootFacts{"Cargo.toml": nil}, ""},
	}
	for _, c := range cases {
		r, ok := For(c.path, c.root)
		got := ""
		if ok {
			got = r.ID()
		}
		if got != c.want {
			t.Errorf("For(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

// TestRunnerCommandsAreFactoryOwned checks each command selects only the
// reproduction, sets the per-test timeout, quotes the path as one word and
// ships the helper it loads.
func TestRunnerCommandsAreFactoryOwned(t *testing.T) {
	cases := []struct {
		runner, path string
		ids          []string
		contains     []string
		helper       string
	}{
		{"go-test-json", "internal/x/it's_test.go", []string{"TestA", "TestB"},
			[]string{"go test -json -count=1 -run '^(TestA|TestB)$' -timeout 150s './internal/x' > '/out/report'"}, ""},
		{"go-test-json", "x_test.go", []string{"TestA"}, []string{" '.' > "}, ""},
		{"pytest-junitxml", "tests/test_x.py", []string{"test_a"},
			[]string{"PYTHONPATH='/inputs/reproduction'", "BG_REPRO_TEST_TIMEOUT=60", "-p bg_repro_timeout", "--junitxml='/out/report' -q 'tests/test_x.py'", "-o addopts="}, pytestTimeoutPlugin},
		{"vitest-json", "src/a.test.ts", []string{"a"},
			[]string{"npx --no-install vitest run 'src/a.test.ts' --reporter=json --outputFile='/out/report' --testTimeout=60000"}, ""},
		{"surefire-xml", "core/src/test/java/com/x/FooTest.java", []string{"a"},
			[]string{`log=$(mktemp) || exit 125; mvn -B -q -f 'core/pom.xml' test-compile >"$log" 2>&1; rc=$?; cat "$log"; `,
				`if [ "$rc" -ne 0 ]; then grep -qF -- '/core/src/test/java/com/x/FooTest.java' "$log" && exit 86; exit 87; fi; mvn `, "-f 'core/pom.xml' -Dtest='FooTest'", "timeout.default=60s",
				"-Djunit.jupiter.execution.timeout.thread.mode.default=SEPARATE_THREAD", "cp 'core/target/surefire-reports/TEST-com.x.FooTest.xml' '/out/report'", "exit $rc"}, ""},
		{"minitest-json", "test/cart_test.rb", []string{"CartTest#test_a"},
			[]string{"BG_REPRO_REPORT='/out/report' BG_REPRO_TEST_TIMEOUT=60", "-I'/inputs/reproduction' -rbg_repro_reporter", "'test/cart_test.rb'"}, minitestReporter},
	}
	for _, c := range cases {
		cmd := runnerByID(t, c.runner).Command(Invocation{Path: c.path, TestIDs: c.ids, ReportPath: "/out/report", HelperDir: "/inputs/reproduction"})
		for _, want := range c.contains {
			if !strings.Contains(cmd.Script, want) {
				t.Errorf("%s: script %q lacks %q", c.runner, cmd.Script, want)
			}
		}
		if cmd.ReportPath != "/out/report" || cmd.Timeout <= 0 || cmd.Timeout > LaunchTimeout {
			t.Errorf("%s: report %q timeout %s", c.runner, cmd.ReportPath, cmd.Timeout)
		}
		if c.helper != "" && len(cmd.Helpers[c.helper]) == 0 || c.helper == "" && len(cmd.Helpers) != 0 {
			t.Errorf("%s: helpers %v, want %q", c.runner, cmd.Helpers, c.helper)
		}
	}
	quoted := goTest{}.Command(Invocation{Path: "internal/x/it's_test.go", TestIDs: []string{"TestA"}, ReportPath: "/r"}).Script
	if !strings.Contains(quoted, `'./internal/x'`) || strings.Contains(goTest{}.Command(Invocation{Path: "a'b/x_test.go", TestIDs: []string{"TestA"}, ReportPath: "/r"}).Script, ` './a'b'`) {
		t.Errorf("a quote in the path is not escaped: %s", quoted)
	}
}

// A report a commit could hold before the launch is never read: every runner
// but surefire-xml writes its report straight to Invocation.ReportPath (which
// the caller removes before the launch), and surefire-xml deletes the
// module's surefire-reports directory before Maven runs, so the report it
// copies out is one this launch wrote.
func TestRunnerReadsNoReportACommitCouldHold(t *testing.T) {
	for _, r := range Runners() {
		p := map[string]string{"go-test-json": "x/a_test.go", "pytest-junitxml": "tests/test_a.py", "vitest-json": "src/a.test.ts", "surefire-xml": "core/src/test/java/com/x/FooTest.java", "minitest-json": "test/a_test.rb"}[r.ID()]
		if p == "" {
			t.Fatalf("runner %s has no case here: add one", r.ID())
		}
		script := r.Command(Invocation{Path: p, TestIDs: []string{"a"}, ReportPath: "/out/report", HelperDir: "/inputs/reproduction"}).Script
		if r.ID() != "surefire-xml" {
			if !strings.Contains(script, "'/out/report'") || strings.Contains(script, "cp ") {
				t.Errorf("%s: script %q does not write its report to the report path", r.ID(), script)
			}
			continue
		}
		rm := strings.Index(script, "rm -rf -- 'core/target/surefire-reports' || exit 125;")
		mvn := strings.Index(script, "mvn ")
		cp := strings.Index(script, "cp 'core/target/surefire-reports/TEST-com.x.FooTest.xml'")
		if rm != 0 || mvn < rm || cp < mvn {
			t.Errorf("surefire-xml: script %q does not delete the module's reports before Maven runs and copy after", script)
		}
	}
	root := surefireXML{}.Command(Invocation{Path: "src/test/java/FooTest.java", TestIDs: []string{"a"}, ReportPath: "/out/report"}).Script
	if !strings.HasPrefix(root, "rm -rf -- 'target/surefire-reports' || exit 125;") {
		t.Errorf("surefire-xml at the root: script %q", root)
	}
}

func TestLaunchTimeoutIsCapped(t *testing.T) {
	ids := make([]string, MaxTestIDs)
	if got := launchTimeout(ids); got != LaunchTimeout {
		t.Errorf("launchTimeout(20 IDs) = %s, want %s", got, LaunchTimeout)
	}
	if got := launchTimeout([]string{"a"}); got != 2*time.Minute {
		t.Errorf("launchTimeout(1 ID) = %s", got)
	}
}

// surefire-xml's compile step: a failed `mvn test-compile` is a compile
// (surefireCompileExit) only when its output names the reproduction's own
// file; any other failure of that step (a dependency that could not be
// fetched, a main source that does not compile) exits surefireBuildExit,
// which reads as missing. The script runs here with a stand-in mvn.
func TestSurefireCompileStepNamesTheReproductionOrIsMissing(t *testing.T) {
	const repro = "core/src/test/java/com/x/FooTest.java"
	script := surefireXML{}.Command(Invocation{Path: repro, TestIDs: []string{"a"}, ReportPath: filepath.Join(t.TempDir(), "report")}).Script
	bin := t.TempDir()
	fake := "#!/bin/sh\ncase \"$*\" in *test-compile*) printf '%s\\n' \"$FAKE_MVN_OUT\"; exit \"$FAKE_MVN_RC\";; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "mvn"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		out  string
		rc   string
		want int
	}{
		"the reproduction does not compile": {"[ERROR] /workspace/" + repro + ":[3,8] cannot find symbol", "1", surefireCompileExit},
		"a main source does not compile":    {"[ERROR] /workspace/core/src/main/java/com/x/Foo.java:[3,8] cannot find symbol", "1", surefireBuildExit},
		"a dependency could not be fetched": {"[ERROR] Failed to execute goal on project core: Could not resolve dependencies", "1", surefireBuildExit},
		"another file ending in its name":   {"[ERROR] /workspace/core/src/test/java/com/x/MyFooTest.java:[3,8] cannot find symbol", "1", surefireBuildExit},
		"it compiles":                       {"", "0", 0},
	} {
		cmd := exec.Command("sh", "-c", script)
		cmd.Dir = t.TempDir()
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_MVN_OUT="+tc.out, "FAKE_MVN_RC="+tc.rc)
		out, err := cmd.CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if code != tc.want || !strings.Contains(string(out), tc.out) {
			t.Errorf("%s: exit %d, output %q; want exit %d with the compile output in the log", name, code, out, tc.want)
		}
	}
}

// namesPath finds the reproduction's path as the toolchains print it
// (relative, "./", absolute, quoted), and not as the tail of a longer name.
func TestNamesPathMatchesOnlyTheWholePath(t *testing.T) {
	for output, want := range map[string]bool{
		"p/repro_test.go:9:15: too many arguments":                      true,
		"./repro_test.go:3:1: undefined: X":                             false, // p is p/repro_test.go
		"ImportError while importing test module '/w/p/repro_test.go'.": true,
		"[ERROR] /workspace/p/repro_test.go:[3,8] cannot find symbol":   true,
		"xp/repro_test.go:1:1: no":                                      false,
		"p/repro_test.go2:1:1: no":                                      false,
		"p/repro_test.gox":                                              false,
		"q/q.go:3:25: cannot use":                                       false,
		"":                                                              false,
	} {
		if got := namesPath(output, "p/repro_test.go"); got != want {
			t.Errorf("namesPath(%q) = %v, want %v", output, got, want)
		}
	}
	if !namesPath("./repro_test.go:3:1: undefined: X", "repro_test.go") {
		t.Error("a root-level file printed as ./repro_test.go is named")
	}
}
