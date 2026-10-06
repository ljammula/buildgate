package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"

	"buildgate/internal/request"
	"buildgate/internal/run"
)

// stubWorkflowTerminator records TerminateWorkflow calls and reports every
// workflow in running as Running.
type stubWorkflowTerminator struct {
	running    map[string]bool
	terminated []string
}

func (s *stubWorkflowTerminator) DescribeWorkflowExecution(_ context.Context, workflowID, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	if _, ok := s.running[workflowID]; !ok {
		return nil, serviceerror.NewNotFound("no such workflow")
	}
	status := enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED
	if s.running[workflowID] {
		status = enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
	}
	return &workflowservice.DescribeWorkflowExecutionResponse{
		WorkflowExecutionInfo: &workflowpb.WorkflowExecutionInfo{Status: status},
	}, nil
}

func (s *stubWorkflowTerminator) TerminateWorkflow(_ context.Context, workflowID, _, _ string, _ ...interface{}) error {
	s.terminated = append(s.terminated, workflowID)
	return nil
}

// deadPID returns the PID of a process that has already exited.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run true: %v", err)
	}
	return cmd.ProcessState.Pid()
}

func seedOwnedRun(t *testing.T, dataDir, id string, state run.State, ownerPID int, workflowID string) {
	t.Helper()
	r := &run.Run{
		ID: id, State: state, CreatedAt: time.Now().Format(time.RFC3339Nano),
		TemporalWorkflowID: workflowID, TemporalAddress: "stub:7233",
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run %s: %v", id, err)
	}
	marker := filepath.Join(run.Dir(dataDir, id), "sandbox-owner.pid")
	if err := os.WriteFile(marker, []byte(strconv.Itoa(ownerPID)), 0o600); err != nil {
		t.Fatalf("seed owner marker %s: %v", id, err)
	}
}

// fakeReclaimDocker writes a docker stand-in that lists the containers in
// the returned psFile (one "name<TAB>runid" per line) for every `ps`, logs
// every invocation to the returned logFile, and succeeds at everything else.
func fakeReclaimDocker(t *testing.T) (docker, psFile, logFile string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	psFile = filepath.Join(dir, "ps.txt")
	logFile = filepath.Join(dir, "calls.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logFile + "\n" +
		"if [ \"$1\" = ps ]; then cat " + psFile + "; fi\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return docker, psFile, logFile
}

// TestReclaimDeadOwnerRunsHaltsOnlyDeadOwnerRun proves the worker start
// reclaim touches exactly the nonterminal run whose owner process is gone:
// halted with the triage line, its containers rm -f'd (its Temporal
// workflow terminated first when still Running) -- while a run owned by a
// live pid and a terminal run keep their records, containers and workflows.
func TestReclaimDeadOwnerRunsHaltsOnlyDeadOwnerRun(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	docker, psFile, logFile := fakeReclaimDocker(t)
	stub := &stubWorkflowTerminator{running: map[string]bool{"wf-dead": false, "wf-live": true, "wf-done": false}}
	orig := fakeTemporalOf(dp).dialTerminatorFn
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return stub, func() {}, nil }
	t.Cleanup(func() { fakeTemporalOf(dp).dialTerminatorFn = orig })

	seedOwnedRun(t, dataDir, "run-dead", run.StateSliceRunning, deadPID(t), "wf-dead")
	seedOwnedRun(t, dataDir, "run-live", run.StateSliceRunning, os.Getpid(), "wf-live")
	seedOwnedRun(t, dataDir, "run-done", run.StateAccepted, deadPID(t), "wf-done")
	containers := "factoryd-sandbox-dead\trun-dead\n" +
		"factoryd-relay-container-dead\trun-dead\n" +
		"factoryd-sandbox-live\trun-live\n" +
		"factoryd-relay-container-live\trun-live\n"
	if err := os.WriteFile(psFile, []byte(containers), 0o600); err != nil {
		t.Fatal(err)
	}

	reclaimDeadOwnerRuns(dp, context.Background(), dataDir, docker, "")

	dead, err := run.Load(dataDir, "run-dead")
	if err != nil {
		t.Fatal(err)
	}
	if dead.State != run.StateHalted || !dead.HaltConfirmed {
		t.Errorf("dead-owner run = %s halt_confirmed=%v, want halted and confirmed", dead.State, dead.HaltConfirmed)
	}
	if dead.Triage != deadOwnerTriage || !strings.Contains(dead.Triage, "factoryd process running this build stopped mid-run") {
		t.Errorf("dead-owner run triage = %q, want %q", dead.Triage, deadOwnerTriage)
	}
	if live, _ := run.Load(dataDir, "run-live"); live.State != run.StateSliceRunning {
		t.Errorf("live-owner run state = %s, want untouched slice_running", live.State)
	}
	if done, _ := run.Load(dataDir, "run-done"); done.State != run.StateAccepted {
		t.Errorf("terminal run state = %s, want untouched accepted", done.State)
	}
	if len(stub.terminated) != 0 {
		t.Errorf("terminated workflows = %v, want none (a Running workflow is resumed, a closed one needs no terminate)", stub.terminated)
	}

	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "rm -f ") {
			removed = append(removed, strings.TrimPrefix(line, "rm -f "))
		}
	}
	for _, name := range removed {
		if !strings.HasSuffix(name, "-dead") {
			t.Errorf("rm -f %s: not a dead-owner container (all rm calls: %v)", name, removed)
		}
	}
	for _, want := range []string{"factoryd-sandbox-dead", "factoryd-relay-container-dead"} {
		found := false
		for _, name := range removed {
			found = found || name == want
		}
		if !found {
			t.Errorf("container %s was not removed (rm calls: %v)", want, removed)
		}
	}
}

