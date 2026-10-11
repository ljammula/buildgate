package workflow

import (
	"buildgate/internal/unresolved"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"errors"
	"fmt"
	"go.temporal.io/sdk/temporal"
	"slices"
)

// startedActivityIDs runs RunWorkflow to acceptance and returns the
// ActivityID of every Activity it started, in start order. pinVersion, when
// set, pins explicitActivityIDsChange to that version.
func startedActivityIDs(t *testing.T, pinVersion *temporalworkflow.Version) []string {
	t.Helper()
	build := BuildActivityResult{Result: runner.Result{ExitCode: 0}}
	verify := VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) { return build, nil },
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) { return verify, nil },
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	if pinVersion != nil {
		env.OnGetVersion(explicitActivityIDsChange, temporalworkflow.DefaultVersion, 1).Return(*pinVersion)
	}
	var mu sync.Mutex
	var ids []string
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, info.ActivityID)
	})
	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	return ids
}

func TestRunWorkflowNamesEachActivity(t *testing.T) {
	ids := startedActivityIDs(t, nil)
	for _, want := range []string{"capture-base-sha", "preflight", "build", "post-build", "verify", "collect-evidence", "evaluate"} {
		if !containsString(ids, want) {
			t.Errorf("ActivityIDs %q lack %q", ids, want)
		}
	}
	if idx := indexOf(ids, "build"); idx < 0 || idx > indexOf(ids, "verify") {
		t.Errorf("build must start before verify: %q", ids)
	}
}

func TestRunWorkflowKeepsDefaultActivityIDsOnTheOldVersion(t *testing.T) {
	old := temporalworkflow.DefaultVersion
	ids := startedActivityIDs(t, &old)
	if len(ids) == 0 {
		t.Fatal("no Activity started")
	}
	for _, id := range ids {
		if strings.Trim(id, "0123456789") != "" {
			t.Errorf("ActivityID %q on DefaultVersion, want the SDK's numeric schedule ID", id)
		}
	}
}

