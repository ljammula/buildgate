package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

// verifyRerunFixture is a real repository whose base commit holds a test the
// build does not touch, with the build's commit on top, whose verify launches
// go through the package's fake sandbox runtime. Each launch exits and prints
// as exits and lines say for its turn (the last entry repeats).
type verifyRerunFixture struct {
	*factoryDirFixture
	base, result string
	exits        []int
	lines        [][]string
	// workspaces is the /workspace source of every launch, in order.
	workspaces []string
	// onLaunch runs as launch n (from 1) is created.
	onLaunch func(n int, workspace string)
}

const flakyTestSource = "package lo\n\nimport \"testing\"\n\nfunc TestBufferWithTimeout(t *testing.T) {}\n"

// flakyFailure is a go test log whose one failure is TestBufferWithTimeout.
var flakyFailure = []string{"--- FAIL: TestBufferWithTimeout (0.17s)", "    channel_test.go:9: Max difference between 150ms and 174.5ms allowed 20ms", "FAIL", "FAIL\texample.com/lo\t2.1s"}

func newVerifyRerunFixture(t *testing.T, change map[string]string, exits []int, lines ...[]string) *verifyRerunFixture {
	t.Helper()
	f := &verifyRerunFixture{factoryDirFixture: newFactoryDirFixture(t, map[string]string{"channel_test.go": flakyTestSource}), exits: exits, lines: lines}
	f.base = f.commit
	if change == nil {
		change = map[string]string{"find.go": "package lo\n\nfunc Find() {}\n"}
	}
	for rel, body := range change {
		writeFile(t, filepath.Join(f.repo, filepath.FromSlash(rel)), body)
	}
	f.result = trustedCommit(t, f.repo)
	f.input.BaseSHA = f.base
	f.input.VerifyCommand = "go test ./..."
	f.rt.onCreate = func(req sandbox.SandboxRequest) {
		workspace := ""
		for _, m := range req.Mounts {
			if m.Target == "/workspace" {
				workspace = m.Source
			}
		}
		f.workspaces = append(f.workspaces, workspace)
		n := len(f.workspaces)
		f.rt.ExitCode = f.exits[min(n, len(f.exits))-1]
		f.rt.Lines = []string{"ok"}
		if len(f.lines) > 0 {
			f.rt.Lines = f.lines[min(n, len(f.lines))-1]
		}
		if f.onLaunch != nil {
			f.onLaunch(n, workspace)
		}
	}
	return f
}

// runActivity executes activity once with checkpointDir, or a fresh one when
// it is empty.
func (f *verifyRerunFixture) runActivity(activity func(context.Context, RunWorkflowInput) (VerifyActivityResult, error), checkpointDir string) VerifyActivityResult {
	f.t.Helper()
	in := f.input
	in.CheckpointDir = checkpointDir
	if in.CheckpointDir == "" {
		in.CheckpointDir = f.t.TempDir()
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activity)
	val, err := env.ExecuteActivity(activity, in)
	if err != nil {
		f.t.Fatalf("verify Activity: %v", err)
	}
	var res VerifyActivityResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res
}

func (f *verifyRerunFixture) verify(checkpointDir string) VerifyActivityResult {
	f.t.Helper()
	return f.runActivity(f.acts.RunVerifyActivity, checkpointDir)
}

