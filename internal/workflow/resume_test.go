package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

// isolatedLostBuildEnv is an isolated RunWorkflow whose build Activity
// returns buildErr. rollbacks counts RollbackIsolatedWorkspaceActivity calls
// and disables counts DisableWorkerGroupWriteActivity calls.
func isolatedLostBuildEnv(t *testing.T, buildErr error, gatePasses bool) (env *testsuite.TestWorkflowEnvironment, rollbacks, disables *int) {
	t.Helper()
	return isolatedEnvWithPrepare(t, buildErr, gatePasses, func() (PrepareIsolatedWorkspaceResult, error) {
		return PrepareIsolatedWorkspaceResult{WorktreePath: "/fixture/data/workspaces/wt", Branch: "factoryd/wt"}, nil
	})
}

// isolatedEnvWithPrepare is isolatedLostBuildEnv with the prepare Activity's
// outcome chosen by the test.
func isolatedEnvWithPrepare(t *testing.T, buildErr error, gatePasses bool, prepare func() (PrepareIsolatedWorkspaceResult, error)) (env *testsuite.TestWorkflowEnvironment, rollbacks, disables *int) {
	t.Helper()
	rollbacks, disables = new(int), new(int)
	var mu sync.Mutex
	env = newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			if buildErr != nil {
				return BuildActivityResult{}, buildErr
			}
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			exit := 0
			if !gatePasses {
				exit = 1
			}
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: exit}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: gatePasses, ExitCode: map[bool]int{true: 0, false: 1}[gatePasses]}, nil
		},
	)
	env.RegisterActivityWithOptions(func(context.Context, PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
		return prepare()
	}, activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, RollbackIsolatedWorkspaceInput) error {
		mu.Lock()
		defer mu.Unlock()
		*rollbacks++
		return nil
	}, activity.RegisterOptions{Name: RollbackIsolatedWorkspaceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, DisableWorkerGroupWriteInput) error {
		mu.Lock()
		defer mu.Unlock()
		*disables++
		return nil
	}, activity.RegisterOptions{Name: DisableWorkerGroupWriteActivityName})
	return env, rollbacks, disables
}

func isolatedFixtureInput() RunWorkflowInput {
	input := fixtureInput()
	input.IsolateWorkspace = true
	input.IsolatedRepoDir = "/fixture/repo"
	input.IsolatedParentDir = "/fixture/data/workspaces"
	return input
}

func lostBuildError() error {
	return temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)
}

func TestRunWorkflowKeepsTheWorktreeWhenTheBuildActivityIsLost(t *testing.T) {
	env, rollbacks, _ := isolatedLostBuildEnv(t, lostBuildError(), true)
	pinNoActivityRetries(env)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow succeeded with a lost build")
	}
	if !StepLostFromError(err) {
		t.Fatalf("StepLostFromError(%v) = false, want the lost build visible on the returned error", err)
	}
	if *rollbacks != 0 {
		t.Errorf("rollback ran %d time(s) for a lost build, want the worktree kept", *rollbacks)
	}
	if wp, br := IsolatedWorkspaceFromError(err); wp != "/fixture/data/workspaces/wt" || br != "factoryd/wt" {
		t.Errorf("IsolatedWorkspaceFromError = (%q, %q); the caller needs the kept worktree on the error", wp, br)
	}
}

func TestRunWorkflowStillRollsBackAnActivityThatFailedAfterRunning(t *testing.T) {
	env, rollbacks, _ := isolatedLostBuildEnv(t, temporal.NewApplicationError("build exited 3", InfrastructureFailureType), true)
	pinNoActivityRetries(env)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow succeeded after a failed build")
	}
	if StepLostFromError(err) {
		t.Fatalf("StepLostFromError(%v) = true for an Activity that ran to an error", err)
	}
	if *rollbacks != 1 {
		t.Errorf("rollback ran %d time(s), want 1", *rollbacks)
	}
}

