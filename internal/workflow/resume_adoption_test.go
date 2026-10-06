package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/run"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

func saveRunRecord(t *testing.T, dataDir string, r *run.Run) {
	t.Helper()
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// runPrepare executes PrepareIsolatedWorkspaceActivity as an Activity and
// returns its result and error.
func runPrepare(t *testing.T, a *Activities, in PrepareIsolatedWorkspaceInput) (PrepareIsolatedWorkspaceResult, error) {
	t.Helper()
	var result PrepareIsolatedWorkspaceResult
	var gotErr error
	wrapper := func(ctx context.Context, in PrepareIsolatedWorkspaceInput) error {
		result, gotErr = a.PrepareIsolatedWorkspaceActivity(ctx, in)
		return nil
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, in); err != nil {
		t.Fatal(err)
	}
	return result, gotErr
}

// R: a resumed run never rolls back its worktree on a non-accepted end.

func TestResumedRunKeepsItsWorktreeWhenTheBuildFails(t *testing.T) {
	env, rollbacks, disables := isolatedLostBuildEnv(t, temporal.NewApplicationError("build exited 3", InfrastructureFailureType), true)
	pinNoActivityRetries(env)
	input := isolatedFixtureInput()
	input.ResumeFrom = &ResumeFrom{RunID: "halted", WorktreePath: "/fixture/data/workspaces/wt", Branch: "factoryd/wt"}
	env.ExecuteWorkflow(RunWorkflow, input)
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow succeeded after a failed build")
	}
	if *rollbacks != 0 {
		t.Errorf("a resumed run rolled back its adopted worktree %d time(s)", *rollbacks)
	}
	if *disables != 1 {
		t.Errorf("group-write revokes = %d, want 1 on a kept worktree", *disables)
	}
	if wp, br := IsolatedWorkspaceFromError(err); wp != "/fixture/data/workspaces/wt" || br != "factoryd/wt" {
		t.Errorf("IsolatedWorkspaceFromError = (%q, %q); the caller needs the kept worktree to flag the new run", wp, br)
	}
}

func TestResumedRunKeepsTheWorktreeWhenAdoptionFailsAfterTheMarkerMoved(t *testing.T) {
	prepare := func() (PrepareIsolatedWorkspaceResult, error) {
		cause := errors.New("stale evidence cleanup failed")
		return PrepareIsolatedWorkspaceResult{}, attachIsolatedWorkspaceDetail(cause.Error(), IsolationFailureType, cause, "/fixture/data/workspaces/wt", "factoryd/wt")
	}
	env, rollbacks, disables := isolatedEnvWithPrepare(t, nil, true, prepare)
	input := isolatedFixtureInput()
	input.ResumeFrom = &ResumeFrom{RunID: "halted"}
	env.ExecuteWorkflow(RunWorkflow, input)
	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow succeeded after a failed adoption")
	}
	if *rollbacks != 0 || *disables != 1 {
		t.Errorf("rollbacks = %d, revokes = %d; want 0 and 1", *rollbacks, *disables)
	}
	if wp, _ := IsolatedWorkspaceFromError(err); wp != "/fixture/data/workspaces/wt" {
		t.Errorf("worktree on error = %q", wp)
	}
}

func TestNonResumedRunStillRollsBackAPrepareFailureThatCreatedAWorktree(t *testing.T) {
	prepare := func() (PrepareIsolatedWorkspaceResult, error) {
		cause := errors.New("stale evidence cleanup failed")
		return PrepareIsolatedWorkspaceResult{}, attachIsolatedWorkspaceDetail(cause.Error(), IsolationFailureType, cause, "/fixture/data/workspaces/wt", "factoryd/wt")
	}
	env, rollbacks, _ := isolatedEnvWithPrepare(t, nil, true, prepare)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	if env.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded after a failed prepare")
	}
	if *rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", *rollbacks)
	}
}

func TestResumedRunQuarantineKeepsTodaysBehaviour(t *testing.T) {
	env, rollbacks, disables := isolatedLostBuildEnv(t, nil, false)
	input := isolatedFixtureInput()
	input.ResumeFrom = &ResumeFrom{RunID: "halted"}
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil || result.State != run.StateQuarantined {
		t.Fatalf("result = %+v, err %v; want quarantined", result, err)
	}
	if *rollbacks != 0 || *disables != 1 {
		t.Errorf("rollbacks = %d, revokes = %d; want 0 and 1", *rollbacks, *disables)
	}
}

