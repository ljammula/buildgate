package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
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
	"buildgate/internal/timereport"
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
// and slot lists while it runs (the repository-lock wait mark is only for
// builds); a step on any other queue is in neither.
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
			var during, slots []string
			var waits bool
			acts := &requestActivities{dp: dp,
				dataDir:   dataDir,
				jobsQueue: jobsQueue,
				specRunner: func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
					during = currentActiveRequests()
					slots = slotRequests.snapshot()
					waits = waitsForRepoLock(ctx)
					return "", nil, errors.New("stop here")
				},
			}
			actEnv.RegisterActivity(acts.AdvanceRequest)
			_, _ = actEnv.ExecuteActivity(acts.AdvanceRequest, "req-1")
			if !slices.Equal(during, tc.want) || !slices.Equal(slots, tc.want) || waits != tc.wantWait {
				t.Errorf("during the step: active %v, slots %v, waits %v; want %v, %v, %v", during, slots, waits, tc.want, tc.want, tc.wantWait)
			}
			if got, slots := currentActiveRequests(), slotRequests.snapshot(); len(got) != 0 || len(slots) != 0 {
				t.Errorf("after the step: active %v, slots %v, want none", got, slots)
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
	stop, err := startWorkerHeartbeat(context.Background(), dir, "", "", "localhost:7233", daemonheartbeat.GitHubLoginUnusable, "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(dir)); err != nil || hb.GitHubLogin != daemonheartbeat.GitHubLoginUnusable {
		t.Errorf("heartbeat GitHubLogin = %q (err %v), want what the worker found at startup", hb.GitHubLogin, err)
	}
	addActiveRequest("lost-3")
	slotRequests.add("lost-4")
	clearActiveRequests()
	stop()
	if ids, _ := daemonheartbeat.WorkerActiveRequests(dir, time.Now()); len(ids) != 0 {
		t.Errorf("final heartbeat lists %v, want none", ids)
	}
	if ids := workerSlotRequests(dir, time.Now()); len(ids) != 0 {
		t.Errorf("final heartbeat lists slot holders %v, want none", ids)
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

// workerSlotRequests returns the SlotRequests of dataDir's worker heartbeat,
// or nil when there is no heartbeat or it is stale.
func workerSlotRequests(dataDir string, now time.Time) []string {
	hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(dataDir))
	if err != nil || daemonheartbeat.Stale(hb, now, daemonheartbeat.WorkerStaleAfter) {
		return nil
	}
	return hb.SlotRequests
}

// workerRepositoryWaits returns the RepositoryWaits of dataDir's worker
// heartbeat, or nil when there is no heartbeat or it is stale.
func workerRepositoryWaits(dataDir string, now time.Time) map[string]string {
	hb, err := daemonheartbeat.Read(daemonheartbeat.WorkerPath(dataDir))
	if err != nil || daemonheartbeat.Stale(hb, now, daemonheartbeat.WorkerStaleAfter) {
		return nil
	}
	return hb.RepositoryWaits
}

// TestBusyRepositoryWaitIsLoggedOncePerWait: a building step that finds the
// repository held writes one start line however often the workflow retries it,
// and the step that finds it free writes the end line.
func TestBusyRepositoryWaitIsLoggedOncePerWait(t *testing.T) {
	repositoryWaits.reset()
	t.Cleanup(repositoryWaits.reset)
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
	acts := &requestActivities{dp: dp, dataDir: dataDir}
	for range 3 {
		if _, err := acts.AdvanceRequest(context.Background(), "req-1"); err == nil {
			t.Fatal("a step on a busy repository returned no error")
		}
	}
	events := func() []string {
		b, err := os.ReadFile(timereport.WaitsPath(dataDir, "req-1"))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var l struct{ Event string }
			if err := json.Unmarshal([]byte(line), &l); err != nil {
				t.Fatal(err)
			}
			got = append(got, l.Event)
		}
		return got
	}
	if got := events(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("wait log after three busy steps = %v, want one start", got)
	}
	// A worker restart empties the tracker, not the log: the next busy step
	// does not start the wait again, and a free repository ends it.
	repositoryWaits.reset()
	if _, err := acts.AdvanceRequest(context.Background(), "req-1"); err == nil {
		t.Fatal("a step on a busy repository returned no error")
	}
	if got := events(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("wait log after a restart and another busy step = %v, want one start", got)
	}
	repositoryWaits.reset()
	acts.endRepositoryWait("req-1")
	acts.endRepositoryWait("req-1") // no wait left: nothing more is written
	if got := events(); !slices.Equal(got, []string{"start", "end"}) {
		t.Errorf("wait log after the wait ended = %v, want start then end", got)
	}
}
