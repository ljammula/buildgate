package main

import (
	"buildgate/internal/daemonheartbeat"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// workerMain is `factoryd worker`: the long-lived process that drives every
// request of one data dir through Temporal, one RequestWorkflow per request
// (internal/workflow/request_workflow.go). It takes worker's flags and
// session config (loadWorkerConfig) and holds worker's drain lock, so a
// worker and a worker never drive one data dir at once.
//
//	factoryd-light-<id>  RequestWorkflow + every activity; uncapped
//	factoryd-jobs-<id>   AdvanceRequest for model jobs and builds;
//	                     max_parallel_jobs at once
//
// <id> is the data dir's random queue id (requestQueueID), which also names
// its request workflow ids (workflow.RequestWorkflowID). At startup the
// worker halts runs a stopped worker left behind (reclaimDeadOwnerRuns) and moves the
// requests building them to resume_review, then starts (or wakes) the workflow of every request
// that is not finished.
func workerMain(dp *deps, args []string) error {
	cfg, dataDir, err := loadWorkerConfig(dp, args)
	if err != nil {
		return err
	}
	unlock, err := acquireWorkerLock(dp, dataDir)
	if err != nil {
		return err
	}
	defer unlock()
	defer useWorkerGlobals(&cfg)()
	// The same liveness heartbeat worker writes, so status, stop, use,
	// upgrade and the console see the worker and the requests it runs.
	heartbeatMode, heartbeatModel := heartbeatRoute(cfg)
	githubLogin := dp.forge.githubLogin(context.Background())
	if githubLogin == daemonheartbeat.GitHubLoginUnusable {
		log.Printf("factoryd worker: this session cannot read a GitHub token (`gh auth token` failed): an accepted ticket's branch cannot be pushed and its pull request cannot be opened. To fix: %s", workerGitHubLoginFix)
	}
	stopHeartbeat, err := startWorkerHeartbeat(context.Background(), dataDir, heartbeatMode, heartbeatModel, cfg.TemporalAddress, githubLogin, cfg.Settings.MaxParallelJobs)
	if err != nil {
		return err
	}
	defer stopHeartbeat()
	defer clearActiveRequests() // runs before stopHeartbeat: the final write lists nothing
	// A build runs inside its AdvanceRequest step; a worker (or worker)
	// stopped mid-build leaves that run's workflow, record and containers
	// behind. Halt them, and put the requests they were building in resume_review, before taking
	// new work, so no workflow starts that ticket over on its own: a human decides.
	_, haltedIDs := reclaimLostWork(dp, context.Background(), dataDir, cfg.Settings.SandboxDocker, cfg.TemporalAddress)

	queueID, err := requestQueueID(dataDir)
	if err != nil {
		return err
	}
	jobsQueue, lightQueue := taskQueuesFor(queueID)
	temporalClient, err := client.Dial(client.Options{HostPort: cfg.TemporalAddress})
	if err != nil {
		return fmt.Errorf("connect to Temporal at %s: %w", cfg.TemporalAddress, err)
	}
	defer temporalClient.Close()

	acts := &requestActivities{dp: dp,
		dataDir:      dataDir,
		jobsQueue:    jobsQueue,
		cfg:          cfg,
		specRunner:   runSpecDraftJob,
		planRunner:   runPlanTicketsJob,
		oracleRunner: runOracleDraftJob,
		buildRunner: func(a0 context.Context, a1 []string, a2 func(*run.Run)) error {
			return runMainWithReady(dp, a0, a1, a2)
		},
	}
	jobs := temporalworker.New(temporalClient, jobsQueue, jobsWorkerOptions(cfg.Settings.MaxParallelJobs))
	jobs.RegisterActivityWithOptions(acts.AdvanceRequest, activity.RegisterOptions{Name: workflow.AdvanceRequestActivityName})
	light := temporalworker.New(temporalClient, lightQueue, lightWorkerOptions())
	acts.registerLight(light)

	if err := jobs.Start(); err != nil {
		return fmt.Errorf("start jobs worker: %w", err)
	}
	defer jobs.Stop()
	if err := light.Start(); err != nil {
		return fmt.Errorf("start light worker: %w", err)
	}
	defer light.Stop()
	log.Printf("factoryd worker: data dir %s, task queues %s (max %d jobs) and %s", dataDir, jobsQueue, cfg.Settings.MaxParallelJobs, lightQueue)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// A request in resume_review has an old workflow still waiting on its lost step; ended
	// here, its lost-step move cannot fire later and undo a decision made in between.
	for id, err := range terminateRequestWorkflows(ctx, temporalClient, queueID, lightQueue, haltedIDs) {
		log.Printf("factoryd worker: end workflow of halted request %s: %v", id, err)
	}
	if err := startRequestWorkflows(ctx, temporalClient, dataDir, queueID, jobsQueue, lightQueue); err != nil {
		return err
	}
	retryPendingWakes(ctx, temporalClient, time.NewTicker(workerAdoptInterval), dataDir, queueID, jobsQueue, lightQueue)
	log.Printf("factoryd worker: stopping")
	return nil
}

// jobsWorkerOptions are the jobs-queue worker's options: at most maxJobs
// steps at once, the SDK's heartbeat throttle bounded (workflow.HeartbeatThrottle).
func jobsWorkerOptions(maxJobs int) temporalworker.Options {
	return temporalworker.Options{
		MaxConcurrentActivityExecutionSize: maxJobs,
		WorkerStopTimeout:                  requestWorkerStopTimeout,
		MaxHeartbeatThrottleInterval:       workflow.HeartbeatThrottle,
	}
}

// lightWorkerOptions are the light-queue worker's options.
func lightWorkerOptions() temporalworker.Options {
	return temporalworker.Options{
		WorkerStopTimeout:            requestWorkerStopTimeout,
		MaxHeartbeatThrottleInterval: workflow.HeartbeatThrottle,
	}
}

// requestWorkerStopTimeout is how long Stop waits for running activities. A
// long job is not waited for: it is lost, and its request waits in resume_review
// for a human decision.
const requestWorkerStopTimeout = 10 * time.Second

// requestQueueIDFileName holds the data dir's random task-queue id. Random
// rather than derived from the path, so two machines that mount one data dir
// at different paths still share its queues.
const requestQueueIDFileName = "worker-queue-id"

// taskQueuesFor names the jobs and light task queues of the data dir whose
// queue id is id.
func taskQueuesFor(id string) (jobs, light string) {
	return "factoryd-jobs-" + id, "factoryd-light-" + id
}

// readRequestQueueID returns dataDir's queue id without creating it; found is
// false when the data dir has none.
func readRequestQueueID(dataDir string) (id string, found bool, err error) {
	path := filepath.Join(dataDir, requestQueueIDFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read %s: %w", path, err)
	}
	id = strings.TrimSpace(string(raw))
	if id == "" {
		return "", false, fmt.Errorf("%s is empty", path)
	}
	return id, true, nil
}

// requestQueueID returns dataDir's random queue id, creating it on first use.
// It names the data dir in its task queues and in its request workflow ids
// (workflow.RequestWorkflowID), the one thing that tells two data dirs on one
// Temporal server apart.
func requestQueueID(dataDir string) (string, error) {
	if id, found, err := readRequestQueueID(dataDir); err != nil || found {
		return id, err
	}
	path := filepath.Join(dataDir, requestQueueIDFileName)
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate worker queue id: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	// O_EXCL: if another process created it first, use theirs.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		_, writeErr := f.Write([]byte(hex.EncodeToString(buf) + "\n"))
		if closeErr := f.Close(); writeErr == nil {
			writeErr = closeErr
		}
		if writeErr != nil {
			return "", fmt.Errorf("write %s: %w", path, writeErr)
		}
	case !errors.Is(err, os.ErrExist):
		return "", fmt.Errorf("create %s: %w", path, err)
	}
	id, found, err := readRequestQueueID(dataDir)
	if err == nil && !found {
		err = fmt.Errorf("%s vanished", path)
	}
	return id, err
}

