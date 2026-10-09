package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/forge"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// runViaRepositoryOwner is runViaTemporal's counterpart that actually
// makes the plan's "at most one worker owns a slice at a time" invariant
// real across separate factoryd invocations, not just within
// internal/workflow's own isolated tests: instead of starting RunWorkflow
// directly on a run-unique task queue (guaranteeing, by construction,
// that this process's own short-lived Worker is the only one that could
// ever execute its Activities), it signals — starting if not already
// running — the RepositoryOwnerWorkflow for -repository on a task queue
// shared by every invocation targeting that repository, then polls the
// owner's durable query until this run's own request completes. A
// separately launched factoryd for the same repository signals the same
// owner execution and is serialized behind whatever it's already
// processing, by RepositoryOwnerWorkflow's own one-at-a-time signal loop.
//
// On its own timeout/cancellation, unlike runViaTemporal (which owns its
// Workflow Execution exclusively and simply terminates it), this cannot
// blindly terminate anything — RepositoryOwnerWorkflow is shared
// infrastructure another invocation may already be relying on. Found
// live, not just reasoned about: the first version of this function left
// an abandoned child running indefinitely rather than risk terminating
// the wrong thing, and a repeated series of abandoned waits during
// testing left dozens of child Workflow Executions permanently "Running"
// on the server, measurably degrading it for every later run.
// pollRepositoryOwnerResult's own terminateOrCancelOwnRequest closes
// this: RepositoryOwnerResult.InProgress identifies whichever request the
// owner is *currently* executing, so a submitter that gives up can verify
// that's genuinely its own request (matched by its own unique RequestID,
// which can never belong to a different one) before terminating that
// exact child — safe specifically because of that match, not a blanket
// "terminate whatever the owner is doing."
func runViaRepositoryOwner(dp *deps, lifecycleCtx context.Context, temporalClient client.Client, r *run.Run, opts runOptions) error {
	// See runViaTemporal's matching comment (including its round-2 update
	// on canonicalPath vs. filepath.Abs): -data-dir defaults to the
	// relative "data", and LogDir/CheckpointDir below are carried
	// per-execution specifically so a Worker other than this process's own
	// (any Worker polling the shared task queue below) can execute
	// RunBuildActivity/RunVerifyActivity correctly.
	absDataDir, err := canonicalPath(opts.DataDir)
	if err != nil {
		// HaltConfirmed: true — this is a purely local failure before any
		// repository owner or Temporal child was ever contacted; there is
		// nothing to strand, so it is definitively terminal, not the
		// ambiguous give-up case HaltConfirmed's doc comment describes.
		r.State = run.StateHalted
		r.HaltConfirmed = true
		if saveErr := save(r, opts.DataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return fmt.Errorf("resolve data dir: %w", err)
	}
	opts.DataDir = absDataDir

	ownerID := workflow.RepositoryOwnerWorkflowID(opts.Repository)
	if divergeErr := checkSandboxDockerAgainstDaemonHeartbeat(opts.DataDir, ownerID, opts.ID, opts.Repository, opts.SandboxImage, opts.SandboxDocker); divergeErr != nil {
		// HaltConfirmed: true — same reasoning as every other pre-signal
		// failure branch in this function: SignalWithStartWorkflow below
		// is never reached, so nothing is stranded for this id.
		r.State = run.StateHalted
		r.HaltConfirmed = true
		if saveErr := save(r, opts.DataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return divergeErr
	}
	// Shared across every invocation targeting this repository, unlike
	// runViaTemporal's run-unique "factoryd-"+id — that's the whole point:
	// every one of them must be able to signal, and contribute a Worker
	// to, the exact same RepositoryOwnerWorkflow execution.
	ownerTaskQueue := "factoryd-repo-" + ownerID
	// Keep child execution on a request-specific queue. The shared owner
	// queue only carries the short owner Workflow tasks; this invocation's
	// Worker therefore cannot borrow another request's long-running Activity
	// and cancel it when this invocation exits.
	runTaskQueue := ownerTaskQueue + "-run-" + opts.ID
	checkpointDir := opts.checkpointDir()
	activities := opts.activities()
	activities.Sandboxes, activities.MeterLedgerRoot = dp.sandbox.runtime(), meterLedgerRoot()
	effectiveTimeout := opts.OverallTimeout
	if effectiveTimeout == 0 {
		effectiveTimeout = time.Duration(opts.TimeoutMinutes+5) * time.Minute
	}
	ownerWorker := temporalworker.New(temporalClient, ownerTaskQueue, workflow.BoundedWorkerOptions(workerStopTimeout))
	ownerWorker.RegisterWorkflow(workflow.RepositoryOwnerWorkflow)
	if err := ownerWorker.Start(); err != nil {
		// HaltConfirmed: true — SignalWithStartWorkflow below is never
		// reached, so this request never entered the shared system at all;
		// nothing to strand for this id specifically.
		r.State = run.StateHalted
		r.HaltConfirmed = true
		if saveErr := save(r, opts.DataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return fmt.Errorf("start repository owner worker: %w", err)
	}
	defer ownerWorker.Stop()

	runWorker := temporalworker.New(temporalClient, runTaskQueue, workflow.BoundedWorkerOptions(workerStopTimeout))
	runWorker.RegisterWorkflow(workflow.RunWorkflow)
	runWorker.RegisterActivity(activities)
	if err := runWorker.Start(); err != nil {
		// HaltConfirmed: true — same reasoning as ownerWorker.Start()'s own
		// failure branch above: SignalWithStartWorkflow is never reached.
		r.State = run.StateHalted
		r.HaltConfirmed = true
		if saveErr := save(r, opts.DataDir); saveErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, saveErr)
		}
		return fmt.Errorf("start run worker: %w", err)
	}
	defer runWorker.Stop()

	runCtx, cancel := context.WithTimeout(lifecycleCtx, effectiveTimeout)
	defer cancel()

	// Every one of these fields (not just LogDir/CheckpointDir) must be
	// carried per-execution here — found live, not just reasoned about
	// (see RunWorkflowInput's doc comment): this run's Activities can be
	// dispatched to any Worker polling the shared taskQueue below, not
	// necessarily this process's own.
	// runOptions.workflowInput is shared with runViaTemporal and with the
	// Temporal-parity test's own runTemporalPathFixture -- see that function's
	// doc comment (run_temporal.go). Project/RepositoryOwnerID/TaskQueue
	// are this path's own, set directly on the result: they are what
	// makes this a repository-owned execution rather than a run-unique
	// one, so they belong here and not in the shared builder.
	runInput := opts.workflowInput()
	runInput.Project = r.Project
	runInput.RepositoryOwnerID = ownerID
	runInput.TaskQueue = runTaskQueue
	// SignalWithStartWorkflow starts the owner if it isn't already running
	// for this repository, or signals the existing one — atomically, so
	// two invocations racing to submit the first run for a repository
	// can't both start a competing owner execution. id is RequestID: it's
	// already this run's own durable, unique identifier.
	if _, err := temporalClient.SignalWithStartWorkflow(runCtx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: opts.ID, Input: runInput},
		client.StartWorkflowOptions{ID: ownerID, TaskQueue: ownerTaskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: opts.Repository},
	); err != nil {
		// SignalWithStart may have accepted the signal before the transport
		// returned an error. Reconcile that ambiguous outcome before reporting
		// this run halted: cancel it if still queued, terminate it if the
		// owner already started this exact child.
		giveUpResult, done, confirmed := terminateOrCancelOwnRequest(temporalClient, ownerID, opts.ID, checkpointDir)
		if done {
			// Found via review: the owner actually has a real, final result
			// for this request despite the ambiguous transport error above —
			// apply it normally instead of forcing a halt that would
			// permanently misreport an accepted or quarantined run.
			return run.WithLock(opts.DataDir, opts.ID, func() error {
				fresh, freshErr := run.Load(opts.DataDir, opts.ID)
				if freshErr != nil {
					return freshErr
				}
				if runTerminalConfirmed(fresh) {
					*r = *fresh
					return alreadyReconciledResultError(fresh)
				}
				stampRouteSkips(giveUpResult.Attempts, opts.Slice)
				return applyRunWorkflowResult(dp, r, opts.DataDir, opts.ID, opts.Ticket, opts.WorkspacePath, opts.BaseSHA, ownerTaskQueue, giveUpResult, true, &opts.ReleasePolicy, forge.GHPullRequestOpener{}, true)
			})
		}
		r.State = run.StateHalted
		r.HaltConfirmed = confirmed
		if giveUpResult.WorkspacePath != "" {
			r.WorkspacePath = giveUpResult.WorkspacePath
			r.Branch = giveUpResult.Branch
		}
		// run.WithLock, reloading and re-checking inside it — found via
		// review: this save used to be unlocked, so it could race the
		// daemon's own reconciliation scan (which can run concurrently the
		// moment the owner accepts this ambiguous SignalWithStart — the
		// scan doesn't know it was ambiguous, only that the request exists
		// and is nonterminal) and overwrite an already-reconciled
		// accepted/quarantined result with this stale halted guess,
		// discarding real terminal evidence and notifications. Same
		// pattern as the other two writers of this run's record.
		if lockErr := run.WithLock(opts.DataDir, opts.ID, func() error {
			fresh, freshErr := run.Load(opts.DataDir, opts.ID)
			if freshErr != nil {
				return freshErr
			}
			if runTerminalConfirmed(fresh) {
				*r = *fresh
				return nil
			}
			return save(r, opts.DataDir)
		}); lockErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, lockErr)
		}
		return fmt.Errorf("signal repository owner workflow: %w", err)
	}

	result, haltConfirmed, err := pollRepositoryOwnerResult(runCtx, temporalClient, ownerID, opts.ID, checkpointDir, r, opts.DataDir, runTaskQueue, opts.TemporalAddress, opts.Repository)
	if err != nil {
		// pollRepositoryOwnerResult's own terminateOrCancelOwnRequest
		// already attempted to terminate this run's own child, if the
		// owner's query still identified it as in progress at the moment
		// this gave up — see that function's doc comment for why that's
		// safe (matched by this run's own unique id) despite the owner
		// itself being shared infrastructure.
		if runCtx.Err() != nil {
			log.Printf("run %s: gave up waiting on repository owner %s (attempted to terminate this run's own in-progress child, if any)", opts.ID, ownerID)
		}
		// Layered recovery, cheapest/most-specific first: if
		// RepositoryOwnerWorkflow's own child-failure isolation already
		// handed back a result (result.Attempts populated directly, no
		// error-Details round-trip needed), prefer that; otherwise fall
		// back the same way runViaTemporal's matching branch does — via
		// AttemptsFromError, then reading the durable checkpoints
		// directly for a client-side timeout/cancellation, which produces
		// neither a result nor an ApplicationError to recover from (see
		// workflow.AttemptsFromError's and
		// workflow.RecoverAttemptsFromCheckpointDir's doc comments).
		attempts := result.Attempts
		if len(attempts) == 0 {
			attempts = workflow.AttemptsFromError(err)
		}
		if len(attempts) == 0 {
			attempts = workflow.RecoverAttemptsFromCheckpointDir(checkpointDir)
		}
		stampRouteSkips(attempts, opts.Slice)
		r.Attempts = append(r.Attempts, attempts...)
		// Found via review: this used to leave r.BaseSHA at its
		// pre-submission value even when the request actually waited
		// behind another queued run and CaptureBaseSHAActivity captured a
		// materially different, fresher HEAD before later failing — the
		// durable halted record would then identify the wrong baseline.
		// result.BaseSHA is only ever populated on a genuinely successful
		// per-request result (RepositoryOwnerWorkflow's own halted-result
		// literal never sets it), so the execution-time value recovered
		// from err's Details — the same wrapActivityFailure round trip
		// AttemptsFromError already uses — is checked first.
		if capturedBaseSHA := workflow.BaseSHAFromError(err); capturedBaseSHA != "" {
			opts.BaseSHA = capturedBaseSHA
		} else if result.BaseSHA != "" {
			opts.BaseSHA = result.BaseSHA
		}
		r.BaseSHA = opts.BaseSHA
		// recoverIsolatedWorkspace: same layered recovery as Attempts/
		// BaseSHA above — prefers result.WorkspacePath/Branch
		// (RepositoryOwnerWorkflow's own child-failure synthesis already
		// recovered these via IsolatedWorkspaceFromError, no second
		// round-trip needed), falling back to recovering them directly
		// from err's own Details for the case where the owner itself never
		// got a result to synthesize from (e.g. this poll gave up before
		// the owner's query ever returned one).
		if wp, br := recoverIsolatedWorkspace(result, err); wp != "" {
			r.WorkspacePath = wp
			r.Branch = br
		}
		r.State = run.StateHalted
		r.HaltConfirmed = haltConfirmed
		// run.WithLock, reloading and re-checking inside it — see the
		// matching comment on the success path's own lock below for why:
		// the owner completing this request with result.Err set is exactly
		// what reconcileReclaimedRun's own failed-result branch also acts
		// on, so a concurrent daemon scan can race this exact save.
		if lockErr := run.WithLock(opts.DataDir, opts.ID, func() error {
			fresh, freshErr := run.Load(opts.DataDir, opts.ID)
			if freshErr != nil {
				return freshErr
			}
			if runTerminalConfirmed(fresh) {
				*r = *fresh
				return nil
			}
			return save(r, opts.DataDir)
		}); lockErr != nil {
			log.Printf("run %s: additionally failed to persist %s state: %v", opts.ID, r.State, lockErr)
		}
		return fmt.Errorf("repository owner workflow did not complete this run: %w", err)
	}

	// run.WithLock, reloading and re-checking run.json's state after
	// acquiring it — found via review, twice: a daemon's periodic reclaim
	// scan can legitimately observe the same owner result at roughly the
	// same time this submitter does (runsNeedingReclaim has no way to
	// distinguish "an abandoned run" from "a live submitter's own run
	// mid-poll"), and its reconcileReclaimedRun performs the identical
	// load-decide-write sequence against the same run.json. The lock alone
	// isn't enough — the first version of this fix took it but still
	// applied the result to the stale pre-lock r instead of re-checking
	// disk, so the "loser" side still re-applied (and re-notified) an
	// already-reconciled result. Reloading fresh inside the lock and
	// checking runTerminalConfirmed before ever calling
	// applyRunWorkflowResult is what actually makes the loser a no-op.
	return run.WithLock(opts.DataDir, opts.ID, func() error {
		fresh, freshErr := run.Load(opts.DataDir, opts.ID)
		if freshErr != nil {
			return freshErr
		}
		if runTerminalConfirmed(fresh) {
			*r = *fresh
			return alreadyReconciledResultError(fresh)
		}
		stampRouteSkips(result.Attempts, opts.Slice)
		result = requireRepoGateResults(opts.GateCommands, result)
		result = requireSetupRan(opts.SetupCommands, result)
		return applyRunWorkflowResult(dp, r, opts.DataDir, opts.ID, opts.Ticket, opts.WorkspacePath, opts.BaseSHA, ownerTaskQueue, result, true, &opts.ReleasePolicy, forge.GHPullRequestOpener{}, true)
	})
}

