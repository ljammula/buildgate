package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"buildgate/internal/forge"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// runTerminalConfirmed reports whether r is durably done in a way that is
// safe to trust without re-verifying against the repository owner. Shared
// by every caller that needs this exact judgment — runsNeedingReclaim,
// terminalReclaimedRunIDs, and reconcileReclaimedRun — so the definition
// of "genuinely terminal" can't drift between them. The judgment itself now
// lives in run.Run.TerminalConfirmed, shared with
// internal/api's own runViewFor -- this stays a thin wrapper so this
// package's existing call sites are untouched.
func runTerminalConfirmed(r *run.Run) bool {
	return r.TerminalConfirmed()
}

// scratchRunInFlight is the sandbox.ReconcileScratchDirs predicate every
// caller shares: a run's scratch cache may still be in use while its
// record is non-terminal. runTerminalConfirmed, not a bare state check, so
// a Temporal submitter that gave up waiting (StateHalted, HaltConfirmed
// false) keeps the cache a daemon-side Worker may still be building in;
// the daemon's own reclaim scan removes it once the run is reconciled
// terminal. A missing record is not in flight.
func scratchRunInFlight(dataDir string) func(runID string) bool {
	return func(runID string) bool {
		r, err := run.Load(dataDir, runID)
		return err == nil && !runTerminalConfirmed(r)
	}
}

// removeScratchDirs removes the scratch cache of every run under dataDir
// that is no longer in flight, logging each removal.
func removeScratchDirs(dataDir string) {
	for _, stale := range sandbox.ReconcileScratchDirs(dataDir, scratchRunInFlight(dataDir)) {
		log.Printf("removed scratch cache left by run %s", stale)
	}
}

// liveRunnerProcessPID reports the PID of a still-running runner.run
// process group for runID, if any -- build_app.py, canonical verification,
// or the optional full-suite command, whichever this run has launched
// (found via review: an earlier version of this glob matched only
// build_app*.log.pid, missing verify.log.pid/full_suite_verify.log.pid).
// Every runner.run invocation leaves its own <logPath>.pid file alongside
// its log while the process it names is running (see runner.run's own
// pidFilePath comment). Glob, not a single well-known name, because a
// retried build's Nth attempt logs to build_app.attempt<N>.log — only the
// most recent attempt that never got to clean up its own pid file (an
// ungraceful kill, or one still genuinely in flight) can still have one
// present; every prior, normally-finished attempt already removed its own
// via its own defer. Returns (0, nil) when nothing is running (nothing to
// preserve). A pid file naming a process the OS reports as neither alive
// nor confirmed dead (any error other than "no such process") is treated
// as ambiguous and returned as an error, so the caller preserves rather
// than guesses.
func liveRunnerProcessPID(dataDir, runID string) (int, error) {
	// *.log.pid, not build_app*.log.pid (found via review): runner.run
	// writes this pid file for every command it launches, not just
	// build_app.py -- canonical verification (verify.log.pid) and the
	// optional full-suite gate (full_suite_verify.log.pid) can leave a
	// live child behind exactly the same way if factoryd is killed while
	// either is running, and both still hold the isolated checkout open.
	matches, err := filepath.Glob(filepath.Join(run.Dir(dataDir, runID), "*.log.pid"))
	if err != nil {
		return 0, fmt.Errorf("glob runner pid files: %w", err)
	}
	for _, path := range matches {
		b, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// Removed between Glob and ReadFile -- the process that
				// owned it just finished; not a liveness signal.
				continue
			}
			return 0, fmt.Errorf("read %s: %w", path, err)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid <= 0 {
			return 0, fmt.Errorf("pid file %s has invalid content %q", path, b)
		}
		// Signal 0: no signal sent, only existence/permission checked.
		// The negated pid targets the whole process group runner.run
		// created with Setpgid (see its own comment) — the group leader's
		// PID as its own PGID, killed as a unit on cancellation elsewhere,
		// so checked as a unit here too.
		switch err := syscall.Kill(-pid, 0); {
		case err == nil:
			return pid, nil // still running (or a zombie group leader -- still "not safe to reap" either way)
		case errors.Is(err, syscall.ESRCH):
			continue // confirmed gone
		case errors.Is(err, syscall.EPERM):
			return pid, nil // exists, owned by someone else -- can't be this reap's own attempt's leftover
		default:
			return 0, fmt.Errorf("check pid %d liveness: %w", pid, err)
		}
	}
	return 0, nil
}

// reconcileIsolationMarkers is called only after the caller has acquired the
// repository Git-common-dir lock. That lock proves no in-scope direct,
// Temporal, or recovery execution can currently mutate this repository, so
// this function deliberately does not reacquire it. Any malformed or
// inconclusive evidence is logged and leaked rather than guessed at. A plain
// direct invocation has no Temporal client, so it conservatively leaves a
// Temporal marker; a later Temporal/daemon reconciliation (or a confirmed
// halt) can establish that cleanup is safe.
// The returned error aggregates only genuine operational failures (marker
// enumeration itself failing; a marker this function decided WAS safe to
// reap failing to actually go away) -- never a marker legitimately or
// ambiguously preserved, which is this function working as designed, not
// a failure. Every caller that treats reconciliation as opportunistic
// startup hygiene (this function's own doc comment above) discards it
// deliberately, exactly as before; only the standalone `factoryd
// reconcile` command (found via review: it was unconditionally returning
// nil, so an operator or script could never tell a real cleanup failure
// from success) propagates it.
func reconcileIsolationMarkers(ctx context.Context, dataDir, repoDir, sandboxDocker string, lock *wsisolation.DirectLock, temporalClient client.Client) error {
	if lock == nil || !lock.HeldExclusive() {
		log.Printf("isolation recovery: refusing reconciliation without held repository ownership")
		return nil
	}
	return reconcileIsolationMarkersWith(ctx, dataDir, repoDir, sandboxDocker, lock, func(ctx context.Context, workflowID string) bool {
		return temporalWorkflowRunning(ctx, temporalClient, workflowID)
	})
}

