package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"buildgate/internal/run"
	"buildgate/internal/unresolved"
	"buildgate/internal/workflow"
)

// terminateClient answers TerminateWorkflow with errs[i] on its i-th call,
// then nil; the rest of client.Client is not used.
type terminateClient struct {
	client.Client
	errs  []error
	calls int
}

func (c *terminateClient) TerminateWorkflow(context.Context, string, string, string, ...interface{}) error {
	c.calls++
	if c.calls <= len(c.errs) {
		return c.errs[c.calls-1]
	}
	return nil
}

func TestATerminateThatMeetsATransportFailureIsTriedAgain(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "connection refused")
	c := &terminateClient{errs: []error{unavailable, context.DeadlineExceeded}}
	if err := terminateRunWorkflow(c, "wf", "run", "why", time.Minute, time.Millisecond); err != nil {
		t.Fatalf("terminate = %v, want nil once the server answers", err)
	}
	if c.calls != 3 {
		t.Fatalf("TerminateWorkflow calls = %d, want 3", c.calls)
	}
}

func TestATerminateStopsWhenItsBudgetIsSpent(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "connection refused")
	c := &terminateClient{errs: []error{unavailable, unavailable, unavailable, unavailable, unavailable, unavailable, unavailable, unavailable}}
	started := time.Now()
	err := terminateRunWorkflow(c, "wf", "run", "why", 50*time.Millisecond, 5*time.Millisecond)
	if !errors.Is(err, unavailable) {
		t.Fatalf("terminate = %v, want the last transport error", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("terminate took %s, want about its 50ms budget", elapsed)
	}
}

func TestATerminateAnswerIsNeverTriedAgain(t *testing.T) {
	for name, answer := range map[string]error{
		"another failure": errors.New("permission denied"),
		"no budget":       status.Error(codes.Unavailable, "down"),
	} {
		c := &terminateClient{errs: []error{answer, answer}}
		budget := time.Minute
		if name == "no budget" {
			budget = 0
		}
		if err := terminateRunWorkflow(c, "wf", "run", "why", budget, time.Millisecond); err == nil || c.calls != 1 {
			t.Errorf("%s: terminate = %v after %d calls, want its error after one", name, err, c.calls)
		}
	}
	gone := &terminateClient{errs: []error{serviceerror.NewNotFound("gone")}}
	if err := terminateRunWorkflow(gone, "wf", "run", "why", time.Minute, time.Millisecond); err != nil {
		t.Errorf("terminate of a workflow already gone = %v, want nil", err)
	}
}

func TestAnOperatorsStopGetsNoTerminateRetry(t *testing.T) {
	lost := status.Error(codes.Unavailable, "down")
	for name, tc := range map[string]struct {
		processLost bool
		contactLost error
		retried     bool
	}{
		"operator stop":        {true, nil, false},
		"contact lost":         {true, lost, true},
		"supervisor timeout":   {false, nil, true},
		"wait failed on wires": {false, lost, true},
	} {
		if got := terminateBudget(tc.processLost, tc.contactLost) > 0; got != tc.retried {
			t.Errorf("%s: retried = %v, want %v", name, got, tc.retried)
		}
	}
}

// A real SIGINT to the process's own signal context reads as an operator's
// stop, not as a lost Temporal; so does the worker stopping, or a server
// answer. Only a transport cause, or the guard's own, is the connection.
func TestLostTemporalContactReadsTheContextCause(t *testing.T) {
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-signalCtx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the signal context never ended")
	}

	cancelledWith := func(cause error) context.Context {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		return ctx
	}
	unavailable := status.Error(codes.Unavailable, "connection refused")
	for name, tc := range map[string]struct {
		parent context.Context
		lost   bool
	}{
		"a real SIGINT":                   {signalCtx, false},
		"the worker's root cancelled":     {cancelledWith(context.Canceled), false},
		"a transport error":               {cancelledWith(unavailable), true},
		"a heartbeat deadline":            {cancelledWith(context.DeadlineExceeded), true},
		"the guard's own cause":           {cancelledWith(&workflow.ContactLostError{Cause: unavailable, Silent: time.Minute}), true},
		"the server asked for a cancel":   {cancelledWith(temporal.NewCanceledError()), false},
		"the server no longer knows it":   {cancelledWith(serviceerror.NewNotFound("activity")), false},
		"the worker shutting down":        {cancelledWith(temporalworker.ErrWorkerShutdown), false},
		"a cancel with no recorded cause": {cancelledWith(nil), false},
	} {
		runCtx, cancel := context.WithTimeout(tc.parent, time.Hour)
		got := lostTemporalContact(runCtx, buildLostWithProcess(runCtx.Err(), false), runCtx.Err(), nil)
		cancel()
		if (got != nil) != tc.lost {
			t.Errorf("%s: contact lost = %v, want %v", name, got, tc.lost)
		}
	}
}

func TestAWaitThatFailedOnTheConnectionIsALostContactButAFailedWorkflowIsNot(t *testing.T) {
	live := context.Background()
	if got := lostTemporalContact(live, false, nil, status.Error(codes.Unavailable, "down")); got == nil {
		t.Error("a wait that failed with Unavailable is not read as a lost contact")
	}
	failed := errors.Join(&temporal.WorkflowExecutionError{}, context.DeadlineExceeded)
	if got := lostTemporalContact(live, false, nil, failed); got != nil {
		t.Errorf("a failed workflow whose error carries a deadline was read as a lost contact: %v", got)
	}
}

