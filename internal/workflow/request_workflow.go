package workflow

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// RequestWorkflow drives one request (internal/request) from submitted to a
// terminal state, one step at a time. Its id is the request id
// (RequestWorkflowID), so a request has at most one driver.
//
// request.json stays the source of truth: every iteration starts with
// LoadRequestStep, which reads it and says what to do next, and every step
// (AdvanceRequest) saves its own outcome there under request.Lock. The
// workflow holds no request state of its own, so a human decision written by
// the CLI or console between iterations is seen by the next LoadRequestStep;
// the decision verbs send RequestWakeSignalName to end a wait early.
//
//	loop
//	  step := LoadRequestStep
//	  step.Done               -> return
//	  step.Advance == jobs    -> AdvanceRequest on the jobs task queue
//	  step.Advance == light   -> AdvanceRequest on the light task queue
//	  the step changed state  -> next iteration at once
//	  step.Wait > 0           -> wait for a wake signal or step.Wait;
//	                             on timeout with step.Remind: RemindRequest
//	  no advance, Wait == 0   -> wait for a wake signal only
//
// A jobs-queue AdvanceRequest runs once (MaximumAttempts 1): a model job or
// build that is lost mid-flight (its heartbeat stops because the worker died)
// is never silently rerun; HaltLostRequestStep halts the request with a reason
// the operator acts on.
func RequestWorkflow(ctx temporalworkflow.Context, input RequestWorkflowInput) error {
	if input.RequestID == "" || input.JobsTaskQueue == "" || input.LightTaskQueue == "" {
		return temporal.NewNonRetryableApplicationError("request workflow input needs a request id and both task queues", "InvalidInput", nil)
	}
	wake := temporalworkflow.GetSignalChannel(ctx, RequestWakeSignalName)
	lightCtx := temporalworkflow.WithActivityOptions(ctx, requestLightActivityOptions(input.LightTaskQueue))
	for {
		// Every decision a pending wake signal announced is already in the
		// request.json LoadRequestStep is about to read; drop those signals
		// so they do not cut the next wait short. Drained before the load,
		// never after: a signal sent while the load runs must still end the
		// wait that follows.
		drainWake(wake)
		var step RequestStep
		if err := temporalworkflow.ExecuteActivity(lightCtx, LoadRequestStepActivityName, input.RequestID).Get(ctx, &step); err != nil {
			return fmt.Errorf("load request %s: %w", input.RequestID, err)
		}
		if step.Done {
			return nil
		}
		advanceFailed := false
		switch step.Advance {
		case "":
		case RequestAdvanceJobs, RequestAdvanceLight:
			var after string
			err := temporalworkflow.ExecuteActivity(
				temporalworkflow.WithActivityOptions(ctx, requestAdvanceActivityOptions(input, step.Advance)),
				AdvanceRequestActivityName, input.RequestID,
			).Get(ctx, &after)
			if err == nil && after != step.State {
				// The step moved the request on: go straight to the next
				// step rather than waiting out this state's timer.
				continue
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				advanceFailed = true
				if step.Advance == RequestAdvanceJobs && lostActivity(err) {
					if haltErr := temporalworkflow.ExecuteActivity(lightCtx, HaltLostRequestStepActivityName, HaltLostRequestStepInput{
						RequestID: input.RequestID, State: step.State,
					}).Get(ctx, nil); haltErr != nil {
						return fmt.Errorf("halt request %s after a lost step: %w", input.RequestID, haltErr)
					}
					continue
				}
				temporalworkflow.GetLogger(ctx).Warn("request step failed; retrying after a pause", "request", input.RequestID, "state", step.State, "error", err)
			}
		default:
			return temporal.NewNonRetryableApplicationError(fmt.Sprintf("unknown advance queue %q", step.Advance), "InvalidStep", nil)
		}
		wait := step.Wait
		if advanceFailed && step.RetryAfter > 0 {
			wait = step.RetryAfter
		}
		if step.Advance == "" || wait > 0 {
			woken, err := waitForWake(ctx, wake, wait)
			if err != nil {
				return err
			}
			if !woken && step.Remind {
				if err := temporalworkflow.ExecuteActivity(lightCtx, RemindRequestActivityName, input.RequestID).Get(ctx, nil); err != nil {
					return fmt.Errorf("remind request %s: %w", input.RequestID, err)
				}
			}
		}
		if temporalworkflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			// Pending wake signals need no carrying: the new execution starts
			// with LoadRequestStep, which reads every decision they announced.
			return temporalworkflow.NewContinueAsNewError(ctx, RequestWorkflow, input)
		}
	}
}

