package workflow

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"buildgate/internal/meter"
	"buildgate/internal/runner"

	"buildgate/internal/sandbox"
	"buildgate/internal/unresolved"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// These run the real Activities through the fake runtime, so they fail if a
// call site loses its attempt count or its relaunch rule, or if an
// Activity's own error typing drops the launch failure.

func countCreates(rt *inspectRuntime) *int {
	n := 0
	rt.onCreate = func(sandbox.SandboxRequest) { n++ }
	return &n
}

func TestAReviewWhoseSandboxNeverStartsIsLaunchedAgainAndThenUnresolved(t *testing.T) {
	f := newReviewFixture(t, map[string]string{"main.go": "package main\n"}, func(string) {})
	f.acts.VerifyMaxAttempts = 2
	f.rt.CreateErr = errors.New("image pull failed")
	creates := countCreates(f.rt)
	_, err := f.run()
	if got := UnresolvedCauseFromError(err); got != unresolved.SandboxLaunch {
		t.Fatalf("cause = %q (err %v), want %q", got, err, unresolved.SandboxLaunch)
	}
	if *creates != 2 {
		t.Errorf("launches = %d, want the 2 that -verify-max-attempts allows: nothing ran, nothing is spent twice", *creates)
	}
}

// A review that started and then lost its wait may have spent tokens: a
// second launch would get the whole ceiling again, so there is none, and the
// failure is not "never started".
func TestAReviewThatStartedIsNotLaunchedAgain(t *testing.T) {
	f := newReviewFixture(t, map[string]string{"main.go": "package main\n"}, func(string) {})
	f.acts.VerifyMaxAttempts = 2
	f.rt.WaitErr = errors.New("wait: connection reset")
	creates := countCreates(f.rt)
	_, err := f.run()
	if err == nil || UnresolvedCauseFromError(err) != "" {
		t.Fatalf("err = %v (cause %q), want a failure that is not unresolved", err, UnresolvedCauseFromError(err))
	}
	if *creates != 1 {
		t.Errorf("launches = %d, want 1", *creates)
	}
}

func TestANamedGateRelaunchesOnlyASandboxThatNeverStarted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fail        func(*inspectRuntime)
		wantCreates int
		wantCause   unresolved.Cause
	}{
		{"never started", func(rt *inspectRuntime) { rt.CreateErr = errors.New("image pull failed") }, 2, unresolved.SandboxLaunch},
		{"started, then lost", func(rt *inspectRuntime) { rt.WaitErr = errors.New("wait: connection reset") }, 1, ""},
	} {
		f := newFactoryDirFixture(t, map[string]string{".factory/x.sh": "true\n"})
		f.acts.VerifyMaxAttempts = 2
		tc.fail(f.rt)
		creates := countCreates(f.rt)
		in := f.input
		in.CheckpointDir = t.TempDir()
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		env.RegisterActivity(f.acts.RunNamedGateActivity)
		_, err := env.ExecuteActivity(f.acts.RunNamedGateActivity, NamedGateActivityInput{RunWorkflowInput: in, Check: "lint", Command: "sh .factory/x.sh"})
		if err == nil || UnresolvedCauseFromError(err) != tc.wantCause {
			t.Errorf("%s: err = %v, cause %q, want cause %q", tc.name, err, UnresolvedCauseFromError(err), tc.wantCause)
		}
		if *creates != tc.wantCreates {
			t.Errorf("%s: launches = %d, want %d", tc.name, *creates, tc.wantCreates)
		}
	}
}

// writeLedgerOnCreate makes the fake runtime leave, for each sandbox it
// creates, the ledger a meter would have written for it.
func writeLedgerOnCreate(t *testing.T, rt *inspectRuntime, ledgerRoot, dataDir, runID string, records ...meter.LedgerRecord) {
	t.Helper()
	name, err := sandbox.MeterRunName(dataDir, runID)
	if err != nil {
		t.Fatal(err)
	}
	rt.onCreate = func(req sandbox.SandboxRequest) {
		dir := filepath.Join(ledgerRoot, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Error(err)
			return
		}
		ledger, err := meter.OpenLedger(filepath.Join(dir, "id-"+req.Name+".jsonl"), log.New(io.Discard, "", 0))
		if err != nil {
			t.Error(err)
			return
		}
		defer ledger.Close()
		for _, rec := range records {
			ledger.Append(rec)
		}
	}
}

