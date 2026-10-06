package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
)

func TestRunWorkflowAccepted(t *testing.T) {
	input := fixtureInput()
	build := BuildActivityResult{Result: runner.Result{ExitCode: 0}}
	verify := VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) { return build, nil },
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) { return verify, nil },
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", result.State, run.StateAccepted)
	}
}

// TestRunWorkflowScopesHeartbeatTimeoutToBuildVerifyActivities is the
// regression test for a real P1 finding from codex review: an earlier
// version of this fix set HeartbeatTimeout on the one shared
// ActivityOptions every Activity RunWorkflow schedules uses, on the
// (incorrect) theory that a HeartbeatTimeout is inert for an Activity that
// never calls RecordHeartbeat. It isn't — Temporal starts that timeout as
// soon as the Activity begins executing regardless, and fails it once it
// elapses with zero heartbeats recorded, silently cutting every
// non-heartbeating Activity's real budget from ActivityStartToCloseTimeout
// (an hour) down to the heartbeat timeout (30s). Only RunBuildActivity/
// RunVerifyActivity actually heartbeat (see activityHeartbeatInterval's
// doc comment), so only they get a non-zero HeartbeatTimeout now; every
// other Activity RunWorkflow schedules must see zero (disabled).
func TestRunWorkflowScopesHeartbeatTimeoutToBuildVerifyActivities(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	registerIsolationActivities(env)
	env.SetTestTimeout(5 * time.Second)

	heartbeatTimeoutFor := make(map[string]time.Duration)
	var mu sync.Mutex
	captureInfo := func(ctx context.Context, name string) {
		mu.Lock()
		defer mu.Unlock()
		heartbeatTimeoutFor[name] = activity.GetInfo(ctx).HeartbeatTimeout
	}

	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ CaptureBaseSHAInput) (string, error) {
			captureInfo(ctx, CaptureBaseSHAActivityName)
			return "fixture-base-sha", nil
		},
		activity.RegisterOptions{Name: CaptureBaseSHAActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ PreflightInput) error {
			captureInfo(ctx, PreflightActivityName)
			return nil
		},
		activity.RegisterOptions{Name: PreflightActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ RunWorkflowInput) (BuildActivityResult, error) {
			captureInfo(ctx, RunBuildActivityName)
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ PostBuildInput) (PostBuildResult, error) {
			captureInfo(ctx, PostBuildActivityName)
			return PostBuildResult{ResultSHA: "fixture-result-sha"}, nil
		},
		activity.RegisterOptions{Name: PostBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ RunWorkflowInput) (VerifyActivityResult, error) {
			captureInfo(ctx, RunVerifyActivityName)
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunVerifyActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ CollectEvidenceInput) (CollectedEvidence, error) {
			captureInfo(ctx, CollectEvidenceActivityName)
			return CollectedEvidence{}, nil
		},
		activity.RegisterOptions{Name: CollectEvidenceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
		activity.RegisterOptions{Name: EvaluateGateActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, input EvaluateRunInput) (policy.EvaluateRunResult, error) {
			captureInfo(ctx, EvaluateRunActivityName)
			return policy.EvaluateRun(input.Policy), nil
		},
		activity.RegisterOptions{Name: EvaluateRunActivityName},
	)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{RunBuildActivityName, RunVerifyActivityName} {
		if heartbeatTimeoutFor[name] == 0 {
			t.Errorf("%s HeartbeatTimeout = 0, want non-zero (it heartbeats)", name)
		}
	}
	for _, name := range []string{CaptureBaseSHAActivityName, PreflightActivityName, PostBuildActivityName, CollectEvidenceActivityName} {
		if got := heartbeatTimeoutFor[name]; got != 0 {
			t.Errorf("%s HeartbeatTimeout = %v, want 0 (disabled) — it never heartbeats, so a non-zero timeout would silently cut its real budget", name, got)
		}
	}
}

