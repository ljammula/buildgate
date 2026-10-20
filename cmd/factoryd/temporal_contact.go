package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/run"
	"buildgate/internal/unresolved"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// A stall of the connection to the Temporal server is not a verdict on a
// build. The helpers here tell it from an operator's stop, ride out a short
// one, and name a long one (unresolved.TemporalUnreachable) so the run's
// worktree is kept and its request waits for a human.

// waitContactBudget is how long a wait on a run's workflow goes on after the
// server stopped answering it; a var so tests need not wait it out.
var waitContactBudget = 2 * time.Minute

// terminateContactBudget is how long a terminate of a run's workflow is
// retried while the server does not answer.
const terminateContactBudget = 2 * time.Minute

// transportFailure reports whether err is a stalled or lost connection to
// the server, not the workflow's own outcome (a failed workflow's error also
// carries deadlines).
func transportFailure(err error) bool {
	var failed *temporal.WorkflowExecutionError
	return !errors.As(err, &failed) && workflow.TemporalContactLost(err)
}

// contactWithinBudget reports whether the server has been silent for less
// than waitContactBudget, starting the clock at the first call.
func contactWithinBudget(since *time.Time) bool {
	if since.IsZero() {
		*since = time.Now()
	}
	return time.Since(*since) < waitContactBudget
}

// lostTemporalContact returns the error that shows a run's wait ended
// because contact with the Temporal server was lost, nil when it did not: the
// run's context was cancelled with a transport cause (the step's heartbeats
// got no answer, workflow.GuardStep), or the wait itself failed on the
// connection while its context was live. A signal, a worker stopping and a
// server answer are not.
func lostTemporalContact(runCtx context.Context, processLost bool, ctxErr, waitErr error) error {
	if processLost {
		if cause := context.Cause(runCtx); workflow.TemporalContactLost(cause) {
			return cause
		}
		return nil
	}
	if ctxErr == nil && transportFailure(waitErr) {
		return waitErr
	}
	return nil
}

// logRunLost says why the run's wait ended, with the real cause.
// The operator's wording is for any cancellation Temporal did not make (a
// signal to this process); the worker stopping or an answer from the server
// is quoted.
func logRunLost(id string, processLost bool, contactLost, cause error) {
	switch {
	case contactLost != nil:
		log.Printf("run %s: lost contact with the Temporal server (%v); the run is halted and its worktree kept for a decision", id, contactLost)
	case processLost && workflow.StoppedByTemporal(cause):
		log.Printf("run %s: stopped by its Temporal activity context (%v); the run is halted and its worktree kept for a decision", id, cause)
	case processLost:
		log.Printf("run %s: operator cancelled this run (SIGINT/SIGTERM)", id)
	}
}

// recordContactLoss names a halt that lost the Temporal server: the cause
// the request reads, and the sentence the operator reads.
func recordContactLoss(r *run.Run, contactLost error) {
	if contactLost == nil {
		return
	}
	r.HaltReasonCode = string(unresolved.TemporalUnreachable)
	r.HaltError = fmt.Sprintf("lost contact with the Temporal server: %v", contactLost)
}

// terminateBudget is how long a terminate is retried while the server does
// not answer: not at all for an operator's own stop, who is waiting.
func terminateBudget(processLost bool, contactLost error) time.Duration {
	if processLost && contactLost == nil {
		return 0
	}
	return terminateContactBudget
}

// terminateRunWorkflow terminates the workflow and reports no error when the
// execution is already gone. A transport failure is tried again, doubling the
// wait from backoff to 15 s, until budget has passed.
func terminateRunWorkflow(c client.Client, workflowID, runID, reason string, budget, backoff time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := c.TerminateWorkflow(ctx, workflowID, runID, reason)
		cancel()
		var notFound *serviceerror.NotFound
		if err == nil || errors.As(err, &notFound) {
			return nil
		}
		if !transportFailure(err) || time.Now().Add(backoff).After(deadline) {
			return err
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 15*time.Second)
	}
}

// keptWorktreeIfUnconfirmed recovers, from the marker the isolated
// workspace wrote beside the run's checkpoints, the worktree of a lost run
// whose halt could not be confirmed against the server (it was not Described
// as terminated, so none was recovered). Nothing else needs the server: the
// worktree is kept for the human's decision, as a confirmed one is. path and
// branch are what was already recovered, kept whether that kept it.
func keptWorktreeIfUnconfirmed(checkpointDir string, keepWanted, haltConfirmed bool, path, branch string, kept bool) (string, string, bool) {
	if !keepWanted || haltConfirmed || path != "" {
		return path, branch, kept
	}
	_, worktreePath, worktreeBranch, _ := workflow.RecoverIsolatedWorkspaceFromCheckpointDir(checkpointDir)
	if worktreePath == "" || !worktreeExists(worktreePath) {
		return path, branch, kept
	}
	log.Printf("keeping isolated workspace %s: its run was lost, and the workflow could not be confirmed stopped", worktreePath)
	if err := wsisolation.DisableWorkerGroupWrite(worktreePath); err != nil {
		log.Printf("failed to revoke worker group write on kept worktree %s: %v", worktreePath, err)
	}
	return worktreePath, worktreeBranch, true
}
