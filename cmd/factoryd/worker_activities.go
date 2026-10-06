package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/workflow"
)

// requestActivities implements RequestWorkflow's activities over the request
// driver's own step functions (advanceRequest and friends), which do one step
// each under their own state checks and save request.json themselves.
type requestActivities struct {
	// dp is the external boundaries this call reaches.
	dp           *deps
	dataDir      string
	jobsQueue    string // the task queue whose steps occupy a job slot
	cfg          requestdriver.WorkerConfig
	specRunner   requestdriver.SpecDraftRunner
	planRunner   requestdriver.PlanTicketsRunner
	oracleRunner requestdriver.OracleDraftRunner
	buildRunner  requestdriver.TicketRunner
}

func (a *requestActivities) registerLight(w temporalworker.Worker) {
	w.RegisterWorkflowWithOptions(workflow.RequestWorkflow, temporalworkflow.RegisterOptions{Name: workflow.RequestWorkflowName})
	w.RegisterActivityWithOptions(a.LoadRequestStep, activity.RegisterOptions{Name: workflow.LoadRequestStepActivityName})
	w.RegisterActivityWithOptions(a.AdvanceRequest, activity.RegisterOptions{Name: workflow.AdvanceRequestActivityName})
	w.RegisterActivityWithOptions(a.RemindRequest, activity.RegisterOptions{Name: workflow.RemindRequestActivityName})
	w.RegisterActivityWithOptions(a.HaltLostRequestStep, activity.RegisterOptions{Name: workflow.HaltLostRequestStepActivityName})
}

// requestStepRetryAfter is how long RequestWorkflow waits before re-running a
// step that returned an error: the worker poll rate.
const requestStepRetryAfter = 2 * time.Second

// LoadRequestStep reads request id and says what RequestWorkflow does next.
// A request that no longer exists ends its workflow.
func (a *requestActivities) LoadRequestStep(_ context.Context, id string) (workflow.RequestStep, error) {
	r, err := request.Load(a.dataDir, id)
	if errors.Is(err, os.ErrNotExist) {
		return workflow.RequestStep{Done: true}, nil
	}
	if err != nil {
		return workflow.RequestStep{}, fmt.Errorf("load request %s: %w", id, err)
	}
	return requestStepFor(r.State, a.cfg), nil
}

// requestStepFor maps a request state to its RequestWorkflow step.
func requestStepFor(state request.State, cfg requestdriver.WorkerConfig) workflow.RequestStep {
	step := workflow.RequestStep{State: string(state), RetryAfter: requestStepRetryAfter}
	switch {
	case requestFinished(state):
		step.Done = true
	case state == request.StateSubmitted:
		step.Advance = workflow.RequestAdvanceLight
	case state == request.StateBuilding:
		step.Advance = workflow.RequestAdvanceJobs
		step.RetryAfter = requestRepoBusyRetryAfter
	case state.RunsJob():
		step.Advance = workflow.RequestAdvanceJobs
	case state == request.StatePRReview:
		// A pass may run a PR-review corrective build, so it takes a job
		// slot. It reads the PR at most once per -pr-poll-interval
		// (pollTicketPR checks its own due time), so the timer matches it.
		step.Advance = workflow.RequestAdvanceJobs
		step.Wait = cfg.PrPollInterval
	case requestdriver.ReviewState(state):
		step.Wait = cfg.HitlReminderInterval
		step.Remind = true
	}
	// halted and quarantined: no step, no timer; a decision wakes it.
	return step
}

// requestRepoBusyRetryAfter is how long a building request waits before
// RequestWorkflow retries a step that found its repository held by another
// build.
const requestRepoBusyRetryAfter = 30 * time.Second

// repositoryBusyErrorType is the application error type of a build step that
// gave its slot back because another build holds the repository.
const repositoryBusyErrorType = "RepositoryBusy"

// stepWaitsForRepoLock reports whether a step on a held repository waits for
// the lock, in its job slot, rather than failing as busy: a build (only the
// race between its RepositoryBusy probe and the run reaches the wait), and a
// pr_review pass, whose corrective round holds the PR's branch exclusively
// and so waits for the builds holding the repository shared.
func stepWaitsForRepoLock(state request.State, onJobs bool) bool {
	return onJobs && (state == request.StateBuilding || state == request.StatePRReview)
}