// TestRunWorkflowWidensBuildVerifyTimeoutForConfiguredMinutes is the
// regression for a codex finding: a build/verify Activity's
// StartToCloseTimeout was the flat ActivityStartToCloseTimeout (an hour)
// regardless of input.TimeoutMinutes, so a run configured to allow longer
// than an hour — sandboxed or not, since sandbox.LaunchSpec's own
// per-attempt deadline is derived from this same Activity context — was
// killed at that flat hour anyway. RunBuildActivity/RunVerifyActivity must
// see the widened timeout; every other Activity must keep the flat default,
// exactly like the HeartbeatTimeout scoping this mirrors.
func TestRunWorkflowWidensBuildVerifyTimeoutForConfiguredMinutes(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	registerIsolationActivities(env)
	env.SetTestTimeout(5 * time.Second)

	startToCloseFor := make(map[string]time.Duration)
	var mu sync.Mutex
	captureInfo := func(ctx context.Context, name string) {
		mu.Lock()
		defer mu.Unlock()
		startToCloseFor[name] = activity.GetInfo(ctx).StartToCloseTimeout
	}

	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ CaptureBaseSHAInput) (string, error) {
			captureInfo(ctx, CaptureBaseSHAActivityName)
			return "fixture-base-sha", nil
		},
		activity.RegisterOptions{Name: CaptureBaseSHAActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ PreflightInput) error {
			captureInfo(ctx, PreflightActivityName)
			return nil
		},
		activity.RegisterOptions{Name: PreflightActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ RunWorkflowInput) (BuildActivityResult, error) {
			captureInfo(ctx, RunBuildActivityName)
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ PostBuildInput) (PostBuildResult, error) {
			captureInfo(ctx, PostBuildActivityName)
			return PostBuildResult{ResultSHA: "fixture-result-sha"}, nil
		},
		activity.RegisterOptions{Name: PostBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ RunWorkflowInput) (VerifyActivityResult, error) {
			captureInfo(ctx, RunVerifyActivityName)
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunVerifyActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, _ CollectEvidenceInput) (CollectedEvidence, error) {
			captureInfo(ctx, CollectEvidenceActivityName)
			return CollectedEvidence{}, nil
		},
		activity.RegisterOptions{Name: CollectEvidenceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
		activity.RegisterOptions{Name: EvaluateGateActivityName},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, input EvaluateRunInput) (policy.EvaluateRunResult, error) {
			captureInfo(ctx, EvaluateRunActivityName)
			return policy.EvaluateRun(input.Policy), nil
		},
		activity.RegisterOptions{Name: EvaluateRunActivityName},
	)

	input := fixtureInput()
	input.TimeoutMinutes = 180 // well beyond the flat hour default

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	wantWidened := time.Duration(input.TimeoutMinutes+5) * time.Minute
	for _, name := range []string{RunBuildActivityName, RunVerifyActivityName} {
		if got := startToCloseFor[name]; got != wantWidened {
			t.Errorf("%s StartToCloseTimeout = %v, want %v (derived from TimeoutMinutes=%d)", name, got, wantWidened, input.TimeoutMinutes)
		}
	}
	for _, name := range []string{CaptureBaseSHAActivityName, PreflightActivityName} {
		if got := startToCloseFor[name]; got != ActivityStartToCloseTimeout {
			t.Errorf("%s StartToCloseTimeout = %v, want the flat default %v", name, got, ActivityStartToCloseTimeout)
		}
	}
	// Host-side git steps that never heartbeat: a lost worker fails them fast.
	for _, name := range []string{PostBuildActivityName, CollectEvidenceActivityName} {
		if got := startToCloseFor[name]; got != 10*time.Minute {
			t.Errorf("%s StartToCloseTimeout = %v, want 10m", name, got)
		}
	}
}

// TestRunWorkflowCombinesBuildAndVerifyAttempts proves RunWorkflow carries
// BuildActivityResult.Attempts and VerifyActivityResult.Attempts through
// into RunWorkflowResult.Attempts, build attempts first — mirroring
// cmd/factoryd, which appends to the same run.Run.Attempts
// slice in that same order as they happen.
func TestRunWorkflowCombinesBuildAndVerifyAttempts(t *testing.T) {
	input := fixtureInput()
	build := BuildActivityResult{
		Result:   runner.Result{ExitCode: 0},
		Attempts: []run.Attempt{{Kind: "build", ExitCode: 0}},
	}
	verify := VerifyActivityResult{
		Result:   runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0},
		Attempts: []run.Attempt{{Kind: "verify", ExitCode: 0}},
	}
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) { return build, nil },
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) { return verify, nil },
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if len(result.Attempts) != 2 || result.Attempts[0].Kind != "build" || result.Attempts[1].Kind != "verify" {
		t.Fatalf("Attempts = %+v, want [build, verify]", result.Attempts)
	}
}