// Only a heartbeat timeout is a lost worker.

func TestStartToCloseTimeoutIsNotALostWorker(t *testing.T) {
	startToClose := temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil)
	if StepLostFromError(startToClose) {
		t.Error("a start-to-close timeout counted as a lost worker")
	}
	if !StepLostFromError(temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)) {
		t.Error("a heartbeat timeout was not a lost worker")
	}
	env, rollbacks, _ := isolatedLostBuildEnv(t, startToClose, true)
	pinNoActivityRetries(env)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	if env.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded")
	}
	if *rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1: a judged timeout still rolls back", *rollbacks)
	}
}

// A kept worktree loses the worker group-write grant.

func TestKeptWorktreeOfALostWorkerRevokesGroupWrite(t *testing.T) {
	env, rollbacks, disables := isolatedLostBuildEnv(t, lostBuildError(), true)
	pinNoActivityRetries(env)
	env.ExecuteWorkflow(RunWorkflow, isolatedFixtureInput())
	if env.GetWorkflowError() == nil {
		t.Fatal("workflow succeeded")
	}
	if *rollbacks != 0 || *disables != 1 {
		t.Errorf("rollbacks = %d, revokes = %d; want 0 and 1", *rollbacks, *disables)
	}
}

// A resumed run's own earlier changes do not trip the pre-dirty refusal.

func TestPreflightActivitySkipsThePreDirtyRefusalForAResumedRun(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("the halted run's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{}
	in := PreflightInput{WorkspacePath: workspacePath, RequiredChangedFiles: []string{"content.txt"}}
	if err := activities.PreflightActivity(context.Background(), in); err == nil {
		t.Fatal("a fresh run with a pre-dirty required file was not refused")
	}
	in.Resumed = true
	if err := activities.PreflightActivity(context.Background(), in); err != nil {
		t.Fatalf("a resumed run was refused for its own halted run's changes: %v", err)
	}
}

// Adoption is atomic and serialised.

func TestSecondResumeOfTheSameRunIsRefused(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	a := &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}
	first := resumeInput(repoDir, dataDir, base, prepared)
	if _, err := runPrepare(t, a, first); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	second := resumeInput(repoDir, dataDir, base, prepared)
	second.DurableRunID = "resumed-run-2"
	second.WorkflowID = "wf-new-2"
	a2 := &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}
	_, err := runPrepare(t, a2, second)
	if err == nil || !strings.Contains(err.Error(), "not kept") {
		t.Fatalf("second resume = %v, want a refusal: the run is no longer kept", err)
	}
	if wp, _ := IsolatedWorkspaceFromError(err); wp != "" {
		t.Error("the refusal attached the worktree: the workflow would act on it")
	}
	if _, statErr := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "resumed-run-2")); !os.IsNotExist(statErr) {
		t.Error("the refused second resume wrote a marker")
	}
	if _, statErr := os.Stat(prepared.WorktreePath); statErr != nil {
		t.Errorf("worktree gone: %v", statErr)
	}
}

func TestAdoptionAbortsWithNoChangeWhenTheOldMarkerCannotBeRemoved(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	markers := filepath.Join(dataDir, "isolation-markers")
	if err := os.Chmod(markers, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(markers, 0o700) })
	a := &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}
	_, err := runPrepare(t, a, resumeInput(repoDir, dataDir, base, prepared))
	if err == nil {
		t.Fatal("adoption succeeded though the halted run's marker could not be removed")
	}
	if wp, _ := IsolatedWorkspaceFromError(err); wp != "" {
		t.Error("an aborted adoption attached the worktree")
	}
	if halted, _ := run.Load(dataDir, "halted-run"); !halted.KeptForResume {
		t.Error("the halted run's flag was cleared by an aborted adoption")
	}
	if _, statErr := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "halted-run")); statErr != nil {
		t.Errorf("halted run's marker changed: %v", statErr)
	}
	if _, statErr := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "resumed-run")); !os.IsNotExist(statErr) {
		t.Error("a marker was written for the resumed run")
	}
}

