package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The Temporal SDK cancels an Activity's context when ONE heartbeat RPC fails
// with a retryable error, so a stall of the server's connection shorter than
// the step's heartbeat timeout would kill a build that lost nothing.
// GuardStep is the one place that changes that, for every Activity that runs
// a long step: the step runs under a context the SDK cannot cancel for a
// transport failure, and is cancelled only by what the server answered, by
// the worker stopping, or by this process hearing nothing from the server for
// a whole heartbeat timeout.

// stepContactInterval is how often a step whose SDK context was cancelled
// heartbeats by itself, to learn whether the server answers and what it says.
const stepContactInterval = 10 * time.Second

// ContactLostError is the cancellation cause of a step that heard nothing
// from the Temporal server, to a heartbeat of its own, for a whole heartbeat
// timeout.
type ContactLostError struct {
	// Cause is the last transport error.
	Cause error
	// Silent is how long no heartbeat succeeded.
	Silent time.Duration
}

func (e *ContactLostError) Error() string {
	return fmt.Sprintf("no heartbeat reached the Temporal server for %s: %v", e.Silent.Round(time.Second), e.Cause)
}

func (e *ContactLostError) Unwrap() error { return e.Cause }

// TemporalContactLost reports whether cause, the cancellation cause of a
// context a step ran under, says the step lost contact with the Temporal
// server: a ContactLostError, or a transport error (a gRPC Unavailable,
// Unknown or DeadlineExceeded status, a deadline, a network error) that a
// heartbeat's cancellation handed on. A cancellation by a server answer, a
// stopping worker, a signal or the operator is not.
func TemporalContactLost(cause error) bool {
	var lost *ContactLostError
	return errors.As(cause, &lost) || isTransportError(cause)
}

// StoppedByTemporal reports whether cause, the cancellation cause of a
// context a step ran under, is Temporal's own word that the step is over: the
// server's cancel request, a pause or reset, an Activity or namespace it no
// longer knows, or the worker shutting down.
func StoppedByTemporal(cause error) bool { return serverAnswered(cause) }

// isTransportError reports whether err is the kind a stalled connection
// produces, as opposed to an answer from the server.
func isTransportError(err error) bool {
	if err == nil || serverAnswered(err) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	// The SDK hands a gRPC failure on as the service error of its code.
	var st *status.Status
	var svc serviceerror.ServiceError
	if errors.As(err, &svc) {
		st = svc.Status()
	} else if s, ok := status.FromError(err); ok {
		st = s
	}
	if st == nil {
		return false
	}
	switch st.Code() {
	case codes.Unavailable, codes.Unknown, codes.DeadlineExceeded:
		return true
	}
	return false
}

// serverAnswered reports whether err is the server's own answer that the
// step is to stop: a cancel request, an Activity paused or reset, an
// Activity or namespace the server no longer knows.
func serverAnswered(err error) bool {
	var notFound *serviceerror.NotFound
	var nsInactive *serviceerror.NamespaceNotActive
	var nsNotFound *serviceerror.NamespaceNotFound
	return temporal.IsCanceledError(err) ||
		errors.Is(err, activity.ErrActivityPaused) || errors.Is(err, activity.ErrActivityReset) ||
		errors.Is(err, temporalworker.ErrWorkerShutdown) ||
		errors.As(err, &notFound) || errors.As(err, &nsInactive) || errors.As(err, &nsNotFound)
}

// guardedKey marks a context GuardStep made, which a step called inside
// another guarded step does not guard again.
type guardedKey struct{}

// heartbeatAnswer is what a heartbeat's response asks of the step.
type heartbeatAnswer struct {
	CancelRequested bool
	Paused          bool
	Reset           bool
}

// cause is the error to cancel the step with, nil when the answer asks
// nothing.
func (a heartbeatAnswer) cause() error {
	switch {
	case a.CancelRequested:
		return temporal.NewCanceledError()
	case a.Paused:
		return activity.ErrActivityPaused
	case a.Reset:
		return activity.ErrActivityReset
	}
	return nil
}

