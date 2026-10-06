package workflow

import (
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

func TestRepositoryOwnerWorkflowProcessesSubmittedRunsSerially(t *testing.T) {
	var mu sync.Mutex
	activeRuns := 0
	maxActiveRuns := 0
	build := func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
		mu.Lock()
		activeRuns++
		if activeRuns > maxActiveRuns {
			maxActiveRuns = activeRuns
		}
		mu.Unlock()
		return BuildActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
	}
	verify := func(_ context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
	}
	// The "run completed" signal for this test's concurrency count is
	// CollectEvidenceActivity, the last Activity RunWorkflow calls before
	// returning — not EvaluateGateActivity, which RunWorkflow no longer
	// invokes (gate evaluation now happens via a direct, in-workflow
	// policy.EvaluateRun call using CollectEvidenceActivity's evidence).
	// This test therefore builds its own env instead of using
	// newWorkflowEnvironment, whose default CollectEvidenceActivity mock
	// has no decrement to override.
	gate := func(context.Context, EvaluateGateInput) (run.GateResult, error) {
		return run.GateResult{Check: "canonical_verify", Passed: true}, nil
	}
	collectEvidence := func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
		mu.Lock()
		activeRuns--
		mu.Unlock()
		return CollectedEvidence{}, nil
	}
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
	env.RegisterActivityWithOptions(build, activity.RegisterOptions{Name: RunBuildActivityName})
	env.RegisterActivityWithOptions(verify, activity.RegisterOptions{Name: RunVerifyActivityName})
	env.RegisterActivityWithOptions(gate, activity.RegisterOptions{Name: EvaluateGateActivityName})
	env.RegisterActivityWithOptions(
		func(_ context.Context, input EvaluateRunInput) (policy.EvaluateRunResult, error) {
			return policy.EvaluateRun(input.Policy), nil
		},
		activity.RegisterOptions{Name: EvaluateRunActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, PostBuildInput) (PostBuildResult, error) {
			return PostBuildResult{ResultSHA: "fixture-result-sha"}, nil
		},
		activity.RegisterOptions{Name: PostBuildActivityName},
	)
	env.RegisterActivityWithOptions(collectEvidence, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	env.SetTestTimeout(5 * time.Second)
	repository := "fixture/repository"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("ticket-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("ticket-2")})
	}, 0)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if maxActiveRuns != 1 {
		t.Fatalf("maximum active runs = %d, want 1", maxActiveRuns)
	}
	if activeRuns != 0 {
		t.Fatalf("active runs after completion = %d, want 0", activeRuns)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("processed runs = %d, want 2", len(result.Runs))
	}
	if len(result.CompletionOrder) != 2 || result.CompletionOrder[0] != "run-1" || result.CompletionOrder[1] != "run-2" {
		t.Fatalf("completion order = %v, want [run-1 run-2]", result.CompletionOrder)
	}
}

// TestRepositoryOwnerWorkflowQueryReportsQueuedRequestIDs proves the
// RepositoryOwnerQueryName query's QueuedRequestIDs field reflects
// whichever requests are still waiting their turn, in dispatch order —
// see RepositoryOwnerResult.QueuedRequestIDs' own doc comment. Needed by
// cmd/factoryd's pollRepositoryOwnerResult (progress-contract.md's
// "queued" stage, 2026-09-18) to compute a waiting submitter's own queue
// position.
func TestRepositoryOwnerWorkflowQueryReportsQueuedRequestIDs(t *testing.T) {
	release := make(chan struct{})
	build := func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
		<-release
		return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
	}
	verify := func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
	}
	env := newWorkflowEnvironment(t, build, verify,
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/queue-position"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-3", Input: fixtureInputForTicket("run-3")})
	}, 0)
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		var got RepositoryOwnerResult
		if err := queryResult.Get(&got); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		if got.InProgress == nil || got.InProgress.RequestID != "run-1" {
			t.Fatalf("InProgress = %+v, want run-1 in progress", got.InProgress)
		}
		if want := []string{"run-2", "run-3"}; !slices.Equal(got.QueuedRequestIDs, want) {
			t.Fatalf("QueuedRequestIDs = %v, want %v", got.QueuedRequestIDs, want)
		}
		close(release)
	}, time.Second)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if len(result.Runs) != 3 {
		t.Fatalf("processed runs = %d, want 3", len(result.Runs))
	}
}

func TestRepositoryOwnerWorkflowDoesNotRepeatCompletedRequest(t *testing.T) {
	buildCalls := 0
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			buildCalls++
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/repository"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		request := SubmitRunSignal{RequestID: "same-request", Input: fixtureInput()}
		env.SignalWorkflow(SubmitRunSignalName, request)
		env.SignalWorkflow(SubmitRunSignalName, request)
	}, 0)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if buildCalls != 1 {
		t.Fatalf("build calls = %d, want 1 for duplicate RequestID", buildCalls)
	}
	if len(result.Runs) != 1 || len(result.CompletionOrder) != 1 || result.CompletionOrder[0] != "same-request" {
		t.Fatalf("result = %+v, want one completed request", result)
	}
}