// liveSandboxContainerName reports whether a sandboxed worker container is
// still present (any state -- see sandbox.WorkerContainerPresentForRun's
// own "-a" doc comment) for runID, so reconciliation can preserve a marker
// whose build ran (or is running) inside Docker rather than as a host
// subprocess -- runner.run's own pid file (see liveRunnerProcessPID) is
// never written for that case (found via review: "sandbox launches do not
// create the runner PID file checked there"). sandboxDocker == "" (an
// operator's own explicit choice, not this function's default) is treated
// as "sandboxing was never in play here" and skipped entirely -- but any
// other failure to check, including sandboxDocker itself not being
// resolvable on this machine, is returned as an error, not treated as
// "absent" (found via review, round 2: an earlier version skipped the
// check whenever the Docker executable couldn't be found at all, but a
// prior sandboxed run's container can still be alive and holding the
// checkout even if *this* invocation's environment can't currently reach
// Docker to prove it -- e.g. a differently-configured PATH, or Docker
// briefly unreachable -- and there is no reliable signal, for a run with
// no completed attempt yet, that it was never sandboxed to begin with).
// An operator who knows their fleet never uses sandboxing at all, and
// wants reconciliation to stop asking Docker anything, can say so
// explicitly with -sandbox-docker="" (see reconcileMain's own flag help).
func liveSandboxContainerName(ctx context.Context, sandboxDocker, dataDir, runID string) (bool, error) {
	if sandboxDocker == "" {
		return false, nil
	}
	return sandbox.WorkerContainerPresentForRun(ctx, sandboxDocker, dataDir, runID)
}

func reconcileIsolationMarkersWith(ctx context.Context, dataDir, repoDir, sandboxDocker string, lock *wsisolation.DirectLock, workflowRunning func(context.Context, string) bool) error {
	if lock == nil || !lock.HeldExclusive() {
		log.Printf("isolation recovery: refusing reconciliation without held repository ownership")
		return nil
	}
	paths, err := wsisolation.ListIsolationMarkerPaths(dataDir)
	if err != nil {
		log.Printf("isolation recovery: could not list markers in %s: %v", dataDir, err)
		return fmt.Errorf("list isolation markers in %s: %w", dataDir, err)
	}
	var errs []error
	for _, path := range paths {
		marker, loadErr := wsisolation.LoadIsolationMarker(path)
		if loadErr != nil {
			log.Printf("isolation recovery: preserving malformed marker %s: %v", path, loadErr)
			continue
		}
		if err := wsisolation.ValidateIsolationMarker(marker, dataDir, repoDir); err != nil {
			log.Printf("isolation recovery: preserving inconclusive marker %s: %v", path, err)
			continue
		}
		expectedPath := wsisolation.IsolationMarkerPath(dataDir, marker.RunID)
		actualCanonical, actualErr := canonicalPath(path)
		expectedCanonical, expectedErr := canonicalPath(expectedPath)
		if actualErr != nil || expectedErr != nil || actualCanonical != expectedCanonical {
			log.Printf("isolation recovery: preserving marker %s because its canonical path does not match %s", path, expectedPath)
			continue
		}
		r, runErr := run.Load(marker.DataDir, marker.RunID)
		if runErr == nil {
			if r.State == run.StateAccepted || r.State == run.StateQuarantined {
				// Mirrors the normal path's own defer (see runSandboxWithRetries's
				// terminalStateSaved defer, same package): that defer is what
				// normally revokes EnableWorkerGroupWrite's grant once a run
				// reaches a terminal state and its worktree is kept rather than
				// rolled back. A factoryd process killed after the terminal
				// run.json save but before that defer runs leaves the grant
				// active forever, since reconciliation on restart previously
				// just preserved the worktree without ever revoking it (found
				// via GitHub Codex App review of PR #62, P2) -- every process
				// sharing factoryd's own GID would keep write access to a
				// host-owned worktree indefinitely. Best-effort and logged, not
				// fatal to reconciliation, for the same reason as the original
				// defer: this is a permission-hygiene cleanup, not evidence.
				if err := wsisolation.DisableWorkerGroupWrite(marker.WorktreePath); err != nil {
					log.Printf("isolation recovery: failed to revoke worker group write on kept terminal worktree %s: %v", marker.WorktreePath, err)
				}
				log.Printf("isolation recovery: preserving %s for terminal run %s", marker.WorktreePath, marker.RunID)
				continue
			}
			if r.KeptForResume {
				// A lost build's worktree holds its round state until a human
				// decides (resume, rebuild or cancel); only
				// clearKeptForResume reaps it. Its containers are removed by
				// the sandbox reconcilers, not here.
				log.Printf("isolation recovery: keeping %s for run %s awaiting a resume decision", marker.WorktreePath, marker.RunID)
				continue
			}
			if r.State == run.StateHalted && r.HaltConfirmed {
				// A confirmed halt is definitive: no execution can resume it.
			} else if r.State == run.StateHalted {
				log.Printf("isolation recovery: preserving marker %s because halted run %s is not confirmed", path, marker.RunID)
				continue
			} else if marker.Mode == "temporal" {
				if completed, checkpointErr := workflow.HasCompletedIsolatedWorkspaceCheckpoint(marker.CheckpointDir, marker.WorkflowID, marker.ActivityRunID, marker.ActivityID, marker.WorktreePath, marker.Branch); checkpointErr != nil {
					log.Printf("isolation recovery: preserving Temporal worktree %s while checkpoint evidence is unreadable: %v", marker.WorktreePath, checkpointErr)
					continue
				} else if completed {
					log.Printf("isolation recovery: preserving resumable Temporal worktree %s with completed prepare checkpoint", marker.WorktreePath)
					continue
				}
				if workflowRunning(ctx, marker.WorkflowID) {
					continue
				}
			}
		} else if !errors.Is(runErr, os.ErrNotExist) {
			log.Printf("isolation recovery: preserving marker %s because run %s could not be loaded: %v", path, marker.RunID, runErr)
			continue
		} else if marker.Mode == "temporal" {
			if completed, checkpointErr := workflow.HasCompletedIsolatedWorkspaceCheckpoint(marker.CheckpointDir, marker.WorkflowID, marker.ActivityRunID, marker.ActivityID, marker.WorktreePath, marker.Branch); checkpointErr != nil {
				log.Printf("isolation recovery: preserving Temporal marker %s while checkpoint evidence is unreadable: %v", path, checkpointErr)
				continue
			} else if completed {
				log.Printf("isolation recovery: preserving Temporal marker %s with completed prepare checkpoint", path)
				continue
			}
			if workflowRunning(ctx, marker.WorkflowID) {
				continue
			}
		}
		if marker.Mode == "temporal" {
			if _, statErr := os.Stat(marker.WorktreePath); os.IsNotExist(statErr) {
				log.Printf("isolation recovery: Temporal marker %s has no expected worktree; leaving evidence for inspection", path)
				continue
			} else if statErr != nil {
				log.Printf("isolation recovery: preserving Temporal marker %s because expected worktree cannot be inspected: %v", path, statErr)
				continue
			}
		}
		// Last line of defense before deleting anything, for both modes:
		// none of the checks above prove the actual build_app.py process
		// is dead, only that its *supervisor* (this run's own factoryd
		// process, or -- for Temporal -- the Workflow) looks done or gone
		// (found via review). An ungracefully killed factoryd gives its
		// child no parent-death signal, so build_app.py can keep running,
		// still reading/writing inside the very worktree about to be
		// deleted, with nothing left to notice or wait for it. See
		// runner.run's own pidFilePath comment for what's checked here.
		if pid, checkErr := liveRunnerProcessPID(marker.DataDir, marker.RunID); checkErr != nil {
			log.Printf("isolation recovery: preserving marker %s because its build process's liveness could not be checked: %v", path, checkErr)
			continue
		} else if pid != 0 {
			log.Printf("isolation recovery: preserving marker %s because its build process (pid %d) is still running", path, pid)
			continue
		}
		// A sandboxed run's build_app.py runs inside Docker, not as a
		// runner.run host subprocess -- liveRunnerProcessPID's own pid
		// file is never written for that case, so it's checked separately
		// here (found via review: a container can outlive an ungracefully
		// killed factoryd exactly the same way a host process can, still
		// holding the isolated checkout mounted).
		if present, checkErr := liveSandboxContainerName(ctx, sandboxDocker, marker.DataDir, marker.RunID); checkErr != nil {
			log.Printf("isolation recovery: preserving marker %s because its sandbox container's liveness could not be checked: %v", path, checkErr)
			continue
		} else if present {
			log.Printf("isolation recovery: preserving marker %s because a sandbox container for it is still present", path)
			continue
		}
		if err := wsisolation.ReapIsolationMarker(repoDir, marker); err != nil {
			// Refused, not failed, in the common case (ReapIsolationMarker's
			// own further safety checks declined) -- but either way, every
			// liveness check above already said this marker WAS safe to
			// reap, so this is worth surfacing to the standalone command,
			// same as a genuine failure.
			log.Printf("isolation recovery: preserving marker %s after safe reap refusal: %v", path, err)
			errs = append(errs, fmt.Errorf("reap marker %s: %w", path, err))
			continue
		}
		if err := wsisolation.RemoveIsolationMarker(path); err != nil {
			log.Printf("isolation recovery: reaped %s but could not remove marker %s: %v", marker.WorktreePath, path, err)
			errs = append(errs, fmt.Errorf("remove marker %s after reaping %s: %w", path, marker.WorktreePath, err))
		} else {
			log.Printf("isolation recovery: reaped stranded worktree %s and branch %s", marker.WorktreePath, marker.Branch)
		}
	}
	// Older isolated runs predate the ownership marker. Their branch/path
	// fields are useful for diagnosis but are not sufficient proof for safe
	// deletion, so report and deliberately leak them.
	entries, readErr := os.ReadDir(filepath.Join(dataDir, "runs"))
	if readErr == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			r, loadErr := run.Load(dataDir, entry.Name())
			if loadErr != nil || r.Branch == "" || r.WorkspacePath == "" || r.WorkspacePath == r.ProjectPath {
				continue
			}
			if _, markerErr := os.Stat(wsisolation.IsolationMarkerPath(dataDir, r.ID)); os.IsNotExist(markerErr) {
				log.Printf("isolation recovery: preserving legacy isolated run %s without ownership marker (worktree %s, branch %s)", r.ID, r.WorkspacePath, r.Branch)
			}
		}
	} else if !os.IsNotExist(readErr) {
		log.Printf("isolation recovery: could not inspect legacy run records in %s: %v", dataDir, readErr)
	}
	return errors.Join(errs...)
}

