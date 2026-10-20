package workflow

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// A step whose heartbeat responses are lost on the way back (the server
// records every heartbeat, the worker hears Unavailable for a few seconds)
// is cancelled by the Temporal SDK when one heartbeat RPC runs out of
// retries, although the server never lost it. Under GuardStep it finishes.

const stalledStepActivity = "StalledStepActivity"

// stalledStep heartbeats every 100 ms for stepLength; guarded runs it under
// GuardStep.
type stalledStep struct {
	guarded    bool
	stepLength time.Duration
	cancelled  atomic.Bool
}

func (s *stalledStep) Run(ctx context.Context) (string, error) {
	if s.guarded {
		var end context.CancelFunc
		ctx, end = GuardStep(ctx)
		defer end()
	}
	start := time.Now()
	for time.Since(start) < s.stepLength {
		activity.RecordHeartbeat(ctx, "working")
		select {
		case <-ctx.Done():
			s.cancelled.Store(true)
			return "", fmt.Errorf("the step was cancelled: %w", context.Cause(ctx))
		case <-time.After(100 * time.Millisecond):
		}
	}
	return "finished", nil
}

func stalledStepWorkflow(ctx temporalworkflow.Context) (string, error) {
	ctx = temporalworkflow.WithActivityOptions(ctx, temporalworkflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		HeartbeatTimeout:    3 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	})
	var out string
	err := temporalworkflow.ExecuteActivity(ctx, stalledStepActivity).Get(ctx, &out)
	return out, err
}

// dialLosingHeartbeatResponses dials the test Temporal server through a gRPC
// interceptor that, from `from` to `until` after start, sends every
// RecordActivityTaskHeartbeat to the server and answers its caller
// Unavailable.
func dialLosingHeartbeatResponses(t *testing.T, start time.Time, from, until time.Duration) client.Client {
	t.Helper()
	address := testTemporalAddress()
	if address == "" {
		t.Skipf("Temporal server at %s is unreachable: the variable is not set (make verify and make verify-live set it to the test Temporal server they start)", testTemporalAddressEnv)
	}
	lose := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		err := invoker(ctx, method, req, reply, cc, opts...)
		if err == nil && strings.HasSuffix(method, "/RecordActivityTaskHeartbeat") {
			if since := time.Since(start); since >= from && since < until {
				return status.Error(codes.Unavailable, "the response was lost")
			}
		}
		return err
	}
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelDial()
	c, err := client.DialContext(dialCtx, client.Options{
		HostPort:          address,
		ConnectionOptions: client.ConnectionOptions{DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(lose)}},
	})
	if err != nil {
		t.Fatalf("test Temporal server at %s does not answer: %v", address, err)
	}
	t.Cleanup(c.Close)
	return c
}

func runStalledStep(t *testing.T, guarded bool) (result string, runErr error, step *stalledStep) {
	t.Helper()
	start := time.Now()
	// The SDK sends a heartbeat RPC at most every 0.8 x the heartbeat timeout
	// (2.4 s) and gives each that long: the window covers a whole RPC.
	c := dialLosingHeartbeatResponses(t, start, 500*time.Millisecond, 4500*time.Millisecond)
	step = &stalledStep{guarded: guarded, stepLength: 8 * time.Second}
	taskQueue := fmt.Sprintf("stalled-step-%d", time.Now().UnixNano())
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(stalledStepWorkflow)
	w.RegisterActivityWithOptions(step.Run, activity.RegisterOptions{Name: stalledStepActivity})
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	t.Cleanup(w.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	exec, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: taskQueue, TaskQueue: taskQueue}, stalledStepWorkflow)
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	runErr = exec.Get(ctx, &result)
	return result, runErr, step
}

// The control: without the guard the SDK cancels the step, which shows the
// injected fault is the one that kills a build.
func TestAnUnguardedStepIsCancelledByAHeartbeatThatRunsOutOfRetries(t *testing.T) {
	_, runErr, step := runStalledStep(t, false)
	if !step.cancelled.Load() {
		t.Fatalf("the step was not cancelled by lost heartbeat responses (workflow error: %v): the fault injected nothing", runErr)
	}
}

func TestAGuardedStepIsNotCancelledByAHeartbeatThatRunsOutOfRetries(t *testing.T) {
	result, runErr, step := runStalledStep(t, true)
	if runErr != nil || result != "finished" || step.cancelled.Load() {
		t.Fatalf("result %q, error %v, cancelled %v; want the step to finish", result, runErr, step.cancelled.Load())
	}
}
