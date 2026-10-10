package workflow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/runner"
)

// failingTests is n go test failures, numbered from 1.
func failingTests(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "--- FAIL: TestCase%02d (0.0%ds)\n    case_test.go:%d: got 1, want 2\n", i, i%10, 10+i)
	}
	b.WriteString("FAIL\nFAIL\tacme/cases\t0.412s\n")
	return b.String()
}

// Two failures of one gate are the same only when the whole output of the two
// runs is the same line for line once run-to-run noise is removed: the elapsed
// time of a test line, a memory address, a timestamp, a UUID and the random
// part of a temporary directory. Everything else is compared as written: a
// test name, a value, a setting, a file name, the number of failures, and any
// line past the first screenful. Output that is empty, unreadable or too large
// to compare is never the same failure.
func TestSameGateFailureIgnoresRunToRunNoiseAndNothingElse(t *testing.T) {
	long := strings.Repeat("error: generated/schema.go: field mismatch in a very long list of fields\n", 40)
	for _, tc := range []struct {
		name             string
		onResult, onBase string
		same             bool
	}{
		{"go test: durations", "--- FAIL: TestSum (0.01s)\n    sum_test.go:9: got 3, want 4\nFAIL\nFAIL\tacme/sum\t3.214s\n",
			"--- FAIL: TestSum (0.23s)\n    sum_test.go:9: got 3, want 4\nFAIL\nFAIL\tacme/sum\t0.871s\n", true},
		{"go test: a passing package's time and terminal colour", "ok  \tacme/api\t0.31s\n\x1b[31m--- FAIL: TestSum (0.01s)\x1b[0m   \nFAIL\n",
			"ok  \tacme/api\t0.52s\n--- FAIL: TestSum (0.02s)\nFAIL\n", true},
		{"pytest: duration, address and temporary directory", "FAILED tests/test_sum.py::test_sum - AssertionError: <Sum object at 0x10fa3c2d0> in /tmp/pytest-of-ci/pytest-3/out\n==== 1 failed, 4 passed in 0.12s ====\n",
			"FAILED tests/test_sum.py::test_sum - AssertionError: <Sum object at 0x7f21bc00a110> in /tmp/pytest-of-ci/pytest-9/out\n==== 1 failed, 4 passed in 1.07s ====\n", true},
		{"lint: timestamp, UUID and milliseconds", "2026-10-09T19:31:02Z error: unused variable 'x' (src/a.go:4:2) run 6f1c2a34-9b1d-4c7e-8a11-0c2d3e4f5a6b\nlint failed after 312 ms\n",
			"2026-10-08T07:02:55Z error: unused variable 'x' (src/a.go:4:2) run 0a9b8c7d-1e2f-4a3b-9c4d-5e6f7a8b9c0d\nlint failed after 1204 ms\n", true},
		{"the random part of a temporary directory", "/tmp/go-build123456/b001/sum.go:3: undefined: x\nopen /var/folders/ab/cd_ef12/T/TestSum4455/001/out.txt: no such file\n",
			"/tmp/go-build998877/b001/sum.go:3: undefined: x\nopen /var/folders/zz/q9_0000/T/TestSum17/001/out.txt: no such file\n", true},

		{"another test name", "--- FAIL: TestSum (0.01s)\nFAIL\n", "--- FAIL: TestSumOfNegatives (0.01s)\nFAIL\n", false},
		{"another file name", "FAILED tests/test_sum.py::test_sum\n1 failed in 0.12s\n", "FAILED tests/test_total.py::test_sum\n1 failed in 0.12s\n", false},
		{"another assertion message", "--- FAIL: TestSum (0.01s)\n    sum_test.go:9: got 3, want 4\nFAIL\n", "--- FAIL: TestSum (0.01s)\n    sum_test.go:9: got 8, want 4\nFAIL\n", false},
		{"another number of failures, in the summary line alone", "FAILED tests/test_sum.py::test_sum\n1 failed, 4 passed in 0.12s\n", "FAILED tests/test_sum.py::test_sum\n2 failed, 3 passed in 0.12s\n", false},
		{"another line and column", "error: unused variable 'x' (src/a.go:4:2)\n", "error: unused variable 'x' (src/a.go:9:2)\n", false},
		{"the base has 14 failing tests and the build fixed the last two", failingTests(12), failingTests(14), false},
		{"the base has 12 failing tests and the build added one in another package", failingTests(12) + "--- FAIL: TestOther (0.01s)\nFAIL\tacme/other\t0.100s\n", failingTests(12), false},
		{"two errors that share their first 1200 bytes", long + "error: a.go: missing return\n", long + "error: b.go: unused import\n", false},
		{"a duration that is a subtest's name", "--- FAIL: TestParseDuration/5s (0.00s)\nFAIL\n", "--- FAIL: TestParseDuration/10m (0.00s)\nFAIL\n", false},
		{"a hex value that is not an address", "    x_test.go:4: got 0x1f\nFAIL\n", "    x_test.go:4: got 0x3\nFAIL\n", false},
		{"a 32-bit hex value", "    x_test.go:4: got 0xdeadbeef\nFAIL\n", "    x_test.go:4: got 0xcafebabe\nFAIL\n", false},
		{"a duration that is a setting", "error: timeout = 10s\n", "error: timeout = 30s\n", false},
		{"a whole-second wait", "error: no reply in 5s\n", "error: no reply in 10s\n", false},
		{"two files under one temporary directory", "/tmp/build/a.go:3: error: undefined x\n", "/tmp/build/b.go:3: error: undefined x\n", false},
		{"two files directly under the temporary root", "error: cannot read /tmp/out123\n", "error: cannot read /tmp/out124\n", false},
		{"a duration beside a letter that is not ASCII", "error: délaié5ms\n", "error: délaié7ms\n", false},
		{"an address beside a letter that is not ASCII", "error: é0x10fa3c2d0\n", "error: é0x7f21bc00a110\n", false},
		{"no output on either side", "", "", false},
		{"only blank lines on either side", "\n\n", "\n\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) runner.Result {
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
				return runner.Result{ExitCode: 1, LogPath: path}
			}
			if got := sameGateFailure(write("result.log", tc.onResult), write("base.log", tc.onBase)); got != tc.same {
				t.Errorf("same failure = %v, want %v\non the result:\n%.600s\non the base:\n%.600s", got, tc.same, tc.onResult, tc.onBase)
			}
		})
	}
	t.Run("two logs that cannot be read", func(t *testing.T) {
		missing := runner.Result{ExitCode: 1, LogPath: filepath.Join(t.TempDir(), "gone.log")}
		if sameGateFailure(missing, missing) {
			t.Error("two unreadable logs compared as the same failure")
		}
	})
	t.Run("two logs too large to compare whole", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "huge.log")
		if err := os.WriteFile(path, []byte(strings.Repeat("FAIL: the same line every time\n", 300_000)), 0o600); err != nil {
			t.Fatal(err)
		}
		huge := runner.Result{ExitCode: 1, LogPath: path}
		if sameGateFailure(huge, huge) {
			t.Error("two logs past the comparison's size limit compared as the same failure")
		}
	})
}