func temporalWorkflowRunning(ctx context.Context, temporalClient client.Client, workflowID string) bool {
	if temporalClient == nil || workflowID == "" {
		log.Printf("isolation recovery: preserving Temporal marker because workflow liveness is inconclusive")
		return true
	}
	descCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	desc, err := temporalClient.DescribeWorkflowExecution(descCtx, workflowID, "")
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return false
		}
		log.Printf("isolation recovery: preserving Temporal marker because workflow %s liveness query failed: %v", workflowID, err)
		return true
	}
	return desc.WorkflowExecutionInfo.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING
}

// runsNeedingReclaim scans dataDir's durable run records and returns the
// IDs of every nonterminal one belonging to repository and not already in
// alreadyReclaimed — factored out of daemonMain's own
// reclaimAbandonedRunQueues closure so the pure "which runs need a
// recovery Worker" decision is unit-testable without a live Temporal
// server or actually starting one. A load failure for one run's record is
// skipped, not fatal to the scan (a run.json can legitimately be
// mid-write; a real problem there surfaces again on the next periodic
// scan).
//
// The repository filter is required — found via review: a `-data-dir`
// shared with runs from a different repository, or from a direct/plain
// `-temporal-address` invocation that never went through a
// RepositoryOwnerWorkflow at all, previously had no way to be excluded.
// Reclaiming one anyway starts a recovery Worker on this daemon's own
// task queue for a request its own repository owner has never heard of,
// and reconcileReclaimedRun then queries that same wrong owner forever —
// it can never observe the request done or in progress, so the run is
// "reclaimed" permanently, doing real (if idle) work for nothing.
func runsNeedingReclaim(dataDir, repository string, alreadyReclaimed map[string]bool) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		if alreadyReclaimed[id] {
			continue
		}
		recovered, loadErr := run.Load(dataDir, id)
		if loadErr != nil {
			continue
		}
		if recovered.Repository != repository {
			continue
		}
		if runTerminalConfirmed(recovered) {
			continue
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// terminalReclaimedRunIDs returns which of reclaimedIDs have since reached
// a terminal state — the mirror image of runsNeedingReclaim, used to know
// which recovery Workers a periodic re-scan can now stop. Found via
// review: without this, every reclaimed run's Worker (and its task-queue
// poller) was kept alive for the rest of the daemon's own lifetime even
// after the run it was recovering finished, so a long-lived daemon
// serving enough abandoned runs over time would accumulate Workers and
// server-side pollers without bound. A load failure is treated as "not
// yet terminal" (keep polling) rather than "terminal" (stop) — the
// conservative direction, since prematurely stopping a Worker still
// servicing a run in progress would strand it exactly like the crash this
// reclaim mechanism exists to recover from. Likewise a StateHalted run
// whose halt is unconfirmed does not count as terminal here either — the
// caller is expected to have already run reconcileReclaimedRun against the
// repository owner first, which sets HaltConfirmed true once the owner
// itself confirms the request is done; until then this function must keep
// the recovery Worker alive on the conservative assumption the child may
// still be running.
func terminalReclaimedRunIDs(dataDir string, reclaimedIDs map[string]bool) []string {
	var ids []string
	for id := range reclaimedIDs {
		recovered, err := run.Load(dataDir, id)
		if err != nil {
			continue
		}
		if runTerminalConfirmed(recovered) {
			ids = append(ids, id)
		}
	}
	return ids
}

// reconcileReclaimedRun asks the repository owner directly whether
// requestID is done, and durably reconciles that into its run.json if
// run.json hasn't caught up — closing two related gaps found via review that
// share the same root cause (run.json's own State field is not a reliable
// proxy for whether a run's Temporal execution has actually finished):
//
//  1. A still-alive submitter that gave up waiting writes StateHalted with
//     HaltConfirmed left false whenever its own best-effort child
//     termination didn't confirm success — the child may genuinely still
//     be running.
//  2. A submitter that crashed outright never calls applyRunWorkflowResult
//     at all, so run.json stays stuck at its last pre-crash nonterminal
//     state forever even after the owner and a recovery Worker finish the
//     child — nothing else ever reconciles it.
//
// Both are resolved the same way: the owner's own durable per-request
// result (RepositoryOwnerResult.Runs/InProgress) is the actual source of
// truth, independent of whatever run.json happens to say.
//
// Returns (terminal, err). terminal is true once this request is fully
// resolved — either it was already terminal and confirmed (nothing to do),
// or this call just reconciled it into run.json. A query failure or a
// request the owner still reports InProgress conservatively returns
// terminal=false, matching terminalReclaimedRunIDs's own "not sure yet,
// keep the Worker" convention — the caller is expected to retry on its next
// periodic scan.
//
// Found via review: runsNeedingReclaim (this function's caller's own
// source of which runs to consider) has no way to distinguish a genuinely
// abandoned run from a live submitter's own run that simply hasn't
// finished polling yet — both look identical from run.json's own
// nonterminal state alone. The daemon's periodic scan can therefore
// observe the same owner result at roughly the same moment a live
// submitter's runViaRepositoryOwner does, racing its own final
// applyRunWorkflowResult call. The cheap, unlocked load below is only ever
// used to decide whether querying the owner is worth doing at all; every
// actual write happens inside run.WithLock, re-checking run.json's state
// again after acquiring it — runViaRepositoryOwner's own final write takes
// the exact same lock (see its call site) — so whichever side gets there
// first performs the real write and the other, finding it already
// terminal, does nothing.
//
// ctx bounds every Temporal query this call makes — found via review: an
// earlier version derived each query's timeout from context.Background(),
// so a scan already in flight when the daemon received SIGTERM ran every
// query out to its own full 5s timeout regardless, delaying shutdown by
// roughly 5s per batch of reconcileConcurrency reclaimed runs still queued
// behind it. Callers should pass their own shutdown-aware context (the
// daemon's signalCtx) so an in-flight query is canceled immediately
// instead.

// queryOwnerForRequestResult asks the repository owner identified by
// ownerID for requestID's own outcome. done is true only when the owner's
// own durable result (RepositoryOwnerResult.Runs) has an entry for
// requestID; err is non-nil when the query itself failed (an inconclusive
// owner, e.g. one that has since idle-completed and been replaced —
// callers should fall back to queryChildWorkflowResult in that case, not
// treat this as requestID's own failure).
func queryOwnerForRequestResult(ctx context.Context, temporalClient client.Client, ownerID, requestID string) (workflow.RunWorkflowResult, bool, error) {
	queryResp, err := temporalClient.QueryWorkflow(ctx, ownerID, "", workflow.RepositoryOwnerQueryName)
	if err != nil {
		return workflow.RunWorkflowResult{}, false, err
	}
	var ownerResult workflow.RepositoryOwnerResult
	if err := queryResp.Get(&ownerResult); err != nil {
		return workflow.RunWorkflowResult{}, false, err
	}
	if ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == requestID {
		// Genuinely still running under this owner execution.
		return workflow.RunWorkflowResult{}, false, nil
	}
	result, done := ownerResult.Runs[requestID]
	return result, done, nil
}

// queryChildWorkflowResult fetches requestID's own RunWorkflow result
// directly from Temporal, independent of any RepositoryOwnerWorkflow
// execution's own survival — see RepositoryOwnerRunWorkflowID's doc
// comment for why the child's own WorkflowID is namespaced by ownerID,
// not requestID alone: requestID is caller-suppliable (POST /runs's
// StartRequest.ID) and never validated for uniqueness across
// repositories, so looking it up unnamespaced could fetch (and durably
// persist) a completely different repository's own result for a colliding
// requestID (found via review of PR #18, P1). done is true only once the
// child has reached a terminal Temporal status and its result was
// successfully fetched; a child that is still running, or was never
// started at all (not yet dispatched — still queued behind other work),
// returns done=false with a nil error, the same "not sure yet" convention
// as queryOwnerForRequestResult. DescribeWorkflowExecution first, rather
// than calling Get directly, so a still-running child returns quickly
// instead of blocking this call out to its own ctx deadline (this can be
// invoked in a tight periodic scan over many reclaimed runs — see
// reconcileConcurrency).
func queryChildWorkflowResult(ctx context.Context, temporalClient client.Client, ownerID, requestID string) (workflow.RunWorkflowResult, bool, error) {
	childWorkflowID := workflow.RepositoryOwnerRunWorkflowID(ownerID, requestID)
	desc, err := temporalClient.DescribeWorkflowExecution(ctx, childWorkflowID, "")
	if err != nil {
		if isNotFoundError(err) {
			// Never started under this WorkflowID at all — still queued.
			return workflow.RunWorkflowResult{}, false, nil
		}
		return workflow.RunWorkflowResult{}, false, err
	}
	if desc.WorkflowExecutionInfo.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		return workflow.RunWorkflowResult{}, false, nil
	}
	var result workflow.RunWorkflowResult
	if err := temporalClient.GetWorkflow(ctx, childWorkflowID, "").Get(ctx, &result); err != nil {
		// A terminal-but-non-Completed close status (canceled/terminated/
		// timed out, or a failure with no recoverable RunWorkflowResult)
		// still carries whatever evidence AttemptsFromError/
		// BaseSHAFromError can recover from it, same as the live
		// give-up/failure paths above.
		isolatedWorkspacePath, isolatedBranch := recoverIsolatedWorkspace(workflow.RunWorkflowResult{}, err)
		result = workflow.RunWorkflowResult{
			State:              run.StateHalted,
			CleanupUnconfirmed: workflow.CleanupUnconfirmedFromError(err),
			Err:                err.Error(),
			Attempts:           workflow.AttemptsFromError(err),
			BaseSHA:            workflow.BaseSHAFromError(err),
			WorkspacePath:      isolatedWorkspacePath,
			Branch:             isolatedBranch,
		}
	}
	return result, true, nil
}