// TestAdoptionRedispatchedAfterTheMarkerMovedContinues simulates a crash
// after the marker moved but before the Activity's checkpoint was saved: the
// redispatch (here, a fresh checkpoint dir) finds the new run's own marker
// and succeeds instead of refusing because the halted run's flag is gone.
func TestAdoptionRedispatchedAfterTheMarkerMovedContinues(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	saveRunRecord(t, dataDir, &run.Run{ID: "resumed-run", State: run.StateSliceRunning})
	in := resumeInput(repoDir, dataDir, base, prepared)
	if _, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, in); err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	// Crash: no checkpoint survived. The halted run's flag is already clear.
	again, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, in)
	if err != nil {
		t.Fatalf("redispatch refused: %v", err)
	}
	if again != prepared {
		t.Errorf("redispatch result = %+v, want %+v", again, prepared)
	}
}

// A crash between writing the new marker and removing the old one leaves
// both: the worktree always has a marker, and the redispatch finishes the move.
func TestAdoptionRedispatchedWithBothMarkersPresentFinishesTheMove(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	saveRunRecord(t, dataDir, &run.Run{ID: "resumed-run", State: run.StateSliceRunning})
	in := resumeInput(repoDir, dataDir, base, prepared)
	if _, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, in); err != nil {
		t.Fatal(err)
	}
	// Rewind to the crash: the halted run's marker and flag are back.
	oldMarker, err := wsisolation.LoadIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, "resumed-run"))
	if err != nil {
		t.Fatal(err)
	}
	oldMarker.RunID = "halted-run"
	if err := wsisolation.WriteIsolationMarker(wsisolation.IsolationMarkerPath(dataDir, "halted-run"), oldMarker); err != nil {
		t.Fatal(err)
	}
	halted, _ := run.Load(dataDir, "halted-run")
	halted.KeptForResume = true
	saveRunRecord(t, dataDir, halted)

	if _, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, in); err != nil {
		t.Fatalf("redispatch with both markers: %v", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "halted-run")); !os.IsNotExist(err) {
		t.Errorf("the halted run's marker survived the redispatch (stat err = %v)", err)
	}
	if _, err := os.Stat(wsisolation.IsolationMarkerPath(dataDir, "resumed-run")); err != nil {
		t.Errorf("the new run's marker is gone: %v", err)
	}
	if got, _ := run.Load(dataDir, "halted-run"); got.KeptForResume {
		t.Error("the halted run is still KeptForResume")
	}
}

func TestAdoptionRecordsTheCarriedSpendOnTheNewRun(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	appendLedger(t, dataDir, "halted-run", 90, 40)
	saveRunRecord(t, dataDir, &run.Run{ID: "resumed-run", State: run.StateSliceRunning})
	if _, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, resumeInput(repoDir, dataDir, base, prepared)); err != nil {
		t.Fatal(err)
	}
	fresh, err := run.Load(dataDir, "resumed-run")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ResumeSpendCarried == nil || *fresh.ResumeSpendCarried != (run.MeterSpend{Tokens: 90, CostMicroUSD: 40}) {
		t.Errorf("ResumeSpendCarried = %+v, want 90 tokens and 40 micro-USD", fresh.ResumeSpendCarried)
	}
}

// Spend carries across a chain of resumes.