// A verify that fails on one test the change does not mention is run once
// more, in the run's own worktree, and its pass is the check's result; the
// first run's exit code, log and failing test are on the record.
func TestVerifyFailingOnAnUntouchedTestPassesOnItsRerun(t *testing.T) {
	f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure, []string{"ok  \texample.com/lo\t2.0s"})
	res := f.verify("")
	if len(f.workspaces) != 2 || f.workspaces[0] != f.workspaces[1] {
		t.Fatalf("launches in %v, want two in the run's worktree", f.workspaces)
	}
	if res.Result.ExitCode != 0 {
		t.Errorf("the check's exit code = %d, want the rerun's 0", res.Result.ExitCode)
	}
	r := res.Rerun
	if r == nil || r.Outcome != run.VerifyRerunPassed || r.FirstExitCode != 1 || r.ExitCode != 0 || !reflect.DeepEqual(r.FailingTests, []string{"TestBufferWithTimeout"}) {
		t.Fatalf("rerun = %+v, want passed after TestBufferWithTimeout failed with exit 1", r)
	}
	if r.FirstLogPath == "" || r.FirstLogPath == res.Result.LogPath || !strings.HasSuffix(res.Result.LogPath, "verify.rerun.log") {
		t.Errorf("first log %q, check's log %q: want the two kept apart, the check's the rerun's", r.FirstLogPath, res.Result.LogPath)
	}
	first, _ := os.ReadFile(r.FirstLogPath)
	if !strings.Contains(string(first), "--- FAIL: TestBufferWithTimeout") || r.FirstLogSHA256 == "" || res.LogSHA256 == r.FirstLogSHA256 {
		t.Errorf("first log = %q (sha %q, check's %q), want the failure kept with its own hash", first, r.FirstLogSHA256, res.LogSHA256)
	}
	if len(res.Attempts) != 2 || res.Attempts[0].ExitCode != 1 || res.Attempts[1].ExitCode != 0 || res.Attempts[1].Kind != "verify" || res.Attempts[1].LogPath != res.Result.LogPath {
		t.Errorf("attempts = %+v, want the first run's and then the rerun's, logged to the check's log", res.Attempts)
	}
}

// A rerun that fails again is the check's result.
func TestVerifyWhoseRerunFailsAgainKeepsFailing(t *testing.T) {
	f := newVerifyRerunFixture(t, nil, []int{1, 1}, flakyFailure)
	res := f.verify("")
	if len(f.workspaces) != 2 || res.Result.ExitCode != 1 {
		t.Fatalf("launches=%d exit=%d, want two launches and the check failing", len(f.workspaces), res.Result.ExitCode)
	}
	if r := res.Rerun; r == nil || r.Outcome != run.VerifyRerunFailed || r.ExitCode != 1 || !strings.HasSuffix(res.Result.LogPath, "verify.rerun.log") {
		t.Errorf("rerun = %+v, check's log %q, want failed, the rerun's", r, res.Result.LogPath)
	}
}

// The worktree is put back to what it held before the first run: what the
// failed run left is not there for its rerun to pass on.
func TestVerifyRerunStartsFromTheWorktreeTheFirstRunHad(t *testing.T) {
	f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure, []string{"ok"})
	f.onLaunch = func(n int, workspace string) {
		left := filepath.Join(workspace, "golden.json")
		switch n {
		case 1:
			writeFile(t, left, "{}\n")
			writeFile(t, filepath.Join(workspace, "find.go"), "package lo\n// edited by the run\n")
		case 2:
			if _, err := os.Lstat(left); err == nil {
				t.Error("the first run's new file is still there for the rerun")
			}
			if body, _ := os.ReadFile(filepath.Join(workspace, "find.go")); strings.Contains(string(body), "edited by the run") {
				t.Error("the first run's edit is still there for the rerun")
			}
		}
	}
	if res := f.verify(""); res.Rerun.Outcome != run.VerifyRerunPassed {
		t.Fatalf("rerun = %+v, want passed", res.Rerun)
	}
}