// isNotFoundError reports whether err is Temporal's own NotFound service
// error — the exact signal that a given WorkflowID has never been used
// (as opposed to any other query failure, which callers must propagate,
// not silently treat as "not started yet").
func isNotFoundError(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}

// signalDeliveredToOwner reports whether a SubmitRunSignal carrying
// requestID was ever durably delivered to the repository owner identified
// by ownerID — the second, previously-documented-open reconciliation gap
// (see CLAIMS.md and the plan's Phase 6 status): a request whose
// SignalWithStartWorkflow attempt failed ambiguously, submitted by a
// process that then crashed before terminateOrCancelOwnRequest could
// resolve it, left runsNeedingReclaim's caller with no way to distinguish
// "genuinely never delivered" from "durably queued, just not drained yet"
// — both look identical to queryOwnerForRequestResult/
// queryChildWorkflowResult, which only ever see Runs/InProgress/the
// child's own started-or-not status, none of which are populated until
// the owner's Select loop actually dequeues the signal.
//
// That is not merely a timing gap closable with a longer wait or a
// smarter heuristic (a timeout-based give-up was tried before this and
// reverted for exactly that reason — see the plan's own history): the
// owner is fully blocked inside childFuture.Get() for its current
// child's ENTIRE execution, not selecting on its signal channel at all
// during that time, so a signal delivered mid-child sits durably queued
// server-side with nothing in workflow code ever observing it until the
// owner's next Select call — which can be many minutes away for a real
// slice. No amount of waiting from outside the workflow can distinguish
// that from "never delivered" without reading the same durable record
// the server itself already holds: this reads the owner's own real
// Workflow Execution History directly (client.GetWorkflowHistory), not
// its in-memory/query-exposed state, since a WorkflowExecutionSignaled
// history event is recorded the instant Temporal's server accepts the
// signal — independent of whether workflow code has processed it yet.
//
// Also checks the CURRENT owner execution's own WorkflowExecutionStarted
// event for a carried-forward RepositoryOwnerWorkflowInput.PendingRequests
// entry matching requestID — closing the Continue-As-New boundary case:
// a signal delivered to a PRIOR owner execution before a Continue-As-New,
// but not yet processed at that boundary, is drained and carried forward
// as that field (see its own doc comment) onto the NEXT execution's own
// start input, visible here without needing to walk back through any
// prior execution's now-separate history at all.
//
// Malformed/undecodable event payloads are skipped, not treated as a
// match or a fatal error — the query's only two truthful answers are
// "found it" and "didn't find it after a full scan", never a guess.
//
// Accepted, bounded limitation (found via review): NotFound from
// GetWorkflowHistory (see its own isNotFoundError branch below) doesn't
// only mean "this owner Workflow ID has never been started" — Temporal
// also returns it for an execution whose history has aged out of the
// server's own configured retention window, which would make a genuinely
// (and possibly fully processed) delivered signal indistinguishable from
// never-delivered. Not chased further here, for two reasons: retention is
// an operational configuration choice, typically measured in days to
// weeks, while this whole reclaim mechanism operates on a timescale of
// minutes to hours after a crash — the windows don't realistically
// overlap in normal operation; and if the signal in question WAS ever
// actually processed into a dispatched child, reconcileReclaimedRun's own
// earlier queryChildWorkflowResult call (checked before this function is
// ever reached — see its call site) already independently recovers that
// child's own real result via its own deterministic WorkflowID, so this
// function's own history scan is only ever reached for a request that
// neither the owner's live state nor its dispatched child already know
// about.

