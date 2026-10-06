package main

import (
	"context"
	"errors"
	"log"
	"time"

	"go.temporal.io/api/serviceerror"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// lostRunOf is the run a request's lost step was running: the current
// ticket's recorded run while building, none for any other step.
func lostRunOf(dataDir string, r *request.Request) string {
	if r.State != request.StateBuilding {
		return ""
	}
	// TicketIndex 0 means ticket 1, as in advanceBuilding.
	ticket, err := requestdriver.TicketAt(r, max(r.TicketIndex, 1))
	if err != nil {
		return ""
	}
	return keptLostRun(dataDir, ticket.RunID)
}

// keptLostRun is runID when its record exists, is non-accepted and was kept
// for a resume, else "": the ticket's recorded run can be a previous
// attempt's (retry keeps it) when the step was lost before the new run
// existed, and such a run has nothing to continue.
func keptLostRun(dataDir, runID string) string {
	if runID == "" {
		return ""
	}
	rec, err := run.Load(dataDir, runID)
	if err != nil || !rec.KeptForResume || rec.State == run.StateAccepted {
		return ""
	}
	return runID
}

// haltRequestsOfLostRuns moves each request whose step was running one of
// runs, the runs reclaimDeadOwnerRuns just halted, to resume_review and
// returns their ids: a request still building that run as its current ticket,
// or in pr_review (the run was a corrective round). A request that has moved
// on is left alone. Best-effort, like the reclaim itself: failures are logged.
func haltRequestsOfLostRuns(dataDir string, runs []*run.Run) []string {
	var halted []string
	for _, lost := range runs {
		if lost.RequestID == "" {
			continue
		}
		err := func() error {
			unlock, err := request.Lock(dataDir, lost.RequestID)
			if err != nil {
				return err
			}
			defer unlock()
			r, err := request.Load(dataDir, lost.RequestID)
			if err != nil {
				return err
			}
			switch r.State {
			case request.StateBuilding:
				// TicketIndex 0 means ticket 1, as in advanceBuilding.
				ticket, err := requestdriver.TicketAt(r, max(r.TicketIndex, 1))
				if err != nil || ticket.RunID != lost.ID {
					return nil
				}
			case request.StatePRReview:
			default:
				return nil
			}
			if err := requestdriver.EnterResumeReview(dataDir, r, keptLostRun(dataDir, lost.ID), time.Now()); err != nil {
				return err
			}
			halted = append(halted, r.ID)
			return nil
		}()
		if err != nil {
			log.Printf("factoryd worker: request %s of lost run %s: %v", lost.RequestID, lost.ID, err)
		}
	}
	return halted
}

// requestWorkflowTerminator is the slice of client.Client
// terminateRequestWorkflows needs, so tests can stub it.
type requestWorkflowTerminator interface {
	TerminateWorkflow(ctx context.Context, workflowID, runID, reason string, details ...interface{}) error
}

// terminateRequestWorkflows ends the RequestWorkflow of each request id. A
// workflow that is already closed or never existed is fine; other failures
// are logged.
func terminateRequestWorkflows(ctx context.Context, c requestWorkflowTerminator, ids []string) {
	for _, id := range ids {
		err := c.TerminateWorkflow(ctx, workflow.RequestWorkflowID(id), "", "request halted at worker start: its step was lost")
		var notFound *serviceerror.NotFound
		if err != nil && !errors.As(err, &notFound) {
			log.Printf("factoryd worker: end workflow of halted request %s: %v", id, err)
		}
	}
}