func TestSecondRunWithResumeFromGetsCeilingsMinusThePriorRunsSpend(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	saveRunRecord(t, dataDir, &run.Run{ID: "halted", State: run.StateHalted})
	appendLedger(t, dataDir, "halted", 300, 120)
	spec := spendSpec(1000, 500)
	if err := capRelayCeilings(dir, "wf", "run", "act", 1, dataDir, "r1", "halted", spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 700 || spec.CostCeilingMicroUSD != 380 {
		t.Errorf("ceilings = %d, %d; want 700, 380 (configured minus the halted run's ledger)", spec.TokenCeiling, spec.CostCeilingMicroUSD)
	}
	// A retry of the resumed run's Activity also charges its own earlier attempt.
	appendLedger(t, dataDir, "r1", 100, 20)
	startAt(t, dir, t.TempDir()) // spend-start of an empty ledger: attempt 1 began at zero
	spec = spendSpec(1000, 500)
	if err := capRelayCeilings(dir, "wf", "run", "act", 2, dataDir, "r1", "halted", spec); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 600 || spec.CostCeilingMicroUSD != 360 {
		t.Errorf("attempt 2 ceilings = %d, %d; want 600, 360", spec.TokenCeiling, spec.CostCeilingMicroUSD)
	}
}

func TestResumeFromRefusesWhenThePriorRunsLedgerIsUnreadable(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	saveRunRecord(t, dataDir, &run.Run{ID: "halted", State: run.StateHalted})
	unreadableLedger(t, dataDir, "halted")
	err := capRelayCeilings(dir, "wf", "run", "act", 1, dataDir, "r1", "halted", spendSpec(1000, 0))
	if !RelayCeilingExceededFromError(err) {
		t.Fatalf("err = %v, want relay ceiling exceeded (fail closed)", err)
	}
}

func TestResumeFromExhaustedPriorSpendRefuses(t *testing.T) {
	dir, dataDir := t.TempDir(), t.TempDir()
	saveRunRecord(t, dataDir, &run.Run{ID: "halted", State: run.StateHalted})
	appendLedger(t, dataDir, "halted", 1000, 0)
	err := capRelayCeilings(dir, "wf", "run", "act", 1, dataDir, "r1", "halted", spendSpec(1000, 0))
	if !RelayCeilingExceededFromError(err) {
		t.Fatalf("err = %v, want relay ceiling exceeded once the halted run spent the ceiling", err)
	}
}

func TestCapRelayForAttemptCarriesResumeFrom(t *testing.T) {
	dataDir := t.TempDir()
	saveRunRecord(t, dataDir, &run.Run{ID: "halted", State: run.StateHalted})
	appendLedger(t, dataDir, "halted", 250, 0)
	a := &Activities{DataDir: dataDir, CheckpointDir: t.TempDir()}
	spec := spendSpec(1000, 0)
	wrapper := func(ctx context.Context) error {
		return a.capRelayForAttempt(ctx, RunWorkflowInput{RunID: "r1", ResumeFrom: &ResumeFrom{RunID: "halted"}}, spec)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper); err != nil {
		t.Fatal(err)
	}
	if spec.TokenCeiling != 750 {
		t.Errorf("token ceiling = %d, want 750", spec.TokenCeiling)
	}
}

func TestChainedResumesCarryTheWholeChainsSpend(t *testing.T) {
	dataDir := t.TempDir()
	// A spent 90 of a 100 ceiling and halted.
	saveRunRecord(t, dataDir, &run.Run{ID: "A", State: run.StateHalted})
	appendLedger(t, dataDir, "A", 90, 0)
	// B resumed A (recorded 90 carried) and spent 10 more.
	saveRunRecord(t, dataDir, &run.Run{ID: "B", State: run.StateHalted, ResumeSpendCarried: &run.MeterSpend{Tokens: 90}})
	appendLedger(t, dataDir, "B", 10, 0)

	// B's own build had 10 left of the ceiling.
	specB := spendSpec(100, 0)
	if err := capRelayCeilings(t.TempDir(), "wf", "run", "act", 1, dataDir, "B", "A", specB); err != nil {
		t.Fatal(err)
	}
	if specB.TokenCeiling != 10 {
		t.Errorf("B's ceiling = %d, want 10", specB.TokenCeiling)
	}
	// C resumes B: 90 carried by B plus B's 10 leaves nothing.
	err := capRelayCeilings(t.TempDir(), "wf", "run", "act", 1, dataDir, "C", "B", spendSpec(100, 0))
	if !RelayCeilingExceededFromError(err) {
		t.Fatalf("C's cap = %v, want relay ceiling exceeded: the chain spent the whole ceiling", err)
	}
}

// Nothing in ResumeFrom is trusted over the record.

func TestAdoptionRefusesABaseSHAThatDisagreesWithTheHaltedRun(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	in := resumeInput(repoDir, dataDir, base, prepared)
	in.Resume.BaseSHA = strings.Repeat("a", 40)
	_, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, in)
	if err == nil || !strings.Contains(err.Error(), "base commit") {
		t.Fatalf("err = %v, want a base commit refusal", err)
	}
	if halted, _ := run.Load(dataDir, "halted-run"); !halted.KeptForResume {
		t.Error("a refused adoption cleared the flag")
	}
}