// TestRunWorkflowHaltsOnPreflightFailureBeforeBuild proves RunWorkflow
// runs PreflightActivity before RunBuildActivity, and a preflight failure
// stops the Workflow without ever invoking the build subprocess.
func TestRunWorkflowHaltsOnPreflightFailureBeforeBuild(t *testing.T) {
	buildCalled := false
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	registerIsolationActivities(env)
	env.RegisterActivityWithOptions(
		func(context.Context, CaptureBaseSHAInput) (string, error) { return "fixture-base-sha", nil },
		activity.RegisterOptions{Name: CaptureBaseSHAActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PreflightInput) error {
			return temporal.NewApplicationError("required file already dirty", PreflightFailureType)
		},
		activity.RegisterOptions{Name: PreflightActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			buildCalled = true
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunBuildActivityName},
	)
	env.SetTestTimeout(5 * time.Second)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow error = nil, want preflight failure")
	}
	if !hasApplicationErrorType(err, PreflightFailureType) {
		t.Fatalf("workflow error does not preserve application error type %q: %v", PreflightFailureType, err)
	}
	if buildCalled {
		t.Fatal("build Activity ran after a preflight failure")
	}
}

func TestRunWorkflowPiTicketStructurePreflight(t *testing.T) {
	validTicket := "This is an existing repo.\n\n## Goal\nship it\n\n## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n## Verification\n`make verify` must pass.\n\n## Commit\nticket(002): ship it\n"
	tests := []struct {
		name      string
		content   string
		wantErr   bool
		wantBuild bool
	}{
		{name: "malformed blocks build", content: "## Goal\nmissing the rest\n", wantErr: true, wantBuild: false},
		{name: "valid proceeds", content: validTicket, wantErr: false, wantBuild: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ticketPath := filepath.Join(t.TempDir(), "002-ticket.md")
			if err := os.WriteFile(ticketPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("write ticket: %v", err)
			}
			buildCalled := false
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			registerIsolationActivities(env)
			env.RegisterActivityWithOptions(func(context.Context, CaptureBaseSHAInput) (string, error) {
				return "fixture-base-sha", nil
			}, activity.RegisterOptions{Name: CaptureBaseSHAActivityName})
			env.RegisterActivityWithOptions((&Activities{}).PreflightActivity, activity.RegisterOptions{Name: PreflightActivityName})
			env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
				buildCalled = true
				return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
			}, activity.RegisterOptions{Name: RunBuildActivityName})
			env.RegisterActivityWithOptions(func(context.Context, PostBuildInput) (PostBuildResult, error) {
				return PostBuildResult{ResultSHA: "fixture-result-sha"}, nil
			}, activity.RegisterOptions{Name: PostBuildActivityName})
			env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
				return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
			}, activity.RegisterOptions{Name: RunVerifyActivityName})
			env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
				return CollectedEvidence{}, nil
			}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
			env.RegisterActivityWithOptions(func(_ context.Context, input EvaluateRunInput) (policy.EvaluateRunResult, error) {
				return policy.EvaluateRun(input.Policy), nil
			}, activity.RegisterOptions{Name: EvaluateRunActivityName})
			env.SetTestTimeout(5 * time.Second)

			input := fixtureInput()
			input.TicketPath = ticketPath
			input.TicketNumber = 2
			env.ExecuteWorkflow(RunWorkflow, input)
			err := env.GetWorkflowError()
			if tt.wantErr {
				if err == nil {
					t.Fatal("workflow error = nil, want malformed ticket preflight failure")
				}
				if !hasApplicationErrorType(err, PreflightFailureType) {
					t.Fatalf("workflow error does not preserve application error type %q: %v", PreflightFailureType, err)
				}
			} else if err != nil {
				t.Fatalf("valid ticket workflow error: %v", err)
			}
			if buildCalled != tt.wantBuild {
				t.Fatalf("buildCalled = %v, want %v", buildCalled, tt.wantBuild)
			}
		})
	}
}

