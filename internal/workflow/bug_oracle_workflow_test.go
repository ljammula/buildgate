package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// bugOracleFixture runs RunWorkflow for a bug request's run whose
// oracle_red_on_base check exits with baseExit. It returns the workflow
// error and result, the ActivityIDs started in order, the checks the named
// gate Activity was asked for, and whether the build ran.
func bugOracleFixture(t *testing.T, input RunWorkflowInput, baseExit int, pin func(*testsuite.TestWorkflowEnvironment)) (error, RunWorkflowResult, []string, []string, bool) {
	t.Helper()
	var mu sync.Mutex
	var checks, ids []string
	built := false
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			mu.Lock()
			defer mu.Unlock()
			built = true
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		mu.Lock()
		defer mu.Unlock()
		checks = append(checks, in.Check)
		exit := 0
		if in.Check == OracleRedOnBaseCheck {
			exit = baseExit
		}
		return VerifyActivityResult{
			Result:   runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: exit},
			Attempts: []run.Attempt{{Kind: in.Check, ExitCode: exit}},
		}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	env.RegisterActivityWithOptions(func(context.Context, CheckChainSuccessorInput) error { return nil }, activity.RegisterOptions{Name: CheckChainSuccessorActivityName})
	env.RegisterActivityWithOptions(func(context.Context, ValidateSliceChainInput) error { return nil }, activity.RegisterOptions{Name: ValidateSliceChainActivityName})
	if pin != nil {
		pin(env)
	}
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		mu.Lock()
		defer mu.Unlock()
		ids = append(ids, info.ActivityID)
	})
	env.ExecuteWorkflow(RunWorkflow, input)
	var result RunWorkflowResult
	err := env.GetWorkflowError()
	if err == nil {
		if getErr := env.GetWorkflowResult(&result); getErr != nil {
			t.Fatalf("workflow result: %v", getErr)
		}
	}
	return err, result, ids, checks, built
}

func bugRunInput() RunWorkflowInput {
	input := fixtureInput()
	input.Bug = true
	input.ReferenceOracleDir = "/fixture/oracle"
	input.ReferenceOracleMountPath = "/oracle"
	setGateCommand(&input, policy.ReferenceOracleGateID, "go test ./.oracle/...")
	return input
}

func TestBugOraclePassingOnBaseHaltsBeforeTheBuild(t *testing.T) {
	err, _, ids, checks, built := bugOracleFixture(t, bugRunInput(), 0, nil)
	if err == nil {
		t.Fatal("workflow succeeded, want a halt: the oracle passes on the base commit")
	}
	if !hasApplicationErrorType(err, OraclePassesOnBaseFailureType) {
		t.Errorf("workflow error does not keep type %q: %v", OraclePassesOnBaseFailureType, err)
	}
	if got := HaltReasonCodeFromError(err); got != run.HaltReasonOraclePassesOnBase {
		t.Errorf("halt reason code = %q, want %q", got, run.HaltReasonOraclePassesOnBase)
	}
	// The run command is the oracle's RUN_COMMAND.txt, often a long script:
	// the halt sentence an operator reads never quotes it.
	if strings.Contains(err.Error(), "go test ./.oracle/...") {
		t.Errorf("halt sentence quotes the oracle's run command: %v", err)
	}
	if built || containsString(ids, "build") {
		t.Errorf("the build ran after an oracle that passes on the base: %q", ids)
	}
	if len(checks) != 1 || checks[0] != OracleRedOnBaseCheck {
		t.Errorf("named gate checks = %q, want only %q", checks, OracleRedOnBaseCheck)
	}
	if attempts := AttemptsFromError(err); len(attempts) != 1 || attempts[0].Kind != OracleRedOnBaseCheck {
		t.Errorf("attempts on the error = %+v, want the check's", attempts)
	}
	if !containsString(ids, "rollback-workspace") {
		t.Errorf("the worktree was not rolled back: %q", ids)
	}
}

func TestBugOracleFailingOnBaseBuilds(t *testing.T) {
	err, result, ids, checks, built := bugOracleFixture(t, bugRunInput(), 1, nil)
	if err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if !built {
		t.Fatalf("the build did not run after an oracle that fails on the base: %q", ids)
	}
	if len(checks) < 1 || checks[0] != OracleRedOnBaseCheck {
		t.Errorf("named gate checks = %q, want %q first", checks, OracleRedOnBaseCheck)
	}
	if red, build := indexOf(ids, "oracle-red-on-base"), indexOf(ids, "build"); red < 0 || build < red {
		t.Errorf("Activity order %q, want oracle-red-on-base before build", ids)
	}
	if len(result.Attempts) == 0 || result.Attempts[0].Kind != OracleRedOnBaseCheck {
		t.Errorf("attempts = %+v, want the check's first", result.Attempts)
	}
}

