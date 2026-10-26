package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"

	"buildgate/internal/workflow"
)

// This file exists because a request workflow started before ids carried the
// data dir's queue id keeps its id for as long as it runs. That is a migration
// of runtime state in Temporal, not a compatibility shim for a flag or key.
// Delete the file, its callers' legacy branch and their tests once no data dir
// can hold a request whose workflow started before that change.

// workflowDescriber is the slice of client.Client that reads a workflow.
type workflowDescriber interface {
	DescribeWorkflowExecution(ctx context.Context, workflowID, runID string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
}

// requestDriverWorkflowID is the one rule for which workflow drives requestID:
// its legacy workflow while that is running on this data dir's light queue
// (it keeps driving the request with the same workflow code, and ends when the
// request finishes), otherwise the workflow named by queueID. A workflow
// Temporal does not know, a closed one, and one on another data dir's queue
// (the same legacy id, another data dir's request) are not the driver. Any
// other Describe failure is an error: guessing could put two drivers on one
// request.
func requestDriverWorkflowID(ctx context.Context, c workflowDescriber, queueID, lightQueue, requestID string) (string, error) {
	legacy := workflow.LegacyRequestWorkflowID(requestID)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	desc, err := c.DescribeWorkflowExecution(ctx, legacy, "")
	var notFound *serviceerror.NotFound
	switch {
	case errors.As(err, &notFound):
		return workflow.RequestWorkflowID(queueID, requestID), nil
	case err != nil:
		return "", fmt.Errorf("look up workflow %s: %w", legacy, err)
	}
	info := desc.GetWorkflowExecutionInfo()
	if info.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING && info.GetTaskQueue() == lightQueue {
		return legacy, nil
	}
	return workflow.RequestWorkflowID(queueID, requestID), nil
}

// wakeOrStartRequestWorkflow wakes requestID's driver workflow: a signal to
// the legacy workflow while it drives the request, and otherwise a
// signal-with-start of the new id, which starts it when it is not running.
func wakeOrStartRequestWorkflow(ctx context.Context, c requestWorkflowStarter, queueID, requestID, jobsQueue, lightQueue string) error {
	id, err := requestDriverWorkflowID(ctx, c, queueID, lightQueue, requestID)
	if err != nil {
		return err
	}
	if id == workflow.LegacyRequestWorkflowID(requestID) {
		err := c.SignalWorkflow(ctx, id, "", workflow.RequestWakeSignalName, nil)
		var notFound *serviceerror.NotFound
		if err == nil {
			return nil
		}
		if !errors.As(err, &notFound) {
			return fmt.Errorf("wake workflow %s: %w", id, err)
		}
		// The legacy workflow finished between the lookup and the signal: the
		// new id's workflow reads the request, finds it done, and returns.
		id = workflow.RequestWorkflowID(queueID, requestID)
	}
	return startRequestWorkflow(ctx, c, id, requestID, jobsQueue, lightQueue)
}