// TestRepositoryOwnerWorkflowCarriesPriorResultAcrossContinueAsNew proves
// RepositoryOwnerWorkflowInput.PriorResult correctly seeds a fresh
// execution's own result — the mechanism RepositoryOwnerWorkflow's own
// Continue-As-New handling relies on so a client polling for an older,
// already-completed request (processed by a *previous* execution before
// this one continued-as-new) still finds it. The SDK's in-memory test
// environment has no way to force a real Continue-As-New (there's no
// public API to make GetContinueAsNewSuggested return true), so this
// exercises the half that's actually independently testable: starting an
// execution as if it were already the *second* one, with PriorResult
// already populated from a "previous" execution, and confirming the new
// request gets appended alongside it rather than the prior entries being
// lost.
func TestRepositoryOwnerWorkflowCarriesPriorResultAcrossContinueAsNew(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/repository"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "new-request", Input: fixtureInput()})
	}, 0)

	priorResult := &RepositoryOwnerResult{
		Runs:            map[string]RunWorkflowResult{"prior-request": {State: run.StateAccepted}},
		CompletionOrder: []string{"prior-request"},
	}
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
		PriorResult: priorResult,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if len(result.Runs) != 2 || result.Runs["prior-request"].State != run.StateAccepted {
		t.Fatalf("result.Runs = %+v, want the prior execution's own request still present alongside the new one", result.Runs)
	}
	if !slices.Equal(result.CompletionOrder, []string{"prior-request", "new-request"}) {
		t.Fatalf("result.CompletionOrder = %v, want [prior-request new-request]", result.CompletionOrder)
	}
	if result.InProgress != nil {
		t.Fatalf("result.InProgress = %+v, want nil once idle — must never carry a stale in-progress marker across executions", result.InProgress)
	}
}

// TestRepositoryOwnerWorkflowProcessesPendingRequestsBeforeSignals is the
// regression test for a real P1 finding from review:
// RepositoryOwnerWorkflowInput.PendingRequests exists specifically
// because Continue-As-New does not redeliver a signal already buffered in
// the old execution's channel — the workflow itself must drain and carry
// it forward, or it's lost outright. This proves the *receiving* half:
// requests carried in via PendingRequests are processed, in order,
// before this execution ever looks at its own freshly signaled requests.
func TestRepositoryOwnerWorkflowProcessesPendingRequestsBeforeSignals(t *testing.T) {
	var processedOrder []string
	var mu sync.Mutex
	env := newWorkflowEnvironment(t,
		func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
			mu.Lock()
			processedOrder = append(processedOrder, input.Ticket)
			mu.Unlock()
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/repository"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "signaled", Input: fixtureInputForTicket("signaled-ticket")})
	}, 0)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
		PendingRequests: []SubmitRunSignal{
			{RequestID: "pending-1", Input: fixtureInputForTicket("pending-1-ticket")},
			{RequestID: "pending-2", Input: fixtureInputForTicket("pending-2-ticket")},
		},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if !slices.Equal(result.CompletionOrder, []string{"pending-1", "pending-2", "signaled"}) {
		t.Fatalf("result.CompletionOrder = %v, want [pending-1 pending-2 signaled] — carried-forward requests processed first, in order", result.CompletionOrder)
	}
	if !slices.Equal(processedOrder, []string{"pending-1-ticket", "pending-2-ticket", "signaled-ticket"}) {
		t.Fatalf("build Activity processing order = %v, want the same order", processedOrder)
	}
}