func TestAWaitOnAServerThatDoesNotAnswerIsWaitedOutWithinItsBudget(t *testing.T) {
	dp := newTestDeps(t)
	stubWorkflowStillRunning(dp, t, false)
	old := waitContactBudget
	waitContactBudget = time.Minute
	t.Cleanup(func() { waitContactBudget = old })
	down := status.Error(codes.Unavailable, "connection refused")
	wf := &fakeWorkflowRun{errs: []error{down, down, context.DeadlineExceeded}}
	var result workflow.RunWorkflowResult
	if err := waitForRunWorkflowWithProgress(dp, context.Background(), nil, wf, "r", &result); err != nil {
		t.Fatalf("wait = %v, want nil once the server answers", err)
	}
	if wf.calls != 4 {
		t.Fatalf("Get calls = %d, want 4", wf.calls)
	}
}

func TestAWaitOnAServerThatNeverAnswersEndsWithItsError(t *testing.T) {
	dp := newTestDeps(t)
	stubWorkflowStillRunning(dp, t, false)
	old := waitContactBudget
	waitContactBudget = 30 * time.Millisecond
	t.Cleanup(func() { waitContactBudget = old })
	down := status.Error(codes.Unavailable, "connection refused")
	errs := make([]error, 100000)
	for i := range errs {
		errs[i] = down
	}
	wf := &fakeWorkflowRun{errs: errs}
	var result workflow.RunWorkflowResult
	if err := waitForRunWorkflowWithProgress(dp, context.Background(), nil, wf, "r", &result); !errors.Is(err, down) {
		t.Fatalf("wait = %v, want the connection error after the budget", err)
	}
}

func TestAHaltThatLostTemporalIsNamedAndNothingElseIs(t *testing.T) {
	r := &run.Run{}
	recordContactLoss(r, nil)
	if r.HaltReasonCode != "" || r.HaltError != "" {
		t.Fatalf("a halt that kept contact was named: %+v", r)
	}
	recordContactLoss(r, status.Error(codes.Unavailable, "down"))
	if r.HaltReasonCode != string(unresolved.TemporalUnreachable) || r.HaltError == "" {
		t.Fatalf("code %q error %q, want temporal_unreachable and a sentence", r.HaltReasonCode, r.HaltError)
	}
}

func TestAnUnconfirmedHaltStillKeepsTheWorktreeTheMarkerNames(t *testing.T) {
	checkpoints := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	writeIsolationMarker(t, checkpoints, worktree, "factoryd/r")

	path, branch, kept := keptWorktreeIfUnconfirmed(checkpoints, true, false, "", "", false)
	if path != worktree || branch != "factoryd/r" || !kept {
		t.Fatalf("got %q %q %v, want the marker's worktree kept", path, branch, kept)
	}
	for name, tc := range map[string]struct{ wanted, confirmed bool }{
		"nothing to keep":   {false, false},
		"halt is confirmed": {true, true},
	} {
		if p, _, k := keptWorktreeIfUnconfirmed(checkpoints, tc.wanted, tc.confirmed, "", "", false); p != "" || k {
			t.Errorf("%s: kept %q (%v), want nothing", name, p, k)
		}
	}
	if p, _, k := keptWorktreeIfUnconfirmed(t.TempDir(), true, false, "", "", false); p != "" || k {
		t.Errorf("no marker: kept %q (%v), want nothing", p, k)
	}
}

// writeIsolationMarker writes the marker PrepareIsolatedWorkspaceActivity
// leaves beside the run's checkpoints.
func writeIsolationMarker(t *testing.T, checkpointDir, worktree, branch string) {
	t.Helper()
	b, err := json.Marshal(map[string]any{"repo_dir": filepath.Dir(worktree), "worktree_path": worktree, "branch": branch})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkpointDir, "isolated-workspace.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The operator's "cancelled (SIGINT/SIGTERM)" line is only for a stop that
// Temporal did not make; a lost server and a worker shutting down say so.
func TestARunLostLogNamesTheRealCause(t *testing.T) {
	down := status.Error(codes.Unavailable, "down")
	for name, tc := range map[string]struct {
		contactLost error
		cause       error
		want        string
	}{
		"a signal to the process": {nil, errors.New("interrupt signal received"), "operator cancelled this run"},
		"the root cancelled":      {nil, context.Canceled, "operator cancelled this run"},
		"the worker shut down":    {nil, temporalworker.ErrWorkerShutdown, "stopped by its Temporal activity context"},
		"the server's cancel":     {nil, temporal.NewCanceledError(), "stopped by its Temporal activity context"},
		"the server lost":         {down, down, "lost contact with the Temporal server"},
	} {
		var out strings.Builder
		log.SetOutput(&out)
		logRunLost("r1", true, tc.contactLost, tc.cause)
		log.SetOutput(os.Stderr)
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: logged %q, want %q", name, out.String(), tc.want)
		}
		if tc.want != "operator cancelled this run" && strings.Contains(out.String(), "operator cancelled") {
			t.Errorf("%s: blamed the operator: %q", name, out.String())
		}
	}
}