// TestOnlyABugRunWithAnApprovedOracleChecksTheBase: a run that is not a bug
// request, one with no oracle, and one that continues earlier work are not
// at the base commit, so none runs the check.
func TestOnlyABugRunWithAnApprovedOracleChecksTheBase(t *testing.T) {
	notBug := bugRunInput()
	notBug.Bug = false
	noOracle := bugRunInput()
	noOracle.ReferenceOracleDir = ""
	noOracle.ReferenceOracleMountPath = ""
	delete(noOracle.GateCommands, policy.ReferenceOracleGateID)
	corrective := bugRunInput()
	corrective.DiffBaseSHA = "ticket-base-sha"
	secondTicket := bugRunInput()
	secondTicket.PriorRunID = "req-001"
	secondTicket.PriorRunState = run.StateAccepted
	secondTicket.PriorRunProjectPath = secondTicket.WorkspacePath
	secondTicket.PriorRunResultSHA = "fixture-base-sha"
	resumedPastBase := bugRunInput()
	resumedPastBase.ResumeFrom = &ResumeFrom{RunID: "lost", WorktreePath: "/fixture/wt", Branch: "b", BaseSHA: "base-sha", BeyondBase: true}
	for name, input := range map[string]RunWorkflowInput{"not a bug": notBug, "no oracle": noOracle, "continuing earlier work": corrective, "a later ticket of the request": secondTicket, "a resume past the base": resumedPastBase} {
		t.Run(name, func(t *testing.T) {
			err, _, ids, checks, built := bugOracleFixture(t, input, 0, nil)
			if err != nil || !built {
				t.Fatalf("err=%v built=%v, want a normal build", err, built)
			}
			if containsString(ids, "oracle-red-on-base") || containsString(checks, OracleRedOnBaseCheck) {
				t.Errorf("the base check ran: ids %q checks %q", ids, checks)
			}
		})
	}
}

// TestHistoryRecordedBeforeTheBugOracleCheckReplaysUnchanged pins the change
// to DefaultVersion, what a history recorded before bug-oracle-red-on-base
// replays as: a bug run goes from the baseline to the build.
func TestHistoryRecordedBeforeTheBugOracleCheckReplaysUnchanged(t *testing.T) {
	pin := func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(bugOracleRedOnBaseChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	err, _, ids, checks, built := bugOracleFixture(t, bugRunInput(), 0, pin)
	if err != nil || !built {
		t.Fatalf("replay of a history from before the check: err=%v built=%v, want a build", err, built)
	}
	if containsString(ids, "oracle-red-on-base") || containsString(checks, OracleRedOnBaseCheck) {
		t.Errorf("the check ran on a history recorded before it: ids %q checks %q", ids, checks)
	}
}

// TestAnOraclePassingOnBaseDoesNotTripTheStopLine: the oracle says nothing
// about the factory, so runs halting on it leave the repository's queue open.
func TestAnOraclePassingOnBaseDoesNotTripTheStopLine(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			t.Error("a build ran after an oracle that passes on the base")
			return BuildActivityResult{}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) { return run.GateResult{}, nil },
	)
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0}}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	repository := "fixture/bug-oracle-stop-line"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		for _, id := range []string{"run-1", "run-2", "run-3"} {
			input := bugRunInput()
			input.Ticket = id
			env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: id, Input: input})
		}
	}, 0)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: 1e9, StopLineFailureThreshold: 2})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.StopLineTripped || result.SystemicFailureStreak != 0 {
		t.Errorf("stop line: tripped=%v streak=%d, want an open queue", result.StopLineTripped, result.SystemicFailureStreak)
	}
	for _, id := range []string{"run-1", "run-2", "run-3"} {
		if got := result.Runs[id]; got.State != run.StateHalted || got.HaltReasonCode != run.HaltReasonOraclePassesOnBase {
			t.Errorf("%s = state %q code %q, want halted with %q", id, got.State, got.HaltReasonCode, run.HaltReasonOraclePassesOnBase)
		}
	}
}