// errOwnerNeverStarted distinguishes "the owner Workflow ID has never
// been started at all" from an ordinary clean scan of an owner that does
// exist. Both cases still need reconcileReclaimedRun's own
// CancelRunSignal safety net before concluding anything — found via
// review, round 2: an earlier version of this fix assumed a never-
// started owner was race-free proof on its own (SignalWithStartWorkflow
// creates the owner if missing, so a landed signal implies the owner
// already exists) and skipped the safety net for that case entirely, but
// the same settling-window problem applies one level earlier — the
// original SignalWithStartWorkflow RPC can still be completing
// server-side (creating the owner) at the exact instant this history
// check runs. The two cases differ only in HOW the tombstone gets sent:
// an owner known to exist can be reached with a plain CancelRunSignal
// (ordinary SignalWorkflow), but a possibly-not-yet-existing owner needs
// SignalWithStartWorkflow for the cancellation too, since a plain Signal
// has nothing to signal if the owner isn't there yet — see
// reconcileReclaimedRun's own errOwnerNeverStarted branch for that.
var errOwnerNeverStarted = errors.New("repository owner workflow has never been started")

func signalDeliveredToOwner(ctx context.Context, temporalClient client.Client, ownerID, requestID string) (bool, error) {
	dataConverter := converter.GetDefaultDataConverter()
	iter := temporalClient.GetWorkflowHistory(ctx, ownerID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			// Found via review: a repository whose owner Workflow has
			// never run at all (a brand-new repository, or one whose
			// only prior execution predates any retained history) — a
			// perfectly ordinary case, not an infrastructure problem —
			// surfaces here as NotFound on the very first Next() call,
			// not as an empty iterator. That's just as definitive an
			// answer as a full scan finding nothing — but callers must
			// still be able to tell the two apart (see
			// errOwnerNeverStarted's own doc comment), so this is
			// returned as a distinguishable sentinel error, not folded
			// into the same (false, nil) a clean scan of an existing
			// owner returns. Every other Next() error remains a genuine
			// query failure, not a found/not-found answer either way.
			if isNotFoundError(err) {
				return false, errOwnerNeverStarted
			}
			return false, err
		}
		if signaled := event.GetWorkflowExecutionSignaledEventAttributes(); signaled != nil && signaled.GetSignalName() == workflow.SubmitRunSignalName {
			var signal workflow.SubmitRunSignal
			if err := dataConverter.FromPayloads(signaled.GetInput(), &signal); err == nil && signal.RequestID == requestID {
				return true, nil
			}
		}
		if started := event.GetWorkflowExecutionStartedEventAttributes(); started != nil {
			var input workflow.RepositoryOwnerWorkflowInput
			if err := dataConverter.FromPayloads(started.GetInput(), &input); err == nil {
				for _, pending := range input.PendingRequests {
					if pending.RequestID == requestID {
						return true, nil
					}
				}
			}
		}
	}
	return false, nil
}