// TestRunWorkflowPreflightFailureAfterIsolationPreservesWorkspaceDetails
// is the regression test for a real P2 finding from a third GitHub Codex
// App review round: when an isolated run's PreflightActivity failed after
// PrepareIsolatedWorkspaceActivity already succeeded, the workflow's own
// deferred rollback correctly discarded the worktree/branch (this is a
// normal in-process error return, not a hard termination — the defer
// genuinely runs), but the returned error used a plain fmt.Errorf %w wrap
// instead of wrapActivityFailure, so a caller had no way to recover the
// worktree path/branch that were actually rolled back — durably
// recording the shared checkout as this run's WorkspacePath instead of
// the isolated one really used and discarded.
func TestRunWorkflowPreflightFailureAfterIsolationPreservesWorkspaceDetails(t *testing.T) {
	const fixtureWorktreePath = "/fixture/data/workspaces/fixture-run"
	const fixtureBranch = "factoryd/fixture-run"
	buildCalled := false
	var rollbackCalled bool
	var mu sync.Mutex

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(
		func(context.Context, CaptureBaseSHAInput) (string, error) { return "fixture-base-sha", nil },
		activity.RegisterOptions{Name: CaptureBaseSHAActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
			return PrepareIsolatedWorkspaceResult{WorktreePath: fixtureWorktreePath, Branch: fixtureBranch}, nil
		},
		activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RollbackIsolatedWorkspaceInput) error {
			mu.Lock()
			rollbackCalled = true
			mu.Unlock()
			return nil
		},
		activity.RegisterOptions{Name: RollbackIsolatedWorkspaceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PreflightInput) error {
			return temporal.NewApplicationError("required file already dirty", PreflightFailureType)
		},
		activity.RegisterOptions{Name: PreflightActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			buildCalled = true
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunBuildActivityName},
	)
	env.SetTestTimeout(5 * time.Second)

	input := fixtureInput()
	input.IsolateWorkspace = true
	input.IsolatedRepoDir = "/fixture/repo"
	input.IsolatedParentDir = "/fixture/data/workspaces"

	env.ExecuteWorkflow(RunWorkflow, input)
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow error = nil, want preflight failure")
	}
	if !hasApplicationErrorType(err, PreflightFailureType) {
		t.Fatalf("workflow error does not preserve application error type %q: %v", PreflightFailureType, err)
	}
	if buildCalled {
		t.Fatal("build Activity ran after a preflight failure")
	}
	mu.Lock()
	called := rollbackCalled
	mu.Unlock()
	if !called {
		t.Fatal("RollbackIsolatedWorkspaceActivity was never called — the deferred rollback did not run")
	}

	wp, br := IsolatedWorkspaceFromError(err)
	if wp != fixtureWorktreePath || br != fixtureBranch {
		t.Fatalf("IsolatedWorkspaceFromError(err) = (%q, %q), want (%q, %q) — the rolled-back worktree/branch must survive on the returned error's Details", wp, br, fixtureWorktreePath, fixtureBranch)
	}
}

// TestRunWorkflowRejectsInvalidIsolatedPredecessorBeforePrepare proves the
// Workflow's own read-only predecessor validation is authoritative even when
// a caller bypasses cmd/factoryd and submits RunWorkflowInput directly.
func TestRunWorkflowRejectsInvalidIsolatedPredecessorBeforePrepare(t *testing.T) {
	tests := []struct {
		name           string
		priorState     run.State
		priorProject   string
		priorResultSHA string
	}{
		{name: "nonaccepted", priorState: run.StateHalted},
		{name: "mismatched project", priorState: run.StateAccepted, priorProject: "/other/project"},
		{name: "unknown result", priorState: run.StateAccepted, priorResultSHA: "0000000000000000000000000000000000000000"},
		{name: "missing result", priorState: run.StateAccepted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			workspacePath := testfixture.NewGitRepo(t)
			resultSHA, err := runner.GitRevParseHEAD(workspacePath)
			if err != nil {
				t.Fatalf("capture fixture HEAD: %v", err)
			}
			priorProject := workspacePath
			if tc.priorProject != "" {
				priorProject = tc.priorProject
			}
			priorResultSHA := resultSHA
			if tc.name == "missing result" {
				priorResultSHA = ""
			} else if tc.priorResultSHA != "" {
				priorResultSHA = tc.priorResultSHA
			}
			prepareCalled := false
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterActivityWithOptions((&Activities{}).CaptureBaseSHAActivity, activity.RegisterOptions{Name: CaptureBaseSHAActivityName})
			env.RegisterActivityWithOptions(func(context.Context, PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
				prepareCalled = true
				return PrepareIsolatedWorkspaceResult{}, nil
			}, activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName})
			env.SetTestTimeout(5 * time.Second)
			input := fixtureInput()
			input.WorkspacePath = workspacePath
			input.IsolateWorkspace = true
			input.IsolatedRepoDir = workspacePath
			input.IsolatedParentDir = t.TempDir()
			input.PriorRunID = "prior-run"
			input.PriorRunState = tc.priorState
			input.PriorRunProjectPath = priorProject
			input.PriorRunResultSHA = priorResultSHA

			env.ExecuteWorkflow(RunWorkflow, input)
			if err := env.GetWorkflowError(); err == nil {
				t.Fatal("workflow error = nil, want predecessor validation failure")
			}
			if prepareCalled {
				t.Fatal("PrepareIsolatedWorkspaceActivity ran after invalid predecessor validation")
			}
		})
	}
}