func TestRepositoryOwnerWorkflowSkipsCanceledPendingRequest(t *testing.T) {
	var processed []string
	env := newWorkflowEnvironment(t,
		func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
			processed = append(processed, input.Ticket)
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/cancel-queued"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
		PendingRequests: []SubmitRunSignal{
			{RequestID: "abandoned", Input: fixtureInputForTicket("abandoned-ticket")},
			{RequestID: "kept", Input: fixtureInputForTicket("kept-ticket")},
		},
		CanceledRequestIDs: []string{"abandoned"},
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if !slices.Equal(processed, []string{"kept-ticket"}) {
		t.Fatalf("processed = %v, want only non-canceled request", processed)
	}
	if result.Runs["abandoned"].State != run.StateHalted {
		t.Fatalf("canceled result = %+v, want halted", result.Runs["abandoned"])
	}
}

func TestCompactRepositoryOwnerResultKeepingMaxNoOpUnderLimit(t *testing.T) {
	result := RepositoryOwnerResult{
		Runs:            map[string]RunWorkflowResult{"a": {State: run.StateAccepted}},
		CompletionOrder: []string{"a"},
	}
	compacted, dropped := compactRepositoryOwnerResultKeepingMax(result, 10)
	if dropped != 0 || len(compacted.Runs) != 1 || !slices.Equal(compacted.CompletionOrder, []string{"a"}) {
		t.Fatalf("compacted = %+v, dropped = %d, want no-op under the limit", compacted, dropped)
	}
}

func TestCompactRepositoryOwnerResultKeepingMaxDropsOldest(t *testing.T) {
	result := RepositoryOwnerResult{
		Runs: map[string]RunWorkflowResult{
			"a": {State: run.StateAccepted},
			"b": {State: run.StateAccepted},
			"c": {State: run.StateQuarantined},
		},
		CompletionOrder: []string{"a", "b", "c"},
		InProgress:      &InProgressRun{RequestID: "d"},
	}
	compacted, dropped := compactRepositoryOwnerResultKeepingMax(result, 2)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if !slices.Equal(compacted.CompletionOrder, []string{"b", "c"}) {
		t.Fatalf("CompletionOrder = %v, want [b c] (oldest, \"a\", dropped)", compacted.CompletionOrder)
	}
	if len(compacted.Runs) != 2 {
		t.Fatalf("Runs = %+v, want exactly the 2 kept entries", compacted.Runs)
	}
	if _, stillPresent := compacted.Runs["a"]; stillPresent {
		t.Error("dropped request \"a\" is still present in Runs")
	}
	if compacted.InProgress == nil || compacted.InProgress.RequestID != "d" {
		t.Errorf("InProgress = %+v, want it preserved through compaction (compaction only trims completed history)", compacted.InProgress)
	}
}

// TestCompactRepositoryOwnerResultKeepingMaxPreservesStopLineState is the
// regression test for a real P1 finding from review: an earlier version
// of compactRepositoryOwnerResultKeepingMax dropped SystemicFailureStreak/
// StopLineTripped/StopLineReason entirely, only ever carrying InProgress
// and trimmed completed-run history forward. If compaction coincided with
// the very child that tripped the stop line, the next execution started
// with the guard silently cleared and could dispatch further untrusted
// repository runs.
func TestCompactRepositoryOwnerResultKeepingMaxPreservesStopLineState(t *testing.T) {
	result := RepositoryOwnerResult{
		Runs: map[string]RunWorkflowResult{
			"a": {State: run.StateAccepted},
			"b": {State: run.StateAccepted},
			"c": {State: run.StateQuarantined},
		},
		CompletionOrder:       []string{"a", "b", "c"},
		SystemicFailureStreak: 2,
		StopLineTripped:       true,
		StopLineReason:        "stop-the-line: 2 consecutive infrastructure failures",
	}
	compacted, _ := compactRepositoryOwnerResultKeepingMax(result, 2)
	if compacted.SystemicFailureStreak != 2 {
		t.Errorf("SystemicFailureStreak = %d, want 2 (preserved across compaction)", compacted.SystemicFailureStreak)
	}
	if !compacted.StopLineTripped {
		t.Error("StopLineTripped = false, want true (preserved across compaction)")
	}
	if compacted.StopLineReason != result.StopLineReason {
		t.Errorf("StopLineReason = %q, want %q", compacted.StopLineReason, result.StopLineReason)
	}
}

// TestRepositoryOwnerWorkflowContinuesAfterChildFailure is the regression
// test for a real finding from review: a failed child RunWorkflow (e.g.
// an infrastructure failure on run-1's build Activity) used to abort
// RepositoryOwnerWorkflow entirely, silently discarding any submit-run
// signals already queued behind it — one broken build could strand every
// later submission for that repository. The owner must instead isolate
// the failure to run-1's own result and keep draining the queue, so
// run-2 still completes.
func TestRepositoryOwnerWorkflowContinuesAfterChildFailure(t *testing.T) {
	build := func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
		if input.Ticket == "ticket-1" {
			attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
			return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("could not start subprocess", InfrastructureFailureType, attempts)
		}
		return BuildActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
	}
	verify := func(_ context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
	}
	gate := func(context.Context, EvaluateGateInput) (run.GateResult, error) {
		return run.GateResult{Check: "canonical_verify", Passed: true}, nil
	}
	env := newWorkflowEnvironment(t, build, verify, gate)
	repository := "fixture/repository"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("ticket-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("ticket-2")})
	}, 0)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v, want the owner to survive a failed child", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if len(result.Runs) != 2 {
		t.Fatalf("processed runs = %d, want 2 (run-2 must still be processed after run-1 fails)", len(result.Runs))
	}
	if got := result.Runs["run-1"]; got.State != run.StateHalted || got.Err == "" {
		t.Errorf("run-1 result = %+v, want StateHalted with a non-empty Err", got)
	}
	// Regression check: the failed child's own collected attempts must
	// survive into the owner's per-request result via AttemptsFromError,
	// not be silently lost along with everything else Get() doesn't
	// deliver on a failed child Workflow.
	if got := result.Runs["run-1"].Attempts; len(got) != 1 || got[0].Kind != "build" || got[0].ExitCode != -1 {
		t.Errorf("run-1 Attempts = %+v, want the failed build Activity's own collected attempt", got)
	}
	if got := result.Runs["run-1"].BaseSHA; got != "fixture-base-sha" {
		t.Errorf("run-1 BaseSHA = %q, want the execution-time BaseSHA recovered from the failed child", got)
	}
	if got := result.Runs["run-2"]; got.State != run.StateAccepted {
		t.Errorf("run-2 result = %+v, want StateAccepted (must not be discarded by run-1's failure)", got)
	}
	if len(result.CompletionOrder) != 2 || result.CompletionOrder[0] != "run-1" || result.CompletionOrder[1] != "run-2" {
		t.Fatalf("completion order = %v, want [run-1 run-2]", result.CompletionOrder)
	}
}

