package workflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/handoff"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/unresolved"
)

// mib is the three disk readings of a launch in MiB: Docker's free space
// before it, after its command exited, and after its sandbox was deleted.
func mib(before, afterExit, afterDelete uint64) *[3]uint64 {
	return &[3]uint64{before << 20, afterExit << 20, afterDelete << 20}
}

// A disk with 20 GiB free before the launch, 0.5 GiB after it, and the
// launch's own layer and volume releasing 100 MiB: it was nearly full already.
var diskAlreadyFull = mib(20<<10, 512, 612)

// The launch itself wrote its way from 30 GiB to 0.5 GiB free, and the delete
// gave 29 GiB back.
var diskTheLaunchFilled = mib(30<<10, 512, 29<<10)

// resourceSteps are the steps whose command is judged by the disk and
// memory rules: the three of fetchSteps, and the baseline verify.
var resourceSteps = append(slices.Clone(fetchSteps), struct {
	name string
	run  func(f *fetchFixture) (VerifyActivityResult, error)
}{"baseline verify", func(f *fetchFixture) (VerifyActivityResult, error) {
	res, err := executeBaseline(f)
	return VerifyActivityResult{Attempts: res.Attempts}, err
}})

func unresolvedAs(t *testing.T, err error, failureType string, cause unresolved.Cause) {
	t.Helper()
	if !hasApplicationErrorType(err, failureType) {
		t.Fatalf("err = %v, want type %q", err, failureType)
	}
	if got := UnresolvedCauseFromError(err); got != cause {
		t.Errorf("cause = %q, want %q", got, cause)
	}
	if got := HaltReasonCodeFromError(err); got != string(cause) {
		t.Errorf("halt reason code = %q, want %q", got, cause)
	}
	if hasApplicationErrorType(err, BaselineVerifyFailureType) {
		t.Errorf("err = %v: a step with no verdict is not a failed baseline", err)
	}
}

func TestAStepThatFailedOnAFullDockerDiskIsUnresolved(t *testing.T) {
	for _, step := range resourceSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 1, disk: diskAlreadyFull})
			res, err := step.run(f)
			unresolvedAs(t, err, SandboxDiskFailureType, unresolved.SandboxDisk)
			if f.launches != 1 {
				t.Errorf("launches = %d, want 1: a second run finds the same disk", f.launches)
			}
			if got := exitCodes(res.Attempts); !slices.Equal(got, []int{1}) {
				t.Errorf("attempt exit codes on the error = %v, want [1]", got)
			}
			if step.name == "baseline verify" {
				if record, loadErr := run.LoadBaselineVerify(f.acts.logDirFor(f.input)); loadErr != nil || record != nil {
					t.Errorf("baseline record = %+v, %v; an unresolved baseline records no result", record, loadErr)
				}
				return
			}
			// A redispatch replays the checkpoint: same type, no new launch.
			_, again := step.run(f)
			if !hasApplicationErrorType(again, SandboxDiskFailureType) || UnresolvedCauseFromError(again) != unresolved.SandboxDisk {
				t.Errorf("replayed checkpoint: err = %v, want type %q", again, SandboxDiskFailureType)
			}
			if f.launches != 1 {
				t.Errorf("launches after the replay = %d, want still 1", f.launches)
			}
		})
	}
}

func TestADiskTheLaunchFilledItselfIsAnOrdinaryFailure(t *testing.T) {
	for _, step := range fetchSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 1, disk: diskTheLaunchFilled})
			res, err := step.run(f)
			if err != nil || res.Result.ExitCode != 1 {
				t.Fatalf("= exit %d, %v; want the command's own failure", res.Result.ExitCode, err)
			}
			if f.launches != 1 {
				t.Errorf("launches = %d, want 1", f.launches)
			}
		})
	}
	t.Run("a disk with room left", func(t *testing.T) {
		f := newFetchFixture(t, fetchLaunch{exit: 1, disk: mib(50<<10, 40<<10, 40<<10+100)})
		if res, err := fetchSteps[0].run(f); err != nil || res.Result.ExitCode != 1 {
			t.Fatalf("= exit %d, %v; want the command's own failure", res.Result.ExitCode, err)
		}
	})
}

