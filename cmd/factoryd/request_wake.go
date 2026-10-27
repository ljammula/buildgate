package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/request"
)

// requestWorkflowDialer opens the Temporal client a wake signals through, so
// tests inject a fake.
type requestWorkflowDialer interface {
	DialRequestWorkflowStarter(ctx context.Context, address string) (requestWorkflowStarter, func(), error)
}

// temporalDialer dials a real Temporal server.
type temporalDialer struct{}

// DialRequestWorkflowStarter opens a Temporal client for address.
func (temporalDialer) DialRequestWorkflowStarter(ctx context.Context, address string) (requestWorkflowStarter, func(), error) {
	dialCtx, cancel := context.WithTimeout(ctx, hostcontrol.TemporalDialTimeout)
	defer cancel()
	c, err := client.DialContext(dialCtx, client.Options{HostPort: address})
	if err != nil {
		return nil, nil, err
	}
	return c, c.Close, nil
}

// wakeRequest starts requestID's RequestWorkflow, or wakes it, after
// a command saved the request's new state. Unless dataDir's heartbeat shows a
// live `factoryd worker` it returns the reason it did not signal; wakeAndMark
// then leaves the request's wake-pending marker, which a running worker
// retries every workerAdoptInterval and a starting worker clears. Signal-with-
// start adopts a request submitted before the worker took over. A boundary
// method so tests can stub it.
func (impl realTemporal) wakeRequest(ctx context.Context, dataDir, requestID string) error {
	return wakeRequestWorkflowWith(impl.dp, ctx, temporalDialer{}, dataDir, requestID)
}

// wakeRequestWorkflowWith is temporal.wakeRequest reaching Temporal through
// dialer.
func wakeRequestWorkflowWith(dp *deps, ctx context.Context, dialer requestWorkflowDialer, dataDir, requestID string) error {
	ctx, cancel := context.WithTimeout(ctx, requestWakeTimeout)
	defer cancel()
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	switch {
	case err != nil:
		return fmt.Errorf("no worker heartbeat in %s: %w", dataDir, err)
	case !hb.Worker():
		return errors.New("the data dir's daemon heartbeat is not a worker's")
	case daemonheartbeat.Stale(hb, time.Now(), workerStaleAfter):
		return errors.New("the worker's heartbeat is stale")
	case !hostcontrol.AliveFactoryd(dp, hb.PID):
		return fmt.Errorf("the worker (pid %d) is not running", hb.PID)
	}
	queueID, err := requestQueueID(dataDir)
	if err != nil {
		return err
	}
	jobs, light := taskQueuesFor(queueID)
	starter, closeFn, err := dialer.DialRequestWorkflowStarter(ctx, hb.TemporalAddress)
	if err != nil {
		return fmt.Errorf("reach Temporal at %s: %w", hb.TemporalAddress, err)
	}
	defer closeFn()
	return wakeOrStartRequestWorkflow(ctx, starter, queueID, requestID, jobs, light)
}

// requestWakeTimeout bounds one wake (dial plus signal-with-start), so an
// unhealthy Temporal never stalls the command or API call that saved the
// decision.
const requestWakeTimeout = 5 * time.Second

// warnWakeFailed tells the operator a decision is saved but the worker was
// not told.
func warnWakeFailed(w io.Writer, requestID string, err error) {
	fmt.Fprintf(w, "warning: request %s is saved, but the worker was not woken (%v); a running worker retries the wake within a minute.\n", requestID, err)
}

// wakeAndMark wakes requestID's workflow and, when the wake fails for any
// reason, leaves the request's wake-pending marker for a running worker to
// retry. The one place a save's wake is marked, for the commands and for serve.
func wakeAndMark(dp *deps, ctx context.Context, dataDir, requestID string) error {
	err := dp.temporal.wakeRequest(ctx, dataDir, requestID)
	if err != nil {
		if markErr := request.MarkWakePending(dataDir, requestID); markErr != nil {
			return fmt.Errorf("%w (and the wake could not be queued for retry: %v)", err, markErr)
		}
	}
	return err
}

// wakeAfterSave wakes requestID's workflow and reports a failure on w, never
// returning it: the command's own result is already saved.
func wakeAfterSave(dp *deps, ctx context.Context, w io.Writer, dataDir, requestID string) {
	if err := wakeAndMark(dp, ctx, dataDir, requestID); err != nil {
		warnWakeFailed(w, requestID, err)
	}
}