// TestRepositoryOwnerWorkflowStaysOpenAcrossMultipleIdlePeriodsWhileTripped
// is the regression test for the real gap the idle-completion fix in
// workflow.go closes: a tripped owner used to complete on its very next
// idle timeout regardless, silently forgetting the trip — a later request
// would start a brand-new owner with StopLineTripped defaulting back to
// false, and `factoryd reset-stop-line` could never reach the completed
// execution again even though a query against it kept reporting
// StopLineTripped=true forever. This trips the stop line with a short
// IdleTimeout and then waits, via a delayed callback, comfortably longer
// than several idle periods would have been — proving the owner is still
// open and still reports StopLineTripped=true well past the point the old
// behavior would have already completed — before finally sending a valid
// reset and confirming the Workflow only then completes. (A later fix
// stopped arming the idle timer at all while tripped, blocking on signals
// alone instead — this test's own wait is unaffected either way, since it
// waits on the reset actually taking effect, not on any particular number
// of timer firings.)
func TestRepositoryOwnerWorkflowStaysOpenAcrossMultipleIdlePeriodsWhileTripped(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
			return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType, attempts)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify must not run after a build Activity infrastructure failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{}, nil
		},
	)
	repository := "fixture/stays-open-past-idle"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	pinNoActivityRetries(env)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 0)
	// Both requests fail (threshold 2), tripping the line almost
	// immediately. IdleTimeout is 50ms; this check at 500ms is ten idle
	// periods later — the old behavior would have completed on the very
	// first one.
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		var stillTripped RepositoryOwnerResult
		if err := queryResult.Get(&stillTripped); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		if !stillTripped.StopLineTripped {
			t.Fatal("StopLineTripped = false after multiple idle periods, want true: the owner must stay open, not silently forget its own trip")
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "confirmed still tripped after multiple idle periods", Generation: stillTripped.StopLineGeneration,
		})
	}, 500*time.Millisecond)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              50 * time.Millisecond,
		StopLineFailureThreshold: 2,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.StopLineTripped {
		t.Error("StopLineTripped = true in the final result, want false: the reset must have cleared it before completion")
	}
	if len(result.StopLineResets) != 1 {
		t.Fatalf("StopLineResets = %+v, want exactly one entry", result.StopLineResets)
	}
}

// TestRepositoryOwnerWorkflowTripsStopLineAfterRecurringInfrastructureFailures
// proves recurring systemic failures stop later queued work automatically.
// Normal gate failures complete with a result and do not affect this streak;
// only typed InfrastructureFailure child errors trip the repository owner.
func TestRepositoryOwnerWorkflowTripsStopLineAfterRecurringInfrastructureFailures(t *testing.T) {
	buildCalls := 0
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			buildCalls++
			return BuildActivityResult{Attempts: []run.Attempt{{Kind: "build", ExitCode: -1}}}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify must not run after an infrastructure failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/stop-line"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	pinNoActivityRetries(env)
	env.RegisterDelayedCallback(func() {
		for i := 1; i <= 4; i++ {
			id := fmt.Sprintf("run-%d", i)
			env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: id, Input: fixtureInputForTicket(id)})
		}
	}, 0)
	// A tripped owner now stays open indefinitely (see the idle-completion
	// fix in workflow.go) rather than completing on its own — this test's
	// own assertions need the Workflow to actually return, so it captures
	// the tripped state via a query (asserted below) and then resets the
	// stop line itself, mirroring the real CLI's own query-then-signal
	// flow.
	var trippedState RepositoryOwnerResult
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		if err := queryResult.Get(&trippedState); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "test cleanup so the workflow completes", Generation: trippedState.StopLineGeneration,
		})
	}, 500*time.Millisecond)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              time.Second,
		StopLineFailureThreshold: 2,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if buildCalls != 2 {
		t.Fatalf("build calls = %d, want 2 before stop-line trips", buildCalls)
	}
	if !trippedState.StopLineTripped || trippedState.SystemicFailureStreak != 2 {
		t.Fatalf("stop-line state = tripped=%v streak=%d, want tripped=true streak=2", trippedState.StopLineTripped, trippedState.SystemicFailureStreak)
	}
	if trippedState.StopLineReason == "" {
		t.Fatal("stop-line reason is empty")
	}
	if len(result.Runs) != 4 {
		t.Fatalf("processed runs = %d, want 4 (later requests must receive halted results)", len(result.Runs))
	}
	for _, id := range []string{"run-3", "run-4"} {
		got := result.Runs[id]
		if got.State != run.StateHalted || got.Err != trippedState.StopLineReason {
			t.Errorf("%s result = %+v, want halted with stop-line reason %q", id, got, trippedState.StopLineReason)
		}
	}
}

func TestRepositoryOwnerWorkflowStopsAfterQuarantine(t *testing.T) {
	buildCalls := 0
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			buildCalls++
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{ExitCode: 1}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: false}, nil
		},
	)
	repository := "fixture/quarantine-stop"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 0)
	// A tripped owner now stays open indefinitely (see the idle-completion
	// fix in workflow.go) rather than completing on its own — reset it
	// once the trip above has already happened so this test's own
	// GetWorkflowResult below actually returns.
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		var current RepositoryOwnerResult
		if err := queryResult.Get(&current); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "test cleanup so the workflow completes", Generation: current.StopLineGeneration,
		})
	}, 500*time.Millisecond)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: time.Second})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if buildCalls != 1 || result.Runs["run-1"].State != run.StateQuarantined || result.Runs["run-2"].State != run.StateHalted {
		t.Fatalf("buildCalls=%d runs=%+v", buildCalls, result.Runs)
	}
}