// alreadyReconciledResultError preserves applyRunWorkflowResult's own
// contract — a non-nil error for anything other than StateAccepted — for
// the "loser" side of the race that lock closure guards against. Found via
// review: returning nil unconditionally there once a concurrent daemon
// reconciliation had already reached a terminal state broke that contract
// outright — a daemon that reconciled this exact request to quarantined a
// moment before this submitter acquired the lock made the submitting
// CLI/API report success (exit 0, "FINAL state=accepted" never even
// printed) for a run the policy gate actually rejected. Factored out for
// direct unit testing, since the race itself is not practical to force
// live.
func alreadyReconciledResultError(r *run.Run) error {
	switch r.State {
	case run.StateAccepted:
		return nil
	case run.StateQuarantined:
		if len(r.Notifications) > 0 {
			return fmt.Errorf("run quarantined: %s", r.Notifications[len(r.Notifications)-1].Reason)
		}
		return fmt.Errorf("run quarantined")
	default:
		return fmt.Errorf("run %s (reconciled by a concurrent recovery scan before this invocation completed it)", r.State)
	}
}

// pollRepositoryOwnerResult repeatedly queries RepositoryOwnerWorkflow's
// durable RepositoryOwnerQueryName result until requestID appears in it
// (this run's own request has completed) or ctx is done. A query error is
// tolerated and retried, not fatal: right after SignalWithStartWorkflow
// returns, the owner's Workflow Task (which registers the query handler)
// may not have been processed by any Worker yet, and a brand-new
// execution racing that registration is expected, not exceptional.
//
// A completed request whose own RunWorkflowResult.Err is set (the child
// RunWorkflow itself failed — see RunWorkflowResult.Err's doc comment) is
// returned as an error too, alongside that same result value (so its
// already-recovered Attempts aren't lost) — mirroring runViaTemporal's own
// convention that a normal, non-error result is only ever State
// Accepted/Quarantined, never Halted.
//
// The returned haltConfirmed is only meaningful alongside a non-nil ctx.Err()
// (the give-up path below) — see terminateOrCancelOwnRequest's doc comment
// for what it means and why a caller recording this run as halted must
// carry it into run.Run.HaltConfirmed. It is always true for every other
// return, since none of those represent this submitter giving up.
// r/dataDir/runTaskQueue/temporalAddress are best-effort: the owner's own
// InProgress.ChildWorkflowID/ChildRunID (see RunWorkflow's own comment on
// deterministicChildWorkflowIDVersion for why a random, server-generated id
// preceded this and can't be recovered after the fact) are the only place
// this run's real Temporal child identity is ever visible before it
// completes — RunWorkflowResult itself carries none of it. Recorded once,
// the first time this poll observes it, so `factoryd status`/the Temporal
// Web UI can find an in-flight -repository run instead of only a completed
// one.
func pollRepositoryOwnerResult(ctx context.Context, temporalClient client.Client, ownerID, requestID, checkpointDir string, r *run.Run, dataDir, runTaskQueue, temporalAddress, repository string) (result workflow.RunWorkflowResult, haltConfirmed bool, err error) {
	recordedChild := false
	lastQueuePosition := -1
	for {
		queryResp, queryErr := temporalClient.QueryWorkflow(ctx, ownerID, "", workflow.RepositoryOwnerQueryName)
		if queryErr == nil {
			var ownerResult workflow.RepositoryOwnerResult
			if decodeErr := queryResp.Get(&ownerResult); decodeErr == nil {
				if !recordedChild && ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == requestID && ownerResult.InProgress.ChildWorkflowID != "" {
					r.TemporalWorkflowID = ownerResult.InProgress.ChildWorkflowID
					r.TemporalRunID = ownerResult.InProgress.ChildRunID
					r.TemporalTaskQueue = runTaskQueue
					r.TemporalAddress = temporalAddress
					if saveErr := save(r, dataDir); saveErr != nil {
						log.Printf("run %s: additionally failed to persist Temporal child workflow ids: %v", requestID, saveErr)
					}
					recordedChild = true
				}
				// See requestsAheadOf's own doc comment: only meaningful
				// while this request is still waiting its turn, and
				// re-emitted only when the count actually changes, per
				// progress-contract.md's "queued" stage.
				if ahead := requestsAheadOf(ownerResult, requestID); ahead != lastQueuePosition {
					if ahead > 0 {
						progressMark(dataDir, requestID, "queued", "note", "", fmt.Sprintf("behind %d run(s) on %s", ahead, repository))
					}
					lastQueuePosition = ahead
				}
				if result, done := ownerResult.Runs[requestID]; done {
					if result.Err != "" {
						return result, !result.CleanupUnconfirmed, fmt.Errorf("%s", result.Err)
					}
					return result, true, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			// Found via review: terminateOrCancelOwnRequest can itself
			// discover the owner already has a real, final result for this
			// request (it raced this ctx expiring) — that must be returned
			// and applied normally, exactly like the loop's own successful
			// case above, never discarded in favor of forcing a halt.
			giveUpResult, done, confirmed := terminateOrCancelOwnRequest(temporalClient, ownerID, requestID, checkpointDir)
			if done {
				if giveUpResult.Err != "" {
					return giveUpResult, !giveUpResult.CleanupUnconfirmed, fmt.Errorf("%s", giveUpResult.Err)
				}
				return giveUpResult, true, nil
			}
			return giveUpResult, confirmed, ctx.Err()
		case <-time.After(repositoryOwnerPollInterval):
		}
	}
}

// requestsAheadOf reports how many other requests must complete or start
// before requestID's own turn, from one query snapshot of the repository
// owner's result (see RepositoryOwnerResult.QueuedRequestIDs' own doc
// comment for why that field is a live, query-time-only snapshot rather
// than durable state). Zero means requestID is not waiting behind
// anything it can see right now — either it is itself InProgress, it has
// already completed (present in Runs), or the owner has nothing else
// queued ahead of it. A request the owner reports neither in progress,
// queued, nor completed also counts as zero — the moment right after
// SignalWithStartWorkflow returns but before the owner's next iteration
// has drained it into pendingQueue, in which case there is nothing yet to
// report it as waiting behind.
func requestsAheadOf(result workflow.RepositoryOwnerResult, requestID string) int {
	if result.InProgress != nil && result.InProgress.RequestID == requestID {
		return 0
	}
	ahead := 0
	if result.InProgress != nil {
		ahead++
	}
	for _, queued := range result.QueuedRequestIDs {
		if queued == requestID {
			return ahead
		}
		ahead++
	}
	return 0
}

// terminateOrCancelOwnRequest is called only once a submitter has given up
// waiting on its own request (a client-side timeout or cancellation) —
// either pollRepositoryOwnerResult's own ctx is already done, or
// SignalWithStartWorkflow itself returned an ambiguous transport error.
// Found live: leaving an abandoned child Workflow Execution running
// indefinitely — the original, intentional design, since the owner is
// shared infrastructure this submitter cannot unilaterally interrupt on
// someone else's behalf — meant that a repeated series of abandoned waits
// (e.g. during testing) left dozens of child Workflow Executions
// permanently "Running" on the server with no Worker ever left polling
// their own now-dead task queue to service them, measurably degrading the
// server for every later run. Acting on this request is safe specifically
// because every path below only ever targets or cancels a match on this
// exact requestID — this submitter's own unique identifier, which can only
// ever belong to a request it is itself waiting on — never a different one
// queued behind or ahead of it. Best-effort: fresh, short-lived contexts
// are used since the caller's own is already done.
//
// Returns (result, done, confirmed). done=true means the owner already has
// a real, final RunWorkflowResult for this request (Runs[requestID]) —
// found via review: an earlier version reduced this to a bare confirmed
// bool even when it had this actual result in hand, so a caller that then
// unconditionally recorded StateHalted could permanently misreport a
// request the owner actually accepted or quarantined. A caller MUST apply
// result the same way a normal (non-give-up) completion would, never force
// Halted, whenever done is true.
//
// When done is false, confirmed carries the original meaning: true only
// when there is a positive reason to believe this request cannot run to
// completion unattended afterward — an in-progress child was actually
// terminated (or the termination call reported it already closed), or a
// still-queued request's CancelRunSignal was delivered AND a re-query
// confirms the owner did not start it anyway in the meantime. confirmed is
// false for every path that only attempted an action without a positive
// signal it succeeded — the owner's query itself failing, TerminateWorkflow
// failing for a reason other than "already completed", or the cancel
// signal itself failing to deliver. A caller that then durably records
// this run as halted (done being false) must also record that the halt is
// unconfirmed, so a later reclaim scan doesn't retire this run's recovery
// Worker (or skip starting one) while the request might still run to
// completion with nothing polling its queue.
//
// Found via review, twice more before the done/result split above: the
// first version returned true from the query simply not finding requestID
// in Runs or InProgress — a request racing its own cancellation signal,
// still queued at query time, incorrectly counted as confirmed and could
// still start running later with nothing waiting on it. The second version
// fixed that by sending CancelRunSignal for the queued case, but trusted
// the signal's own delivery as proof of safety — the owner does not
// consume queued signals while blocked awaiting its current child's
// childFuture.Get, so it can still dequeue and start this exact request
// between this function's initial query and the moment the owner actually
// processes the delivered cancellation. Only a re-query after sending it,
// terminating the child directly if the owner started it anyway, closes
// that window.
// checkpointDir is this specific request's own (filepath.Join(dataDir,
// "temporal-checkpoints", requestID)) — used only to attempt an isolated
// workspace's caller-side rollback after a confirmed hard termination
// (see rollbackIsolatedWorkspaceIfTerminated's own doc comment for why
// that's necessary at all: TerminateWorkflow, used below, closes the
// execution without running another workflow task, so the child
// RunWorkflow's own deferred rollback is never actually scheduled).
func terminateOrCancelOwnRequest(temporalClient client.Client, ownerID, requestID, checkpointDir string) (result workflow.RunWorkflowResult, done bool, confirmed bool) {
	// terminateInProgress's own workspacePath/branch return values (found
	// via a live end-to-end check, not just reasoned about: the actual
	// rollback ran correctly, but the durable run record still showed the
	// shared checkout instead of the isolated one) let both call sites
	// below build a populated RunWorkflowResult instead of discarding
	// this into the empty literal they used to return — mirroring
	// runViaTemporal's own recoveredWorktreePath/recoveredBranch handling
	// for the same give-up-and-terminate case.
	terminateInProgress := func(inProgress *workflow.InProgressRun) (confirmed bool, workspacePath, branch string) {
		terminateCtx, cancelTerminate := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelTerminate()
		err := temporalClient.TerminateWorkflow(terminateCtx, inProgress.ChildWorkflowID, inProgress.ChildRunID,
			"factoryd: submitting invocation gave up waiting (supervisor timeout or operator cancellation); this child was still this invocation's own in-progress request")
		confirmed = err == nil
		if !confirmed {
			var notFound *serviceerror.NotFound
			confirmed = errors.As(err, &notFound)
		}
		if confirmed {
			workspacePath, branch = rollbackIsolatedWorkspaceIfTerminated(temporalClient, inProgress.ChildWorkflowID, inProgress.ChildRunID, checkpointDir, false)
		}
		return confirmed, workspacePath, branch
	}

	queryOwner := func() (workflow.RepositoryOwnerResult, error) {
		var ownerResult workflow.RepositoryOwnerResult
		queryCtx, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelQuery()
		queryResp, err := temporalClient.QueryWorkflow(queryCtx, ownerID, "", workflow.RepositoryOwnerQueryName)
		if err != nil {
			return ownerResult, err
		}
		return ownerResult, queryResp.Get(&ownerResult)
	}

	ownerResult, err := queryOwner()
	if err != nil {
		return workflow.RunWorkflowResult{}, false, false
	}
	if r, ok := ownerResult.Runs[requestID]; ok {
		// The owner already finished this request by the time this
		// submitter gave up — there is no child left to strand, and its
		// real result must be applied, not discarded in favor of a forced
		// halt.
		return r, !r.CleanupUnconfirmed, true
	}
	if ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == requestID {
		confirmed, wp, br := terminateInProgress(ownerResult.InProgress)
		return workflow.RunWorkflowResult{WorkspacePath: wp, Branch: br}, false, confirmed
	}

	// Neither done nor in progress: still queued (or the owner hasn't yet
	// processed the SubmitRunSignal that would make it either). The owner
	// consumes CancelRunSignal durably and drops the request if it is still
	// queued when processed.
	cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	signalErr := temporalClient.SignalWorkflow(cancelCtx, ownerID, "", workflow.CancelRunSignalName,
		workflow.CancelRunSignal{RequestID: requestID})
	cancelCancel()
	if signalErr != nil {
		return workflow.RunWorkflowResult{}, false, false
	}

	// Delivery alone isn't proof of safety — the owner does not consume
	// queued signals while blocked awaiting its current child's
	// childFuture.Get, so it can dequeue and start this exact request any
	// time between the signal above and whenever it actually gets around to
	// processing it. Found via a later review round: a single re-query
	// right after sending the signal raced this window just as easily as
	// no re-query at all — the request can still be absent from both Runs
	// and InProgress simply because the owner hasn't reached it yet, not
	// because the cancellation actually took. Poll for a bounded window
	// instead of trusting one snapshot: only a query that positively shows
	// this request done (Runs[requestID], regardless of whether that's the
	// canceled-before-execution result or the owner started it anyway and
	// it already finished) or in progress (terminated directly, same as the
	// original in-progress branch above) counts as confirmed. Timing out
	// still queued/invisible is the conservative, honest outcome — this
	// function cannot tell "definitely dropped" from "owner just hasn't
	// looked at it yet" without one of those two positive signals.
	pollDeadline := time.Now().Add(5 * time.Second)
	for {
		ownerResult, err = queryOwner()
		if err == nil {
			if r, ok := ownerResult.Runs[requestID]; ok {
				return r, !r.CleanupUnconfirmed, true
			}
			if ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == requestID {
				confirmed, wp, br := terminateInProgress(ownerResult.InProgress)
				return workflow.RunWorkflowResult{WorkspacePath: wp, Branch: br}, false, confirmed
			}
		}
		if time.Now().After(pollDeadline) {
			return workflow.RunWorkflowResult{}, false, false
		}
		time.Sleep(repositoryOwnerPollInterval)
	}
}