// A build can cause its own OOM kill, so a step after it that fails with the
// worker OOM-killed is a judged failure: it is not run again, is not
// unresolved, and the next build is told the fact from the factory's reading.
func TestAPostBuildOOMIsAJudgedFailureNamedToTheNextBuild(t *testing.T) {
	for _, step := range fetchSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 137, oom: true})
			res, err := step.run(f)
			if err != nil || res.Result.ExitCode != 137 {
				t.Fatalf("= exit %d, %v; want the command's own failure", res.Result.ExitCode, err)
			}
			if f.launches != 1 {
				t.Errorf("launches = %d, want 1: an OOM-killed command is never run again", f.launches)
			}
			if len(res.Attempts) != 1 || res.Attempts[0].Resources == nil || !res.Attempts[0].Resources.OOMKilled {
				t.Fatalf("attempts = %+v, want the OOM fact recorded", res.Attempts)
			}
			// The record the next build is handed.
			check := map[string]string{"canonical verify": "canonical_verify", "full suite": "full_suite_verify", "named gate": "lint"}[step.name]
			r := &run.Run{ID: "r", State: run.StateQuarantined, BaseSHA: "1111111", ResultSHA: "2222222", Attempts: res.Attempts,
				GateResults: []run.GateResult{{Check: check, Passed: false, ExitCode: 137}}}
			doc := handoff.Build(r, t.TempDir())
			if len(doc.Checks) != 1 || !strings.Contains(doc.Checks[0].Environment, "for want of memory") {
				t.Fatalf("checks = %+v, want the factory's OOM sentence on %s", doc.Checks, check)
			}
			if md := doc.Markdown(); !strings.Contains(md, "The factory observed that the kernel killed a process of this step for want of memory") {
				t.Errorf("the next build's record lacks the OOM line:\n%s", md)
			}
		})
	}
	t.Run("a failure that was not OOM-killed says nothing of memory", func(t *testing.T) {
		f := newFetchFixture(t, fetchLaunch{exit: 1})
		res, _ := fetchSteps[0].run(f)
		r := &run.Run{ID: "r", State: run.StateQuarantined, Attempts: res.Attempts, GateResults: []run.GateResult{{Check: "canonical_verify", Passed: false, ExitCode: 1}}}
		if doc := handoff.Build(r, t.TempDir()); len(doc.Checks) != 1 || doc.Checks[0].Environment != "" || strings.Contains(doc.Markdown(), "memory") {
			t.Errorf("checks = %+v", doc.Checks)
		}
	})
}

// sidecarDisk reads Docker's free space as a launch whose sidecar filled a
// volume leaves it: nearly none after the command exits, the space coming
// back only when the registry proxy is cleaned up.
type sidecarDisk struct {
	*inspectRuntime
	reads   int
	cleaned *bool
}

func (s *sidecarDisk) DockerDiskFree(context.Context) (sandbox.DiskSpace, error) {
	s.reads++
	free := uint64(30 << 30)
	switch {
	case s.reads == 3:
		free = 512 << 20
	case s.reads == 4 && *s.cleaned:
		free = 29 << 30
	case s.reads == 4:
		free = 600 << 20
	}
	return sandbox.DiskSpace{FreeBytes: free, TotalBytes: 1 << 40}, nil
}

