package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeTemporalWorkflowLister is a temporalWorkflowLister that returns a
// fixed response or error, and records the query it was called with so
// tests can assert doctorCheckStaleTemporalWorkflowsUsing actually filters
// by ExecutionStatus/WorkflowType/StartTime rather than listing everything.
type fakeTemporalWorkflowLister struct {
	resp     *workflowservice.ListWorkflowExecutionsResponse
	err      error
	gotQuery string
}

func (f *fakeTemporalWorkflowLister) ListWorkflow(ctx context.Context, request *workflowservice.ListWorkflowExecutionsRequest) (*workflowservice.ListWorkflowExecutionsResponse, error) {
	f.gotQuery = request.Query
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func staleExecution(id, workflowType string, start time.Time) *workflowpb.WorkflowExecutionInfo {
	return &workflowpb.WorkflowExecutionInfo{
		Execution: &commonpb.WorkflowExecution{WorkflowId: id},
		Type:      &commonpb.WorkflowType{Name: workflowType},
		StartTime: timestamppb.New(start),
	}
}

func TestDoctorCheckStaleTemporalWorkflowsOkWhenNoneReturned(t *testing.T) {
	t.Parallel()
	lister := &fakeTemporalWorkflowLister{resp: &workflowservice.ListWorkflowExecutionsResponse{}}
	now := time.Now()
	check := doctorCheckStaleTemporalWorkflowsUsing(context.Background(), lister, "localhost:7233", now)
	if check.Err != nil {
		t.Fatalf("expected ok, got Err=%v", check.Err)
	}
	if !strings.Contains(lister.gotQuery, "ExecutionStatus = 'Running'") {
		t.Errorf("query missing ExecutionStatus filter: %q", lister.gotQuery)
	}
	for _, wantType := range staleTemporalWorkflowTypes {
		if !strings.Contains(lister.gotQuery, wantType) {
			t.Errorf("query missing workflow type %q: %q", wantType, lister.gotQuery)
		}
	}
}

func TestDoctorCheckStaleTemporalWorkflowsWarnsAndNamesEachOrphan(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-7 * 24 * time.Hour)
	lister := &fakeTemporalWorkflowLister{resp: &workflowservice.ListWorkflowExecutionsResponse{
		Executions: []*workflowpb.WorkflowExecutionInfo{
			staleExecution("repo-owner-abc123", "RepositoryOwnerWorkflow", old),
			staleExecution("repo-owner-abc123-run-1", "RunWorkflow", old),
		},
	}}
	check := doctorCheckStaleTemporalWorkflowsUsing(context.Background(), lister, "localhost:7233", now)
	if check.Err == nil {
		t.Fatalf("expected a warning, got ok")
	}
	if !check.Advisory {
		t.Errorf("expected Advisory=true (warn, never FAIL) -- doctor must never auto-terminate a workflow")
	}
	if !strings.Contains(check.Err.Error(), "2 Running execution") {
		t.Errorf("Err should name the count: %v", check.Err)
	}
	for _, id := range []string{"repo-owner-abc123", "repo-owner-abc123-run-1"} {
		if !strings.Contains(check.Fix, "--workflow-id "+id) {
			t.Errorf("Fix missing terminate command for %s: %q", id, check.Fix)
		}
	}
	if !strings.Contains(check.Fix, "temporal workflow terminate --address localhost:7233") {
		t.Errorf("Fix should name the exact CLI command: %q", check.Fix)
	}
}

func TestDoctorCheckStaleTemporalWorkflowsCapsFixLinesAtFive(t *testing.T) {
	t.Parallel()
	now := time.Now()
	old := now.Add(-7 * 24 * time.Hour)
	var executions []*workflowpb.WorkflowExecutionInfo
	for i := 0; i < 8; i++ {
		executions = append(executions, staleExecution(fmt.Sprintf("wf-%d", i), "RunWorkflow", old))
	}
	lister := &fakeTemporalWorkflowLister{resp: &workflowservice.ListWorkflowExecutionsResponse{Executions: executions}}
	check := doctorCheckStaleTemporalWorkflowsUsing(context.Background(), lister, "localhost:7233", now)
	if !strings.Contains(check.Err.Error(), "8 Running execution") {
		t.Errorf("Err should still report the full count 8: %v", check.Err)
	}
	if got := strings.Count(check.Fix, "temporal workflow terminate"); got != 5 {
		t.Errorf("Fix should list at most 5 terminate commands, got %d in %q", got, check.Fix)
	}
}

func TestDoctorCheckStaleTemporalWorkflowsWarnsNotFailsWhenListFails(t *testing.T) {
	t.Parallel()
	lister := &fakeTemporalWorkflowLister{err: errors.New("visibility not enabled")}
	check := doctorCheckStaleTemporalWorkflowsUsing(context.Background(), lister, "localhost:7233", time.Now())
	if check.Err == nil {
		t.Fatalf("expected an error surfaced as warn")
	}
	if !check.Advisory {
		t.Errorf("a failed visibility query is an environment limitation, not a FAIL -- expected Advisory=true")
	}
	if check.Fix == "" {
		t.Errorf("expected a Fix explaining advanced visibility is required")
	}
}
