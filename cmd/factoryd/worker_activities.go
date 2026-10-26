package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/consolelink"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/unresolved"
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
	// cancelPoll is how often a running step looks for its request being
	// cancelled; 0 is requestHeartbeatInterval.
	cancelPoll time.Duration

	// consoleStart guards consoleStartedAt, when ensureConsole last tried.
	consoleStart     sync.Mutex
	consoleStartedAt time.Time

	// steps is the requests with a step running (AdvanceRequest).
	steps stepGuard
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
	if r.State.Finished() {
		// Nothing can retry from a round of a request that is over.
		requestdriver.PruneRoundRefs(a.dp, r)
	}
	step := requestStepFor(r.State, a.cfg)
	if r.State == request.StateBuilding && r.StackedWait != nil {
		step.Wait = stackedWaitRecheck(a.cfg)
	}
	if step.Remind {
		step.Wait = reviewWait(r, a.cfg.HitlReminderInterval, a.cfg.Settings.ReviewGateExpiry, time.Now())
	}
	return step, nil
}

// minReviewWait is the shortest timer a review step sets: a Wait of 0 means
// wait for a signal only, so a gate about to expire (or already past its
// deadline) still wakes the workflow.
const minReviewWait = time.Second

// reviewWait is how long a request at a review gate waits before the workflow
// looks again: the reminder interval, cut short to the time left before the
// gate expires (review_gate_expiry), and never below minReviewWait.
func reviewWait(r *request.Request, reminder, expiry time.Duration, now time.Time) time.Duration {
	wait := reminder
	if deadline, ok := r.GateDeadline(expiry); ok {
		wait = min(wait, deadline.Sub(now))
	}
	return max(wait, minReviewWait)
}

