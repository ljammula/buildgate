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
// <id> is the data dir's random queue id (requestTaskQueues). At startup the
// worker halts runs a stopped worker left behind (reclaimDeadOwnerRuns) and moves the
// requests building them to resume_review, then starts (or wakes) the workflow of every request
// that is not done or cancelled.
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
		log.Printf("factoryd worker: this session cannot use the GitHub login (`gh auth status` failed): an accepted ticket's branch cannot be pushed and its pull request cannot be opened. To fix: %s", workerGitHubLoginFix)
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
	reclaimed := reclaimDeadOwnerRuns(dp, context.Background(), dataDir, cfg.Settings.SandboxDocker, cfg.TemporalAddress)
	haltedIDs := haltRequestsOfLostRuns(dataDir, reclaimed)
	reclaimDeadRequestJobs(context.Background(), dataDir, cfg.Settings.SandboxDocker)

	jobsQueue, lightQueue, err := requestTaskQueues(dataDir)
	if err != nil {
		return err
	}
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
	jobs := temporalworker.New(temporalClient, jobsQueue, temporalworker.Options{
		MaxConcurrentActivityExecutionSize: cfg.Settings.MaxParallelJobs,
		WorkerStopTimeout:                  requestWorkerStopTimeout,
	})
	jobs.RegisterActivityWithOptions(acts.AdvanceRequest, activity.RegisterOptions{Name: workflow.AdvanceRequestActivityName})
	light := temporalworker.New(temporalClient, lightQueue, temporalworker.Options{WorkerStopTimeout: requestWorkerStopTimeout})
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
	terminateRequestWorkflows(ctx, temporalClient, haltedIDs)
	if err := startRequestWorkflows(ctx, temporalClient, dataDir, jobsQueue, lightQueue); err != nil {
		return err
	}
	<-ctx.Done()
	log.Printf("factoryd worker: stopping")
	return nil
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
	path := filepath.Join(dataDir, requestQueueIDFileName)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			return "", "", fmt.Errorf("generate worker queue id: %w", err)
		}
		raw = []byte(hex.EncodeToString(buf))
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return "", "", fmt.Errorf("create data dir: %w", err)
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
				return "", "", fmt.Errorf("write %s: %w", path, writeErr)
			}
		case errors.Is(createErr, os.ErrExist):
			if raw, err = os.ReadFile(path); err != nil {
				return "", "", fmt.Errorf("read %s: %w", path, err)
			}
		default:
			return "", "", fmt.Errorf("create %s: %w", path, createErr)
		}
	} else if err != nil {
		return "", "", fmt.Errorf("read %s: %w", path, err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return "", "", fmt.Errorf("%s is empty", path)
	}
	return "factoryd-jobs-" + id, "factoryd-light-" + id, nil
}

// requestWorkflowStarter is the slice of client.Client startRequestWorkflow
// needs, so tests can stub it.
type requestWorkflowStarter interface {
	SignalWithStartWorkflow(ctx context.Context, workflowID, signalName string, signalArg interface{}, options client.StartWorkflowOptions, workflow interface{}, workflowArgs ...interface{}) (client.WorkflowRun, error)
}

// startRequestWorkflow starts requestID's RequestWorkflow, or wakes it when
// it is already running, in one call.
func startRequestWorkflow(ctx context.Context, c requestWorkflowStarter, requestID, jobsQueue, lightQueue string) error {
	_, err := c.SignalWithStartWorkflow(ctx, workflow.RequestWorkflowID(requestID), workflow.RequestWakeSignalName, nil,
		client.StartWorkflowOptions{TaskQueue: lightQueue},
		workflow.RequestWorkflowName,
		workflow.RequestWorkflowInput{RequestID: requestID, JobsTaskQueue: jobsQueue, LightTaskQueue: lightQueue})
	if err != nil {
		return fmt.Errorf("start workflow for request %s: %w", requestID, err)
	}
	return nil
}

// startRequestWorkflows starts or wakes the workflow of every request in
// dataDir that is not done or cancelled.
func startRequestWorkflows(ctx context.Context, c requestWorkflowStarter, dataDir, jobsQueue, lightQueue string) error {
	requests, err := request.List(dataDir)
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}
	for _, r := range requests {
		if requestFinished(r.State) {
			continue
		}
		if err := startRequestWorkflow(ctx, c, r.ID, jobsQueue, lightQueue); err != nil {
			return err
		}
	}
	return nil
}

// requestFinished reports whether a request in state can never move again.
// halted and quarantined are not finished: retry, send back and amend scope
// revive them.
func requestFinished(state request.State) bool {
	return state == request.StateDone || state == request.StateCancelled
}
