package main

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/testfixture"
	wsisolation "buildgate/internal/workspace"
)

// jobsQueueProbe reports the task queue the activity test environment runs
// activities on.
func jobsQueueProbe(t *testing.T, env *testsuite.TestActivityEnvironment) string {
	t.Helper()
	probe := func(ctx context.Context) (string, error) {
		return activity.GetInfo(ctx).TaskQueue, nil
	}
	env.RegisterActivity(probe)
	val, err := env.ExecuteActivity(probe)
	if err != nil {
		t.Fatal(err)
	}
	var queue string
	if err := val.Get(&queue); err != nil {
		t.Fatal(err)
	}
	return queue
}

// TestAdvanceRequestPublishesTheRequestOnlyOnTheJobsQueue: a step on the jobs
// task queue occupies a job slot, so its request is in the heartbeat's active
// list while it runs (the repository-lock wait mark is only for builds); a step
// on any other queue is not.
func TestAdvanceRequestPublishesTheRequestOnlyOnTheJobsQueue(t *testing.T) {
	dp := newTestDeps(t)
	for _, tc := range []struct {
		name     string
		onJobs   bool
		want     []string
		wantWait bool
	}{
		{"jobs queue", true, []string{"req-1"}, false},
		{"other queue", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			actEnv := suite.NewTestActivityEnvironment()
			jobsQueue := "factoryd-jobs-other"
			if tc.onJobs {
				jobsQueue = jobsQueueProbe(t, actEnv)
			}
			dataDir := t.TempDir()
			saveRequestInState(t, dataDir, "req-1", request.StateSpecDrafting)
			var during []string
			var waits bool
			acts := &requestActivities{dp: dp,
				dataDir:   dataDir,
				jobsQueue: jobsQueue,
				specRunner: func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
					during = currentActiveRequests()
					waits = waitsForRepoLock(ctx)
					return "", nil, errors.New("stop here")
				},
			}
			actEnv.RegisterActivity(acts.AdvanceRequest)
			_, _ = actEnv.ExecuteActivity(acts.AdvanceRequest, "req-1")
			if !slices.Equal(during, tc.want) || waits != tc.wantWait {
				t.Errorf("during the step: active %v, waits %v; want %v, %v", during, waits, tc.want, tc.wantWait)
			}
			if got := currentActiveRequests(); len(got) != 0 {
				t.Errorf("after the step: active %v, want none", got)
			}
		})
	}
}

// TestAdvanceRequestBuildingOnABusyRepositoryGivesItsSlotBack: the step
// returns RepositoryBusy at once, without running the build, and the workflow
// retries it after requestRepoBusyRetryAfter.
func TestAdvanceRequestBuildingOnABusyRepositoryGivesItsSlotBack(t *testing.T) {
	dp := newTestDeps(t)
	repo := testfixture.NewGitRepo(t)
	dataDir := t.TempDir()
	r := request.New("req-1", repo, "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateBuilding
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	owner, err := wsisolation.AcquireDirectLock(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	acts := &requestActivities{dp: dp,
		dataDir: dataDir,
		buildRunner: func(context.Context, []string, func(*run.Run)) error {
			t.Error("the build ran against a busy repository")
			return nil
		},
	}
	_, err = acts.AdvanceRequest(context.Background(), "req-1")
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != repositoryBusyErrorType {
		t.Fatalf("err = %v, want a %s application error", err, repositoryBusyErrorType)
	}
	if got := requestStepFor(request.StateBuilding, requestdriver.WorkerConfig{}).RetryAfter; got != 30*time.Second {
		t.Errorf("building RetryAfter = %v, want 30s", got)
	}
}

// TestClearActiveRequests: a step that outlives the worker's stop never
// removes itself; the clear leaves the final heartbeat empty.
func TestClearActiveRequests(t *testing.T) {
	addActiveRequest("lost-1")
	addActiveRequest("lost-2")
	clearActiveRequests()
	if got := currentActiveRequests(); len(got) != 0 {
		t.Errorf("active after clear = %v", got)
	}
	dir := t.TempDir()
	stop, err := startWorkerHeartbeat(context.Background(), dir, "", "", "localhost:7233", 3)
	if err != nil {
		t.Fatal(err)
	}
	addActiveRequest("lost-3")
	clearActiveRequests()
	stop()
	if ids, _ := daemonheartbeat.WorkerActiveRequests(dir, time.Now()); len(ids) != 0 {
		t.Errorf("final heartbeat lists %v, want none", ids)
	}
}

// TestAcquireDirectRunLockWaitsOnlyWhenMarked: with the wait mark a busy lock
// is waited on and taken once released, and cancel ends the wait with the
// context error; without it the lock fails at once with ErrBusy.
func TestAcquireDirectRunLockWaitsOnlyWhenMarked(t *testing.T) {
	repo := testfixture.NewGitRepo(t)
	owner, err := wsisolation.AcquireDirectLock(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	if _, err := acquireDirectRunLock(context.Background(), repo); !errors.Is(err, wsisolation.ErrBusy) {
		t.Fatalf("unmarked ctx: err = %v, want ErrBusy", err)
	}

	cancelCtx, cancel := context.WithCancel(withWaitForRepoLock(context.Background()))
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := acquireDirectRunLock(cancelCtx, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: err = %v, want context.Canceled", err)
	}

	type result struct {
		lock *wsisolation.DirectLock
		err  error
	}
	done := make(chan result, 1)
	go func() {
		lock, err := acquireDirectRunLock(withWaitForRepoLock(context.Background()), repo)
		done <- result{lock, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("returned while the lock was held: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("after release: %v", r.err)
		}
		_ = r.lock.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("did not take the lock after it was released")
	}
}

func TestStepWaitsForRepoLockOnlyForJobsQueueBuildsAndPRReview(t *testing.T) {
	for _, tc := range []struct {
		state  request.State
		onJobs bool
		want   bool
	}{
		{request.StateBuilding, true, true},
		{request.StatePRReview, true, true},
		{request.StateBuilding, false, false},
		{request.StatePRReview, false, false},
		{request.StateSpecDrafting, true, false},
		{request.StatePlanning, true, false},
	} {
		if got := stepWaitsForRepoLock(tc.state, tc.onJobs); got != tc.want {
			t.Errorf("stepWaitsForRepoLock(%s, %v) = %v, want %v", tc.state, tc.onJobs, got, tc.want)
		}
	}
}