const (
	// RequestWorkflowName is the registered name of RequestWorkflow.
	RequestWorkflowName = "RequestWorkflow"
	// RequestWakeSignalName ends a RequestWorkflow wait early: sent after a
	// decision (approve, reject, send back, retry, amend scope, cancel) is
	// written to request.json.
	RequestWakeSignalName = "request-wake"

	// LoadRequestStepActivityName reads request.json and returns a RequestStep.
	LoadRequestStepActivityName = "LoadRequestStep"
	// AdvanceRequestActivityName runs one request step and saves its outcome.
	AdvanceRequestActivityName = "AdvanceRequest"
	// RemindRequestActivityName sends a due review reminder.
	RemindRequestActivityName = "RemindRequest"
	// HaltLostRequestStepActivityName halts a request whose step was lost.
	HaltLostRequestStepActivityName = "HaltLostRequestStep"

	// RequestAdvanceJobs runs the step on the jobs task queue (model jobs and
	// builds, capped by the worker's max_parallel_jobs).
	RequestAdvanceJobs = "jobs"
	// RequestAdvanceLight runs the step on the uncapped light task queue.
	RequestAdvanceLight = "light"

	// requestStepHeartbeatTimeout is how long a step may go without a
	// heartbeat before Temporal declares it lost; AdvanceRequest heartbeats
	// far more often than this.
	requestStepHeartbeatTimeout = 2 * time.Minute
	// requestStepStartToClose bounds one step. A ticket build carries its own
	// round and verify timeouts; this only stops a step that hangs forever.
	requestStepStartToClose = 48 * time.Hour
)

// RequestWorkflowID is the workflow id of requestID's RequestWorkflow.
func RequestWorkflowID(requestID string) string {
	return "factoryd-request-" + requestID
}

// RequestWorkflowInput names the request and the worker's two task queues.
type RequestWorkflowInput struct {
	RequestID      string
	JobsTaskQueue  string
	LightTaskQueue string
}

// RequestStep is LoadRequestStep's answer: what RequestWorkflow does next.
type RequestStep struct {
	// State is the request state the step was read in, for logs and halt
	// reasons.
	State string
	// Done ends the workflow: the request is terminal or gone.
	Done bool
	// Advance is RequestAdvanceJobs, RequestAdvanceLight, or "" for no step.
	Advance string
	// Wait, after the step (or instead of one), waits this long for a wake
	// signal. Zero with no step waits for the signal only.
	Wait time.Duration
	// Remind runs RemindRequest when Wait times out.
	Remind bool
	// RetryAfter replaces Wait after a step that failed, so a failing step is
	// retried at the poll rate rather than in a tight loop.
	RetryAfter time.Duration
}

// HaltLostRequestStepInput is HaltLostRequestStep's input. The request is
// halted only if it is still in State.
type HaltLostRequestStepInput struct {
	RequestID string
	State     string
}

func requestLightActivityOptions(taskQueue string) temporalworkflow.ActivityOptions {
	return temporalworkflow.ActivityOptions{
		TaskQueue:           taskQueue,
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
		},
	}
}

func requestAdvanceActivityOptions(input RequestWorkflowInput, advance string) temporalworkflow.ActivityOptions {
	taskQueue := input.LightTaskQueue
	if advance == RequestAdvanceJobs {
		taskQueue = input.JobsTaskQueue
	}
	return temporalworkflow.ActivityOptions{
		TaskQueue:           taskQueue,
		StartToCloseTimeout: requestStepStartToClose,
		HeartbeatTimeout:    requestStepHeartbeatTimeout,
		WaitForCancellation: true,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1},
	}
}

// lostActivity reports whether err means the step stopped without
// finishing (its worker went away), as opposed to a step that ran and
// returned an error.
func lostActivity(err error) bool {
	var timeoutErr *temporal.TimeoutError
	if !errors.As(err, &timeoutErr) {
		return false
	}
	switch timeoutErr.TimeoutType() {
	case enumspb.TIMEOUT_TYPE_HEARTBEAT, enumspb.TIMEOUT_TYPE_START_TO_CLOSE:
		return true
	}
	return false
}

func drainWake(wake temporalworkflow.ReceiveChannel) {
	for wake.ReceiveAsync(nil) {
	}
}

// waitForWake blocks until a wake signal arrives (true) or, when timeout is
// positive, until it elapses (false).
func waitForWake(ctx temporalworkflow.Context, wake temporalworkflow.ReceiveChannel, timeout time.Duration) (bool, error) {
	woken := false
	selector := temporalworkflow.NewSelector(ctx)
	selector.AddReceive(wake, func(c temporalworkflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		woken = true
	})
	if timeout > 0 {
		timerCtx, cancelTimer := temporalworkflow.WithCancel(ctx)
		defer cancelTimer()
		selector.AddFuture(temporalworkflow.NewTimer(timerCtx, timeout), func(temporalworkflow.Future) {})
	}
	selector.Select(ctx)
	if !woken && ctx.Err() != nil {
		return false, ctx.Err()
	}
	return woken, nil
}
