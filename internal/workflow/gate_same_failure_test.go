package workflow

import (
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/runner"
)

// Two failures of one gate are compared with what changes from run to run
// blanked on the failing lines themselves (durations, addresses, temporary
// paths, timestamps, UUIDs: what the build loop's round signature blanks),
// and with nothing else blanked: another test, file, message or count is
// another failure.
func TestSameGateFailureIgnoresRunToRunNoiseAndNothingElse(t *testing.T) {
	for _, tc := range []struct {
		name             string
		onResult, onBase string
		same             bool
	}{
		{"go test: durations", "--- FAIL: TestSum (0.01s)\n    sum_test.go:9: got 3, want 4\nFAIL\nFAIL\tacme/sum\t3.214s\n",
			"--- FAIL: TestSum (0.23s)\n    sum_test.go:9: got 3, want 4\nFAIL\nFAIL\tacme/sum\t0.871s\n", true},
		{"pytest: duration, address and temporary path", "FAILED tests/test_sum.py::test_sum - AssertionError: <Sum object at 0x10fa3c2d0> in /tmp/pytest-of-ci/pytest-3/out\n1 failed, 4 passed in 0.12s\n",
			"FAILED tests/test_sum.py::test_sum - AssertionError: <Sum object at 0x7f21bc00a110> in /tmp/pytest-of-ci/pytest-9/out\n1 failed, 4 passed in 1.07s\n", true},
		{"lint: timestamp, UUID and milliseconds", "2026-10-09T19:31:02Z error: unused variable 'x' (src/a.go:4:2) run 6f1c2a34-9b1d-4c7e-8a11-0c2d3e4f5a6b\nlint failed after 312 ms\n",
			"2026-10-08T07:02:55Z error: unused variable 'x' (src/a.go:4:2) run 0a9b8c7d-1e2f-4a3b-9c4d-5e6f7a8b9c0d\nlint failed after 1204 ms\n", true},
		{"another test name", "--- FAIL: TestSum (0.01s)\nFAIL\n", "--- FAIL: TestSumOfNegatives (0.01s)\nFAIL\n", false},
		{"another file name", "FAILED tests/test_sum.py::test_sum\n1 failed in 0.12s\n", "FAILED tests/test_total.py::test_sum\n1 failed in 0.12s\n", false},
		{"another assertion message", "--- FAIL: TestSum (0.01s)\n    sum_test.go:9: got 3, want 4\nFAIL\n", "--- FAIL: TestSum (0.01s)\n    sum_test.go:9: got 8, want 4\nFAIL\n", false},
		{"another number of failures", "FAILED tests/test_sum.py::test_sum\n1 failed, 4 passed in 0.12s\n", "FAILED tests/test_sum.py::test_sum\n2 failed, 3 passed in 0.12s\n", false},
		{"another line and column", "error: unused variable 'x' (src/a.go:4:2)\n", "error: unused variable 'x' (src/a.go:9:2)\n", false},
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
				t.Errorf("same failure = %v, want %v\non the result:\n%s\non the base:\n%s", got, tc.same, tc.onResult, tc.onBase)
			}
		})
	}
}