// requestHeartbeatInterval is how often AdvanceRequest heartbeats; well
// inside RequestWorkflow's heartbeat timeout.
const requestHeartbeatInterval = 20 * time.Second

// AdvanceRequest runs one step of request id (advanceRequest), saves its
// outcome and returns the state the request is in afterwards. It heartbeats
// for as long as the step runs, so a worker that dies mid-step is noticed,
// and a cancel of the activity cancels the step.
func (a *requestActivities) AdvanceRequest(ctx context.Context, id string) (string, error) {
	r, err := request.Load(a.dataDir, id)
	if err != nil {
		return "", fmt.Errorf("load request %s: %w", id, err)
	}
	if requestdriver.RequestDriverOwnsState(r.State) {
		onJobs := a.runsOnJobsQueue(ctx)
		if onJobs && r.State.RunsJob() {
			// A pr_review poll is not a job, as in worker's driver.
			addActiveRequest(id)
			defer removeActiveRequest(id)
		}
		if r.State == request.StateBuilding {
			// A build on a repository another build holds gives its slot
			// back at once; the workflow retries after requestRepoBusyRetryAfter.
			if repoBusy(r.Workspace, a.dataDir) {
				return "", temporal.NewApplicationError("repository busy: another build holds it", repositoryBusyErrorType)
			}
		}
		if stepWaitsForRepoLock(r.State, onJobs) {
			ctx = withWaitForRepoLock(ctx)
		}
		stopHeartbeat := heartbeatUntilDone(ctx, requestHeartbeatInterval)
		err = requestdriver.AdvanceRequest(a.dp, ctx, a.dataDir, r, a.cfg, a.specRunner, a.planRunner, a.oracleRunner, a.buildRunner)
		stopHeartbeat()
		if err != nil {
			return "", err
		}
	}
	after, err := request.Load(a.dataDir, id)
	if err != nil {
		return "", fmt.Errorf("reload request %s: %w", id, err)
	}
	return string(after.State), nil
}

// runsOnJobsQueue reports whether ctx is an activity running on the jobs task
// queue (a job slot), as opposed to the light queue or a direct call.
func (a *requestActivities) runsOnJobsQueue(ctx context.Context) bool {
	return activity.IsActivity(ctx) && activity.GetInfo(ctx).TaskQueue == a.jobsQueue
}

// heartbeatUntilDone records an activity heartbeat every interval until the
// returned stop function is called or ctx ends. Outside an activity (a direct
// call in a test) there is nothing to heartbeat.
func heartbeatUntilDone(ctx context.Context, interval time.Duration) func() {
	if !activity.IsActivity(ctx) {
		return func() {}
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				activity.RecordHeartbeat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

// RemindRequest sends request id's review reminder when one is due.
func (a *requestActivities) RemindRequest(_ context.Context, id string) error {
	return requestdriver.RemindIfDue(a.dataDir, id, a.cfg.HitlReminderInterval, time.Now)
}

// HaltLostRequestStep moves a request whose step stopped without finishing to
// resume_review, provided it is still in the state that step ran in: a step
// that saved its outcome before it was lost has already moved the request on.
// Nothing reruns on its own: the operator resumes, rebuilds or cancels. A
// pr_review request is left to poll again: most passes only read the PR, and a
// lost corrective round's run is reclaimed, with its request, at the next
// worker start (haltRequestsOfLostRuns).
func (a *requestActivities) HaltLostRequestStep(_ context.Context, in workflow.HaltLostRequestStepInput) error {
	if in.State == string(request.StatePRReview) {
		return nil
	}
	unlock, err := request.Lock(a.dataDir, in.RequestID)
	if err != nil {
		return fmt.Errorf("lock request %s: %w", in.RequestID, err)
	}
	defer unlock()
	r, err := request.Load(a.dataDir, in.RequestID)
	if err != nil {
		return fmt.Errorf("load request %s: %w", in.RequestID, err)
	}
	if string(r.State) != in.State {
		return nil
	}
	return requestdriver.EnterResumeReview(a.dataDir, r, lostRunOf(a.dataDir, r), time.Now())
}