func TestResumePreconditionsRefuseAChangedSpec(t *testing.T) {
	dataDir, _, _, _ := preparedHaltedRun(t, "halted-run")
	halted, _ := run.Load(dataDir, "halted-run")
	halted.SpecSHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	saveRunRecord(t, dataDir, halted)
	docker := noContainersDocker(t)
	ok, reasons, err := CheckResumePreconditions(context.Background(), dataDir, docker, "halted-run", "2222222222222222222222222222222222222222222222222222222222222222", 3)
	if err != nil || ok || !strings.Contains(strings.Join(reasons, "|"), "spec changed") {
		t.Fatalf("ok=%v reasons=%v err=%v; want a spec-changed refusal", ok, reasons, err)
	}
	ok, reasons, err = CheckResumePreconditions(context.Background(), dataDir, docker, "halted-run", halted.SpecSHA256, 3)
	if err != nil || !ok {
		t.Fatalf("same spec: ok=%v reasons=%v err=%v", ok, reasons, err)
	}
}

func TestAdoptionRefusesAChangedSpec(t *testing.T) {
	dataDir, repoDir, base, prepared := preparedHaltedRun(t, "halted-run")
	halted, _ := run.Load(dataDir, "halted-run")
	halted.SpecSHA256 = "1111111111111111111111111111111111111111111111111111111111111111"
	saveRunRecord(t, dataDir, halted)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("an edited ticket\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := resumeInput(repoDir, dataDir, base, prepared)
	in.SpecPath = spec
	_, err := runPrepare(t, &Activities{CheckpointDir: t.TempDir(), SandboxDocker: noContainersDocker(t)}, in)
	if err == nil || !strings.Contains(err.Error(), "spec changed") {
		t.Fatalf("err = %v, want a spec-changed refusal", err)
	}
}

func TestResumePreconditionsRefuseARoundStateWithNoRoundLeft(t *testing.T) {
	dataDir, _, _, prepared := preparedHaltedRun(t, "halted-run")
	writeFile(t, filepath.Join(prepared.WorktreePath, RoundStateFileName), `{"version":1,"last_completed_round":3}`)
	docker := noContainersDocker(t)
	ok, reasons, err := CheckResumePreconditions(context.Background(), dataDir, docker, "halted-run", "", 3)
	if err != nil || ok || !strings.Contains(strings.Join(reasons, "|"), "no round is left") {
		t.Fatalf("ok=%v reasons=%v err=%v; want a no-round-left refusal", ok, reasons, err)
	}
	if ok, reasons, err = CheckResumePreconditions(context.Background(), dataDir, docker, "halted-run", "", 4); err != nil || !ok {
		t.Fatalf("with 4 max rounds: ok=%v reasons=%v err=%v", ok, reasons, err)
	}
}

func TestSpendCarriedChargesTheHaltedRunsRecordedCarryToo(t *testing.T) {
	dataDir := t.TempDir()
	saveRunRecord(t, dataDir, &run.Run{ID: "B", State: run.StateHalted, ResumeSpendCarried: &run.MeterSpend{Tokens: 90, CostMicroUSD: 5}})
	appendLedger(t, dataDir, "B", 10, 2)
	got, err := CarriedSpend(dataDir, "B")
	if err != nil || got != (run.MeterSpend{Tokens: 100, CostMicroUSD: 7}) {
		t.Fatalf("CarriedSpend = %+v, %v; want 100 tokens, 7 micro-USD", got, err)
	}
	// An unreadable ledger is an error, never zero.
	bad := t.TempDir()
	saveRunRecord(t, bad, &run.Run{ID: "B", State: run.StateHalted})
	unreadableLedger(t, bad, "B")
	if _, err := CarriedSpend(bad, "B"); err == nil {
		t.Error("CarriedSpend swallowed an unreadable ledger")
	}
}