// TestRepositoryOwnerWorkflowStopsOnCleanupUnconfirmedChild is the
// regression for an "owner stop-line behavior" test gap: a child
// RunWorkflow failing with CleanupUnconfirmedFailureType
// (a sandbox container whose removal could not be confirmed — see
// RunBuildActivity's own errors.Is(runErr, sandbox.ErrCleanupUnconfirmed)
// tagging) must trip the stop line immediately, on its own dedicated
// signal, exactly like the committed-then-failed-child case
// (TestRepositoryOwnerWorkflowStopsAfterCommittedThenFailedChild) — not
// merely accumulate toward the consecutive-infrastructure-failure
// threshold, since an unconfirmed cleanup means a worker container may
// still be running, unrelated to how many ordinary infra failures preceded
// it.
func TestRepositoryOwnerWorkflowStopsOnCleanupUnconfirmedChild(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{}, temporal.NewApplicationError("sandbox container cleanup is unconfirmed", CleanupUnconfirmedFailureType)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify Activity must not run after a build Activity cleanup-unconfirmed failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			t.Fatal("evaluate gate Activity must not run after a build Activity cleanup-unconfirmed failure")
			return run.GateResult{}, nil
		},
	)
	repository := "fixture/cleanup-unconfirmed-stop"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 0)
	// A tripped owner stays open indefinitely — capture the tripped state
	// via a query (asserted below) and then reset it so this test's own
	// GetWorkflowResult actually returns, same convention as every other
	// stop-line test here.
	var trippedState RepositoryOwnerResult
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		if err := queryResult.Get(&trippedState); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "test cleanup so the workflow completes", Generation: trippedState.StopLineGeneration,
		})
	}, 500*time.Millisecond)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: time.Second})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if !trippedState.StopLineTripped {
		t.Fatal("StopLineTripped = false, want true: a single cleanup-unconfirmed child must trip the stop line immediately")
	}
	if !strings.Contains(trippedState.StopLineReason, "cleanup is unconfirmed") {
		t.Errorf("StopLineReason = %q, want it to name the cleanup-unconfirmed cause", trippedState.StopLineReason)
	}
	if got := result.Runs["run-1"]; got.State != run.StateHalted || !got.CleanupUnconfirmed {
		t.Errorf("run-1 result = %+v, want StateHalted with CleanupUnconfirmed=true", got)
	}
	if got := result.Runs["run-2"]; got.State != run.StateHalted || got.Err != trippedState.StopLineReason {
		t.Errorf("run-2 result = %+v, want StateHalted with the stop-line reason %q (never dispatched)", got, trippedState.StopLineReason)
	}
}

// TestRepositoryOwnerWorkflowRecordsRelayCeilingHaltReasonCode is the
// regression test for a code-review finding: RunWorkflowResult
// had no HaltReasonCode field at all, so a run submitted through POST /runs
// (which always goes through RepositoryOwnerWorkflow -- see
// repositoryAPIStarter) that halted on its own relay token/cost ceiling
// left run.Run.HaltReasonCode empty on the durable record, silently
// dropping the "tell this apart from an ordinary halt" feature for exactly
// the execution path Q3 named. Unlike the CleanupUnconfirmed case above,
// RelayCeilingExceededFromError is checked against the child's own
// classified ApplicationError, not a bare state flag -- see
// RunWorkflow.HaltReasonCode's own doc comment for why this must be
// recovered here rather than assumed to travel automatically.
func TestRepositoryOwnerWorkflowRecordsRelayCeilingHaltReasonCode(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{}, temporal.NewApplicationError("relay token/cost ceiling exceeded", RelayCeilingExceededFailureType)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify Activity must not run after a build Activity relay-ceiling failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			t.Fatal("evaluate gate Activity must not run after a build Activity relay-ceiling failure")
			return run.GateResult{}, nil
		},
	)
	repository := "fixture/relay-ceiling-halt-reason"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
	}, 0)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: time.Second})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	got := result.Runs["run-1"]
	if got.State != run.StateHalted {
		t.Fatalf("run-1 state = %v, want StateHalted", got.State)
	}
	if got.HaltReasonCode != run.HaltReasonRelayCeilingExceeded {
		t.Errorf("run-1 HaltReasonCode = %q, want %q", got.HaltReasonCode, run.HaltReasonRelayCeilingExceeded)
	}
}