// TestRunWorkflowRejectsSupersededIsolatedPriorRun is the Temporal-path
// regression test for a real gap (found via review, 2026-09-02): an
// isolated chained run starts its own worktree directly from the declared
// predecessor's recorded ResultSHA, so CaptureBaseSHAActivity/
// ValidateSliceChainActivity's baseSHA comparison ends up comparing
// prior.ResultSHA against itself and can never fail — exactly the case
// run.FindChainSuccessor exists to catch (see its own doc comment). Without CheckChainSuccessorActivity porting that same
// check to RunWorkflow, a -prior-run already superseded by a later
// accepted slice would be silently accepted instead of rejected on this
// path, discarding the superseding slice's work with no error at all.
func TestRunWorkflowRejectsSupersededIsolatedPriorRun(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	resultSHA, err := runner.GitRevParseHEAD(workspacePath)
	if err != nil {
		t.Fatalf("capture fixture HEAD: %v", err)
	}
	prepareCalled := false
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions((&Activities{}).CaptureBaseSHAActivity, activity.RegisterOptions{Name: CaptureBaseSHAActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, input CheckChainSuccessorInput) error {
		if input.PriorRunID != "prior-run" || input.ProjectPath != workspacePath {
			t.Errorf("CheckChainSuccessorActivity input = %+v, want prior-run/%s", input, workspacePath)
		}
		return temporal.NewApplicationError("prior run already superseded by a later accepted slice", SliceChainFailureType)
	}, activity.RegisterOptions{Name: CheckChainSuccessorActivityName})
	env.RegisterActivityWithOptions(func(context.Context, PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
		prepareCalled = true
		return PrepareIsolatedWorkspaceResult{}, nil
	}, activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName})
	env.SetTestTimeout(5 * time.Second)

	input := fixtureInput()
	input.WorkspacePath = workspacePath
	input.IsolateWorkspace = true
	input.IsolatedRepoDir = workspacePath
	input.IsolatedParentDir = t.TempDir()
	input.PriorRunID = "prior-run"
	input.PriorRunState = run.StateAccepted
	input.PriorRunProjectPath = workspacePath
	input.PriorRunResultSHA = resultSHA

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("workflow error = nil, want a superseded-predecessor rejection")
	}
	if prepareCalled {
		t.Fatal("PrepareIsolatedWorkspaceActivity ran after a superseded predecessor was found")
	}
}