// A failure is not rerun, and the check is its first run, when the failing
// test or what it tests is mentioned by the change, when the log names no
// test or a file that did not compile, or when too many tests fail.
func TestVerifyFailureIsNotRerunUnlessItNamesAFewTestsTheChangeDoesNotMention(t *testing.T) {
	cases := map[string]struct {
		change map[string]string
		lines  []string
	}{
		"the change edits the test": {
			change: map[string]string{"channel_test.go": flakyTestSource + "// tightened\n"},
			lines:  flakyFailure,
		},
		"the change adds what the test names": {
			change: map[string]string{"buffer.go": "package lo\n\nfunc BufferWithTimeout() {}\n"},
			lines:  flakyFailure,
		},
		"the change deletes a file that named the test": {
			change: map[string]string{"channel_test.go": ""},
			lines:  flakyFailure,
		},
		"no test is named": {
			lines: []string{"panic: test timed out after 10m0s", "FAIL\texample.com/lo\t600.1s"},
		},
		"a file did not compile": {
			lines: []string{"./find.go:3:1: syntax error: unexpected }", "--- FAIL: TestBufferWithTimeout (0.17s)"},
		},
		"too many tests fail": {
			lines: []string{"--- FAIL: TestA (0s)", "--- FAIL: TestB (0s)", "--- FAIL: TestC (0s)", "--- FAIL: TestD (0s)"},
		},
		"the race detector reported": {
			lines: append([]string{"==================", "WARNING: DATA RACE", "Write at 0x00c000012345 by goroutine 7:"}, flakyFailure...),
		},
		"a test panicked": {
			lines: append([]string{"panic: runtime error: index out of range [3] with length 3"}, flakyFailure...),
		},
		"the change names what the test is for in another case": {
			change: map[string]string{"buffer.go": "package lo\n\nfunc bufferWithTimeout() {}\n"},
			lines:  flakyFailure,
		},
		"a changed path names the test": {
			change: map[string]string{"testdata/TestBufferWithTimeout.golden": "x\n"},
			lines:  flakyFailure,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newVerifyRerunFixture(t, c.change, []int{1, 0}, c.lines)
			if c.change != nil && c.change["channel_test.go"] == "" {
				if err := os.Remove(filepath.Join(f.repo, "channel_test.go")); err != nil {
					t.Fatal(err)
				}
				gitIn(t, f.repo, "commit", "-qam", "delete the test")
			}
			res := f.verify("")
			if len(f.workspaces) != 1 || res.Result.ExitCode != 1 || res.Rerun != nil {
				t.Errorf("launches=%d exit=%d rerun=%+v, want the first run alone, failing", len(f.workspaces), res.Result.ExitCode, res.Rerun)
			}
		})
	}
}

// A run whose base may already hold the ticket's work (a resumed run with no
// diff base) cannot tell what its change mentions, so it is not rerun.
func TestVerifyOfARunWithNoKnownBaseIsNotRerun(t *testing.T) {
	f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure)
	f.input.OnBranch = "factoryd/earlier"
	if res := f.verify(""); len(f.workspaces) != 1 || res.Rerun != nil {
		t.Errorf("launches=%d rerun=%+v, want no rerun", len(f.workspaces), res.Rerun)
	}
}

// A retry of an Activity whose worker died mid-rerun returns the first run's
// failure, checkpointed before the rerun, and launches nothing.
func TestVerifyRetriedAfterItsWorkerDiedMidRerunKeepsTheFirstFailure(t *testing.T) {
	checkpointDir := t.TempDir()
	f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure, []string{"ok"})
	var interrupted []byte
	var checkpointPath string
	f.onLaunch = func(n int, _ string) {
		if n != 2 {
			return
		}
		all, _ := filepath.Glob(filepath.Join(checkpointDir, "activity-checkpoints", "*.json"))
		for _, m := range all {
			if !strings.Contains(filepath.Base(m), ".attempt-") {
				checkpointPath = m
			}
		}
		interrupted, _ = os.ReadFile(checkpointPath)
	}
	f.verify(checkpointDir)
	if !strings.Contains(string(interrupted), verifyRerunInterrupted) {
		t.Fatalf("the checkpoint saved before the rerun does not record it as interrupted:\n%s", interrupted)
	}
	if err := os.WriteFile(checkpointPath, interrupted, 0o600); err != nil {
		t.Fatal(err)
	}
	res := f.verify(checkpointDir)
	if len(f.workspaces) != 2 {
		t.Errorf("the retry launched %d more sandbox(es), want none", len(f.workspaces)-2)
	}
	if r := res.Rerun; res.Result.ExitCode != 1 || r == nil || r.Outcome != run.VerifyRerunNotRun || r.Reason != verifyRerunInterrupted {
		t.Errorf("exit=%d rerun=%+v, want the first failure with the rerun not run: interrupted", res.Result.ExitCode, r)
	}
}

