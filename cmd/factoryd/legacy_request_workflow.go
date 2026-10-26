package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"

	"buildgate/internal/request"
	"buildgate/internal/workflow"
)

// This file holds everything that deals with a request workflow started under
// workflow.LegacyRequestWorkflowID. Delete it, with its callers, once no data
// dir can hold a request whose workflow started before ids carried the queue id.

// workflowDescriber is the slice of client.Client that reads a workflow.
type workflowDescriber interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// legacyRequestWorkflowOnQueue is the one home of "this request's legacy
// workflow is still running on this data dir's light queue". It returns the
// workflow's description when so. A workflow Temporal does not know, a closed
// one, or one on another data dir's queue (the same legacy id, another
// data dir's request) is not an answer: false, nil error.
func legacyRequestWorkflowOnQueue(ctx context.Context, c workflowDescriber, requestID, lightQueue string) (*workflowservice.DescribeWorkflowExecutionResponse, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	desc, err := c.DescribeWorkflowExecution(ctx, workflow.LegacyRequestWorkflowID(requestID), "")
	var notFound *serviceerror.NotFound
	if errors.As(err, &notFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("look up workflow %s: %w", workflow.LegacyRequestWorkflowID(requestID), err)
	}
	info := desc.GetWorkflowExecutionInfo()
	if info.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING || info.GetTaskQueue() != lightQueue {
		return nil, false, nil
	}
	return desc, true, nil
}

// requestWorkflowIDIn is the id of requestID's workflow in the data dir whose
// queue id is queueID; a data dir with no queue id has only legacy ids.
func requestWorkflowIDIn(queueID, requestID string) string {
	if queueID == "" {
		return workflow.LegacyRequestWorkflowID(requestID)
	}
	return workflow.RequestWorkflowID(queueID, requestID)
}

// stepInFlight reports whether desc has an AdvanceRequest step started and not
// finished: a model job or build the workflow's worker was running.
func stepInFlight(desc *workflowservice.DescribeWorkflowExecutionResponse) bool {
	for _, a := range desc.GetPendingActivities() {
		if a.GetActivityType().GetName() == workflow.AdvanceRequestActivityName && a.GetState() == enumspb.PENDING_ACTIVITY_STATE_STARTED {
			return true
		}
	}
	return false
}

// retireLegacyRequestWorkflows ends each unfinished request's workflow that
// still has its legacy id on lightQueue, so the request is driven once, by the
// workflow startRequestWorkflows starts. request.json is the source of truth
// and RequestWorkflow keeps no request state, so nothing is lost, except a
// step in flight: spec, oracle and plan drafting jobs leave no run record for
// reclaimLostWork, so halt puts such a request in resume_review first (the
// lost-step path, HaltLostRequestStep) and a human decides, as they would
// had the old workflow timed the step out.
//
// A request whose workflow cannot be looked up, halted or ended is logged and
// returned in the skip set: it is left alone and not started this time, as a
// new workflow beside the old one would drive it twice. Only a failure to list
// the requests is an error.
func retireLegacyRequestWorkflows(ctx context.Context, c workflowTerminator, dataDir, lightQueue string, halt func(requestID, state string) error) (skip map[string]bool, err error) {
	requests, err := request.List(dataDir)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	skip = map[string]bool{}
	for _, r := range requests {
		if r.State.Finished() {
			continue
		}
		if err := retireLegacyRequestWorkflow(ctx, c, r, lightQueue, halt); err != nil {
			log.Printf("factoryd worker: request %s: %v; its old workflow is left running", r.ID, err)
			skip[r.ID] = true
		}
	}
	return skip, nil
}

func retireLegacyRequestWorkflow(ctx context.Context, c workflowTerminator, r *request.Request, lightQueue string, halt func(requestID, state string) error) error {
	desc, onQueue, err := legacyRequestWorkflowOnQueue(ctx, c, r.ID, lightQueue)
	if err != nil || !onQueue {
		return err
	}
	if stepInFlight(desc) {
		if err := halt(r.ID, string(r.State)); err != nil {
			return fmt.Errorf("halt its lost step: %w", err)
		}
	}
	if _, err := terminateRunningWorkflow(ctx, c, workflow.LegacyRequestWorkflowID(r.ID), "", "replaced by a workflow id that names its data dir"); err != nil {
		return fmt.Errorf("end its old workflow: %w", err)
	}
	return nil
}