func TestADiskASidecarFilledIsAnOrdinaryFailure(t *testing.T) {
	f := newFetchFixture(t, fetchLaunch{exit: 1})
	cleaned := false
	f.acts.Sandboxes = &sidecarDisk{inspectRuntime: f.rt, cleaned: &cleaned}
	cleanup := f.acts.registryProxyHooks.CleanupRegistryProxy
	f.acts.registryProxyHooks.CleanupRegistryProxy = func(ctx context.Context, h *sandbox.RegistryProxyHandle) error {
		cleaned = true
		return cleanup(ctx, h)
	}
	res, err := fetchSteps[0].run(f)
	if err != nil || res.Result.ExitCode != 1 {
		t.Fatalf("= exit %d, %v; want the command's own failure: the space the proxy gave back counts as the launch's own", res.Result.ExitCode, err)
	}
	if got := res.Attempts[0].Resources; got == nil || got.DockerDiskFreeAfterDelete != 29<<30 {
		t.Errorf("recorded readings = %+v, want the release read after the proxy's cleanup", got)
	}
}

func TestAPassIsAPassWhateverTheReadings(t *testing.T) {
	for _, step := range resourceSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 0, oom: true, disk: diskAlreadyFull})
			res, err := step.run(f)
			if err != nil || res.Result.ExitCode != 0 && step.name != "baseline verify" {
				t.Fatalf("= exit %d, %v; want a pass", res.Result.ExitCode, err)
			}
			if f.launches != 1 {
				t.Errorf("launches = %d, want 1", f.launches)
			}
		})
	}
}

func TestUnknownReadingsJudgeAsWithoutTheRule(t *testing.T) {
	// An unreadable disk (no launch scripts one) and a disk that is full.
	for _, step := range fetchSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 1})
			res, err := step.run(f)
			if err != nil || res.Result.ExitCode != 1 || f.launches != 1 {
				t.Fatalf("= exit %d, %v, launches %d; want the command's own failure", res.Result.ExitCode, err, f.launches)
			}
		})
	}
	// Readings the factory could not take are not read as a cause.
	unknown := sandbox.Resources{OOMKilled: true, DiskFree1: 1}
	if killedForMemory(unknown) || diskRanOut(unknown, true) || diskRanOut(unknown, false) {
		t.Errorf("readings that were never taken read as a cause: %+v", unknown)
	}
	noDelete := sandbox.Resources{DiskKnown: true, DiskFree0: 20 << 30, DiskFree1: 1}
	if diskRanOut(noDelete, false) || !diskRanOut(noDelete, true) {
		t.Errorf("a disk whose delete reading is missing: strict %v, baseline %v; want false and true", diskRanOut(noDelete, false), diskRanOut(noDelete, true))
	}
}

func TestABaselineThatRanOutOfDiskOrMemoryIsUnresolvedNotAFailedBaseline(t *testing.T) {
	t.Run("disk, even one the launch filled", func(t *testing.T) {
		f := newFetchFixture(t, fetchLaunch{exit: 1, disk: diskTheLaunchFilled})
		_, err := executeBaseline(f)
		unresolvedAs(t, err, SandboxDiskFailureType, unresolved.SandboxDisk)
	})
	t.Run("memory on the first run", func(t *testing.T) {
		f := newFetchFixture(t, fetchLaunch{exit: 137, oom: true})
		res, err := executeBaseline(f)
		unresolvedAs(t, err, SandboxMemoryFailureType, unresolved.SandboxMemory)
		if f.launches != 1 || len(res.Attempts) != 1 {
			t.Errorf("launches = %d, attempts on the error = %d, want 1 and 1", f.launches, len(res.Attempts))
		}
		if record, loadErr := run.LoadBaselineVerify(f.acts.logDirFor(f.input)); loadErr != nil || record != nil {
			t.Errorf("baseline record = %+v, %v; an unresolved baseline records no result", record, loadErr)
		}
	})
	t.Run("neither: an ordinary failed baseline", func(t *testing.T) {
		f := newFetchFixture(t, fetchLaunch{exit: 1, disk: mib(50<<10, 40<<10, 40<<10+100)})
		_, err := executeBaseline(f)
		if !hasApplicationErrorType(err, BaselineVerifyFailureType) || UnresolvedCauseFromError(err) != "" {
			t.Fatalf("err = %v, want type %q", err, BaselineVerifyFailureType)
		}
	})
}