// TestRepositoryOwnerWorkflowResetsFailureStreakAfterSuccessfulChild is the
// regression test for a real P1 finding from review: the streak reset used
// to live only inside the "child failed with a non-infrastructure error"
// branch, so a genuinely successful child (err == nil, whether accepted or
// quarantined on its own policy merits) never reset
// SystemicFailureStreak at all. Intermittent infrastructure failures
// separated by successful runs could then still accumulate toward
// StopLineFailureThreshold as if they were consecutive. Four runs here
// alternate infra-failure/success/infra-failure/success; with a threshold
// of 2, the stop line must never trip since no two failures are ever back
// to back.
func TestRepositoryOwnerWorkflowResetsFailureStreakAfterSuccessfulChild(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
			if input.Ticket == "run-1" || input.Ticket == "run-3" {
				attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
				return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType, attempts)
			}
			return BuildActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
		},
		func(_ context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/streak-reset"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		for i := 1; i <= 4; i++ {
			id := fmt.Sprintf("run-%d", i)
			env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: id, Input: fixtureInputForTicket(id)})
		}
	}, 0)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              time.Second,
		StopLineFailureThreshold: 2,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.StopLineTripped {
		t.Fatalf("StopLineTripped = true, want false: failures were never consecutive (each followed by a success)")
	}
	if result.SystemicFailureStreak != 0 {
		t.Errorf("SystemicFailureStreak = %d, want 0 after the last run succeeded", result.SystemicFailureStreak)
	}
	for _, id := range []string{"run-2", "run-4"} {
		if got := result.Runs[id]; got.State != run.StateAccepted {
			t.Errorf("%s result = %+v, want StateAccepted", id, got)
		}
	}
	for _, id := range []string{"run-1", "run-3"} {
		if got := result.Runs[id]; got.State != run.StateHalted || got.Err == "" {
			t.Errorf("%s result = %+v, want StateHalted with a non-empty Err (its own infra failure, not the stop line)", id, got)
		}
	}
}

// TestRepositoryOwnerWorkflowStopsAfterCommittedThenFailedChild is the
// regression test for a real P1 finding from review: a child whose build
// succeeded and committed via PostBuildActivity's own safety-net commit,
// but whose later RunVerifyActivity then failed infrastructurally (a
// verify timeout, not a policy rejection — so this never reaches
// StateQuarantined and the existing stop-owner-after-quarantine guard
// never sees it), left committed-but-never-canonically-verified content
// in the shared workspace. Without stopping immediately, the very next
// queued request's CaptureBaseSHAActivity would silently inherit that
// unverified HEAD as its own base. This registers its own env (not
// newWorkflowEnvironment's shared one) so PostBuildActivity can report
// CommittedByWorker=true.
func TestRepositoryOwnerWorkflowStopsAfterCommittedThenFailedChild(t *testing.T) {
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
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		// The safety-net commit landed: this is the exact evidence the
		// owner needs to tell "workspace untouched" apart from "workspace
		// mutated, then the child failed anyway".
		func(context.Context, PostBuildInput) (PostBuildResult, error) {
			return PostBuildResult{ResultSHA: "committed-sha", CommittedByWorker: true}, nil
		},
		activity.RegisterOptions{Name: PostBuildActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{}, temporal.NewApplicationError("verify subprocess timed out", InfrastructureFailureType)
		},
		activity.RegisterOptions{Name: RunVerifyActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
			t.Fatal("collect evidence Activity must not run after a verify Activity infrastructure failure")
			return CollectedEvidence{}, nil
		},
		activity.RegisterOptions{Name: CollectEvidenceActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			t.Fatal("evaluate gate Activity must not run after a verify Activity infrastructure failure")
			return run.GateResult{}, nil
		},
		activity.RegisterOptions{Name: EvaluateGateActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, EvaluateRunInput) (policy.EvaluateRunResult, error) {
			t.Fatal("evaluate run Activity must not run after a verify Activity infrastructure failure")
			return policy.EvaluateRunResult{}, nil
		},
		activity.RegisterOptions{Name: EvaluateRunActivityName},
	)
	env.SetTestTimeout(5 * time.Second)

	repository := "fixture/committed-then-failed"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	pinNoActivityRetries(env)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 0)
	// A tripped owner now stays open indefinitely (see the idle-completion
	// fix in workflow.go) rather than completing on its own — capture the
	// tripped state via a query (asserted below) and then reset it so this
	// test's own GetWorkflowResult actually returns.
	var trippedState RepositoryOwnerResult
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		if err := queryResult.Get(&trippedState); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "test cleanup so the workflow completes", Generation: trippedState.StopLineGeneration,
		})
	}, 500*time.Millisecond)
	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: time.Second})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if !trippedState.StopLineTripped {
		t.Fatal("StopLineTripped = false, want true: a single committed-then-failed child must trip the stop line immediately, not wait for a consecutive-failure threshold")
	}
	if got := result.Runs["run-1"]; got.State != run.StateHalted || got.Err == "" {
		t.Errorf("run-1 result = %+v, want StateHalted with its own verify-timeout Err", got)
	}
	if got := result.Runs["run-2"]; got.State != run.StateHalted || got.Err != trippedState.StopLineReason {
		t.Errorf("run-2 result = %+v, want StateHalted with the stop-line reason %q (never dispatched)", got, trippedState.StopLineReason)
	}
}

