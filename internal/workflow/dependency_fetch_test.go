package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
	"buildgate/internal/unresolved"
)

// fetchLaunch is one launch of a fetchFixture's command: its exit code and
// how many requests the registry proxy's upstream fails while it runs.
type fetchLaunch struct {
	exit     int
	failures int
	// oom makes Docker report the launch's worker OOM-killed; disk, when
	// set, is Docker's free bytes before the launch, after its exit and
	// after its delete.
	oom  bool
	disk *[3]uint64
}

// fetchFixture runs the real verify, baseline, full-suite and gate
// Activities through the fake sandbox runtime with a registry proxy whose
// count of failed upstream requests the test scripts, launch by launch.
type fetchFixture struct {
	t      *testing.T
	rt     *inspectRuntime
	acts   *Activities
	input  RunWorkflowInput
	script []fetchLaunch
	// launches is how many sandboxes were created; proxies how many registry
	// proxies were started.
	launches, proxies int
	// total is the running proxy's count, as its log would show it.
	total int
	// unreadable makes the proxy's count unknown from that read on (reads
	// are numbered from 1; 0 never).
	unreadable, reads int
}

func newFetchFixture(t *testing.T, script ...fetchLaunch) *fetchFixture {
	t.Helper()
	rt := &inspectRuntime{WorkerRuntime: &sandboxtest.WorkerRuntime{Lines: []string{"could not fetch"}}}
	acts, input, _ := runtimeActivities(t, rt.WorkerRuntime)
	acts.Sandboxes = rt
	f := &fetchFixture{t: t, rt: rt, acts: acts, script: script}
	rt.DiskFree = scriptedDisk(script)
	input.RunID = "fetch-run"
	input.BaseSHA = ""
	input.SandboxUser = fmt.Sprintf("65532:%d", os.Getgid())
	input.RegistryProxyPolicy = testRegistryProxyPolicy()
	input.VerifyCommand, input.FullSuiteCommand = "make verify", "make test"
	input.CheckpointDir = t.TempDir()
	f.input = input
	rt.onCreate = func(sandbox.SandboxRequest) {
		if f.launches >= len(f.script) {
			t.Errorf("launch %d: the test scripted only %d", f.launches+1, len(f.script))
			return
		}
		rt.ExitCode = f.script[f.launches].exit
		rt.OOMKilled = f.script[f.launches].oom
		f.total += f.script[f.launches].failures
		f.launches++
	}
	acts.registryProxyHooks = sandbox.RegistryProxyHooks{
		StartRegistryProxy: func(context.Context, string, sandbox.RegistryProxySpec) (*sandbox.RegistryProxyHandle, error) {
			f.proxies++
			// Each proxy is a new container: its count starts at nothing.
			f.total = 0
			return &sandbox.RegistryProxyHandle{
				WorkerBaseURL: "http://registry-proxy:8092",
				AuditFacts:    sandbox.RegistryProxyLaunchFacts{NetworkName: "factoryd-registryproxy-test", ContainerName: "proxy", WorkerBaseURL: "http://registry-proxy:8092"},
			}, nil
		},
		CleanupRegistryProxy: func(context.Context, *sandbox.RegistryProxyHandle) error { return nil },
		Address:              func(context.Context, string, string, string) (string, error) { return "172.28.0.2", nil },
		UpstreamFailures: func(context.Context, string, string) (int, bool) {
			f.reads++
			if f.unreadable != 0 && f.reads >= f.unreadable {
				return 0, false
			}
			return f.total, true
		},
	}
	return f
}

// scriptedDisk is the free bytes Docker reports across the whole script,
// three readings a launch; nil when no launch scripts any (the disk then
// cannot be read). A launch that scripts none reads as roomy.
func scriptedDisk(script []fetchLaunch) []uint64 {
	var readings []uint64
	scripted := false
	for _, l := range script {
		disk := [3]uint64{100 << 30, 100 << 30, 100 << 30}
		if l.disk != nil {
			disk, scripted = *l.disk, true
		}
		readings = append(readings, disk[:]...)
	}
	if !scripted {
		return nil
	}
	return readings
}

// fetchSteps are the Activities that rerun a command after a fetch failure.
var fetchSteps = []struct {
	name string
	run  func(f *fetchFixture) (VerifyActivityResult, error)
}{
	{"canonical verify", func(f *fetchFixture) (VerifyActivityResult, error) {
		return executeVerifyStep(f.t, f.acts.RunVerifyActivity, f.input)
	}},
	{"full suite", func(f *fetchFixture) (VerifyActivityResult, error) {
		return executeVerifyStep(f.t, f.acts.RunFullSuiteVerifyActivity, f.input)
	}},
	{"named gate", func(f *fetchFixture) (VerifyActivityResult, error) {
		return executeVerifyStep(f.t, f.acts.RunNamedGateActivity, NamedGateActivityInput{RunWorkflowInput: f.input, Check: "lint", Command: "make lint"})
	}},
}