// haltReclaimedRunConfirmed durably records requestID as a confirmed halt
// — used only once its non-delivery to the repository owner is genuinely
// certain (see reconcileReclaimedRun's two call sites), not merely
// probable. Re-checks run.json under lock in case a concurrent writer
// (most plausibly the original submitter itself, if it turns out not to
// have crashed after all) already reconciled it in the meantime.
// workspacePath/branch (both optional; pass "" when the caller has none)
// let a confirmed termination that also ran an isolated workspace's
// caller-side rollback (terminateOrCancelOwnRequest's own
// rollbackIsolatedWorkspaceIfTerminated) record the truth into this
// durable record too, mirroring every other give-up path's own recovery
// — found via a live end-to-end check, not just reasoned about: the
// actual git-level rollback already ran correctly regardless, but the
// durable record still showed the shared checkout instead of the
// isolated one when this parameter was missing.
func haltReclaimedRunConfirmed(dataDir, requestID, workspacePath, branch string) (bool, error) {
	var terminal bool
	lockErr := run.WithLock(dataDir, requestID, func() error {
		fresh, freshErr := run.Load(dataDir, requestID)
		if freshErr != nil {
			return freshErr
		}
		if runTerminalConfirmed(fresh) {
			terminal = true
			return nil
		}
		fresh.State = run.StateHalted
		fresh.HaltConfirmed = true
		if workspacePath != "" {
			fresh.WorkspacePath = workspacePath
			fresh.Branch = branch
		}
		if saveErr := save(fresh, dataDir); saveErr != nil {
			return saveErr
		}
		terminal = true
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	return terminal, nil
}

func reconcileReclaimedRun(dp *deps, ctx context.Context, temporalClient client.Client, ownerID, requestID, dataDir string) (bool, error) {
	recovered, loadErr := run.Load(dataDir, requestID)
	if loadErr != nil {
		return false, loadErr
	}
	if runTerminalConfirmed(recovered) {
		return true, nil
	}

	queryCtx, cancelQuery := context.WithTimeout(ctx, 5*time.Second)
	result, done, queryErr := queryOwnerForRequestResult(queryCtx, temporalClient, ownerID, requestID)
	cancelQuery()
	if !done {
		if queryErr != nil {
			// The owner query itself is inconclusive (e.g. this owner
			// execution has since idle-completed and a fresh one with an
			// empty Runs map answered instead, or none is currently
			// running at all) — fall through to asking Temporal about the
			// child directly rather than giving up.
			log.Printf("run %s: repository owner query inconclusive, checking child workflow directly: %v", requestID, queryErr)
		}
		// deterministicChildWorkflowIDFallback (found via review): the
		// owner's own in-memory Runs/InProgress is not durable across an
		// ordinary idle-completion (see RepositoryOwnerWorkflow's own
		// comment on that return) — only across Continue-As-New. A
		// request whose owner execution has since completed and been
		// replaced is otherwise unreachable forever, punted to "leave it
		// for a later scan" on every single call. RunWorkflow's own
		// WorkflowID is deterministic — RepositoryOwnerRunWorkflowID(ownerID,
		// requestID), namespaced by this same ownerID so it can't collide
		// with another repository's own requestID (see that helper's own
		// doc comment) — so its durable result can be fetched directly
		// from Temporal, independent of any owner execution's own
		// survival.
		childCtx, cancelChild := context.WithTimeout(ctx, 5*time.Second)
		childResult, childDone, childErr := queryChildWorkflowResult(childCtx, temporalClient, ownerID, requestID)
		cancelChild()
		if childErr != nil {
			return false, childErr
		}
		if !childDone {
			// Neither the owner nor the child's own deterministic
			// WorkflowID knows anything about this request yet. That's
			// still consistent with "durably queued, not drained by the
			// owner's Select loop yet" — the owner can be fully blocked
			// on a long-running current child for the child's entire
			// execution (see signalDeliveredToOwner's own doc comment),
			// during which nothing here would see it either. Check the
			// owner's actual Workflow Execution History directly before
			// giving up as merely "not sure yet": only a full scan that
			// finds no matching signal is a positive, reconcilable
			// answer that this request genuinely never reached the
			// owner at all — closing the plan's previously-documented
			// gap (a SignalWithStartWorkflow that failed before ever
			// reaching the owner could never be reconciled).
			historyCtx, cancelHistory := context.WithTimeout(ctx, 5*time.Second)
			delivered, historyErr := signalDeliveredToOwner(historyCtx, temporalClient, ownerID, requestID)
			cancelHistory()
			if errors.Is(historyErr, errOwnerNeverStarted) {
				// "The owner has never been started" is not itself a
				// stronger, race-free answer (found via review, round 2):
				// the original SignalWithStartWorkflow RPC could still be
				// completing server-side (creating the owner) at the
				// instant this history check ran. A plain CancelRunSignal
				// (ordinary SignalWorkflow) has nothing to signal if the
				// owner doesn't exist yet, so this ensures the owner
				// exists via SignalWithStartWorkflow instead — atomically
				// creating it if it's still missing, or reaching the
				// existing one if the original call's effect has landed
				// by now — durably recording the cancellation either way.
				//
				// That alone still isn't sufficient confirmation (found
				// via review, round 4): the RPC succeeding only proves
				// the cancellation was DELIVERED, not that the owner
				// applied it before it might have ALSO received and
				// dispatched the original submit signal in a genuine
				// race. This deliberately does NOT reuse
				// terminateOrCancelOwnRequest's own re-query-and-poll for
				// that (a real bug an earlier version of this exact fix
				// had: that function's poll waits for the owner to
				// record a Runs[requestID] result, which — for a request
				// that may never actually have a real submit signal
				// coming at all, this branch's whole premise — only ever
				// gets written when a matching SubmitRunSignal is later
				// dequeued and found already canceled; with no submit
				// signal guaranteed to exist, that wait can hang for its
				// entire timeout with nothing to observe). Only a
				// same-shape in-progress check is actually meaningful
				// here: query once for whether the owner raced ahead and
				// already dispatched this exact request despite the
				// cancellation just sent, and terminate that child
				// directly if so (mirroring terminateOrCancelOwnRequest's
				// own terminateInProgress helper) — anything else
				// (nothing running, nothing completed) is itself the
				// positive confirmation this branch needs: the
				// cancellation is now durably recorded, so any future
				// submit signal will see it and be dropped before ever
				// dispatching.
				cancelCtx, cancelCancel := context.WithTimeout(ctx, 5*time.Second)
				_, cancelErr := temporalClient.SignalWithStartWorkflow(cancelCtx, ownerID, workflow.CancelRunSignalName,
					workflow.CancelRunSignal{RequestID: requestID},
					client.StartWorkflowOptions{ID: ownerID, TaskQueue: "factoryd-repo-" + ownerID},
					workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: recovered.Repository},
				)
				cancelCancel()
				if cancelErr != nil {
					// Could not even confirm the owner now exists —
					// leave this for a later scan rather than conclude
					// anything, same conservative convention as every
					// other inconclusive case here.
					return false, nil
				}
				queryCtx, cancelQuery2 := context.WithTimeout(ctx, 5*time.Second)
				queryResp, queryErr2 := temporalClient.QueryWorkflow(queryCtx, ownerID, "", workflow.RepositoryOwnerQueryName)
				cancelQuery2()
				if queryErr2 != nil {
					return false, nil
				}
				var ownerResult workflow.RepositoryOwnerResult
				if err := queryResp.Get(&ownerResult); err != nil {
					return false, nil
				}
				if r, ok := ownerResult.Runs[requestID]; ok {
					result = r
				} else if ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == requestID {
					termCtx, cancelTerm := context.WithTimeout(context.Background(), 5*time.Second)
					termErr := temporalClient.TerminateWorkflow(termCtx, ownerResult.InProgress.ChildWorkflowID, ownerResult.InProgress.ChildRunID,
						"factoryd: reclaimed run confirmed canceled before execution; this child raced ahead of that cancellation")
					cancelTerm()
					var notFound *serviceerror.NotFound
					if termErr != nil && !errors.As(termErr, &notFound) {
						return false, nil
					}
					return haltReclaimedRunConfirmed(dataDir, requestID, "", "")
				} else {
					// Accepted, explicitly tracked residual limitation
					// (found via review): the cancellation just sent only
					// lives in this owner execution's own in-memory
					// canceled[] map. RepositoryOwnerWorkflow returns
					// (ordinarily, not via Continue-As-New) after its own
					// IdleTimeout (defaultRepositoryOwnerIdle, 24h) if it
					// receives no further work — discarding that map
					// entirely, since an ordinary return carries nothing
					// forward the way a Continue-As-New does. If the
					// original SignalWithStartWorkflow's delayed effect
					// still hasn't landed by the time that happens, and
					// this repository receives no other work in the
					// meantime either, a later SignalWithStartWorkflow
					// for the SAME original signal would start a brand
					// new owner execution with no memory of this
					// cancellation, and could dispatch the request for
					// real after this process has already retired its
					// recovery Worker on the strength of this confirmed
					// halt. Not chased further here: closing it fully
					// would mean changing RepositoryOwnerWorkflow's own
					// idle-completion determinism (new replay-safety
					// surface, not a decision to make casually), for a
					// compound event requiring the repository to receive
					// zero other work for up to 24h AND the original RPC
					// to still be unsettled after that long — a
					// fundamentally different order of probability than
					// the sub-second-to-low-minutes settling windows the
					// rest of this fix closes.
					return haltReclaimedRunConfirmed(dataDir, requestID, "", "")
				}
			} else if historyErr != nil || delivered {
				// A history-read failure (other than errOwnerNeverStarted
				// above) or a positive delivery finding both fall back to
				// the existing conservative convention: leave it for a
				// later scan. A delivered-but-undrained signal will
				// eventually surface via Runs/InProgress once the owner
				// gets to it; nothing more to reconcile right now.
				return false, nil
			} else {
				// The owner exists but its history shows nothing — still
				// not proof on its own: found via review (GitHub's
				// automated PR reviewer, P1) — a still-settling
				// SignalWithStartWorkflow whose server-side write was
				// still completing at the exact instant this scan ran
				// has no upper bound on how long that can take, so any
				// fixed grace period before re-scanning (an earlier
				// version of this fix waited a flat 3s) is just as much
				// an unproven time-based guess as the original reverted
				// give-up attempt, merely with a longer fuse. The actual
				// fix: don't try to prove non-delivery at all — make it
				// safe even if we're wrong, by reusing
				// terminateOrCancelOwnRequest (already hardened for the
				// live submitter's own matching case): it sends
				// CancelRunSignal, then polls for a positive result —
				// appropriate here because a real submit signal is known
				// (or strongly suspected) to actually exist somewhere in
				// this case, unlike the errOwnerNeverStarted branch
				// above, so waiting for the owner to eventually process
				// and record it is a reasonable, bounded wait, not an
				// indefinite hang on something that may never arrive.
				giveUpResult, done2, confirmed := terminateOrCancelOwnRequest(temporalClient, ownerID, requestID, filepath.Join(dataDir, "temporal-checkpoints", requestID))
				switch {
				case done2:
					result = giveUpResult
				case confirmed:
					return haltReclaimedRunConfirmed(dataDir, requestID, giveUpResult.WorkspacePath, giveUpResult.Branch)
				default:
					return false, nil
				}
			}
		} else {
			result = childResult
		}
	}

	var terminal bool
	lockErr := run.WithLock(dataDir, requestID, func() error {
		fresh, freshErr := run.Load(dataDir, requestID)
		if freshErr != nil {
			return freshErr
		}
		if runTerminalConfirmed(fresh) {
			// A concurrent writer (most likely the submitter's own final
			// result save) already reconciled this while this call was
			// still querying the owner or waiting for the lock.
			terminal = true
			return nil
		}
		if result.Err != "" {
			// Replace, not append (found via review): fresh.Attempts may
			// already hold the same attempts, recovered from checkpoints
			// or the give-up error's own Details, by
			// terminateOrCancelOwnRequest's caller before it saved this
			// run as unconfirmed-halted (see that save's own comment).
			// result.Attempts is the owner's authoritative complete
			// history for this request (see applyRunWorkflowResult's
			// matching comment), so appending onto that stale prefix would
			// duplicate every attempt the give-up path already recovered.
			fresh.Attempts = result.Attempts
			if result.BaseSHA != "" {
				fresh.BaseSHA = result.BaseSHA
			}
			// recoverIsolatedWorkspace with a nil err: result is already
			// terminal here (queryChildWorkflowResult's own synthesis, if
			// this reclaim path needed it, already ran) — nothing further
			// to recover from a live error object at this point.
			if wp, br := recoverIsolatedWorkspace(result, nil); wp != "" {
				fresh.WorkspacePath = wp
				fresh.Branch = br
			}
			fresh.State = run.StateHalted
			fresh.HaltConfirmed = !result.CleanupUnconfirmed
			// save, not fresh.Save directly — found via review: the raw
			// method left UpdatedAt at its stale pre-crash value, so the
			// console and audit record showed this terminal transition as
			// having happened whenever the run was last touched before the
			// crash, not when it was actually reconciled. result.Err as
			// the halt cause: it is the only record of why the child failed, e.g. an
			// Activity heartbeat timeout after its worker died -- without
			// it HaltError stayed empty and the halt alert pointed at logs
			// that never captured the failure.
			if saveErr := save(fresh, dataDir, errors.New(result.Err)); saveErr != nil {
				return saveErr
			}
			terminal = true
			return nil
		}
		// applyRunWorkflowResult returns a non-nil, informational-only
		// error exactly when it just recorded a quarantine (see its own
		// doc comment) — that durable save already happened either way, so
		// only an error alongside any other resulting state is a genuine
		// save failure worth propagating.
		// attributeWorkspaceEvidence=false — found via review: by the time a
		// crashed submitter's run is reconciled here, the repository owner
		// may already have started a later queued run against the same
		// shared workspace, which overwrites BUILD_EVIDENCE.json with that
		// later run's own evidence. Reading it now would attribute someone
		// else's provider/model/usage to this run's audit record.
		// Cleared, not left as-is, for the same reason as the result.Err
		// branch above: applyRunWorkflowResult appends result.Attempts
		// (the owner's authoritative complete history) onto whatever
		// fresh.Attempts already holds, and a give-up path may have
		// already recovered and saved that same history from checkpoints
		// before this reconciliation ran.
		fresh.Attempts = nil
		// nil releasePolicy: applyRunWorkflowResult itself now recovers
		// the effective release.MergePolicy from fresh's own persisted
		// ReleasePolicy (run.Run.ReleasePolicy, set at run start in
		// runMainWithReady) via mergePolicyFromRun, centrally -- see that
		// function's own top-of-file doc comment -- rather than always
		// evaluating against an always-deny release.MergePolicy{}. Before
		// this, reconciliation ran on the daemon side, long after the original
		// submitter's own -release-* flags were gone, with no way to
		// recover them, so EVERY reconciled accepted run was denied
		// release regardless of what the operator actually configured.
		// This used to recover the policy here directly (a Codex-review
		// follow-up moved that recovery into applyRunWorkflowResult so
		// every releasePolicy==nil caller shares it, not just this one).
		// A legacy run record (started before ReleasePolicy existed)
		// still keeps today's fail-closed release.MergePolicy{} behavior,
		// logged by applyRunWorkflowResult itself. true: this call
		// already runs inside its own caller's run.WithLock for requestID
		// (see that lock's own acquisition above in this function), so
		// applyRunWorkflowResult's accepted-run side effects must not
		// take it again.
		if applyErr := applyRunWorkflowResult(dp, fresh, dataDir, requestID, fresh.Ticket, fresh.WorkspacePath, fresh.BaseSHA, "reclaimed", result, false, nil, forge.GHPullRequestOpener{}, true); applyErr != nil && fresh.State != run.StateQuarantined {
			return applyErr
		}
		terminal = true
		return nil
	})
	if lockErr != nil {
		return false, lockErr
	}
	return terminal, nil
}