// TestRepositoryOwnerWorkflowResumesAfterResetStopLineSignal proves an
// attributed ResetStopLineSignal clears a tripped stop line and lets
// queued work resume — the one durable recovery path for every stop-line
// trip condition (recurring infra failures, a quarantined run, or a
// committed-then-failed child), none of which have any other way to
// resume once tripped.
func TestRepositoryOwnerWorkflowResumesAfterResetStopLineSignal(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(_ context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
			if input.Ticket == "run-1" {
				attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
				return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType, attempts)
			}
			// run-2 must actually succeed once unblocked, so its own
			// result distinguishes "the reset worked, run-2 executed"
			// from "the reset didn't work, run-2 was halted by a stale
			// stop line" — both would otherwise leave StopLineTripped
			// true again if run-2 also failed.
			return BuildActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
		},
		func(_ context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
			if input.Ticket == "run-1" {
				t.Fatal("verify must not run after run-1's build Activity infrastructure failure")
			}
			return VerifyActivityResult{Result: runner.Result{Command: []string{input.Ticket}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	repository := "fixture/reset-stop-line"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	pinNoActivityRetries(env)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
	}, 0)
	env.RegisterDelayedCallback(func() {
		// A reset with no attribution must be silently ignored — the same
		// fail-closed requirement run.ApplyOverride enforces — and must
		// not itself unblock run-2 below. Delayed well past run-1's own
		// completion so the stop line is already tripped before either
		// reset attempt below runs.
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{})
	}, 100*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		// Mirrors the real CLI flow (resetStopLineMain): query the current
		// generation immediately before signaling, exactly as an operator
		// would, rather than assuming which value it landed on.
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		var current RepositoryOwnerResult
		if err := queryResult.Get(&current); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "operator@example.com", Reason: "confirmed workspace clean", Generation: current.StopLineGeneration,
		})
	}, 200*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 300*time.Millisecond)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              time.Second,
		StopLineFailureThreshold: 1,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.StopLineTripped {
		t.Error("StopLineTripped = true, want false: the attributed reset must have cleared it")
	}
	if result.SystemicFailureStreak != 0 {
		t.Errorf("SystemicFailureStreak = %d, want 0 after the reset", result.SystemicFailureStreak)
	}
	if got := result.Runs["run-1"]; got.State != run.StateHalted {
		t.Errorf("run-1 result = %+v, want StateHalted (its own infra failure)", got)
	}
	if got := result.Runs["run-2"]; got.State != run.StateAccepted {
		t.Errorf("run-2 result = %+v, want StateAccepted: it must have actually run after the reset, not been halted by a stop line that should already be clear", got)
	}
	if len(result.StopLineResets) != 1 || result.StopLineResets[0].By != "operator@example.com" || result.StopLineResets[0].Reason != "confirmed workspace clean" {
		t.Fatalf("StopLineResets = %+v, want exactly one attributed entry (the unattributed reset must not have been recorded)", result.StopLineResets)
	}
	if result.StopLineResets[0].At == "" {
		t.Error("StopLineResets[0].At is empty, want a timestamp")
	}
}

// TestRepositoryOwnerWorkflowIgnoresResetWhileNotTripped is the regression
// test for a real P2 finding from review: an attributed reset used to
// clear SystemicFailureStreak unconditionally, even when the stop line
// wasn't tripped at all — issuing it after one infrastructure failure but
// before the configured consecutive-failure threshold silently erased
// that in-progress streak (real safety evidence) and could prevent the
// stop line from ever tripping. A threshold of 3 here means run-1's
// single failure alone must never trip it; the reset arriving in that
// state must be a no-op, not recorded, and must not interfere with run-2
// and run-3 (both also infra failures) still accumulating toward the real
// trip.
func TestRepositoryOwnerWorkflowIgnoresResetWhileNotTripped(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
			return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType, attempts)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify must not run after a build Activity infrastructure failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{}, nil
		},
	)
	repository := "fixture/reset-while-not-tripped"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	pinNoActivityRetries(env)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
	}, 0)
	env.RegisterDelayedCallback(func() {
		// Not tripped yet (threshold is 3, this is the first failure) —
		// generation 0 correctly matches the untripped StopLineGeneration
		// zero value, so this exercises the StopLineTripped check
		// specifically, not the generation check.
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{By: "operator@example.com", Reason: "premature reset attempt"})
	}, 100*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 200*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-3", Input: fixtureInputForTicket("run-3")})
	}, 300*time.Millisecond)
	// A tripped owner now stays open indefinitely (see the idle-completion
	// fix in workflow.go) rather than completing on its own — capture the
	// tripped state via a query (asserted below, before this cleanup reset
	// itself adds an entry to StopLineResets) and then reset it so this
	// test's own GetWorkflowResult actually returns.
	var trippedState RepositoryOwnerResult
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		if err := queryResult.Get(&trippedState); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "test cleanup so the workflow completes", Generation: trippedState.StopLineGeneration,
		})
	}, 500*time.Millisecond)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              time.Second,
		StopLineFailureThreshold: 3,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if !trippedState.StopLineTripped {
		t.Error("StopLineTripped = false, want true: three consecutive failures must still trip the line despite the premature reset attempt")
	}
	if trippedState.SystemicFailureStreak != 3 {
		t.Errorf("SystemicFailureStreak = %d, want 3: the premature reset must not have erased the in-progress streak", trippedState.SystemicFailureStreak)
	}
	if len(trippedState.StopLineResets) != 0 {
		t.Errorf("StopLineResets = %+v, want none: a reset issued while untripped must not be recorded as accepted", trippedState.StopLineResets)
	}
}