// TestTheBaseCheckGetsTheOracleButNoBaseRerun: the check mounts the oracle
// like the reference_oracle gate, and its non-zero exit (the wanted result)
// is not rerun on the base commit.
func TestTheBaseCheckGetsTheOracleButNoBaseRerun(t *testing.T) {
	for _, check := range []string{policy.ReferenceOracleGateID, OracleRedOnBaseCheck} {
		if !isOracleMountingCheck(check) {
			t.Errorf("%s does not mount the oracle", check)
		}
		input := NamedGateActivityInput{RunWorkflowInput: bugRunInput(), Check: check}
		if got := pendingGateBaseCheck(input, runner.Result{ExitCode: 1}, nil, nil); got != nil {
			t.Errorf("%s: a failed run has a pending base rerun: %+v", check, got)
		}
	}
	if isOracleMountingCheck("lint") {
		t.Error("lint mounts the oracle")
	}
}

// TestAResumeLostBeforeItsBuildChecksTheBaseAgain: a resumed run whose halted
// run never got past the base is at the base commit and is asked.
func TestAResumeLostBeforeItsBuildChecksTheBaseAgain(t *testing.T) {
	input := bugRunInput()
	input.ResumeFrom = &ResumeFrom{RunID: "lost", WorktreePath: "/fixture/wt", Branch: "b", BaseSHA: "base-sha"}
	err, _, ids, checks, _ := bugOracleFixture(t, input, 0, nil)
	if err == nil || !hasApplicationErrorType(err, OraclePassesOnBaseFailureType) {
		t.Fatalf("err = %v, want the resumed run halted by the check", err)
	}
	if !containsString(ids, "oracle-red-on-base") || len(checks) != 1 {
		t.Errorf("ids %q checks %q, want the check made once", ids, checks)
	}
}

func TestNewResumeFromSaysWhetherTheHaltedRunGotPastTheBase(t *testing.T) {
	for name, tc := range map[string]struct {
		halted *run.Run
		want   bool
	}{
		"lost in the baseline":  {&run.Run{ID: "a", BaseSHA: "b", Attempts: []run.Attempt{{Kind: run.BaselineVerifyAttemptKind}, {Kind: OracleRedOnBaseCheck}}}, false},
		"the build began":       {&run.Run{ID: "a", BaseSHA: "b", Attempts: []run.Attempt{{Kind: "build"}}}, true},
		"a result":              {&run.Run{ID: "a", BaseSHA: "b", ResultSHA: "r"}, true},
		"chained after another": {&run.Run{ID: "a", BaseSHA: "b", PriorRunID: "p"}, true},
	} {
		if got := NewResumeFrom(tc.halted).BeyondBase; got != tc.want {
			t.Errorf("%s: BeyondBase = %v, want %v", name, got, tc.want)
		}
	}
}

// TestAnOracleThatRanNoTestOnBaseHaltsWithItsOwnCode: pytest's 5 and the
// timeout and not-found codes are not a red test, nor is a go test that ran
// no test.
func TestAnOracleThatRanNoTestOnBaseHaltsWithItsOwnCode(t *testing.T) {
	for _, code := range []int{5, 124, 125, 126, 127} {
		err, _, _, _, built := bugOracleFixture(t, bugRunInput(), code, nil)
		if err == nil || !hasApplicationErrorType(err, OracleNotRunOnBaseFailureType) || built {
			t.Errorf("exit %d: err = %v built = %v, want a halt of type %s", code, err, built, OracleNotRunOnBaseFailureType)
			continue
		}
		if got := HaltReasonCodeFromError(err); got != run.HaltReasonOracleNotRunOnBase {
			t.Errorf("exit %d: halt reason code = %q", code, got)
		}
	}
	if err, _, _, _, built := bugOracleFixture(t, bugRunInput(), 2, nil); err != nil || !built {
		t.Errorf("exit 2 is a failing test: err = %v built = %v, want a build", err, built)
	}
}

func TestOracleRanNothingReadsAGoTestLog(t *testing.T) {
	for log, want := range map[string]bool{
		"?   \tx/y\t[no test files]\n":                                             true,
		"ok  \tx/y\t0.003s [no tests to run]\ntesting: warning: no tests to run\n": true,
		"?   \tx/y\t[no test files]\nok  \tx/z\t0.01s\n":                           false,
		"--- PASS: TestOracleA (0.00s)\nPASS\n":                                    false,
		"":                                                                         false,
	} {
		if got := oracleRanNothing(log); got != want {
			t.Errorf("log %q: oracleRanNothing = %v, want %v", log, got, want)
		}
	}
}