// The rerun is bounded by what is left of the step's time limit: with less
// left than the first run took it is not launched.
func TestVerifyRerunIsNotStartedWithTooLittleOfTheTimeLimitLeft(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), verifyRerunReserve+30*time.Second)
	defer cancel()
	pending := VerifyActivityResult{Result: runner.Result{ExitCode: 1}, DurationMs: time.Minute.Milliseconds(), Rerun: &run.VerifyRerun{Outcome: run.VerifyRerunNotRun, FirstExitCode: 1}}
	launched := false
	res, err := (&Activities{}).rerunVerify(ctx, RunWorkflowInput{}, verifyRelaunch{launch: func(context.Context, func(int) string) (runner.Result, error) {
		launched = true
		return runner.Result{}, nil
	}}, verifyRerunStep{kind: "verify"}, pending, workspaceSnapshot{})
	if err != nil || launched || res.Rerun.Outcome != run.VerifyRerunNotRun || !strings.Contains(res.Rerun.Reason, "time limit") || res.Result.ExitCode != 1 {
		t.Errorf("launched=%v result=%+v rerun=%+v, want no launch and the first failure", launched, res.Result, res.Rerun)
	}
}

// The full suite is rerun by the same rule.
func TestFullSuiteFailingOnAnUntouchedTestPassesOnItsRerun(t *testing.T) {
	f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure, []string{"ok"})
	f.input.FullSuiteCommand = "go test ./..."
	res := f.runActivity(f.acts.RunFullSuiteVerifyActivity, "")
	if len(f.workspaces) != 2 || res.Result.ExitCode != 0 || !res.Rerun.PassedOnRerun() || !strings.HasSuffix(res.Result.LogPath, "full_suite_verify.rerun.log") {
		t.Errorf("launches=%d exit=%d rerun=%+v log=%q, want the full suite passing on its rerun", len(f.workspaces), res.Result.ExitCode, res.Rerun, res.Result.LogPath)
	}
}

// A baseline that would halt on one test the ticket does not name is run once
// more; when the rerun passes, the build goes on and the record says why.
func TestBaselineFailingOnAnUnnamedTestThatPassesOnItsRerunDoesNotHalt(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	logs := []string{"--- FAIL: TestWaitFor (0.07s)\nFAIL\n", "ok  \texample.com/lo\t1.0s\n"}
	exits := []int{1, 0}
	logDir := t.TempDir()
	launches := 0
	activities := &Activities{
		LogDir:  logDir,
		DataDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, logPath func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			launches++
			path := logPath(1)
			if err := os.WriteFile(path, []byte(logs[launches-1]), 0o644); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			result := runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: exits[launches-1], StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: path}
			onAttempt(1, result, nil)
			return result, nil
		},
	}
	input.VerifyCommand = "make verify"
	input.SpecPath = filepath.Join(t.TempDir(), "ticket.md")
	writeFile(t, input.SpecPath, "## Goal\nclamp a value\n")
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBaselineVerifyActivity)
	raw, err := env.ExecuteActivity(activities.RunBaselineVerifyActivity, input)
	if err != nil {
		t.Fatalf("a baseline that passed on its rerun halted: %v", err)
	}
	var result BaselineVerifyResult
	if err := raw.Get(&result); err != nil {
		t.Fatal(err)
	}
	if launches != 2 || !result.Record.Passed || !result.Record.Rerun.PassedOnRerun() || !reflect.DeepEqual(result.Record.Rerun.FailingTests, []string{"TestWaitFor"}) {
		t.Errorf("launches=%d record=%+v rerun=%+v, want passed on the rerun after TestWaitFor failed", launches, result.Record, result.Record.Rerun)
	}
	if record := loadBaselineRecord(t, logDir); !record.Rerun.PassedOnRerun() {
		t.Errorf("saved record's rerun = %+v, want passed", record.Rerun)
	}
}

// A baseline failure the ticket names is the ticket's work: it is not rerun.
func TestBaselineFailureTheTicketNamesIsNotRerun(t *testing.T) {
	input := fixtureInput()
	input.WorkspacePath = baselineRepo(t)
	result, err, _, launches := runBaselineActivity(t, input, "## Goal\nmake TestWaitFor pass\n", "--- FAIL: TestWaitFor (0.07s)\nFAIL\n", 1, nil)
	if err != nil || launches != 1 || result.Record.Rerun != nil || !result.Record.Expected {
		t.Errorf("err=%v launches=%d record=%+v, want one launch, expected, no rerun", err, launches, result.Record)
	}
}