// TestRunWorkflowRejectsSupersededPriorRunOnNonIsolatedPath is the
// regression test for a real P1 finding from a GitHub Codex App review
// (2026-09-03) on this branch's own PR: CheckChainSuccessorActivity was
// gated on isolatePriorResult (this run being the isolated one), leaving a
// mixed chain unprotected -- accepted run A advances the shared checkout;
// isolated run B declares A as its prior and supersedes it without moving
// that checkout; a later run C (isolated or not) that also declares A as
// its prior then captures a baseSHA that still equals A's ResultSHA (the
// shared HEAD never moved), so ValidateSliceChainActivity's own comparison
// passes untouched even though B has already superseded A -- silently
// reopening the chain and discarding B's accepted work. Gating on
// PriorRunID alone (matching realMain's own unconditional placement of
// run.FindChainSuccessor) closes this for every declared chain, isolated
// or not.
func TestRunWorkflowRejectsSupersededPriorRunOnNonIsolatedPath(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	buildCalled := false
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	// The non-isolated path exists only for a history recorded before
	// RunWorkflow required isolation, so replay as one.
	env.OnGetVersion(requireIsolatedWorkspaceChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	env.RegisterActivityWithOptions((&Activities{}).CaptureBaseSHAActivity, activity.RegisterOptions{Name: CaptureBaseSHAActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, input CheckChainSuccessorInput) error {
		if input.PriorRunID != "prior-run" || input.ProjectPath != workspacePath {
			t.Errorf("CheckChainSuccessorActivity input = %+v, want prior-run/%s", input, workspacePath)
		}
		return temporal.NewApplicationError("prior run already superseded by a later accepted slice", SliceChainFailureType)
	}, activity.RegisterOptions{Name: CheckChainSuccessorActivityName})
	// Registered as an always-succeeds stub, not omitted: an omitted
	// registration would itself fail the workflow with an "activity not
	// found" error, which would make this test pass for the wrong reason
	// regardless of whether CheckChainSuccessorActivity's own gating is
	// correct — the point of this test is that CheckChainSuccessorActivity
	// must be reached and must be what halts the workflow, not merely that
	// something halts it.
	env.RegisterActivityWithOptions(func(context.Context, ValidateSliceChainInput) error {
		return nil
	}, activity.RegisterOptions{Name: ValidateSliceChainActivityName})
	env.RegisterActivityWithOptions(func(context.Context, PreflightInput) error {
		return nil
	}, activity.RegisterOptions{Name: PreflightActivityName})
	env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
		buildCalled = true
		return BuildActivityResult{}, nil
	}, activity.RegisterOptions{Name: RunBuildActivityName})
	env.SetTestTimeout(5 * time.Second)

	input := fixtureInput()
	input.WorkspacePath = workspacePath
	// The scenario's whole point: this run is NOT isolated, so
	// isolatePriorResult is false and the old isolatePriorResult-gated
	// call would never have fired.
	input.IsolateWorkspace = false
	input.PriorRunID = "prior-run"
	input.PriorRunState = run.StateAccepted
	input.PriorRunProjectPath = workspacePath
	input.PriorRunResultSHA = "0000000000000000000000000000000000000000"

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("workflow error = nil, want a superseded-predecessor rejection")
	}
	if buildCalled {
		t.Fatal("RunBuildActivity ran after a superseded predecessor was found")
	}
}

func TestRunWorkflowGateFailureQuarantines(t *testing.T) {
	input := fixtureInput()
	build := BuildActivityResult{Result: runner.Result{ExitCode: 0}}
	verify := VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 1}}
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) { return build, nil },
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) { return verify, nil },
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: false, ExitCode: 1}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", result.State, run.StateQuarantined)
	}
}

// TestRunWorkflowPreservesAttemptsOnBuildFailure is the regression test
// for a real P1 finding from review: Temporal never delivers an
// Activity's return value to its caller alongside a non-nil error, so a
// failed build's own attempts were silently lost even though
// RunBuildActivity had already collected them — exactly the failure path
// where this evidence matters most. RunBuildActivity attaches its
// collected attempts to its error's Details (see its doc comment);
// AttemptsFromError must recover them from RunWorkflow's own returned
// error.
func TestRunWorkflowPreservesAttemptsOnBuildFailure(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
			return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("could not start subprocess", InfrastructureFailureType, attempts)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow error = nil, want infrastructure failure")
	}
	if !hasApplicationErrorType(err, InfrastructureFailureType) {
		t.Fatalf("workflow error does not preserve application error type %q: %v", InfrastructureFailureType, err)
	}
	attempts := AttemptsFromError(err)
	if len(attempts) != 1 || attempts[0].Kind != "build" || attempts[0].ExitCode != -1 {
		t.Fatalf("AttemptsFromError(workflow error) = %+v, want the build Activity's own collected attempt", attempts)
	}
}

// TestRunWorkflowPreservesAttemptsOnPostBuildFailure is
// TestRunWorkflowPreservesAttemptsOnBuildFailure's counterpart for a
// later Activity failing after the build already succeeded and collected
// attempts — PostBuildActivity here has no attempts of its own, so the
// only source is result.Attempts accumulated from the successful build,
// passed through wrapActivityFailure's priorAttempts.
func TestRunWorkflowPreservesAttemptsOnPostBuildFailure(t *testing.T) {
	buildAttempts := []run.Attempt{{Kind: "build", ExitCode: 0}}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	registerIsolationActivities(env)
	env.RegisterActivityWithOptions(
		func(context.Context, CaptureBaseSHAInput) (string, error) { return "fixture-base-sha", nil },
		activity.RegisterOptions{Name: CaptureBaseSHAActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PreflightInput) error { return nil },
		activity.RegisterOptions{Name: PreflightActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}, Attempts: buildAttempts}, nil
		},
		activity.RegisterOptions{Name: RunBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PostBuildInput) (PostBuildResult, error) {
			return PostBuildResult{}, temporal.NewApplicationError("workspace was rolled back", InfrastructureFailureType)
		},
		activity.RegisterOptions{Name: PostBuildActivityName},
	)
	env.SetTestTimeout(5 * time.Second)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow error = nil, want post-build activity failure")
	}
	attempts := AttemptsFromError(err)
	if len(attempts) != 1 || attempts[0].Kind != "build" {
		t.Fatalf("AttemptsFromError(workflow error) = %+v, want the build Activity's own already-accumulated attempt", attempts)
	}
}

