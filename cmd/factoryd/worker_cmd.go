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

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

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
// <id> is the data dir's random queue id (requestTaskQueues). At startup the
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
	jobsQueue, lightQueue := "factoryd-jobs-"+queueID, "factoryd-light-"+queueID
	temporalClient, err := client.Dial(client.Options{HostPort: cfg.TemporalAddress})
	if err != nil {
		return fmt.Errorf("connect to Temporal at %s: %w", cfg.TemporalAddress, err)
	}
	defer temporalClient.Close()

	// A request in flight at an upgrade is still running under the id it had
	// before ids carried the queue id; end that workflow before the workers
	// poll, so it never drives the request beside the one started below.
	if err := retireLegacyRequestWorkflows(context.Background(), temporalClient, dataDir, lightQueue); err != nil {
		return err
	}

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
	terminateRequestWorkflows(ctx, temporalClient, queueID, haltedIDs)
	if err := startRequestWorkflows(ctx, temporalClient, dataDir, queueID, jobsQueue, lightQueue); err != nil {
		return err
	}
	<-ctx.Done()
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

// requestTaskQueues returns dataDir's jobs and light task queue names,
// creating its queue id on first use.
func requestTaskQueues(dataDir string) (jobs, light string, err error) {
	id, err := requestQueueID(dataDir)
	if err != nil {
		return "", "", err
	}
	return "factoryd-jobs-" + id, "factoryd-light-" + id, nil
}

// requestQueueID returns dataDir's random queue id, creating it on first use.
// It names the data dir in its task queues and in its request workflow ids
// (workflow.RequestWorkflowID), the one thing that tells two data dirs on one
// Temporal server apart.
func requestQueueID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, requestQueueIDFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate worker queue id: %w", err)
		}
		raw = []byte(hex.EncodeToString(buf))
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return "", fmt.Errorf("create data dir: %w", err)
		}
		// O_EXCL: if another process created it first, use theirs.
		f, createErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		switch {
		case createErr == nil:
			_, writeErr := f.Write(append(raw, '\n'))
			if closeErr := f.Close(); writeErr == nil {
				writeErr = closeErr
			}
			if writeErr != nil {
				return "", fmt.Errorf("write %s: %w", path, writeErr)
			}
		case errors.Is(createErr, os.ErrExist):
			if raw, err = os.ReadFile(path); err != nil {
				return "", fmt.Errorf("read %s: %w", path, err)
			}
		default:
			return "", fmt.Errorf("create %s: %w", path, createErr)
		}
	} else if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return id, nil
}

// requestWorkflowStarter is the slice of client.Client startRequestWorkflow
// needs, so tests can stub it.
type requestWorkflowStarter interface {
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg interface{}, options client.StartWorkflowOptions, workflow interface{}, workflowArgs ...interface{}) (client.WorkflowRun, error)
}

// startRequestWorkflow starts requestID's RequestWorkflow, or wakes it when
// it is already running, in one call.
func startRequestWorkflow(ctx context.Context, c requestWorkflowStarter, queueID, requestID, jobsQueue, lightQueue string) error {
	_, err := c.SignalWithStartWorkflow(ctx, workflow.RequestWorkflowID(queueID, requestID), workflow.RequestWakeSignalName, nil,
		client.StartWorkflowOptions{TaskQueue: lightQueue},
		workflow.RequestWorkflowName,
		workflow.RequestWorkflowInput{RequestID: requestID, JobsTaskQueue: jobsQueue, LightTaskQueue: lightQueue})
	if err != nil {
		return fmt.Errorf("start workflow for request %s: %w", requestID, err)
	}
	return nil
}

// startRequestWorkflows starts or wakes the workflow of every request in
// dataDir that is not finished.
func startRequestWorkflows(ctx context.Context, c requestWorkflowStarter, dataDir, queueID, jobsQueue, lightQueue string) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	for _, r := range requests {
		if r.State.Finished() {
			continue
		}
		if err := startRequestWorkflow(ctx, c, queueID, r.ID, jobsQueue, lightQueue); err != nil {
			return err
		}
	}
	return nil
}

// retireLegacyRequestWorkflows ends each unfinished request's workflow that
// still has the id requests had before ids carried the queue id
// (workflow.LegacyRequestWorkflowID), so the request is driven once, by the
// workflow startRequestWorkflows starts. Only a workflow on lightQueue is
// ended: the same legacy id on another data dir's queue is that data dir's
// request, which this worker must not touch. request.json is the source of
// truth and RequestWorkflow keeps no state of its own, so nothing is lost; a
// step in flight belongs to the worker that was stopped for the upgrade and is
// already halted by reclaimLostWork.
func retireLegacyRequestWorkflows(ctx context.Context, c workflowTerminator, dataDir, lightQueue string) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	for _, r := range requests {
		if r.State.Finished() {
			continue
		}
		if err := retireLegacyRequestWorkflow(ctx, c, workflow.LegacyRequestWorkflowID(r.ID), lightQueue); err != nil {
			return err
		}
	}
	return nil
}

// retireLegacyRequestWorkflow ends workflowID if it is running on lightQueue.
func retireLegacyRequestWorkflow(ctx context.Context, c workflowTerminator, id, lightQueue string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var notFound *serviceerror.NotFound
	desc, err := c.DescribeWorkflowExecution(ctx, id, "")
	if errors.As(err, &notFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look up workflow %s: %w", id, err)
	}
	info := desc.GetWorkflowExecutionInfo()
	if info.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING || info.GetTaskQueue() != lightQueue {
		return nil
	}
	if err := c.TerminateWorkflow(ctx, id, "", "replaced by a workflow id that names its data dir"); err != nil && !errors.As(err, &notFound) {
		return fmt.Errorf("end workflow %s: %w", id, err)
	}
	return nil
}