func TestAStepLaunchRefusedForWantOfDiskIsUnresolved(t *testing.T) {
	for _, step := range resourceSteps {
		t.Run(step.name, func(t *testing.T) {
			f := newFetchFixture(t, fetchLaunch{exit: 1, disk: mib(1<<10, 1<<10, 1<<10)})
			_, err := step.run(f)
			unresolvedAs(t, err, SandboxDiskFailureType, unresolved.SandboxDisk)
			if f.launches != 0 {
				t.Errorf("launches = %d: the command must never be released", f.launches)
			}
			if f.proxies != 0 {
				t.Errorf("registry proxies started = %d: nothing may be pulled onto a full disk", f.proxies)
			}
		})
	}
	refused := fmt.Errorf("sandbox attempts exhausted: %w", fmt.Errorf("%w: %w", sandbox.ErrSandboxNotStarted, sandbox.ErrSandboxDiskLow))
	for name, typeOf := range map[string]func(error) string{"launchErrorType": launchErrorType, "buildActivityErrorType": buildActivityErrorType, "checkpointErrorType": checkpointErrorType} {
		if got := typeOf(refused); got != SandboxDiskFailureType {
			t.Errorf("%s = %q, want %q: the build is refused too", name, got, SandboxDiskFailureType)
		}
	}
}

func TestTheBuildRecordsResourcesButIsNotUnresolvedByThem(t *testing.T) {
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, args ...string) (runner.Result, error) {
			res := runner.Result{ExitCode: 1, Command: args, Resources: sandbox.Resources{
				DiskFree0: 20 << 30, DiskFree1: 512 << 20, DiskFree2: 600 << 20, DiskKnown: true, ReleasedKnown: true, OOMKilled: true, OOMKnown: true,
			}}
			if onAttempt != nil {
				onAttempt(1, res, nil)
			}
			return res, nil
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	raw, err := env.ExecuteActivity(activities.RunBuildActivity, fixtureInput())
	if err != nil {
		t.Fatalf("RunBuildActivity: %v (a build is not made unresolved by the readings)", err)
	}
	var result BuildActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatal(err)
	}
	want := &run.AttemptResources{DockerDiskFreeBefore: 20 << 30, DockerDiskFreeAfterExit: 512 << 20, DockerDiskFreeAfterDelete: 600 << 20, OOMKilled: true}
	if len(result.Attempts) != 1 || result.Attempts[0].Resources == nil || *result.Attempts[0].Resources != *want {
		t.Errorf("attempts = %+v, want the readings %+v recorded", result.Attempts, want)
	}
}

func TestTheAttemptsOfAVerifyRecordTheReadings(t *testing.T) {
	f := newFetchFixture(t, fetchLaunch{exit: 0, disk: mib(50<<10, 49<<10, 50<<10)})
	res, err := fetchSteps[0].run(f)
	if err != nil || len(res.Attempts) != 1 {
		t.Fatalf("= %v, %d attempts", err, len(res.Attempts))
	}
	got := res.Attempts[0].Resources
	want := &run.AttemptResources{DockerDiskFreeBefore: 50 << 30, DockerDiskFreeAfterExit: 49 << 30, DockerDiskFreeAfterDelete: 50 << 30}
	if got == nil || *got != *want {
		t.Errorf("recorded readings = %+v, want %+v", got, want)
	}
}