// executeVerifyStep runs one Activity and returns its result; on an error
// the result holds only the attempts the error carries.
func executeVerifyStep(t *testing.T, step, input any) (VerifyActivityResult, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(step)
	val, err := env.ExecuteActivity(step, input)
	if err != nil {
		return VerifyActivityResult{Attempts: AttemptsFromError(err)}, err
	}
	var res VerifyActivityResult
	if err := val.Get(&res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func exitCodes(attempts []run.Attempt) []int {
	codes := []int{}
	for _, a := range attempts {
		codes = append(codes, a.ExitCode)
	}
	return codes
}

func TestACommandThatFailedWhileARegistryWasUnreachableIsRunOnceMore(t *testing.T) {
	for _, step := range fetchSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 1, failures: 2}, fetchLaunch{exit: 0})
			res, err := step.run(f)
			if err != nil {
				t.Fatalf("the step failed although its second run passed: %v", err)
			}
			if res.Result.ExitCode != 0 {
				t.Errorf("exit code = %d, want the second run's 0", res.Result.ExitCode)
			}
			if f.launches != 2 {
				t.Errorf("launches = %d, want 2", f.launches)
			}
			// Both runs are evidence, each with its own log.
			if got := exitCodes(res.Attempts); !slices.Equal(got, []int{1, 0}) {
				t.Fatalf("attempt exit codes = %v, want [1 0]", got)
			}
			if res.Attempts[0].LogPath == res.Attempts[1].LogPath || res.Result.LogPath != res.Attempts[1].LogPath {
				t.Errorf("logs: attempts %q and %q, result %q; want one per run and the result's the second", res.Attempts[0].LogPath, res.Attempts[1].LogPath, res.Result.LogPath)
			}
			for _, a := range res.Attempts {
				if _, err := os.Stat(a.LogPath); err != nil {
					t.Errorf("attempt log: %v", err)
				}
			}
		})
	}
}

func TestACommandThatFailsTwiceWhileARegistryIsUnreachableIsUnresolved(t *testing.T) {
	for _, step := range fetchSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 1, failures: 1}, fetchLaunch{exit: 2, failures: 3})
			res, err := step.run(f)
			if !hasApplicationErrorType(err, DependencyFetchFailureType) {
				t.Fatalf("err = %v, want type %q", err, DependencyFetchFailureType)
			}
			if got := UnresolvedCauseFromError(err); got != unresolved.DependencyFetch {
				t.Errorf("cause = %q, want %q", got, unresolved.DependencyFetch)
			}
			if got := HaltReasonCodeFromError(err); got != string(unresolved.DependencyFetch) {
				t.Errorf("halt reason code = %q, want %q", got, unresolved.DependencyFetch)
			}
			if f.launches != 2 {
				t.Errorf("launches = %d, want 2: the command is run again once, never more", f.launches)
			}
			if got := exitCodes(res.Attempts); !slices.Equal(got, []int{1, 2}) {
				t.Errorf("attempt exit codes on the error = %v, want [1 2]", got)
			}
			// A redispatch replays the checkpoint: same type, no new launch.
			_, again := step.run(f)
			if !hasApplicationErrorType(again, DependencyFetchFailureType) || UnresolvedCauseFromError(again) != unresolved.DependencyFetch {
				t.Errorf("replayed checkpoint: err = %v, want type %q", again, DependencyFetchFailureType)
			}
			if f.launches != 2 {
				t.Errorf("launches after the replay = %d, want still 2", f.launches)
			}
		})
	}
}