func TestRunWorkflowQuarantineKeepsItsWorktreeAndAcceptedRunDoesNotRollBack(t *testing.T) {
	env, rollbacks, disables := isolatedLostBuildEnv(t, nil, false)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %s, want quarantined for a failed gate", result.State)
	}
	if *rollbacks != 0 || *disables != 1 {
		t.Errorf("rollbacks = %d, group-write revokes = %d; want 0 and 1 (a quarantined worktree is kept for an override, unchanged)", *rollbacks, *disables)
	}
}

// TestRunWorkflowReplayedFromBeforeKeepWorktreeWhenLostRollsBack pins the
// version to DefaultVersion, as a history recorded before the change replays:
// the lost build then rolls back exactly as it did.
func TestRunWorkflowReplayedFromBeforeKeepWorktreeWhenLostRollsBack(t *testing.T) {
	env, rollbacks, _ := isolatedLostBuildEnv(t, lostBuildError(), true)
	pinNoActivityRetries(env)
	env.OnGetVersion(keepWorktreeWhenLostChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	if env.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded with a lost build")
	}
	if *rollbacks != 1 {
		t.Errorf("rollback ran %d time(s) under DefaultVersion, want the old behaviour (1)", *rollbacks)
	}
}

func TestRunWorkflowPassesResumeFromAndUsesItsBaseSHA(t *testing.T) {
	var gotPrepare PrepareIsolatedWorkspaceInput
	var gotBuild RunWorkflowInput
	env := newWorkflowEnvironment(t,
		func(_ context.Context, in RunWorkflowInput) (BuildActivityResult, error) {
			gotBuild = in
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	env.RegisterActivityWithOptions(func(_ context.Context, in PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
		gotPrepare = in
		return PrepareIsolatedWorkspaceResult{WorktreePath: in.Resume.WorktreePath, Branch: in.Resume.Branch}, nil
	}, activity.RegisterOptions{Name: PrepareIsolatedWorkspaceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, RollbackIsolatedWorkspaceInput) error { return nil },
		activity.RegisterOptions{Name: RollbackIsolatedWorkspaceActivityName})
	env.RegisterActivityWithOptions(func(context.Context, DisableWorkerGroupWriteInput) error { return nil },
		activity.RegisterOptions{Name: DisableWorkerGroupWriteActivityName})

	input := isolatedFixtureInput()
	input.ResumeFrom = &ResumeFrom{RunID: "halted", WorktreePath: "/fixture/data/workspaces/old", Branch: "factoryd/old", BaseSHA: "original-base"}
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	if gotPrepare.Resume == nil || gotPrepare.Resume.RunID != "halted" {
		t.Errorf("Prepare input Resume = %+v, want the halted run", gotPrepare.Resume)
	}
	if gotBuild.WorkspacePath != "/fixture/data/workspaces/old" {
		t.Errorf("build workspace = %q, want the adopted worktree", gotBuild.WorkspacePath)
	}
	if gotBuild.BaseSHA != "original-base" {
		t.Errorf("build BaseSHA = %q, want the halted run's base (not the shared checkout's HEAD at resume)", gotBuild.BaseSHA)
	}
	if gotBuild.ResumeFrom == nil {
		t.Error("build input lost ResumeFrom")
	}
}

// preparedHaltedRun runs PrepareIsolatedWorkspaceActivity once for runID
// (the halted run) and records the halted, kept run, as a reclaimed lost
// build leaves it. It returns the data dir, repo, base and prepared result.
func preparedHaltedRun(t *testing.T, haltedID string) (dataDir, repoDir, base string, prepared PrepareIsolatedWorkspaceResult) {
	t.Helper()
	repoDir = testfixture.NewGitRepo(t)
	var err error
	base, err = runner.GitRevParseHEAD(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	dataDir = t.TempDir()
	input := PrepareIsolatedWorkspaceInput{
		RepoDir: repoDir, ParentDir: filepath.Join(dataDir, "workspaces"), RunID: "old-worktree",
		BaseSHA: base, DataDir: dataDir, DurableRunID: haltedID, WorkflowID: "wf-old",
	}
	activities := &Activities{CheckpointDir: t.TempDir()}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.PrepareIsolatedWorkspaceActivity)
	encoded, err := env.ExecuteActivity(activities.PrepareIsolatedWorkspaceActivity, input)
	if err != nil {
		t.Fatalf("prepare the halted run's worktree: %v", err)
	}
	if err := encoded.Get(&prepared); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wsisolation.Remove(repoDir, prepared.WorktreePath, prepared.Branch) })
	halted := &run.Run{
		ID: haltedID, State: run.StateHalted, HaltConfirmed: true, KeptForResume: true,
		ProjectPath: repoDir, WorkspacePath: prepared.WorktreePath, Branch: prepared.Branch, BaseSHA: base,
	}
	if err := halted.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return dataDir, repoDir, base, prepared
}

