package main

import (
	"context"
	"errors"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// cancelWhenRequestCancelled returns ctx made to end, with cause
// request.ErrCancelled, as soon as request id is found cancelled: `factoryd
// cancel` and POST /requests/{id}/cancel only write the request's state, so a
// step already running (a build in its sandbox, a drafting job, a PR-review
// round) is stopped by this poll, every `every`. The returned func ends the
// poll and releases ctx; call it when the step returns.
func cancelWhenRequestCancelled(ctx context.Context, dataDir, id string, every time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if r, err := request.Load(dataDir, id); err == nil && r.State == request.StateCancelled {
					cancel(request.ErrCancelled)
					return
				}
			}
		}
	}()
	return ctx, func() {
		close(done)
		cancel(nil)
	}
}

// cancelledByOperator reports whether a run's wait ended because its request
// was cancelled (the cause cancelWhenRequestCancelled sets).
func cancelledByOperator(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), request.ErrCancelled)
}

// requestCancelledReason is the HaltError of a run stopped by its request's cancel.
const requestCancelledReason = "the request was cancelled while this run was building; nothing failed"

// recordRequestCancelled names a halt caused by the request's cancel: the code
// stats and triage count as an operator's end, not an infrastructure failure.
func recordRequestCancelled(r *run.Run, cancelled bool) {
	if !cancelled {
		return
	}
	r.HaltReasonCode = run.HaltReasonRequestCancelled
	r.HaltError = requestCancelledReason
}

// stopsBuildNow reports whether the build's Activity is cancelled with the
// workflow's termination: its process was told to stop, or its request was
// cancelled. Either way nobody is left to wait for the sandbox.
func stopsBuildNow(processLost, cancelled bool) bool {
	return processLost || cancelled
}

// keepsWorktree reports whether a halted run's worktree is kept for a human's
// decision. A cancelled request needs none: its worktree is rolled back.
func keepsWorktree(keepEligible, cancelled, processLost, resumed bool, contactLost error) bool {
	return keepEligible && !cancelled && (processLost || resumed || contactLost != nil)
}

// keptAtHalt reports whether the halted run record is flagged
// KeptForResume: a worktree was recovered, still exists, and the halt is one
// that keeps it (lost with its process, or a resumed run).
func keptAtHalt(keepEligible, cancelled bool, worktree string, keptForResume, resumed, stepLost bool) bool {
	return keepEligible && !cancelled && worktree != "" && (keptForResume || resumed || stepLost) && worktreeExists(worktree)
}