// requestWorkflowStarter is the slice of client.Client startRequestWorkflow
// and wakeRequestWorkflowWith need, so tests can stub it.
type requestWorkflowStarter interface {
	workflowDescriber
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg interface{}, options client.StartWorkflowOptions, workflow interface{}, workflowArgs ...interface{}) (client.WorkflowRun, error)
}

// startRequestWorkflow starts requestID's RequestWorkflow under workflowID, or
// wakes it when it is already running, in one call.
func startRequestWorkflow(ctx context.Context, c requestWorkflowStarter, workflowID, requestID, jobsQueue, lightQueue string) error {
	_, err := c.SignalWithStartWorkflow(ctx, workflowID, workflow.RequestWakeSignalName, nil,
		client.StartWorkflowOptions{TaskQueue: lightQueue},
		workflow.RequestWorkflowName,
		workflow.RequestWorkflowInput{RequestID: requestID, JobsTaskQueue: jobsQueue, LightTaskQueue: lightQueue})
	if err != nil {
		return fmt.Errorf("start workflow for request %s: %w", requestID, err)
	}
	return nil
}

// startRequestWorkflows starts or wakes the workflow of every request in
// dataDir that is not finished. A request whose legacy workflow is still its
// driver (requestDriverWorkflowID) is woken there, not started a second time;
// a lookup that fails fails the start. The wake-pending marker of each request
// it woke is cleared, and a finished request's marker with it.
func startRequestWorkflows(ctx context.Context, c requestWorkflowStarter, dataDir, queueID, jobsQueue, lightQueue string) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	marks, err := request.ListWakePending(dataDir)
	if err != nil {
		return fmt.Errorf("list wake markers: %w", err)
	}
	pending := make(map[string]request.WakePending, len(marks))
	for _, m := range marks {
		pending[m.ID] = m
	}
	for _, r := range requests {
		m, marked := pending[r.ID]
		// A marked finished request is woken too: the save that finished it
		// (a cancel at a gate) is what its sleeping workflow has not seen.
		if !r.State.Finished() || marked {
			if err := wakeOrStartRequestWorkflow(ctx, c, queueID, r.ID, jobsQueue, lightQueue); err != nil {
				return err
			}
		}
		if marked {
			if err := request.ClearWakePending(dataDir, m); err != nil {
				log.Printf("factoryd worker: clear wake marker of request %s: %v", r.ID, err)
			}
		}
	}
	return nil
}

