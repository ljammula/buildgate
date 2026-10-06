package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"go.temporal.io/sdk/client"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/hostcontrol"
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
// a command saved the request's new state. It does nothing (nil) unless
// dataDir's heartbeat shows a live `factoryd worker`: worker and a data dir
// with no daemon pick requests up from request.json themselves. Signal-with-
// start adopts a request submitted before the worker took over. a boundary method
// so tests can stub it.
func (impl realTemporal) wakeRequest(ctx context.Context, dataDir, requestID string) error {
	return wakeRequestWorkflowWith(impl.dp, ctx, temporalDialer{}, dataDir, requestID)
}

// wakeRequestWorkflowWith is temporal.wakeRequest reaching Temporal through
// dialer.
func wakeRequestWorkflowWith(dp *deps, ctx context.Context, dialer requestWorkflowDialer, dataDir, requestID string) error {
	ctx, cancel := context.WithTimeout(ctx, requestWakeTimeout)
	defer cancel()
	hb, err := daemonheartbeat.Read(workerHeartbeatPath(dataDir))
	if err != nil || !hb.Worker() || daemonheartbeat.Stale(hb, time.Now(), workerStaleAfter) || !hostcontrol.AliveFactoryd(dp, hb.PID) {
		return nil
	}
	jobs, light, err := requestTaskQueues(dataDir)
	if err != nil {
		return err
	}
	starter, closeFn, err := dialer.DialRequestWorkflowStarter(ctx, hb.TemporalAddress)
	if err != nil {
		return fmt.Errorf("reach Temporal at %s: %w", hb.TemporalAddress, err)
	}
	defer closeFn()
	return startRequestWorkflow(ctx, starter, requestID, jobs, light)
}

// requestWakeTimeout bounds one wake (dial plus signal-with-start), so an
// unhealthy Temporal never stalls the command or API call that saved the
// decision.
const requestWakeTimeout = 5 * time.Second

// warnWakeFailed tells the operator a decision is saved but the worker was
// not told.
func warnWakeFailed(w io.Writer, requestID string, err error) {
	fmt.Fprintf(w, "warning: request %s is saved, but the worker could not be woken (%v); it picks the request up at its next start.\n", requestID, err)
}

// wakeAfterSave wakes requestID's workflow and reports a failure on w, never
// returning it: the command's own result is already saved.
func wakeAfterSave(dp *deps, ctx context.Context, w io.Writer, dataDir, requestID string) {
	if err := dp.temporal.wakeRequest(ctx, dataDir, requestID); err != nil {
		warnWakeFailed(w, requestID, err)
	}
}