func TestTestWordsNameWhatAChangeWouldMention(t *testing.T) {
	cases := []struct {
		test  string
		words []string
		file  string
	}{
		{"TestBufferWithTimeout", []string{"TestBufferWithTimeout", "BufferWithTimeout"}, ""},
		{"ExampleNewThrottleBy", []string{"ExampleNewThrottleBy", "NewThrottleBy"}, ""},
		{"ExampleClamp_float", []string{"ExampleClamp_float", "Clamp"}, ""},
		{"TestIt", []string{"TestIt"}, ""},
		{"tests/test_sorted.py::TestKeys::test_count_range", []string{"TestKeys", "test_count_range", "count_range"}, "tests/test_sorted.py"},
		{"tests/test_a.py::test_x", []string{"test_x"}, "tests/test_a.py"},
	}
	for _, c := range cases {
		words, file := testWords(c.test)
		if !reflect.DeepEqual(words, c.words) || file != c.file {
			t.Errorf("testWords(%q) = %q, %q; want %q, %q", c.test, words, file, c.words, c.file)
		}
	}
}

func TestContainsIdentifierMatchesWholeIdentifiersOnly(t *testing.T) {
	for _, c := range []struct {
		text, word string
		want       bool
	}{
		{"func BufferWithTimeout() {}", "BufferWithTimeout", true},
		{"BufferWithTimeout", "BufferWithTimeout", true},
		{"func BufferWithTimeoutCtx() {}", "BufferWithTimeout", false},
		{"lo.XBufferWithTimeout", "BufferWithTimeout", false},
		{"x := BufferWithTimeout2; y := BufferWithTimeout(1)", "BufferWithTimeout", true},
		{"count_range_key", "count_range", false},
	} {
		if got := containsIdentifier(c.text, c.word); got != c.want {
			t.Errorf("containsIdentifier(%q, %q) = %v, want %v", c.text, c.word, got, c.want)
		}
	}
}

// A README that already lists the failing test's subject does not rule out
// its rerun when the change edits other lines of it; a line the change adds
// or removes that names it does, and a code file is still searched whole.
func TestChangeMentionsSearchesOnlyTheChangedLinesOfADocumentationFile(t *testing.T) {
	base := map[string]string{
		"README.md":       "# lo\n\n- BufferWithTimeout\n-- Synchronize\n",
		"docs/llms.txt":   "BufferWithTimeout\n",
		"channel.go":      "package lo\n\nfunc BufferWithTimeout() {}\n\nfunc Other() {}\n",
		"testdata/in.txt": "BufferWithTimeout\n",
		"notes.md":        "BufferWithTimeout\n",
		".gitattributes":  "notes.md -diff\n",
		"channel_test.go": flakyTestSource,
	}
	readme := "# lo\n\n- BufferWithTimeout\n-- Synchronize\n- Clamp\n"
	for name, c := range map[string]struct {
		change      map[string]string // a path to a body; "" deletes the file
		uncommitted bool
		want        string
	}{
		"another line of the README":   {change: map[string]string{"README.md": readme, "docs/llms.txt": "BufferWithTimeout\nClamp\n"}},
		"a README line naming it":      {change: map[string]string{"README.md": "# lo\n\n- BufferWithTimeout (now flaky)\n-- Synchronize\n"}, want: "README.md mentions BufferWithTimeout"},
		"a removed README line":        {change: map[string]string{"README.md": "# lo\n\n- BufferWithTimeout\n"}, want: "README.md mentions Synchronize"},
		"a new README naming it":       {change: map[string]string{"docs/new.md": "Synchronize\n"}, want: "docs/new.md mentions Synchronize"},
		"a deleted README":             {change: map[string]string{"README.md": ""}, want: "README.md mentions BufferWithTimeout"},
		"another function of its file": {change: map[string]string{"channel.go": "package lo\n\nfunc BufferWithTimeout() {}\n\nfunc Other() { _ = 1 }\n"}, want: "channel.go mentions BufferWithTimeout"},
		"a test input under testdata":  {change: map[string]string{"testdata/in.txt": "BufferWithTimeout\nmore\n"}, want: "testdata/in.txt mentions BufferWithTimeout"},
		"a doc git shows no hunk for":  {change: map[string]string{"notes.md": "BufferWithTimeout\nmore\n"}, want: "notes.md mentions BufferWithTimeout"},
		"an uncommitted README edit":   {change: map[string]string{"README.md": readme}, uncommitted: true},
		"an untracked doc naming it":   {change: map[string]string{"docs/new.md": "Synchronize\n"}, uncommitted: true, want: "docs/new.md mentions Synchronize"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFactoryDirFixture(t, base)
			for rel, body := range c.change {
				path := filepath.Join(f.repo, filepath.FromSlash(rel))
				if body == "" {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					continue
				}
				writeFile(t, path, body)
			}
			if !c.uncommitted {
				trustedCommit(t, f.repo)
			}
			if why := changeMentions(context.Background(), f.repo, f.commit, []string{"TestBufferWithTimeout", "TestSynchronize"}); why != c.want {
				t.Errorf("changeMentions = %q, want %q", why, c.want)
			}
		})
	}
}

