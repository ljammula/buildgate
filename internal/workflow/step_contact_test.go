package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTemporalContactLostReadsTheContextCause(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		lost  bool
	}{
		"gRPC Unavailable":                 {status.Error(codes.Unavailable, "connection refused"), true},
		"gRPC Unknown":                     {status.Error(codes.Unknown, "reset"), true},
		"gRPC DeadlineExceeded":            {status.Error(codes.DeadlineExceeded, "slow"), true},
		"the SDK's Unavailable":            {serviceerror.NewUnavailable("down"), true},
		"the SDK's DeadlineExceeded":       {serviceerror.NewDeadlineExceeded("context deadline exceeded"), true},
		"the SDK's PermissionDenied":       {serviceerror.NewPermissionDenied("no", ""), false},
		"a context deadline":               {context.DeadlineExceeded, true},
		"the guard's own cause":            {&ContactLostError{Cause: context.DeadlineExceeded, Silent: time.Minute}, true},
		"the server asked for a cancel":    {temporal.NewCanceledError(), false},
		"the server no longer knows it":    {serviceerror.NewNotFound("activity"), false},
		"the namespace is not found":       {serviceerror.NewNamespaceNotFound("ns"), false},
		"an activity paused":               {activity.ErrActivityPaused, false},
		"an activity reset":                {activity.ErrActivityReset, false},
		"the worker shutting down":         {temporalworker.ErrWorkerShutdown, false},
		"a plain cancel from the root":     {context.Canceled, false},
		"a permission error from the wire": {status.Error(codes.PermissionDenied, "no"), false},
		"no cause":                         {nil, false},
	} {
		if got := TemporalContactLost(tc.cause); got != tc.lost {
			t.Errorf("%s: TemporalContactLost = %v, want %v", name, got, tc.lost)
		}
	}
}

// sdkCancelled is a context the SDK has cancelled with cause, as its
// heartbeat does.
func sdkCancelled(cause error) context.Context {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	return ctx
}

func TestAHeartbeatTransportFailureDoesNotCancelTheStep(t *testing.T) {
	var beats atomic.Int32
	g := stepGuard{name: "RunBuildActivity", timeout: time.Minute, interval: 5 * time.Millisecond,
		heartbeat: func(context.Context) (heartbeatAnswer, error) { beats.Add(1); return heartbeatAnswer{}, nil }}
	step, end := guardStep(sdkCancelled(status.Error(codes.Unavailable, "connection refused")), g)
	defer end()

	deadline := time.Now().Add(10 * time.Second)
	for beats.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if beats.Load() < 3 {
		t.Fatalf("heartbeats = %d, want the step to keep heartbeating after the failure", beats.Load())
	}
	if err := step.Err(); err != nil {
		t.Fatalf("the step was cancelled by a transport failure: %v (cause %v)", err, context.Cause(step))
	}
}

func TestAStepSurvivesTheServerFailingTheHeartbeatsForLessThanTheTimeout(t *testing.T) {
	var beats atomic.Int32
	g := stepGuard{name: "RunBuildActivity", timeout: 2 * time.Second, interval: 5 * time.Millisecond,
		heartbeat: func(context.Context) (heartbeatAnswer, error) {
			if beats.Add(1) <= 5 {
				return heartbeatAnswer{}, status.Error(codes.Unavailable, "down")
			}
			return heartbeatAnswer{}, nil
		}}
	step, end := guardStep(sdkCancelled(status.Error(codes.Unavailable, "down")), g)
	defer end()
	for beats.Load() < 10 {
		time.Sleep(time.Millisecond)
	}
	if err := step.Err(); err != nil {
		t.Fatalf("the step was cancelled during a short stall: %v (cause %v)", err, context.Cause(step))
	}
}

func TestNoSuccessfulHeartbeatForTheTimeoutCancelsTheStep(t *testing.T) {
	unavailable := status.Error(codes.Unavailable, "connection refused")
	g := stepGuard{name: "RunBuildActivity", timeout: 60 * time.Millisecond, interval: 5 * time.Millisecond,
		heartbeat: func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{}, unavailable }}
	step, end := guardStep(sdkCancelled(unavailable), g)
	defer end()

	select {
	case <-step.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the step was never cancelled")
	}
	var lost *ContactLostError
	if cause := context.Cause(step); !errors.As(cause, &lost) || !errors.Is(cause, unavailable) {
		t.Fatalf("cause = %v, want a ContactLostError carrying the transport error", cause)
	}
	if lost.Silent < 60*time.Millisecond {
		t.Errorf("silent = %s, want at least the timeout", lost.Silent)
	}
	if !TemporalContactLost(context.Cause(step)) {
		t.Error("the guard's own cause is not read as a lost Temporal")
	}
}