func noContainersDocker(t *testing.T) string {
	t.Helper()
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return docker
}

func resumeInput(repoDir, dataDir, base string, prepared PrepareIsolatedWorkspaceResult) PrepareIsolatedWorkspaceInput {
	return PrepareIsolatedWorkspaceInput{
		RepoDir: repoDir, ParentDir: filepath.Join(dataDir, "workspaces"), RunID: "new-worktree-id",
		BaseSHA: base, DataDir: dataDir, DurableRunID: "resumed-run", WorkflowID: "wf-new",
		Resume: &ResumeFrom{RunID: "halted-run", WorktreePath: prepared.WorktreePath, Branch: prepared.Branch, BaseSHA: base},
	}
}

func TestPrepareIsolatedWorkspaceActivityAdoptsTheKeptWorktree(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	activities := &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.PrepareIsolatedWorkspaceActivity)
	encoded, err := env.ExecuteActivity(activities.PrepareIsolatedWorkspaceActivity, resumeInput(repoDir, dataDir, base, prepared))
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	var adopted PrepareIsolatedWorkspaceResult
	if err := encoded.Get(&adopted); err != nil {
		t.Fatal(err)
	}
	if adopted != prepared {
		t.Errorf("adopted = %+v, want the halted run's own worktree %+v", adopted, prepared)
	}
	marker, err := wsisolation.LoadIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, "resumed-run"))
	if err != nil {
		t.Fatalf("new run's marker: %v", err)
	}
	if marker.RunID != "resumed-run" || marker.WorktreePath != prepared.WorktreePath || marker.Branch != prepared.Branch || !marker.Prepared || marker.WorkflowID != "wf-new" || marker.Mode != "temporal" {
		t.Errorf("marker = %+v, want it moved to the resumed run over the same worktree", marker)
	}
	if err := wsisolation.ValidateIsolationMarker(marker, dataDir, repoDir); err != nil {
		t.Errorf("moved marker is not valid for reconcile: %v", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "halted-run")); !os.IsNotExist(err) {
		t.Errorf("halted run's marker still exists (stat err = %v)", err)
	}
	if halted, _ := run.Load(dataDir, "halted-run"); halted.KeptForResume {
		t.Error("halted run is still KeptForResume after adoption")
	}
	if _, err := os.Stat(prepared.WorktreePath); err != nil {
		t.Errorf("adopted worktree is gone: %v", err)
	}
}