func TestIsDocumentationPath(t *testing.T) {
	for p, want := range map[string]bool{
		"README.md": true, "docs/data/core-clamp.md": true, "doc/source/api.rst": true, "docs/static/llms.txt": true, "CHANGES.adoc": true,
		"channel.go": false, "tests/expected.txt": false, "pkg/testdata/golden.md": false, "fixtures/a.rst": false, "Makefile": false,
		"requirements-dev.txt": false, "constraints.txt": false, "CMakeLists.txt": false, "examples/basic.md": false, "ui/__snapshots__/a.md": false,
	} {
		if got := isDocumentationPath(p); got != want {
			t.Errorf("isDocumentationPath(%q) = %v, want %v", p, got, want)
		}
	}
}

// A changed pytest file is the failing test's own file: no rerun.
func TestChangeMentionsAPytestFileItChanges(t *testing.T) {
	f := newVerifyRerunFixture(t, map[string]string{"tests/test_a.py": "def test_x():\n    pass\n"}, []int{1})
	if why := changeMentions(context.Background(), f.repo, f.base, []string{"tests/test_a.py::test_other"}); !strings.Contains(why, "tests/test_a.py") {
		t.Errorf("changeMentions = %q, want the changed test file named", why)
	}
	if why := changeMentions(context.Background(), f.repo, f.base, []string{"tests/test_b.py::test_y"}); why != "" {
		t.Errorf("changeMentions = %q, want nothing for a test the change does not reach", why)
	}
}

// A file the change left uncommitted or untracked is part of the change: one
// that mentions the failing test rules the rerun out.
func TestVerifyIsNotRerunWhenAnUncommittedFileMentionsTheTest(t *testing.T) {
	for name, write := range map[string]func(f *verifyRerunFixture){
		"untracked": func(f *verifyRerunFixture) {
			writeFile(f.t, filepath.Join(f.repo, "notes_test.go"), "package lo\n// TestBufferWithTimeout is slow\n")
		},
		"uncommitted": func(f *verifyRerunFixture) {
			writeFile(f.t, filepath.Join(f.repo, "find.go"), "package lo\n// see TestBufferWithTimeout\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure)
			write(f)
			if res := f.verify(""); len(f.workspaces) != 1 || res.Rerun != nil {
				t.Errorf("launches=%d rerun=%+v, want no rerun", len(f.workspaces), res.Rerun)
			}
		})
	}
}