// TestRepositoryOwnerWorkflowRejectsStaleGenerationReset is the regression
// test for a real P1 finding from review: a reset signal sent while a
// child is still executing stays buffered (the owner is blocked on
// childFuture.Get, not receiving signals) — if that same child then
// commits and fails, tripping the stop line for a reason the human who
// sent the reset never observed, the next loop iteration's unconditional
// drain must not apply that stale authorization and clear the brand-new
// trip. Generation 0 (captured before any trip existed) must be rejected
// once the line has since tripped to generation 1.
func TestRepositoryOwnerWorkflowRejectsStaleGenerationReset(t *testing.T) {
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			attempts := []run.Attempt{{Kind: "build", ExitCode: -1}}
			return BuildActivityResult{Attempts: attempts}, temporal.NewApplicationError("worker unavailable", InfrastructureFailureType, attempts)
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			t.Fatal("verify must not run after a build Activity infrastructure failure")
			return VerifyActivityResult{}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{}, nil
		},
	)
	repository := "fixture/stale-generation-reset"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	pinNoActivityRetries(env)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("run-1")})
	}, 0)
	env.RegisterDelayedCallback(func() {
		// Generation 0 — the zero value, as if captured before this
		// repository's stop line had ever tripped — sent well after run-1
		// has already tripped it to generation 1.
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{By: "operator@example.com", Reason: "stale authorization", Generation: 0})
	}, 100*time.Millisecond)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-2", Input: fixtureInputForTicket("run-2")})
	}, 200*time.Millisecond)
	// A tripped owner now stays open indefinitely (see the idle-completion
	// fix in workflow.go) rather than completing on its own — capture the
	// tripped state via a query (asserted below, before this cleanup reset
	// itself adds an entry to StopLineResets) and then reset it so this
	// test's own GetWorkflowResult actually returns.
	var trippedState RepositoryOwnerResult
	env.RegisterDelayedCallback(func() {
		queryResult, err := env.QueryWorkflow(RepositoryOwnerQueryName)
		if err != nil {
			t.Fatalf("query repository owner: %v", err)
		}
		if err := queryResult.Get(&trippedState); err != nil {
			t.Fatalf("decode query result: %v", err)
		}
		env.SignalWorkflow(ResetStopLineSignalName, ResetStopLineSignal{
			By: "test", Reason: "test cleanup so the workflow completes", Generation: trippedState.StopLineGeneration,
		})
	}, 300*time.Millisecond)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:               repository,
		IdleTimeout:              time.Second,
		StopLineFailureThreshold: 1,
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RepositoryOwnerResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if !trippedState.StopLineTripped {
		t.Error("StopLineTripped = false, want true: the stale-generation reset must have been rejected")
	}
	if trippedState.StopLineGeneration != 1 {
		t.Errorf("StopLineGeneration = %d, want 1", trippedState.StopLineGeneration)
	}
	if len(trippedState.StopLineResets) != 0 {
		t.Errorf("StopLineResets = %+v, want none: a stale-generation reset must not be recorded as accepted", trippedState.StopLineResets)
	}
	if got := result.Runs["run-2"]; got.Err != trippedState.StopLineReason {
		t.Errorf("run-2 result = %+v, want it halted by the still-tripped stop line", got)
	}
}

// TestRepositoryOwnerWorkflowPropagatesCancellation is the regression test
// for a real finding from review: canceling the repository owner's own
// context while it was waiting on a child used to be caught by the same
// error branch as a child execution failure, converting the owner's own
// cancellation into a per-request halted result and letting the loop
// continue (or the owner complete successfully) instead of reporting the
// cancellation. The build Activity here blocks on its own context so the
// child is still in flight when the owner is canceled.
func TestRepositoryOwnerWorkflowPropagatesCancellation(t *testing.T) {
	build := func(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
		<-ctx.Done()
		return BuildActivityResult{}, ctx.Err()
	}
	verifyCalled := false
	verify := func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
		verifyCalled = true
		return VerifyActivityResult{}, nil
	}
	gate := func(context.Context, EvaluateGateInput) (run.GateResult, error) {
		return run.GateResult{}, nil
	}
	env := newWorkflowEnvironment(t, build, verify, gate)
	repository := "fixture/repository"
	env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: RepositoryOwnerWorkflowID(repository)})
	env.RegisterWorkflow(RunWorkflow)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SubmitRunSignalName, SubmitRunSignal{RequestID: "run-1", Input: fixtureInputForTicket("ticket-1")})
	}, 0)
	env.RegisterDelayedCallback(func() {
		env.CancelWorkflow()
	}, time.Millisecond)

	env.ExecuteWorkflow(RepositoryOwnerWorkflow, RepositoryOwnerWorkflowInput{
		Repository:  repository,
		IdleTimeout: time.Second,
	})

	err := env.GetWorkflowError()
	if err == nil {
		t.Fatal("workflow error = nil, want the owner's own cancellation to be reported, not swallowed")
	}
	if verifyCalled {
		t.Error("verify Activity ran; build should have been canceled before completing")
	}
}