// What the meter recorded about a review's model requests reaches the
// review's attempt on the run record: a later reader decides from it, so a
// dropped copy anywhere between the ledger and the attempt must fail here.
func TestAReviewAttemptRecordsHowItsModelRequestsEnded(t *testing.T) {
	f := newReviewFixture(t, map[string]string{"main.go": "package main\n"}, func(string) {})
	writeLedgerOnCreate(t, f.rt, f.acts.MeterLedgerRoot, f.acts.dataDirFor(f.input.RunWorkflowInput), f.acts.runIDFor(f.input.RunWorkflowInput),
		meter.LedgerRecord{Kind: meter.LedgerKindAdmit, RequestID: "a"},
		meter.LedgerRecord{Kind: meter.LedgerKindComplete, RequestID: "a", Settled: meter.SettledReleased, Status: 429, RetryAfterSeconds: 60},
		meter.LedgerRecord{Kind: meter.LedgerKindAdmit, RequestID: "b"},
		meter.LedgerRecord{Kind: meter.LedgerKindBudgetExceeded, RequestID: "c"},
	)
	res, err := f.run()
	attempts := res.Attempts
	if err != nil {
		attempts = AttemptsFromError(err)
	}
	if len(attempts) == 0 || attempts[0].RelayCalls == nil {
		t.Fatalf("attempts = %+v (err %v), want the review attempt to carry its model requests", attempts, err)
	}
	got := *attempts[0].RelayCalls
	if got.RateLimited != 1 || got.Unanswered != 1 || got.DeniedBudget != 1 || got.Answered != 0 || got.RetryAt == "" || got.Legacy {
		t.Errorf("RelayCalls = %+v, want one 429 with its retry time, one unanswered, one budget refusal", got)
	}
}

// A build that exits "did not succeed" while the meter's ledger shows the
// route was not serving it is no verdict on the work: the Activity fails
// with the cause's own type, RunWorkflow keeps the worktree, and no
// corrective build is spent against the same route.
func TestABuildWhoseModelRouteDidNotServeItIsUnresolved(t *testing.T) {
	limit := meter.Calls{Answered: 12, RateLimited: 3, RefusedAtEnd: 3, RefusedAtEndBy: meter.RefusedByProviderLimit}
	for _, tc := range []struct {
		name string
		res  runner.Result
		err  error
		want unresolved.Cause
	}{
		{"did not succeed, the provider's limit from some point on", runner.Result{ExitCode: 1, RelayCalls: limit}, nil, unresolved.ProviderQuota},
		{"did not succeed, nothing ever answered", runner.Result{ExitCode: 1, RelayCalls: meter.Calls{Unanswered: 4}}, nil, unresolved.ModelUnavailable},
		{"did not succeed, the meter refused", runner.Result{ExitCode: 1, RelayCalls: meter.Calls{Answered: 2, DeniedBudget: 1, RefusedAtEnd: 1, RefusedAtEndBy: meter.RefusedByMeter}}, nil, unresolved.MeterDenied},
		{"did not succeed with every request answered: the gates judge it", runner.Result{ExitCode: 1, RelayCalls: meter.Calls{Answered: 40}}, nil, ""},
		{"succeeded although the limit was hit at the end: a result", runner.Result{ExitCode: 0, RelayCalls: limit}, nil, ""},
		{"the repository's setup failed before any turn", runner.Result{ExitCode: SetupFailedExitCode, RelayCalls: meter.Calls{}}, nil, ""},
		{"called no model", runner.Result{ExitCode: 1}, nil, ""},
		{"a ledger that cannot say", runner.Result{ExitCode: 1, RelayCalls: meter.Calls{Failed: 3, Legacy: true}}, nil, ""},
	} {
		err := withModelRouteFailure(tc.err, tc.res)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want none", tc.name, err)
			}
			continue
		}
		crossed := temporal.NewApplicationErrorWithCause("build subprocess infrastructure failure", buildActivityErrorType(err), err)
		if got := UnresolvedCauseFromError(crossed); got != tc.want {
			t.Errorf("%s: cause across the Temporal boundary = %q (type %q), want %q", tc.name, got, buildActivityErrorType(err), tc.want)
		}
		if !StepLostFromError(crossed) || HaltReasonCodeFromError(crossed) != string(tc.want) {
			t.Errorf("%s: the worktree would be rolled back, or the halt carries code %q", tc.name, HaltReasonCodeFromError(crossed))
		}
	}
	// A launch error is its own finding and is never replaced.
	launch := errors.New("sandbox attempts exhausted")
	if got := withModelRouteFailure(launch, runner.Result{ExitCode: -1, RelayCalls: limit}); got != launch {
		t.Errorf("a launch error became %v", got)
	}
	for cause, typ := range modelRouteFailureTypes {
		if unresolvedFailureTypes[typ] != cause || !slices.Contains(nonRetryableActivityFailureTypes, typ) {
			t.Errorf("%s: type %q does not map back to it, or Temporal would retry it", cause, typ)
		}
	}
}