// TestReclaimDeadOwnerRunsLeavesRunWhenWorkflowCannotBeTerminated proves a
// run whose Temporal workflow cannot be reached stays as it is, so a later
// start retries instead of confirming a halt while the workflow may run on.
func TestReclaimDeadOwnerRunsLeavesRunWhenWorkflowCannotBeTerminated(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	orig := fakeTemporalOf(dp).dialTerminatorFn
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) {
		return nil, nil, context.DeadlineExceeded
	}
	t.Cleanup(func() { fakeTemporalOf(dp).dialTerminatorFn = orig })
	seedOwnedRun(t, dataDir, "run-dead", run.StateSliceRunning, deadPID(t), "wf-dead")

	reclaimDeadOwnerRuns(dp, context.Background(), dataDir, "", "")

	if r, _ := run.Load(dataDir, "run-dead"); r.State != run.StateSliceRunning {
		t.Errorf("run state = %s, want slice_running (left for a later start)", r.State)
	}
}

// TestRunOwnerDeadNeedsAMarkerNamingADeadPID pins the liveness rule: only a
// marker whose pid is gone counts; no marker or a live pid does not.
func TestRunOwnerDeadNeedsAMarkerNamingADeadPID(t *testing.T) {
	dataDir := t.TempDir()
	seedOwnedRun(t, dataDir, "dead", run.StateSliceRunning, deadPID(t), "")
	seedOwnedRun(t, dataDir, "live", run.StateSliceRunning, os.Getpid(), "")
	bare := &run.Run{ID: "bare", State: run.StateSliceRunning, CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := bare.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{"dead": true, "live": false, "bare": false} {
		if got := runOwnerDead(dataDir, id); got != want {
			t.Errorf("runOwnerDead(%s) = %v, want %v", id, got, want)
		}
	}
}

// errDescribeTerminator fails every Describe with a non-NotFound error.
type errDescribeTerminator struct{ stubWorkflowTerminator }

func (errDescribeTerminator) DescribeWorkflowExecution(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return nil, context.DeadlineExceeded
}

// TestReclaimDeadOwnerRunsLeavesRunWhenDescribeFails proves an unreachable
// Temporal neither terminates a workflow nor halts its run.
func TestReclaimDeadOwnerRunsLeavesRunWhenDescribeFails(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, reqID := buildingFixture(dp, t, 1)
	stub := &errDescribeTerminator{}
	orig := fakeTemporalOf(dp).dialTerminatorFn
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return stub, func() {}, nil }
	t.Cleanup(func() { fakeTemporalOf(dp).dialTerminatorFn = orig })
	seedDriverOwnedRun(t, dataDir, reqID, "run-x", "wf-x")

	reclaimDeadOwnerRuns(dp, context.Background(), dataDir, "", "")

	if r, _ := run.Load(dataDir, "run-x"); r.State != run.StateSliceRunning {
		t.Errorf("run = state %s, want untouched slice_running", r.State)
	}
	if len(stub.terminated) != 0 {
		t.Errorf("terminated = %v, want none", stub.terminated)
	}
}

// seedDriverOwnedRun seeds a dead-owner slice_running run recorded as the
// first ticket's run of the building request reqID.
func seedDriverOwnedRun(t *testing.T, dataDir, reqID, runID, workflowID string) {
	t.Helper()
	seedOwnedRun(t, dataDir, runID, run.StateSliceRunning, deadPID(t), workflowID)
	r, err := run.Load(dataDir, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.RequestID = reqID
	r.TemporalRunID = "tr-" + runID
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	req, err := request.Load(dataDir, reqID)
	if err != nil {
		t.Fatal(err)
	}
	req.Tickets[0].RunID = runID
	if err := req.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// TestReclaimDeadOwnerRunsHaltsRunningWorkflowItsRequestStillBuilds proves a
// dead-owner run whose request is still building it is halted, its Running
// workflow terminated, and returned: the worker halts that request from the
// returned runs, never re-attaching to the lost build.
func TestReclaimDeadOwnerRunsHaltsRunningWorkflowItsRequestStillBuilds(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, reqID := buildingFixture(dp, t, 1)
	stub := &stubWorkflowTerminator{running: map[string]bool{"wf-w": true}}
	orig := fakeTemporalOf(dp).dialTerminatorFn
	fakeTemporalOf(dp).dialTerminatorFn = func(context.Context, string) (workflowTerminator, func(), error) { return stub, func() {}, nil }
	t.Cleanup(func() { fakeTemporalOf(dp).dialTerminatorFn = orig })
	seedDriverOwnedRun(t, dataDir, reqID, "run-w", "wf-w")

	halted := reclaimDeadOwnerRuns(dp, context.Background(), dataDir, "", "")

	if r, _ := run.Load(dataDir, "run-w"); r.State != run.StateHalted || !r.HaltConfirmed {
		t.Errorf("run = state %s halt_confirmed %v, want halted and confirmed", r.State, r.HaltConfirmed)
	}
	if len(stub.terminated) != 1 {
		t.Errorf("terminated = %v, want the run's workflow", stub.terminated)
	}
	if len(halted) != 1 || halted[0].ID != "run-w" {
		t.Errorf("returned runs = %v, want [run-w]", halted)
	}
}