func TestRunWorkflowInfrastructureFailureIsDistinctFromGateFailure(t *testing.T) {
	verifyCalled := false
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{}, temporal.NewApplicationError("could not start subprocess", InfrastructureFailureType)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			verifyCalled = true
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow error = nil, want infrastructure failure")
	}
	if !hasApplicationErrorType(err, InfrastructureFailureType) {
		t.Fatalf("workflow error does not preserve application error type %q: %v", InfrastructureFailureType, err)
	}
	if verifyCalled {
		t.Fatal("verify Activity ran after build infrastructure failure")
	}
}

func hasApplicationErrorType(err error, want string) bool {
	for err != nil {
		var applicationErr *temporal.ApplicationError
		if errors.As(err, &applicationErr) && applicationErr.Type() == want {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func newWorkflowEnvironment(
	t *testing.T,
	build func(context.Context, RunWorkflowInput) (BuildActivityResult, error),
	verify func(context.Context, RunWorkflowInput) (VerifyActivityResult, error),
	gate func(context.Context, EvaluateGateInput) (run.GateResult, error),
) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnvironmentSansCommitOracles(t, build, verify, gate)
	// Default no-op mock: no fixture below has an oracle manifest with a
	// target_path, so the Activity reports Active=false and the workflow keeps
	// CollectEvidenceActivity's values, as before it existed.
	env.RegisterActivityWithOptions(
		func(context.Context, CommitOraclesInput) (CommittedOracles, error) { return CommittedOracles{}, nil },
		activity.RegisterOptions{Name: CommitOraclesActivityName},
	)
	return env
}

// newWorkflowEnvironmentSansCommitOracles is newWorkflowEnvironment without the
// default CommitOraclesActivity mock, for tests that register their own.
func newWorkflowEnvironmentSansCommitOracles(
	t *testing.T,
	build func(context.Context, RunWorkflowInput) (BuildActivityResult, error),
	verify func(context.Context, RunWorkflowInput) (VerifyActivityResult, error),
	gate func(context.Context, EvaluateGateInput) (run.GateResult, error),
) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(
		func(context.Context, CaptureBaseSHAInput) (string, error) { return "fixture-base-sha", nil },
		activity.RegisterOptions{Name: CaptureBaseSHAActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PreflightInput) error { return nil },
		activity.RegisterOptions{Name: PreflightActivityName},
	)
	env.RegisterActivityWithOptions(build, activity.RegisterOptions{Name: RunBuildActivityName})
	env.RegisterActivityWithOptions(verify, activity.RegisterOptions{Name: RunVerifyActivityName})
	env.RegisterActivityWithOptions(gate, activity.RegisterOptions{Name: EvaluateGateActivityName})
	env.RegisterActivityWithOptions(
		func(_ context.Context, input EvaluateRunInput) (policy.EvaluateRunResult, error) {
			return policy.EvaluateRun(input.Policy), nil
		},
		activity.RegisterOptions{Name: EvaluateRunActivityName},
	)
	// Default no-op mocks: no test below declares Allowed-Files/Required-*
	// keys or exercises an ancestry violation, so a fixed successful
	// PostBuild and an empty CollectedEvidence (skipping every gate
	// EvaluateRun would otherwise run beyond canonical_verify) keep every
	// existing test's behavior identical to before these Activities
	// existed. A test that needs to exercise them registers its own
	// env directly instead of using this shared helper (see
	// TestRepositoryOwnerWorkflowPropagatesCancellation for the pattern).
	env.RegisterActivityWithOptions(
		func(context.Context, PostBuildInput) (PostBuildResult, error) {
			return PostBuildResult{ResultSHA: "fixture-result-sha"}, nil
		},
		activity.RegisterOptions{Name: PostBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
			return CollectedEvidence{}, nil
		},
		activity.RegisterOptions{Name: CollectEvidenceActivityName},
	)
	registerIsolationActivities(env)
	env.SetTestTimeout(5 * time.Second)
	return env
}

// registerIsolationActivities registers the prepare, rollback and
// disable-group-write Activities an isolated fixtureInput run calls, with
// the worktree path the fixtures already build in.
func registerIsolationActivities(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(
		func(context.Context, PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
			return PrepareIsolatedWorkspaceResult{WorktreePath: "/fixture/workspace", Branch: "factoryd/fixture"}, nil
		},
		activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RollbackIsolatedWorkspaceInput) error { return nil },
		activity.RegisterOptions{Name: RollbackIsolatedWorkspaceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, DisableWorkerGroupWriteInput) error { return nil },
		activity.RegisterOptions{Name: DisableWorkerGroupWriteActivityName},
	)
}