// Everything else is judged as it was before the count existed.
func TestAFailureIsOrdinaryUnlessTheProxyCountedAFetchFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		script     []fetchLaunch
		unreadable int
		noProxy    bool
		wantExit   int
	}{
		{name: "no registry failed: the command is not run again", script: []fetchLaunch{{exit: 1}}, wantExit: 1},
		{name: "the second run fails with every registry reachable", script: []fetchLaunch{{exit: 1, failures: 1}, {exit: 3}}, wantExit: 3},
		{name: "the count cannot be read before the launch", script: []fetchLaunch{{exit: 1, failures: 1}}, unreadable: 1, wantExit: 1},
		{name: "the count cannot be read after the launch", script: []fetchLaunch{{exit: 1, failures: 1}}, unreadable: 2, wantExit: 1},
		{name: "the second run's count cannot be read", script: []fetchLaunch{{exit: 1, failures: 1}, {exit: 3, failures: 1}}, unreadable: 4, wantExit: 3},
		{name: "the run has no registry proxy", script: []fetchLaunch{{exit: 1, failures: 1}}, noProxy: true, wantExit: 1},
		{name: "a command that passes, whatever the proxy counted", script: []fetchLaunch{{exit: 0, failures: 5}}, wantExit: 0},
	} {
		for _, step := range fetchSteps {
			t.Run(tc.name+"/"+step.name, func(t *testing.T) {
				f := newFetchFixture(t, tc.script...)
				f.unreadable = tc.unreadable
				if tc.noProxy {
					f.input.RegistryProxyPolicy = nil
				}
				res, err := step.run(f)
				if err != nil {
					t.Fatalf("err = %v, want the command's own result", err)
				}
				if res.Result.ExitCode != tc.wantExit {
					t.Errorf("exit code = %d, want %d", res.Result.ExitCode, tc.wantExit)
				}
				if f.launches != len(tc.script) {
					t.Errorf("launches = %d, want %d", f.launches, len(tc.script))
				}
				if tc.noProxy && (f.proxies != 0 || f.reads != 0) {
					t.Errorf("proxies started = %d, count reads = %d, want none", f.proxies, f.reads)
				}
			})
		}
	}
}

// executeBaseline runs the baseline verify on f's repository at its commit.
func executeBaseline(f *fetchFixture) (BaselineVerifyResult, error) {
	f.t.Helper()
	in := f.input
	in.BaseSHA = in.ProjectConfigCommitSHA
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(f.acts.RunBaselineVerifyActivity)
	val, err := env.ExecuteActivity(f.acts.RunBaselineVerifyActivity, in)
	if err != nil {
		return BaselineVerifyResult{Attempts: AttemptsFromError(err)}, err
	}
	var res BaselineVerifyResult
	if err := val.Get(&res); err != nil {
		f.t.Fatal(err)
	}
	return res, nil
}

// A baseline that fails with a registry unreachable says nothing about the
// repository: it is not "the verify command fails on the untouched
// repository".
func TestABaselineThatFailsTwiceWhileARegistryIsUnreachableIsUnresolvedNotAFailedBaseline(t *testing.T) {
	f := newFetchFixture(t, fetchLaunch{exit: 1, failures: 1}, fetchLaunch{exit: 1, failures: 1})
	res, err := executeBaseline(f)
	if !hasApplicationErrorType(err, DependencyFetchFailureType) || hasApplicationErrorType(err, BaselineVerifyFailureType) {
		t.Fatalf("err = %v, want type %q and not %q", err, DependencyFetchFailureType, BaselineVerifyFailureType)
	}
	if got := HaltReasonCodeFromError(err); got != string(unresolved.DependencyFetch) {
		t.Errorf("halt reason code = %q, want %q, not %q", got, unresolved.DependencyFetch, run.HaltReasonBaselineVerifyFailed)
	}
	if f.launches != 2 || len(res.Attempts) != 2 {
		t.Errorf("launches = %d, attempts on the error = %d, want 2 and 2", f.launches, len(res.Attempts))
	}
	if record, loadErr := run.LoadBaselineVerify(f.acts.logDirFor(f.input)); loadErr != nil || record != nil {
		t.Errorf("baseline record = %+v, %v; an unresolved baseline records no result", record, loadErr)
	}
}

func TestABaselineWhoseSecondRunPassesIsAPassingBaseline(t *testing.T) {
	f := newFetchFixture(t, fetchLaunch{exit: 1, failures: 1}, fetchLaunch{exit: 0})
	res, err := executeBaseline(f)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if !res.Record.Passed || res.Record.Halts() || f.launches != 2 {
		t.Errorf("record = %+v, launches = %d; want a passing baseline after 2 launches", res.Record, f.launches)
	}
}

func TestABaselineThatFailsWithEveryRegistryReachableStillHaltsAsAFailedBaseline(t *testing.T) {
	f := newFetchFixture(t, fetchLaunch{exit: 1})
	_, err := executeBaseline(f)
	if !hasApplicationErrorType(err, BaselineVerifyFailureType) || UnresolvedCauseFromError(err) != "" {
		t.Fatalf("err = %v (cause %q), want type %q", err, UnresolvedCauseFromError(err), BaselineVerifyFailureType)
	}
	if f.launches != 1 {
		t.Errorf("launches = %d, want 1", f.launches)
	}
}