func TestPrepareIsolatedWorkspaceActivityRefusesToAdoptWithoutDeletingTheWorktree(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	docker := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nif [ \"$1\" = ps ]; then echo deadbeef; fi\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{CheckpointDir: t.TempDir(), SandboxDocker: docker}
	wrapper := func(ctx context.Context, in PrepareIsolatedWorkspaceInput) error {
		_, err := activities.PrepareIsolatedWorkspaceActivity(ctx, in)
		if err == nil {
			return errors.New("adoption succeeded with a live container")
		}
		if !strings.Contains(err.Error(), "container") {
			return err
		}
		if wp, br := IsolatedWorkspaceFromError(err); wp != "" || br != "" {
			return errors.New("refusal attached worktree details: the workflow would roll the worktree back")
		}
		return nil
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, resumeInput(repoDir, dataDir, base, prepared)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(prepared.WorktreePath); err != nil {
		t.Errorf("worktree gone after a refused adoption: %v", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "halted-run")); err != nil {
		t.Errorf("halted run's marker gone after a refused adoption: %v", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "resumed-run")); !os.IsNotExist(err) {
		t.Errorf("a marker was written for the resumed run despite the refusal (stat err = %v)", err)
	}
}

func TestPrepareIsolatedWorkspaceActivityRefusesAResumeInputThatDisagreesWithTheMarker(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	in := resumeInput(repoDir, dataDir, base, prepared)
	in.Resume.Branch = "factoryd/someone-else"
	activities := &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}
	wrapper := func(ctx context.Context, in PrepareIsolatedWorkspaceInput) error {
		_, err := activities.PrepareIsolatedWorkspaceActivity(ctx, in)
		if err == nil || !strings.Contains(err.Error(), "does not match") {
			return errors.New("adoption did not refuse a mismatched resume input")
		}
		return nil
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, in); err != nil {
		t.Fatal(err)
	}
}

func runBuildArgs(t *testing.T, repo string, input RunWorkflowInput) []string {
	t.Helper()
	var got []string
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, args ...string) (runner.Result, error) {
			got = args
			return runner.Result{}, nil
		},
	}
	input.WorkspacePath = repo
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
		t.Fatal(err)
	}
	return got
}

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

func TestRunBuildActivityOfAResumedRunPassesTheHandoffAndRoundState(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	roundState := filepath.Join(repo, RoundStateFileName)
	writeFile(t, roundState, `{"version":1,"last_completed_round":1}`)
	writeFile(t, filepath.Join(repo, buildSessionDir, "s.jsonl"), "old session\n")
	writeFile(t, filepath.Join(repo, "partial.go"), "package partial\n")
	input := fixtureInput()
	input.BaseSHA = gitOut(t, repo, "rev-parse", "HEAD")
	input.RunID = "resumed-run"
	input.ResumeFrom = &ResumeFrom{RunID: "halted-run", BaseSHA: input.BaseSHA}

	args := runBuildArgs(t, repo, input)

	if got, ok := argValue(args, "--resume-from-state"); !ok || got != roundState {
		t.Errorf("--resume-from-state = %q (present %v), want %s; argv %v", got, ok, roundState, args)
	}
	note, ok := argValue(args, "--handoff")
	if !ok {
		t.Fatalf("a resumed run's first attempt passed no --handoff: %v", args)
	}
	b, err := os.ReadFile(note)
	if err != nil || !strings.Contains(string(b), "halted-run") || !strings.Contains(string(b), "partial.go") {
		t.Errorf("handoff note = %q (err %v), want it to name the halted run and the kept file", b, err)
	}
	if _, err := os.Stat(roundState); err != nil {
		t.Errorf("the handoff deleted the round-state file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, buildSessionDir)); !os.IsNotExist(err) {
		t.Errorf("the dead build's harness session survived the handoff (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "partial.go")); err != nil {
		t.Errorf("the kept work was lost: %v", err)
	}
}

func TestRunBuildActivityOfAResumedRunWithoutRoundStateOmitsResumeFromState(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	input := fixtureInput()
	input.BaseSHA = gitOut(t, repo, "rev-parse", "HEAD")
	input.RunID = "resumed-run"
	input.ResumeFrom = &ResumeFrom{RunID: "halted-run", BaseSHA: input.BaseSHA}

	args := runBuildArgs(t, repo, input)

	if _, ok := argValue(args, "--resume-from-state"); ok {
		t.Errorf("--resume-from-state passed with no round-state file: %v", args)
	}
	if _, ok := argValue(args, "--handoff"); !ok {
		t.Errorf("a resume still runs the handoff when there is no round state: %v", args)
	}
}

func TestRunBuildActivityWithoutResumeFromIgnoresRoundState(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	writeFile(t, filepath.Join(repo, RoundStateFileName), `{"version":1,"last_completed_round":1}`)
	args := runBuildArgs(t, repo, fixtureInput())
	if _, ok := argValue(args, "--resume-from-state"); ok {
		t.Errorf("an ordinary run passed --resume-from-state: %v", args)
	}
}