func TestAServerAnswerThatEndsTheStepEndsItAtOnce(t *testing.T) {
	alive := func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }
	for name, tc := range map[string]struct {
		sdkCause  error
		heartbeat func(context.Context) (heartbeatAnswer, error)
		check     func(error) bool
	}{
		"a cancel the SDK heard": {temporal.NewCanceledError(), alive, temporal.IsCanceledError},
		"the activity not found": {serviceerror.NewNotFound("a"), alive, func(err error) bool {
			var nf *serviceerror.NotFound
			return errors.As(err, &nf)
		}},
		"the worker shutting down": {temporalworker.ErrWorkerShutdown, alive, func(err error) bool { return errors.Is(err, temporalworker.ErrWorkerShutdown) }},
		"a cancel from the root":   {context.Canceled, alive, func(err error) bool { return errors.Is(err, context.Canceled) }},
		"a cancel in a heartbeat answer after a stall": {status.Error(codes.Unavailable, "down"),
			func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{CancelRequested: true}, nil }, temporal.IsCanceledError},
		"a pause in a heartbeat answer after a stall": {status.Error(codes.Unavailable, "down"),
			func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{Paused: true}, nil },
			func(err error) bool { return errors.Is(err, activity.ErrActivityPaused) }},
		"the activity gone after a stall": {status.Error(codes.Unavailable, "down"),
			func(context.Context) (heartbeatAnswer, error) {
				return heartbeatAnswer{}, serviceerror.NewNotFound("a")
			},
			func(err error) bool {
				var nf *serviceerror.NotFound
				return errors.As(err, &nf)
			}},
	} {
		g := stepGuard{name: "x", timeout: time.Minute, interval: 5 * time.Millisecond, heartbeat: tc.heartbeat}
		step, end := guardStep(sdkCancelled(tc.sdkCause), g)
		select {
		case <-step.Done():
			if cause := context.Cause(step); !tc.check(cause) {
				t.Errorf("%s: cause = %v", name, cause)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: the step was not cancelled", name)
		}
		end()
	}
}

func TestAStepWithoutAHeartbeatTimeoutIsCancelledLikeBefore(t *testing.T) {
	g := stepGuard{name: "x", timeout: 0, interval: 5 * time.Millisecond,
		heartbeat: func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }}
	step, end := guardStep(sdkCancelled(status.Error(codes.Unavailable, "down")), g)
	defer end()
	select {
	case <-step.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a step with no heartbeat timeout outlived its cancelled context")
	}
}

func TestAStepKeepsTheActivitysValuesAndDeadlineButNotItsCancellation(t *testing.T) {
	type key struct{}
	parent, cancelParent := context.WithCancelCause(context.WithValue(context.Background(), key{}, "v"))
	deadline := time.Now().Add(time.Hour)
	parent, cancelDeadline := context.WithDeadline(parent, deadline)
	defer cancelDeadline()
	g := stepGuard{name: "x", timeout: time.Minute, interval: time.Hour,
		heartbeat: func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }}
	step, end := guardStep(parent, g)
	defer end()
	cancelParent(status.Error(codes.Unavailable, "down"))
	time.Sleep(20 * time.Millisecond)

	if step.Value(key{}) != "v" {
		t.Error("the step lost the activity context's values")
	}
	if got, ok := step.Deadline(); !ok || !got.Equal(deadline) {
		t.Errorf("deadline = %v (%v), want the activity's %v", got, ok, deadline)
	}
	if step.Err() != nil {
		t.Errorf("the step ended with its parent: %v", step.Err())
	}
}

func TestEndingTheGuardStopsItsWatcher(t *testing.T) {
	g := stepGuard{name: "x", timeout: time.Minute, interval: 5 * time.Millisecond,
		heartbeat: func(context.Context) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }}
	step, end := guardStep(context.Background(), g)
	end()
	if step.Err() == nil {
		t.Fatal("the step outlived its guard")
	}
}

// Called outside an Activity (a direct call in a test), GuardStep is a no-op.
func TestGuardStepOutsideAnActivityReturnsTheContext(t *testing.T) {
	ctx := context.Background()
	got, end := GuardStep(ctx)
	defer end()
	if got != ctx {
		t.Fatal("GuardStep changed a context that is not an activity's")
	}
}