// scriptedLaunch is a commandLaunch that returns results in turn, sets what
// the proxy counted during each, and records the attempt numbers its hooks
// and log path were given.
type scriptedLaunch struct {
	results  []runner.Result
	errs     []error
	failures []int
	calls    int
	logs     []string
	befores  []int
}

func (s *scriptedLaunch) launch(ctx context.Context, logPath func(int) string, before func(int) error, after func(int, runner.Result, error) error) (runner.Result, error) {
	i := s.calls
	s.calls++
	if s.errs[i] != nil {
		return s.results[i], s.errs[i]
	}
	if err := before(1); err != nil {
		return runner.Result{}, err
	}
	s.logs = append(s.logs, logPath(1))
	if into, _ := ctx.Value(registryFetchesKey{}).(*registryFetches); into != nil {
		*into = registryFetches{failures: s.failures[i], known: true}
	}
	return s.results[i], after(1, s.results[i], nil)
}

func (s *scriptedLaunch) rerun(ctx context.Context) (res runner.Result, afters []int, err error) {
	res, err = rerunAfterFetchFailure(ctx,
		func(attempt int) string { return fmt.Sprintf("log-%d", attempt) },
		func(attempt int) error { s.befores = append(s.befores, attempt); return nil },
		func(attempt int, _ runner.Result, _ error) error { afters = append(afters, attempt); return nil },
		s.launch)
	return res, afters, err
}

func ranFor(d time.Duration, exit int) runner.Result {
	start := time.Now().Add(-d)
	return runner.Result{ExitCode: exit, StartedAt: start, FinishedAt: start.Add(d)}
}

func TestTheSecondRunsAttemptsAreNumberedAfterTheFirsts(t *testing.T) {
	s := &scriptedLaunch{results: []runner.Result{ranFor(time.Second, 1), ranFor(time.Second, 0)}, errs: []error{nil, nil}, failures: []int{1, 0}}
	res, afters, err := s.rerun(context.Background())
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("= %+v, %v; want the second run's pass", res, err)
	}
	if !slices.Equal(s.befores, []int{1, 2}) || !slices.Equal(afters, []int{1, 2}) || !slices.Equal(s.logs, []string{"log-1", "log-2"}) {
		t.Errorf("before hooks %v, after hooks %v, logs %v; want attempts 1 and 2", s.befores, afters, s.logs)
	}
}

// A second run the Activity has no time for would turn a judged failure into
// a timeout: the first failure stands instead.
func TestTheCommandIsNotRunAgainWithoutTimeForIt(t *testing.T) {
	t.Run("less than twice the first run's time is left", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		s := &scriptedLaunch{results: []runner.Result{ranFor(time.Hour, 1)}, errs: []error{nil}, failures: []int{1}}
		res, _, err := s.rerun(ctx)
		if err != nil || res.ExitCode != 1 || s.calls != 1 {
			t.Errorf("= exit %d, %v after %d launch(es); want the first failure and no second launch", res.ExitCode, err, s.calls)
		}
	})
	t.Run("the second launch found no room before the deadline", func(t *testing.T) {
		s := &scriptedLaunch{results: []runner.Result{ranFor(time.Second, 1), {ExitCode: -1}}, errs: []error{nil, outOfTime(time.Minute, nil)}, failures: []int{1, 0}}
		res, _, err := s.rerun(context.Background())
		if err != nil || res.ExitCode != 1 || s.calls != 2 {
			t.Errorf("= exit %d, %v after %d launch(es); want the first failure to stand", res.ExitCode, err, s.calls)
		}
	})
}

// Any other error of the second launch is that launch's own: a sandbox that
// never started, or one that may still exist, is not hidden behind the first
// run's exit code.
func TestTheSecondLaunchsOwnErrorIsReturned(t *testing.T) {
	for _, launchErr := range []error{
		fmt.Errorf("sandbox attempts exhausted: %w", sandbox.ErrSandboxNotStarted),
		outOfTime(time.Minute, sandbox.ErrCleanupUnconfirmed),
	} {
		s := &scriptedLaunch{results: []runner.Result{ranFor(time.Second, 1), {ExitCode: -1}}, errs: []error{nil, launchErr}, failures: []int{1, 0}}
		res, _, err := s.rerun(context.Background())
		if !errors.Is(err, launchErr) || res.ExitCode != -1 {
			t.Errorf("= exit %d, %v; want the second launch's %v", res.ExitCode, err, launchErr)
		}
	}
}
