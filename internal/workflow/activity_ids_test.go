package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"

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