// What the first run left in ignored files is removed before the rerun:
// state a failing run wrote cannot make its rerun pass.
func TestVerifyRerunRemovesTheIgnoredFilesTheFirstRunLeft(t *testing.T) {
	f := newVerifyRerunFixture(t, map[string]string{".gitignore": "cache/\n*.marker\n", "find.go": "package lo\n"}, []int{1, 0}, flakyFailure, []string{"ok"})
	writeFile(t, filepath.Join(f.repo, "cache", "kept.bin"), "from the build\n")
	f.onLaunch = func(n int, workspace string) {
		switch n {
		case 1:
			writeFile(t, filepath.Join(workspace, ".flaky.marker"), "")
			writeFile(t, filepath.Join(workspace, "cache", "new.bin"), "from the failed run\n")
		case 2:
			for _, left := range []string{".flaky.marker", "cache/new.bin"} {
				if _, err := os.Lstat(filepath.Join(workspace, left)); err == nil {
					t.Errorf("%s, which the failed run wrote, is still there for the rerun", left)
				}
			}
			if _, err := os.Lstat(filepath.Join(workspace, "cache", "kept.bin")); err != nil {
				t.Errorf("an ignored file from before the first run is gone: %v", err)
			}
		}
	}
	if res := f.verify(""); !res.Rerun.PassedOnRerun() {
		t.Fatalf("rerun = %+v, want passed", res.Rerun)
	}
}

// A file the build left uncommitted whose content the first run changed
// cannot be put back: the failure is not rerun.
func TestVerifyIsNotRerunWhenTheFirstRunChangedAnUncommittedFile(t *testing.T) {
	f := newVerifyRerunFixture(t, nil, []int{1, 0}, flakyFailure, []string{"ok"})
	writeFile(t, filepath.Join(f.repo, "snapshot.txt"), "before\n")
	f.onLaunch = func(n int, workspace string) {
		if n == 1 {
			writeFile(t, filepath.Join(workspace, "snapshot.txt"), "updated by the failed run\n")
		}
	}
	res := f.verify("")
	if r := res.Rerun; len(f.workspaces) != 1 || r == nil || r.Outcome != run.VerifyRerunNotRun || !strings.Contains(r.Reason, "snapshot.txt") || res.Result.ExitCode != 1 {
		t.Errorf("launches=%d exit=%d rerun=%+v, want no launch, not_run naming snapshot.txt", len(f.workspaces), res.Result.ExitCode, r)
	}
}

// A rerun whose launch fails fails the step as a first run's would, with the
// launch error's type, and the checkpoint says so.
func TestVerifyRerunLaunchErrorFailsTheStep(t *testing.T) {
	repo := baselineRepo(t)
	writeFile(t, filepath.Join(repo, "flaky_test.go"), flakyTestSource)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "flaky")
	head := gitIn(t, repo, "rev-parse", "HEAD")
	calls := 0
	logPath := filepath.Join(t.TempDir(), "verify.log")
	writeFile(t, logPath, strings.Join(flakyFailure, "\n")+"\n")
	acts := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			if calls == 2 {
				return runner.Result{ExitCode: -1}, sandbox.ErrCleanupUnconfirmed
			}
			started := time.Now()
			res := runner.Result{ExitCode: 1, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: logPath}
			onAttempt(1, res, nil)
			return res, nil
		},
	}
	input := fixtureInput()
	input.WorkspacePath, input.BaseSHA, input.CheckpointDir = repo, head, t.TempDir()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(acts.RunVerifyActivity)
	_, err := env.ExecuteActivity(acts.RunVerifyActivity, input)
	var appErr *temporal.ApplicationError
	if calls != 2 || !errors.As(err, &appErr) || appErr.Type() != CleanupUnconfirmedFailureType {
		t.Fatalf("calls=%d err=%v, want the rerun's unconfirmed removal failing the step as %s", calls, err, CleanupUnconfirmedFailureType)
	}
	// A retry returns the same failure and launches nothing.
	retry := suite.NewTestActivityEnvironment()
	retry.RegisterActivity(acts.RunVerifyActivity)
	_, err = retry.ExecuteActivity(acts.RunVerifyActivity, input)
	if calls != 2 || !errors.As(err, &appErr) || appErr.Type() != CleanupUnconfirmedFailureType {
		t.Errorf("retry: calls=%d err=%v, want the checkpointed failure and no launch", calls, err)
	}
}