// workerAdoptInterval is how often a running worker retries the wakes that
// failed after a save (a new request or a decision), which left a marker.
const workerAdoptInterval = time.Minute

// retryPendingWakes runs retryWakes on every tick until ctx is done, then
// stops the ticker.
func retryPendingWakes(ctx context.Context, c requestWorkflowStarter, ticker *time.Ticker, dataDir, queueID, jobsQueue, lightQueue string) {
	defer ticker.Stop()
	retryOnTicks(ctx, c, ticker.C, dataDir, queueID, jobsQueue, lightQueue)
}

// retryOnTicks is the loop of retryPendingWakes over a tick channel.
func retryOnTicks(ctx context.Context, c requestWorkflowStarter, ticks <-chan time.Time, dataDir, queueID, jobsQueue, lightQueue string) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			retryWakes(ctx, c, dataDir, queueID, jobsQueue, lightQueue)
		}
	}
}

// retryWakes redoes the wake of every request with a wake-pending marker, the
// one the save's own wake would have done, and clears the marker once it
// succeeded. A finished request is woken too: the save that finished it (a
// cancel at a gate) must still end its sleeping workflow, which reads the
// request, finds it done and returns. A failure is logged, one line
// per request, and the marker kept for the next tick. A request with no
// marker is not touched: nothing is described, started or signalled for it.
func retryWakes(ctx context.Context, c requestWorkflowStarter, dataDir, queueID, jobsQueue, lightQueue string) {
	marks, err := request.ListWakePending(dataDir)
	if err != nil {
		log.Printf("factoryd worker: retry wakes: %v", err)
		return
	}
	for _, m := range marks {
		if err := retryWake(ctx, c, dataDir, queueID, jobsQueue, lightQueue, m); err != nil {
			log.Printf("factoryd worker: retry wake of request %s: %v", m.ID, err)
		}
	}
}

func retryWake(ctx context.Context, c requestWorkflowStarter, dataDir, queueID, jobsQueue, lightQueue string, m request.WakePending) error {
	if err := wakeOrStartRequestWorkflow(ctx, c, queueID, m.ID, jobsQueue, lightQueue); err != nil {
		return err
	}
	return request.ClearWakePending(dataDir, m)
}
