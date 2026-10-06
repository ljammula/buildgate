package workflow

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

type leaseTestResult struct {
	Value string `json:"value"`
}

func leaseCheckpoint(value string) activityCheckpoint[leaseTestResult] {
	return activityCheckpoint[leaseTestResult]{Completed: true, WorkflowID: "wf", RunID: "run", ActivityID: "act", Result: leaseTestResult{Value: value}}
}

func TestStaleAttemptCheckpointSaveIsRefusedAfterLaterAttemptTakesLease(t *testing.T) {
	dir := t.TempDir()
	path := activityCheckpointPath(dir, "wf", "run", "act")
	if err := takeActivityLease(dir, "wf", "run", "act", 2); err != nil {
		t.Fatal(err)
	}
	if err := saveActivityCheckpoint(path, leaseCheckpoint("live"), 2); err != nil {
		t.Fatalf("live attempt save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	err = saveActivityCheckpoint(path, leaseCheckpoint("stale"), 1)
	if !errors.Is(err, errActivitySuperseded) {
		t.Fatalf("stale save err = %v, want errActivitySuperseded", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("stale save changed the live checkpoint:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCheckpointSaveWithoutLeaseWorksAtAttemptOne(t *testing.T) {
	dir := t.TempDir()
	path := activityCheckpointPath(dir, "wf", "run", "act")
	if err := saveActivityCheckpoint(path, leaseCheckpoint("only"), 1); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, _, found, err := loadActivityCheckpointForExecution[leaseTestResult](dir, "wf", "run", "act")
	if err != nil || !found || got.Result.Value != "only" {
		t.Fatalf("load = %+v found=%v err=%v", got, found, err)
	}
}

func TestEarlierAttemptCannotTakeLaterAttemptsLease(t *testing.T) {
	dir := t.TempDir()
	if err := takeActivityLease(dir, "wf", "run", "act", 2); err != nil {
		t.Fatal(err)
	}
	if err := takeActivityLease(dir, "wf", "run", "act", 1); !errors.Is(err, errActivitySuperseded) {
		t.Fatalf("attempt 1 take err = %v, want errActivitySuperseded", err)
	}
	if err := takeActivityLease(dir, "wf", "run", "act", 2); err != nil {
		t.Fatalf("re-take at the same attempt: %v", err)
	}
	if err := takeActivityLease(dir, "wf", "run", "act", 3); err != nil {
		t.Fatalf("attempt 3 take: %v", err)
	}
}

func TestLeaseLockSerialisesTakeAndSave(t *testing.T) {
	dir := t.TempDir()
	path := activityCheckpointPath(dir, "wf", "run", "act")
	leasePath := activityLeasePath(dir, "wf", "run", "act")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = saveActivityCheckpoint(path, leaseCheckpoint("stale"), 1)
		}()
		go func() {
			defer wg.Done()
			if err := takeActivityLease(dir, "wf", "run", "act", 2); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// Once the lease names attempt 2, no attempt-1 save may land, however
	// the goroutines interleaved: overwrite the file as the live attempt
	// and check a final stale save cannot replace it.
	if err := saveActivityCheckpoint(path, leaseCheckpoint("live"), 2); err != nil {
		t.Fatal(err)
	}
	if err := saveActivityCheckpoint(path, leaseCheckpoint("stale"), 1); !errors.Is(err, errActivitySuperseded) {
		t.Fatalf("stale save err = %v, want errActivitySuperseded", err)
	}
	got, _, found, err := loadActivityCheckpointForExecution[leaseTestResult](dir, "wf", "run", "act")
	if err != nil || !found || got.Result.Value != "live" {
		t.Fatalf("checkpoint = %+v found=%v err=%v, want live", got, found, err)
	}
	if lease, ok, err := readActivityLease(leasePath); err != nil || !ok || lease.ActivityAttempt != 2 {
		t.Fatalf("lease = %+v ok=%v err=%v", lease, ok, err)
	}
}

func TestFenceEarlierAttemptsAtAttemptOneDoesNothing(t *testing.T) {
	dir := t.TempDir()
	dockerCalls := filepath.Join(t.TempDir(), "calls")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\necho \"$@\" >> "+dockerCalls+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	a := &Activities{CheckpointDir: dir, DataDir: t.TempDir(), SandboxDocker: docker}
	input := RunWorkflowInput{RunID: "run-1"}
	wrapper := func(ctx context.Context) error {
		if err := a.fenceEarlierAttempts(ctx, input, true); err != nil {
			return err
		}
		return a.fenceAttempt(ctx, dir)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper); err != nil {
		t.Fatalf("fence at attempt 1: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "activity-checkpoints")); len(entries) != 0 {
		t.Errorf("attempt 1 created files: %v", entries)
	}
	if _, err := os.Stat(dockerCalls); !os.IsNotExist(err) {
		t.Errorf("attempt 1 called docker (stat err = %v)", err)
	}
}

func TestCheckLeaseRefusesSupersededAttemptWithNonRetryableType(t *testing.T) {
	dir := t.TempDir()
	if err := takeActivityLease(dir, "wf", "run", "act", 2); err != nil {
		t.Fatal(err)
	}
	err := checkLeaseForExecution(dir, "wf", "run", "act", 1)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != ActivitySupersededType || !appErr.NonRetryable() {
		t.Fatalf("attempt 1 err = %v, want non-retryable %s", err, ActivitySupersededType)
	}
	if err := checkLeaseForExecution(dir, "wf", "run", "act", 2); err != nil {
		t.Errorf("attempt 2: %v", err)
	}
	if err := checkLeaseForExecution(t.TempDir(), "wf", "run", "act", 1); err != nil {
		t.Errorf("no lease: %v", err)
	}
}

func TestRetryAfterFenceFindsStaleAttemptsLandedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	path := activityCheckpointPath(dir, "wf", "run", "act")
	if err := saveActivityCheckpoint(path, leaseCheckpoint("from attempt 1"), 1); err != nil {
		t.Fatal(err)
	}
	// Fence first, then load: the retry returns the completed checkpoint.
	if err := takeActivityLease(dir, "wf", "run", "act", 2); err != nil {
		t.Fatal(err)
	}
	got, _, found, err := loadActivityCheckpointForExecution[leaseTestResult](dir, "wf", "run", "act")
	if err != nil || !found || got.Result.Value != "from attempt 1" {
		t.Fatalf("load after fence = %+v found=%v err=%v", got, found, err)
	}
}

func TestLeaseCheckedBeforeHookRefusesStaleAttemptWithoutRunningHook(t *testing.T) {
	dir := t.TempDir()
	a := &Activities{CheckpointDir: dir}
	hookRan := false
	wrapper := func(ctx context.Context) error {
		info := activity.GetInfo(ctx)
		if err := takeActivityLease(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 2); err != nil {
			return err
		}
		return a.leaseChecked(ctx, dir, func(int) error { hookRan = true; return nil })(1)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != ActivitySupersededType {
		t.Fatalf("err = %v, want %s", err, ActivitySupersededType)
	}
	if hookRan {
		t.Error("before hook ran for a superseded attempt")
	}
}

// TestRunSandboxWithRetriesRefusesSupersededAttemptBeforeAnyLifecycle: a
// superseded attempt must stop before Begin*Lifecycle, not only before a
// container attempt -- compose projects and networks are named per run, so a
// stale attempt's setup or deferred cleanup would hit the live attempt's.
func TestRunSandboxWithRetriesRefusesSupersededAttemptBeforeAnyLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	checkpointDir := filepath.Join(dataDir, "checkpoints")
	marker := filepath.Join(dataDir, "docker-called")
	docker := filepath.Join(dataDir, "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &Activities{}
	input := fixtureInput()
	input.RunID = "superseded-run"
	input.DataDir = dataDir
	input.CheckpointDir = checkpointDir
	input.SandboxDocker = docker

	wrapper := func(ctx context.Context, in RunWorkflowInput) (runner.Result, error) {
		info := activity.GetInfo(ctx)
		if err := takeActivityLease(checkpointDir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 2); err != nil {
			return runner.Result{}, err
		}
		return a.runSandboxWithRetries(ctx, in, func(int) string { return filepath.Join(dataDir, "log") }, 1, nil, nil, nil, nil, &sandbox.ComposeServicesSpec{}, "", "", nil, nil, "python3")
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != ActivitySupersededType {
		t.Fatalf("err = %v, want an ActivitySuperseded application error", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("docker was invoked for a superseded attempt")
	}
}

// TestCheckSandboxScopeRefusesDivergingRequestNonRetryably: the fence's
// reap executes the request's Docker executable, so a diverging
// -sandbox-docker or -data-dir is refused first, and never retried.
func TestCheckSandboxScopeRefusesDivergingRequestNonRetryably(t *testing.T) {
	a := &Activities{SandboxDocker: "/usr/local/bin/docker", DataDir: "/worker/data"}
	for name, input := range map[string]RunWorkflowInput{
		"docker":   {SandboxDocker: "/fixture/request-supplied-docker", DataDir: "/worker/data"},
		"data dir": {SandboxDocker: "/usr/local/bin/docker", DataDir: "/other/data"},
	} {
		err := a.checkSandboxScope(input)
		var appErr *temporal.ApplicationError
		if !errors.As(err, &appErr) || !appErr.NonRetryable() {
			t.Errorf("%s: err = %v, want a non-retryable application error", name, err)
		}
	}
	if err := a.checkSandboxScope(RunWorkflowInput{SandboxDocker: "/usr/local/bin/docker", DataDir: "/worker/data"}); err != nil {
		t.Errorf("matching scope refused: %v", err)
	}
}
