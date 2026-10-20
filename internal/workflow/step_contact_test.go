package workflow

import (
	"context"
	"errors"
	"slices"
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
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) { beats.Add(1); return heartbeatAnswer{}, nil }}
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
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) {
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
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) { return heartbeatAnswer{}, unavailable }}
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
	alive := func(context.Context, []any) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }
	for name, tc := range map[string]struct {
		sdkCause  error
		heartbeat func(context.Context, []any) (heartbeatAnswer, error)
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
			func(context.Context, []any) (heartbeatAnswer, error) {
				return heartbeatAnswer{CancelRequested: true}, nil
			}, temporal.IsCanceledError},
		"a pause in a heartbeat answer after a stall": {status.Error(codes.Unavailable, "down"),
			func(context.Context, []any) (heartbeatAnswer, error) { return heartbeatAnswer{Paused: true}, nil },
			func(err error) bool { return errors.Is(err, activity.ErrActivityPaused) }},
		"the activity gone after a stall": {status.Error(codes.Unavailable, "down"),
			func(context.Context, []any) (heartbeatAnswer, error) {
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
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }}
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
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }}
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
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) { return heartbeatAnswer{}, nil }}
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

// The bound the guard gives, at a scale a test can run: the SDK's context is
// cancelled after two throttles of silence, the guard counts the silence from
// then minus those two throttles, and heartbeats of its own every interval.
// A stall that ends three intervals before the timeout loses nothing; one
// that outlasts the timeout stops the step with a ContactLostError. With the
// production numbers (timeout 2 min, interval 10 s) that is a stall of about
// 90 s.
func TestAStallShorterThanTheTimeoutLessThreeBeatsLosesNothingAndALongerOneParks(t *testing.T) {
	const timeout, interval, throttle = 600 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond
	run := func(stall time.Duration) (context.Context, context.CancelFunc) {
		start := time.Now()
		g := stepGuard{name: "RunBuildActivity", timeout: timeout, interval: interval, throttle: throttle,
			heartbeat: func(context.Context, []any) (heartbeatAnswer, error) {
				if time.Since(start) < stall {
					return heartbeatAnswer{}, status.Error(codes.Unavailable, "down")
				}
				return heartbeatAnswer{}, nil
			}}
		// The SDK notices after two throttles.
		sdk, cancel := context.WithCancelCause(context.Background())
		time.AfterFunc(2*throttle, func() { cancel(status.Error(codes.Unavailable, "down")) })
		return guardStep(sdk, g)
	}

	short, endShort := run(timeout - 3*interval)
	defer endShort()
	time.Sleep(timeout + 200*time.Millisecond)
	if short.Err() != nil {
		t.Errorf("a stall of timeout - 3 intervals stopped the step: %v", context.Cause(short))
	}

	long, endLong := run(timeout + 300*time.Millisecond)
	defer endLong()
	select {
	case <-long.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a stall longer than the timeout never stopped the step")
	}
	var lost *ContactLostError
	if !errors.As(context.Cause(long), &lost) || lost.Silent < timeout {
		t.Errorf("cause = %v, want a ContactLostError of at least the timeout", context.Cause(long))
	}
}

func TestAWorkerThatBeginsToStopStopsAStepThatIsRidingOutAStall(t *testing.T) {
	stop := make(chan struct{})
	g := stepGuard{name: "x", timeout: time.Minute, interval: 5 * time.Millisecond, workerStop: stop,
		heartbeat: func(context.Context, []any) (heartbeatAnswer, error) {
			return heartbeatAnswer{}, status.Error(codes.Unavailable, "down")
		}}
	step, end := guardStep(sdkCancelled(status.Error(codes.Unavailable, "down")), g)
	defer end()
	time.Sleep(30 * time.Millisecond)
	if step.Err() != nil {
		t.Fatalf("the step ended before the worker began to stop: %v", context.Cause(step))
	}
	close(stop)
	select {
	case <-step.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("a worker that began to stop did not stop the step")
	}
	if cause := context.Cause(step); !errors.Is(cause, temporalworker.ErrWorkerShutdown) {
		t.Errorf("cause = %v, want the worker-shutdown error", cause)
	}
}

func TestTheGuardsOwnHeartbeatsCarryTheStepsLastDetails(t *testing.T) {
	var got atomic.Value
	g := stepGuard{name: "x", timeout: time.Minute, interval: 5 * time.Millisecond,
		heartbeat: func(_ context.Context, details []any) (heartbeatAnswer, error) {
			got.Store(details)
			return heartbeatAnswer{}, nil
		}}
	// Details recorded before the stall are the ones sent after it.
	g.state = &stepState{}
	g.state.set([]any{"build", 42})
	step, end := guardStep(sdkCancelled(status.Error(codes.Unavailable, "down")), g)
	defer end()
	deadline := time.Now().Add(10 * time.Second)
	for got.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	details, _ := got.Load().([]any)
	if len(details) != 2 || details[0] != "build" {
		t.Fatalf("heartbeat details = %v, want the step's last", details)
	}
	if step.Value(guardedKey{}) != g.state {
		t.Error("the step's context does not carry its state")
	}
}

// A step cancelled for a lost Temporal is an Activity failure of its own
// type, which Temporal does not retry and which names the cause.
func TestAContactLostStepFailsWithItsOwnNonRetryableType(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&ContactLostError{Cause: context.DeadlineExceeded, Silent: time.Minute})
	err := stepErr(ctx, errors.New("run build: context canceled"))
	if got := launchErrorType(err); got != TemporalUnreachableFailureType {
		t.Fatalf("type = %q, want %q", got, TemporalUnreachableFailureType)
	}
	if got := UnresolvedCauseFromError(temporal.NewApplicationError("x", launchErrorType(err))); got != "temporal_unreachable" {
		t.Errorf("cause = %q, want temporal_unreachable", got)
	}
	if countsTowardStopLine(temporal.NewApplicationError("x", TemporalUnreachableFailureType)) {
		t.Error("a lost Temporal counts toward the stop line")
	}
	if !slices.Contains(nonRetryableActivityFailureTypes, TemporalUnreachableFailureType) {
		t.Error("the type is retried")
	}
	if got := stepErr(context.Background(), errors.New("x")); got.Error() != "x" {
		t.Errorf("an uncancelled step's error changed: %v", got)
	}
}