// stepGuard holds what GuardStep needs from the outside, so a test needs no
// Temporal server and no long wait.
type stepGuard struct {
	// name is the Activity type, for the log.
	name string
	// timeout is the heartbeat timeout of the Activity; zero means it has
	// none, and no stall is ridden out.
	timeout time.Duration
	// interval is how often a heartbeat is sent once the SDK's context is
	// cancelled.
	interval time.Duration
	// heartbeat sends one heartbeat to the server by itself.
	heartbeat func(ctx context.Context) (heartbeatAnswer, error)
}

// GuardStep returns the context a long Activity step runs under, and the
// function that ends the guard (defer it). Called with a context that is not
// an Activity's (a direct call in a test), it returns it unchanged.
func GuardStep(activityCtx context.Context) (context.Context, context.CancelFunc) {
	if !activity.IsActivity(activityCtx) || activityCtx.Value(guardedKey{}) != nil {
		return activityCtx, func() {}
	}
	info := activity.GetInfo(activityCtx)
	return guardStep(activityCtx, stepGuard{
		name:     info.ActivityType.Name,
		timeout:  info.HeartbeatTimeout,
		interval: stepContactInterval,
		heartbeat: func(ctx context.Context) (heartbeatAnswer, error) {
			resp, err := activity.GetClient(activityCtx).WorkflowService().RecordActivityTaskHeartbeat(ctx, &workflowservice.RecordActivityTaskHeartbeatRequest{
				TaskToken: info.TaskToken,
				Namespace: info.Namespace,
				Identity:  "factoryd step guard",
			})
			if err != nil {
				return heartbeatAnswer{}, err
			}
			return heartbeatAnswer{CancelRequested: resp.GetCancelRequested(), Paused: resp.GetActivityPaused(), Reset: resp.GetActivityReset()}, nil
		},
	})
}

func guardStep(activityCtx context.Context, g stepGuard) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(activityCtx)
	cancelDeadline := context.CancelFunc(func() {})
	if deadline, ok := activityCtx.Deadline(); ok {
		base, cancelDeadline = context.WithDeadline(base, deadline)
	}
	stepCtx, cancel := context.WithCancelCause(context.WithValue(base, guardedKey{}, true))
	go g.watch(activityCtx, stepCtx, cancel)
	return stepCtx, func() {
		cancel(context.Canceled)
		cancelDeadline()
	}
}

// watch ends the step when the SDK's context says so for a reason the step
// must obey, and rides out one it need not.
func (g stepGuard) watch(activityCtx, stepCtx context.Context, cancel context.CancelCauseFunc) {
	select {
	case <-stepCtx.Done():
		return
	case <-activityCtx.Done():
	}
	cause := context.Cause(activityCtx)
	if activityCtx.Err() == context.DeadlineExceeded || g.timeout <= 0 || !isTransportError(cause) {
		cancel(cause)
		return
	}
	log.Printf("%s: contact with the Temporal server failed (%v); the step goes on and heartbeats itself for up to %s", g.name, cause, g.timeout)
	g.rideOut(stepCtx, cancel, cause)
}

// rideOut heartbeats every interval until the step ends. A success restarts
// the clock; a server answer that asks the step to stop stops it; a whole
// timeout without a success stops it with a ContactLostError.
func (g stepGuard) rideOut(stepCtx context.Context, cancel context.CancelCauseFunc, lastErr error) {
	lastContact := time.Now()
	lost := true
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()
	for {
		beatCtx, cancelBeat := context.WithTimeout(stepCtx, g.interval)
		answer, err := g.heartbeat(beatCtx)
		cancelBeat()
		switch {
		case stepCtx.Err() != nil:
			return
		case err == nil:
			if stop := answer.cause(); stop != nil {
				cancel(stop)
				return
			}
			if lost {
				log.Printf("%s: contact with the Temporal server is back", g.name)
				lost = false
			}
			lastContact = time.Now()
		case serverAnswered(err):
			cancel(err)
			return
		default:
			lastErr, lost = err, true
			if silent := time.Since(lastContact); silent >= g.timeout {
				cancel(&ContactLostError{Cause: lastErr, Silent: silent})
				return
			}
		}
		select {
		case <-stepCtx.Done():
			return
		case <-ticker.C:
		}
	}
}