func fixtureInput() RunWorkflowInput {
	return fixtureInputForTicket("fixture-ticket")
}

func fixtureInputForTicket(ticket string) RunWorkflowInput {
	return RunWorkflowInput{
		Ticket:        ticket,
		WorkspacePath: "/fixture/workspace",
		SpecPath:      "/fixture/spec.md",
		BaseSHA:       "base-sha",
		// RunWorkflow refuses a non-isolated run: every fixture is isolated
		// and its environment registers the prepare/rollback/disable mocks
		// (registerIsolationActivities).
		IsolateWorkspace:  true,
		IsolatedRepoDir:   "/fixture/repo",
		IsolatedParentDir: "/fixture/data/workspaces",
		// tests_added always runs; these fixtures exercise
		// unrelated behavior and declare no ChangedFiles matching a test
		// pattern, so opt out rather than have every one of them fail a
		// gate none of them are testing.
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	}
}

// setGateCommand sets input.GateCommands[id] = command, initializing the
// map on first use -- GateCommands (keyed by policy.CommandGate.ID)
// replaced RunWorkflowInput's old individual LintCommand/SecurityCommand/
// UnitTestCommand/IntegrationTestCommand/ReferenceOracleCommand fields;
// this is the direct-assignment equivalent the many fixtures across this
// package's tests used those fields for.
func setGateCommand(input *RunWorkflowInput, id, command string) {
	if input.GateCommands == nil {
		input.GateCommands = map[string]string{}
	}
	input.GateCommands[id] = command
}

// TestRunWorkflowIsolatedRunIDIsLegible proves RunWorkflow schedules
// PrepareIsolatedWorkspaceActivity with the human-legible slug-shorthash
// RunID (isolatedWorkspaceRunID), not a bare hash.
func TestRunWorkflowIsolatedRunIDIsLegible(t *testing.T) {
	var captured PrepareIsolatedWorkspaceInput
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(context.Context, CaptureBaseSHAInput) (string, error) {
		return "fixture-base-sha", nil
	}, activity.RegisterOptions{Name: CaptureBaseSHAActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, input PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
		captured = input
		return PrepareIsolatedWorkspaceResult{}, errors.New("stop after prepare payload")
	}, activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName})
	env.SetTestTimeout(5 * time.Second)

	input := fixtureInput()
	input.RunID = "add-mood-worstweekday-001"
	input.IsolateWorkspace = true
	input.IsolatedRepoDir = "/fixture/repo"
	input.IsolatedParentDir = "/fixture/parent"
	env.ExecuteWorkflow(RunWorkflow, input)

	if !strings.HasPrefix(captured.RunID, "add-mood-worstweekday-001-") {
		t.Fatalf("RunID = %q, want the legible slug-shorthash form", captured.RunID)
	}
}

// pinNoActivityRetries replays RunWorkflow as a run started before the
// activity-retries version: one attempt per Activity, as every new execution
// also runs. For tests that name that history explicitly.
func pinNoActivityRetries(env *testsuite.TestWorkflowEnvironment) {
	env.OnGetVersion(activityRetriesChange, temporalworkflow.DefaultVersion, 2).Return(temporalworkflow.DefaultVersion)
}

// pinActivityRetriesVersion1 replays RunWorkflow as a run started under
// activity-retries version 1, which retries the Activities named in
// retriedActivityNames once: the history of a workflow still in flight when
// new executions stopped retrying.
func pinActivityRetriesVersion1(env *testsuite.TestWorkflowEnvironment) {
	env.OnGetVersion(activityRetriesChange, temporalworkflow.DefaultVersion, 2).Return(temporalworkflow.Version(activityRetriesOn))
}