func TestNextActivityIDSuffixesARepeatedName(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{1, "rollback-workspace"}, {2, "rollback-workspace-2"}, {3, "rollback-workspace-3"}} {
		if got := nextActivityID("rollback-workspace", tc.n); got != tc.want {
			t.Errorf("nextActivityID(rollback-workspace, %d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func containsString(list []string, s string) bool { return indexOf(list, s) >= 0 }

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func TestASandboxRerunIsALostStepThatIsNotRetried(t *testing.T) {
	rerun := fmt.Errorf("sandbox bg-x: %w", sandbox.ErrSandboxRerun)
	if got := buildActivityErrorType(rerun); got != SandboxRerunFailureType {
		t.Errorf("buildActivityErrorType = %q, want %q", got, SandboxRerunFailureType)
	}
	// A command killed mid-request also leaves its request unsettled; the
	// rerun is the cause and names the failure.
	both := errors.Join(rerun, fmt.Errorf("%w: 1 request unsettled", sandbox.ErrRelayCeilingExceeded))
	if got := buildActivityErrorType(both); got != SandboxRerunFailureType {
		t.Errorf("buildActivityErrorType with an unsettled request = %q, want %q", got, SandboxRerunFailureType)
	}
	if !slices.Contains(nonRetryableActivityFailureTypes, SandboxRerunFailureType) {
		t.Error("a build whose command was started twice would be retried over a half-edited worktree")
	}
	crossed := temporal.NewApplicationErrorWithCause("build subprocess infrastructure failure", SandboxRerunFailureType, rerun)
	if !StepLostFromError(fmt.Errorf("workflow: %w", crossed)) {
		t.Error("the worktree of a build lost to a rerun would be rolled back")
	}
	other := temporal.NewApplicationError("build failed", InfrastructureFailureType)
	if StepLostFromError(other) {
		t.Error("an ordinary infrastructure failure keeps its rollback")
	}
}

func TestASandboxThatNeverStartedIsAnUnresolvedStepThatKeepsItsWorktree(t *testing.T) {
	launch := fmt.Errorf("%w: create sandbox: image pull failed", sandbox.ErrSandboxNotStarted)
	if got := launchErrorType(launch); got != SandboxLaunchFailureType {
		t.Errorf("launchErrorType = %q, want %q", got, SandboxLaunchFailureType)
	}
	if got := buildActivityErrorType(fmt.Errorf("sandbox attempts exhausted: %w", launch)); got != SandboxLaunchFailureType {
		t.Errorf("buildActivityErrorType through the attempt loop's wrap = %q, want %q", got, SandboxLaunchFailureType)
	}
	if got := launchErrorType(outOfTime(time.Minute, launch)); got != SandboxLaunchFailureType {
		t.Errorf("a failed launch the deadline left no room to repeat = %q, want %q", got, SandboxLaunchFailureType)
	}
	if got := checkpointErrorType(launch); got != SandboxLaunchFailureType {
		t.Errorf("checkpointErrorType = %q: a replayed checkpoint would read as an infrastructure failure and roll back", got)
	}
	if !slices.Contains(nonRetryableActivityFailureTypes, SandboxLaunchFailureType) {
		t.Error("Temporal would retry a launch failure on its own (SC-017)")
	}
	crossed := temporal.NewApplicationErrorWithCause("review subprocess infrastructure failure", SandboxLaunchFailureType, launch)
	wrapped := fmt.Errorf("workflow: %w", crossed)
	if !StepLostFromError(wrapped) || UnresolvedCauseFromError(wrapped) != unresolved.SandboxLaunch {
		t.Errorf("cause across the Temporal boundary = %q, want %q and a kept worktree", UnresolvedCauseFromError(wrapped), unresolved.SandboxLaunch)
	}
	if got := HaltReasonCodeFromError(wrapped); got != string(unresolved.SandboxLaunch) {
		t.Errorf("HaltReasonCodeFromError = %q, want the cause", got)
	}
}

// None of these is "the sandbox never started its command": each has its own
// type, or the command ran. A step that hangs or prints without end must not
// be able to turn its own failure into an unjudged, uncounted one.
func TestWhatIsNotALaunchFailure(t *testing.T) {
	notStarted := fmt.Errorf("%w: create: gateway unreachable", sandbox.ErrSandboxNotStarted)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an unconfirmed teardown: the sandbox may still exist", fmt.Errorf("%v; cleanup: %w", notStarted, sandbox.ErrCleanupUnconfirmed), CleanupUnconfirmedFailureType},
		{"never started, joined with an unconfirmed teardown", errors.Join(notStarted, sandbox.ErrCleanupUnconfirmed), CleanupUnconfirmedFailureType},
		{"a reached ceiling", fmt.Errorf("%w: refused", sandbox.ErrRelayCeilingExceeded), RelayCeilingExceededFailureType},
		{"a build's command started twice", sandbox.ErrSandboxRerun, SandboxRerunFailureType},
		{"a failed start after an earlier attempt's command ran", afterACommandRan(notStarted, true), InfrastructureFailureType},
		{"a command that started and hit the sandbox's timeout", fmt.Errorf("sandbox bg-x: %w", context.DeadlineExceeded), InfrastructureFailureType},
		{"a command whose wait was lost", errors.New("openshell: wait bg-x: connection reset"), InfrastructureFailureType},
		{"a message that only says so", errors.New("the sandbox did not start the worker command"), InfrastructureFailureType},
		{"an error from before the launch", outOfTime(time.Minute, nil), InfrastructureFailureType},
		{"a refused .factory mount", fmt.Errorf("mount: %w", sandbox.ErrCommitDirMount), FactoryDirFailureType},
	} {
		if got := buildActivityErrorType(tc.err); got != tc.want {
			t.Errorf("%s: type = %q, want %q", tc.name, got, tc.want)
		}
	}
	// One sign of a command started twice is its exit code, and a verify or
	// gate step runs the repository's own command, which could return it: on
	// those steps it must stay a failure that rolls back, never a lost step.
	if got := launchErrorType(sandbox.ErrSandboxRerun); got != InfrastructureFailureType {
		t.Errorf("launchErrorType of a rerun (verify, gate, review) = %q, want %q", got, InfrastructureFailureType)
	}
	if got := afterACommandRan(notStarted, false); !errors.Is(got, sandbox.ErrSandboxNotStarted) {
		t.Errorf("a step none of whose attempts ran lost its type: %v", got)
	}
}

func TestUnresolvedCauseFromErrorReadsOnlyTemporalsOwnTypes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want unresolved.Cause
	}{
		{"nil", nil, ""},
		{"a heartbeat timeout", lostBuildError(), unresolved.WorkerLost},
		{"a command started twice", temporal.NewApplicationError("x", SandboxRerunFailureType), unresolved.WorkerLost},
		{"a launch failure", temporal.NewApplicationError("x", SandboxLaunchFailureType), unresolved.SandboxLaunch},
		{"an infrastructure failure, which old histories hold for a rolled-back halt", temporal.NewApplicationError("x", InfrastructureFailureType), ""},
		{"a message that only says so", errors.New("SandboxLaunch: heartbeat timeout: worker_lost"), ""},
		{"a failed baseline", temporal.NewApplicationError("x", BaselineVerifyFailureType), ""},
	} {
		if got := UnresolvedCauseFromError(tc.err); got != tc.want {
			t.Errorf("%s: cause = %q, want %q", tc.name, got, tc.want)
		}
	}
}