func TestEveryUnresolvedFailureTypeIsNonRetryable(t *testing.T) {
	for failureType, cause := range unresolvedFailureTypes {
		if !slices.Contains(nonRetryableActivityFailureTypes, failureType) {
			t.Errorf("Temporal would retry %q, which gives no verdict on the work", failureType)
		}
		if !cause.Known() {
			t.Errorf("%q maps to %q, which is not a known cause", failureType, cause)
		}
		// Across the Temporal boundary the type alone names the cause and
		// keeps the worktree.
		crossed := wrapActivityFailure("run verify activity", temporal.NewApplicationErrorWithCause("verify subprocess infrastructure failure", failureType, errors.New("cause")), nil, "base", false, "/wt", "branch")
		if !StepLostFromError(crossed) || UnresolvedCauseFromError(crossed) != cause {
			t.Errorf("%q across the Temporal boundary: cause %q, want %q and a kept worktree", failureType, UnresolvedCauseFromError(crossed), cause)
		}
	}
	// Each environment error maps to its own type, and a sandbox that may
	// still exist stays a safety stop above them all.
	for failureType, runErr := range map[string]error{
		DependencyFetchFailureType: errDependencyFetch,
		SandboxDiskFailureType:     errSandboxDisk,
		SandboxMemoryFailureType:   errSandboxMemory,
	} {
		failed := fmt.Errorf("%w (exit code 1)", runErr)
		if got := launchErrorType(failed); got != failureType {
			t.Errorf("launchErrorType(%v) = %q, want %q", runErr, got, failureType)
		}
		if got := checkpointErrorType(failed); got != failureType {
			t.Errorf("checkpointErrorType = %q: a replayed checkpoint must carry %q", got, failureType)
		}
		if got := launchErrorType(errors.Join(failed, sandbox.ErrCleanupUnconfirmed)); got != CleanupUnconfirmedFailureType {
			t.Errorf("%q with an unconfirmed teardown: type = %q, want %q", failureType, got, CleanupUnconfirmedFailureType)
		}
		if got := UnresolvedCauseFromError(errors.New(failureType + ": a message that only says so")); got != "" {
			t.Errorf("a message that only says %q read as cause %q", failureType, got)
		}
	}
}

func TestRunWorkflowKeepsTheWorktreeOfAVerifyStepThatGaveNoVerdict(t *testing.T) {
	for _, failureType := range []string{DependencyFetchFailureType, SandboxDiskFailureType, SandboxMemoryFailureType} {
		t.Run(failureType, func(t *testing.T) {
			rollbacks := 0
			env := newWorkflowEnvironment(t,
				func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
					return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
				},
				func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
					return VerifyActivityResult{}, temporal.NewApplicationError("verify subprocess infrastructure failure", failureType)
				},
				func(context.Context, EvaluateGateInput) (run.GateResult, error) {
					t.Error("the gate was evaluated for a verify that gave no verdict")
					return run.GateResult{}, nil
				},
			)
			env.RegisterActivityWithOptions(func(context.Context, PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
				return PrepareIsolatedWorkspaceResult{WorktreePath: "/fixture/data/workspaces/wt", Branch: "factoryd/wt"}, nil
			}, activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName})
			env.RegisterActivityWithOptions(func(context.Context, RollbackIsolatedWorkspaceInput) error {
				rollbacks++
				return nil
			}, activity.RegisterOptions{Name: RollbackIsolatedWorkspaceActivityName})
			env.RegisterActivityWithOptions(func(context.Context, DisableWorkerGroupWriteInput) error { return nil }, activity.RegisterOptions{Name: DisableWorkerGroupWriteActivityName})
			env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
			err := env.GetWorkflowError()
			if got, want := HaltReasonCodeFromError(err), string(unresolvedFailureTypes[failureType]); got != want || want == "" {
				t.Fatalf("halt reason code = %q (err %v), want %q", got, err, want)
			}
			if rollbacks != 0 {
				t.Errorf("rollback ran %d time(s): the worktree is kept for a resume decision", rollbacks)
			}
			if wp, br := IsolatedWorkspaceFromError(err); wp == "" || br == "" {
				t.Errorf("IsolatedWorkspaceFromError = (%q, %q); the caller needs the kept worktree on the error", wp, br)
			}
		})
	}
}