// requestStepFor maps a request state to its RequestWorkflow step.
func requestStepFor(state request.State, cfg requestdriver.WorkerConfig) workflow.RequestStep {
	step := workflow.RequestStep{State: string(state), RetryAfter: requestStepRetryAfter}
	switch {
	case state.Finished():
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

// stackedWaitRecheck is how long a building request whose ticket waits on
// the earlier ticket's pull-request branch (request.StackedWait) waits before
// its step checks again: the PR poll interval, as the branch moves on a
// person's push or a review round, or the repository retry when that is 0.
func stackedWaitRecheck(cfg requestdriver.WorkerConfig) time.Duration {
	if cfg.PrPollInterval > 0 {
		return cfg.PrPollInterval
	}
	return requestRepoBusyRetryAfter
}

// requestRepoBusyRetryAfter is how long a building request waits before
// RequestWorkflow retries a step that found its repository held by another
// build.
const requestRepoBusyRetryAfter = 30 * time.Second

// stepInProgressErrorType is the application error type of an AdvanceRequest
// refused because a step of the same request is already running.
const stepInProgressErrorType = "StepInProgress"

// stepGuard holds the requests with a step running. In-process is enough:
// every activity of a data dir runs in the one worker process that holds the
// drain lock (acquireWorkerLock in workerMain, which starts both the jobs and
// the light task queue workers). The zero value is ready.
type stepGuard struct {
	mu   sync.Mutex
	held map[string]bool
}

// begin marks id as running a step and reports whether it was free.
func (g *stepGuard) begin(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held[id] {
		return false
	}
	if g.held == nil {
		g.held = map[string]bool{}
	}
	g.held[id] = true
	return true
}

// running reports whether a step of id is running in this process.
func (g *stepGuard) running(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held[id]
}

func (g *stepGuard) end(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.held, id)
}

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
const requestHeartbeatInterval = 5 * time.Second

// AdvanceRequest runs one step of request id (advanceRequest), saves its
// outcome and returns the state the request is in afterwards. It heartbeats
// for as long as the step runs, so a worker that dies mid-step is noticed,
// and a cancel of the activity, or of the request, cancels the step. A stall of the connection to
// Temporal shorter than the heartbeat timeout does not.
func (a *requestActivities) AdvanceRequest(ctx context.Context, id string) (string, error) {
	// One step per request at a time. A second RequestWorkflow for the same
	// request (an older binary's wake started one under another id) must not
	// run the step beside the first: it fails with stepInProgressErrorType,
	// which RequestWorkflow handles as any failed step, waiting RetryAfter
	// before it loads the request again.
	if !a.steps.begin(id) {
		return "", temporal.NewApplicationError("another step of this request is running", stepInProgressErrorType)
	}
	defer a.steps.end(id)
	// The step runs under a context a stalled connection to Temporal cannot
	// cancel (workflow.GuardStep): a build that lost nothing is not killed.
	ctx, endGuard := workflow.GuardStep(ctx)
	defer endGuard()
	// A cancel of the request stops the step (cause request.ErrCancelled): a
	// build in its sandbox included, whose worktree is rolled back.
	ctx, endWatch := cancelWhenRequestCancelled(ctx, a.dataDir, id, cmp.Or(a.cancelPoll, requestHeartbeatInterval))
	defer endWatch()
	a.ensureConsole()
	r, err := request.Load(a.dataDir, id)
	if err != nil {
		return "", fmt.Errorf("load request %s: %w", id, err)
	}
	if r.State != request.StateBuilding {
		repositoryWaits.clear(id)
	}
	if requestdriver.RequestDriverOwnsState(r.State) {
		onJobs := a.runsOnJobsQueue(ctx)
		if onJobs {
			// Every step on the jobs queue holds a slot, a pr_review pass
			// included: status says what a request waiting for one is
			// waiting on.
			slotRequests.add(id)
			defer slotRequests.remove(id)
		}
		ownSteps := 0
		if onJobs && r.State.RunsJob() {
			// A pr_review poll is not a job, as in worker's driver.
			ownSteps = 1
			addActiveRequest(id)
			defer removeActiveRequest(id)
		}
		if r.State == request.StateBuilding {
			// A memory request whose AGENTS.md moved since it was proposed
			// would be refused at release: it is halted before a build is
			// spent on it.
			if reason := staleMemoryRequest(ctx, a.dp, a.dataDir, r); reason != "" {
				repositoryWaits.clear(id)
				if err := requestdriver.HaltRequest(a.dataDir, r, reason, time.Now()); err != nil {
					return "", err
				}
				return string(r.State), nil
			}
			// A build on a repository another build holds gives its slot
			// back at once; the workflow retries after requestRepoBusyRetryAfter.
			// A step of this process for the request is refused above
			// (stepGuard); this covers a build another process owns, which
			// is still building this ticket: a second build of it would
			// run beside the first.
			if live, why := a.ticketRunStillLive(r, ownSteps); live {
				// Its own run, not another build, holds it.
				repositoryWaits.clear(id)
				log.Printf("request %s: not starting another build step: %s", r.ID, why)
				return "", temporal.NewApplicationError("the ticket's run is still being built by a live process", repositoryBusyErrorType)
			}
			if repoBusy(r.Workspace, a.dataDir) {
				// status says the request waits for the repository, from the
				// first time it was found busy.
				repositoryWaits.markBusy(id, time.Now())
				return "", temporal.NewApplicationError("repository busy: another build holds it", repositoryBusyErrorType)
			}
			repositoryWaits.clear(id)
		}
		if stepWaitsForRepoLock(r.State, onJobs) {
			ctx = withWaitForRepoLock(ctx)
			// A wait for the lock inside the slot reaches the heartbeat, so
			// status names it.
			ctx = withRepoLockWaitHook(ctx, func(waiting bool) {
				if waiting {
					repositoryWaits.markBusy(id, time.Now())
				} else {
					repositoryWaits.clear(id)
				}
			})
		}
		// Whatever the outcome (a panic included), the step no longer waits
		// inside its slot.
		defer repositoryWaits.clear(id)
		stopHeartbeat := heartbeatUntilDone(ctx, requestHeartbeatInterval)
		err = requestdriver.AdvanceRequest(a.dp, ctx, a.dataDir, r, a.cfg, a.specRunner, a.planRunner, a.oracleRunner, a.buildRunner)
		stopHeartbeat()
		if err != nil && !errors.Is(err, request.ErrCancelledOnDisk) {
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

// consoleStartEvery is the least time between two attempts to start the
// console: a `serve` that cannot start, or one the operator stopped, is not
// started again on every step.
const consoleStartEvery = 5 * time.Minute

// ensureConsole starts this data dir's `serve` when none answers, so that
// the notification a step ends in has a page to link to: one attempt at a
// time, and at most one every consoleStartEvery. It does nothing under
// FACTORYD_AUTOSTART=0 or without a session config; a notification then has
// no link and says to run `factoryd console`.
func (a *requestActivities) ensureConsole() {
	if consolelink.ServeAddress(a.dataDir) != "" || !hostcontrol.AutostartEnabled() || a.cfg.SessionConfigPath == "" {
		return
	}
	a.consoleStart.Lock()
	defer a.consoleStart.Unlock()
	if consolelink.ServeAddress(a.dataDir) != "" || (!a.consoleStartedAt.IsZero() && time.Since(a.consoleStartedAt) < consoleStartEvery) {
		return
	}
	a.consoleStartedAt = time.Now()
	binaryPath, err := a.dp.host.executable()
	if err != nil {
		return
	}
	hostcontrol.EnsureServe(a.dp, log.Writer(), binaryPath, a.cfg.SessionConfigPath, a.dataDir)
}

// ticketRunStillLive reports whether the run of r's current ticket is being
// built right now, and why it says so ("" when not): a building request and a
// run record that has not finished, and either a step of this process in
// flight for the request beyond the self steps counted for the caller (the
// build runs in this process, so its owner pid is always alive and says
// nothing), or, for a run another process owns, that process alive with a
// fresh owner heartbeat. Anything not shown is "not live": the run is lost.
func (a *requestActivities) ticketRunStillLive(r *request.Request, self int) (bool, string) {
	if r.State != request.StateBuilding {
		return false, ""
	}
	ticket, err := requestdriver.TicketAt(r, max(r.TicketIndex, 1))
	if err != nil || ticket.RunID == "" {
		return false, ""
	}
	rec, err := run.Load(a.dataDir, ticket.RunID)
	if err != nil || rec.State == run.StateAccepted || rec.State == run.StateQuarantined || rec.State == run.StateHalted {
		return false, ""
	}
	if n := countOf(currentActiveRequests(), r.ID); n > self {
		return true, fmt.Sprintf("a step of this process is still running run %s", ticket.RunID)
	}
	pid, ok := sandbox.OwnerPID(a.dataDir, ticket.RunID)
	if ok && pid != os.Getpid() && !runOwnerDead(a.dataDir, ticket.RunID) && sandbox.OwnerHeartbeatFresh(a.dataDir, ticket.RunID) {
		return true, fmt.Sprintf("process %d is supervising run %s (fresh owner heartbeat)", pid, ticket.RunID)
	}
	return false, ""
}

// RemindRequest sends request id's review reminder when one is due, after
// halting the request if its gate has expired.
func (a *requestActivities) RemindRequest(_ context.Context, id string) error {
	a.ensureConsole()
	// A gate past its review_gate_expiry halts instead of reminding.
	if expired, err := requestdriver.ExpireIfDue(a.dataDir, id, a.cfg.Settings.ReviewGateExpiry, time.Now()); err != nil || expired {
		return err
	}
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
	if a.steps.running(in.RequestID) {
		// The server timed the step out, but the step is still running in
		// this process (a drafting job has no run to show it): halting now
		// would have it save its stale request over the operator's resume
		// when it ends. A new step waits for it (stepGuard).
		log.Printf("request %s: its step's heartbeat timed out, but the step is still running; not moving it to resume_review", r.ID)
		return nil
	}
	if live, why := a.ticketRunStillLive(r, 0); live {
		// The server timed the step out, but its run is not lost: its owner
		// process is alive and its sandbox is there. The step moves the
		// request itself when the run ends, and a new step waits for it.
		log.Printf("request %s: its step's heartbeat timed out, but the ticket's run is still live (%s); not moving it to resume_review", r.ID, why)
		return nil
	}
	return requestdriver.EnterResumeReviewHoldingLock(a.dataDir, r, request.LostStep{RunID: lostRunOf(a.dataDir, r), Cause: unresolved.WorkerLost}, time.Now())
}

// countOf is how many entries of ids are id.
func countOf(ids []string, id string) int {
	n := 0
	for _, x := range ids {
		if x == id {
			n++
		}
	}
	return n
}
