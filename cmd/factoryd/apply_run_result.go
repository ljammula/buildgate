package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/codereview"
	"buildgate/internal/evidence"
	"buildgate/internal/forge"
	"buildgate/internal/notify"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/triage"
	"buildgate/internal/workflow"
)

// releasePolicy is nil only for reconcileReclaimedRun's own call when it
// could not recover a policy itself (a legacy run predating
// run.Run.ReleasePolicy -- see that call site's own doc comment): a nil
// releasePolicy no longer means an automatic release.MergePolicy{}
// evaluation below -- decisionPolicy first tries mergePolicyFromRun(r) to
// recover the policy from r's own persisted record before falling back to
// that always-deny zero value. openEvidencePullRequest gets the
// policy the decision actually used (prPolicy below) whenever one was
// passed or recovered, so the PR's risk header reports the protected-path
// evaluation that really happened; only an unrecoverable legacy run (which
// is always denied, so never opens a PR) keeps the "not evaluated" header.
// Found in review of Phase 1: passing the nil pointer made a recovered,
// allowed run's PR claim protected paths were "not evaluated".
//
// opener is forge.GHPullRequestOpener{} at every real call site;
// injectable so a test can exercise this function's -open-pull-request
// handling (and its interaction with underRunLock below) with a fake one
// instead of needing a real GitHub remote.
//
// underRunLock must be true only when the caller already holds id's
// run.WithLock for this entire call (runViaRepositoryOwner's and
// reconcileReclaimedRun's own call sites both load r fresh and pass it in
// without ever releasing that lock) -- this function's own accepted-run
// side effects (recordAcceptedRunSideEffects) rely on it to decide whether
// they may safely re-save r directly or must take their own lock instead,
// since run.WithLock is not reentrant within one process (found via Codex
// review of PR #86: an earlier version always took its own lock here,
// which deadlocks under these two callers).
func applyRunWorkflowResult(dp *deps, r *run.Run, dataDir, id, ticket, workspacePath, baseSHA, taskQueue string, result workflow.RunWorkflowResult, attributeWorkspaceEvidence bool, releasePolicy *release.MergePolicy, opener forge.PullRequestOpener, underRunLock bool) error {
	// result.BaseSHA is CaptureBaseSHAActivity's fresh read of the
	// workspace, not this caller's own pre-submission guess — see
	// RunWorkflowResult.BaseSHA's doc comment for why only this value is
	// guaranteed to reflect what the run was actually evaluated against
	// (a queued run behind another one against the same workspace would
	// otherwise keep an r.BaseSHA/log line that predates its own real
	// starting point).
	if result.BaseSHA != "" {
		baseSHA = result.BaseSHA
	}
	r.BaseSHA = baseSHA
	// The run's baseline record is on disk by now; the triage sentence
	// built below, before the save, words a halt on it.
	r.AttachBaselineVerify(dataDir)
	// result.WorkspacePath/Branch are set only when this run isolated its
	// execution (RunWorkflowInput.IsolateWorkspace) — see
	// RunWorkflowResult's doc comment. r.ProjectPath (already set from
	// *workspace before this run was ever submitted) deliberately stays
	// untouched, as the execDir/*workspace
	// split does, so "which project is this run about" keeps meaning that even
	// once r.WorkspacePath diverges from it.
	isolatedWorkspacePath := workspacePath
	if result.WorkspacePath != "" {
		isolatedWorkspacePath = result.WorkspacePath
		r.WorkspacePath = result.WorkspacePath
		r.Branch = result.Branch
	}
	r.State = result.State
	// See workflow.RunWorkflowResult.HaltReasonCode's own doc comment:
	// every run submitted through POST /runs reaches this exact call
	// (repositoryAPIStarter always wraps apiStartStarter in
	// RepositoryOwnerWorkflow), so this is the one place that path's
	// relay-ceiling classification (recorded there since the child's raw
	// error never reaches this function directly, unlike runViaTemporal's
	// own halt handling) actually lands on the durable run
	// record (found via code review).
	r.HaltReasonCode = result.HaltReasonCode
	// Found via review: this is, by construction, always a confirmed
	// result (the owner itself is the source of truth for it, whether
	// applied fresh or via reconcileReclaimedRun reconciling a run.json
	// previously written by a give-up path — see HaltConfirmed's own doc
	// comment), so it must always mark that here rather than leave
	// whatever r.HaltConfirmed happened to already be. Left stale false, a
	// later operator override to StateHalted would still read as
	// unconfirmed, and the worker's reclaim scan would call this function
	// on it again — silently overwriting the override with this same
	// owner result, including re-sending its quarantine notification.
	r.HaltConfirmed = true
	r.ResultSHA = result.ResultSHA
	r.CommittedByFactoryd = result.CommittedByWorker
	r.ChangedFiles = result.ChangedFiles
	r.Install = result.Install
	r.DependencyLockfilesTouched = evidence.DependencyLockfilesTouched(result.ChangedFiles)
	recordComposeEvidence(r, dataDir)
	r.DependencyChanges = result.DependencyChanges
	r.DiffStat = result.DiffStat
	r.ChangeSize = result.ChangeSize
	r.DiffAvailable = result.DiffAvailable
	r.DiffTruncated = result.DiffTruncated
	r.GateResults = result.GateResults
	r.TestsRedOnBase = result.TestsRedOnBase
	r.Challenge = result.Challenge
	r.Notes = result.Notes
	// result.Attempts carries every RunBuildActivity/RunVerifyActivity
	// attempt.
	r.Attempts = append(r.Attempts, result.Attempts...)

	// Found live (review): this Temporal-routed path returned without
	// ever loading BUILD_EVIDENCE.json, so r.AgentEvidence (the agent's
	// own provider/model/rounds/usage, distinct from the gate outcome
	// above) was always left empty here even though the same build run
	// directly records it — the durable audit record shouldn't depend on
	// which execution engine happened to run it.
	//
	// Loaded before the quarantine branch below saves anything, not after
	// — found via a later review round: a second save call after the
	// quarantine branch's own (needed to persist state before the Discord
	// wait — see its comment) reused this same in-memory r, which by then
	// could already be stale relative to disk if an operator's override
	// landed via the API server in between the two saves: that second
	// save would silently overwrite (lose) the override's Overrides entry
	// and revert State back to quarantined. Collapsing to the one save
	// below closes that window entirely — nothing here ever writes r a
	// second time.
	//
	// attributeWorkspaceEvidence gates this specifically for
	// reconcileReclaimedRun's own delayed call (found via review): that
	// call can run an arbitrary amount of time after this run's own build
	// actually finished — long enough for the repository owner to have
	// already started a *later* queued run against the same shared
	// workspace, which overwrites BUILD_EVIDENCE.json with that later
	// run's own provider/model/usage. Reading it here in that situation
	// would attribute someone else's evidence to this run's audit record.
	// A reconciled run's r.AgentEvidence simply stays nil (the same "not
	// collected" case an old run predating this field, or a read/parse
	// failure, already represents) rather than risk that.
	if attributeWorkspaceEvidence {
		// isolatedWorkspacePath, not workspacePath: an isolated run's
		// BUILD_EVIDENCE.json lives in its own worktree, which the shared
		// checkout at workspacePath never sees — reading the wrong one here
		// would either miss this run's real agent evidence entirely or
		// misattribute stale content already sitting in the shared checkout.
		loadAgentEvidence(r, isolatedWorkspacePath, dataDir, id)
		if summary := RoundSummary(r.AgentEvidence, buildCostUSD(r)); summary != "" {
			progressMark(dataDir, id, "build", "note", "", summary)
		}
		attachHarnessEval(r, dataDir, id)

		// The review's evidence file (CODE_REVIEW_EVIDENCE.json), best-effort,
		// os.IsNotExist-is-not-a-warning: a ticket whose run never reached the
		// review (a build/verify failure, an earlier gate failure) never wrote
		// it. The read is attempted only when the review was configured for
		// this run: a non-isolated workspace reused across runs must never
		// retain and attribute a PRIOR run's leftover evidence file to a run
		// whose own policy was off (see
		// workflow.RunWorkflowResult.CodeReviewConfigured's own doc comment).
		// Inside this attributeWorkspaceEvidence block on purpose: a reclaimed
		// run's delayed call must never attribute a later run's evidence to
		// this one.
		if result.CodeReviewConfigured {
			src := filepath.Join(isolatedWorkspacePath, codereview.EvidenceFile)
			dst := filepath.Join(run.Dir(dataDir, id), codereview.EvidenceFile)
			if err := evidence.RetainFile(src, dst); err != nil && !os.IsNotExist(err) {
				fmt.Printf("run %s: warning: could not retain %s: %v\n", id, codereview.EvidenceFile, err)
			}
		}
		loadCodeReview(r, dataDir, id)
	}

	// Before the release decision below reads it, and before the one save
	// that persists it. isolatedWorkspacePath shares the repository's object
	// store; only git objects are read from it.
	recordMemoryEdit(dp, r, dataDir, isolatedWorkspacePath)

	if r.State != run.StateAccepted {
		var failedChecks []string
		for _, g := range result.GateResults {
			if !g.Passed {
				failedChecks = append(failedChecks, g.Check)
			}
		}
		// reason/State both branch on r.State, not hardcoded to
		// StateQuarantined -- this Temporal-routed path reaches here for a
		// halted run too (e.g. a give-up before any policy gate ever ran),
		// and until this fix the notification recorded a durable
		// "quarantined" claim regardless, with an empty, misleading
		// "policy gate did not pass: " reason for a run that never
		// reached a gate decision at all (found via the 2026-09-05 Opus
		// review, S2).
		reason := fmt.Sprintf("policy gate did not pass: %s", strings.Join(failedChecks, ", "))
		if r.State == run.StateHalted {
			reason = "run halted before a policy gate decision was reached"
		}
		// Factory-authored triage (run.Run.Triage) leads the reason so the
		// alert says why, not just which gate; save(dp, ) would compute it
		// later anyway, but this notification is built first.
		if t := triage.Run(r, dataDir); t != "" {
			r.Triage = t
			reason = fmt.Sprintf("%s — %s", reason, t)
		}
		// Next: retry the owning request when this run belongs to one
		// (mirrors watch.go's printRunRecap), else watch the run directly.
		next := fmt.Sprintf("factoryd watch %s", id)
		if owning := findOwningRequest(dataDir, id); owning != "" {
			next = fmt.Sprintf("factoryd retry %s", owning)
		}
		delivered := true
		n := notify.Notification{
			RunID:     id,
			Ticket:    ticket,
			Reason:    reason,
			State:     r.State,
			SentAt:    time.Now().Format(time.RFC3339),
			Delivered: &delivered,
			RunDir:    run.AbsDir(dataDir, id),
			Next:      next,
			Link:      consoleRunURL(resolveConsoleBaseURL(dp, "", dataDir), id),
		}
		notifyCtx, cancelNotify := context.WithTimeout(context.Background(), 2*time.Second)
		notifyErr := (notify.LogNotifier{Path: filepath.Join(run.Dir(dataDir, id), "notifications.log")}).Notify(notifyCtx, n)
		cancelNotify()
		if notifyErr != nil {
			log.Printf("run %s: append quarantine notification: %v", id, notifyErr)
			delivered = false
			n.DeliveryError = notifyErr.Error()
		}
		r.Notifications = append(r.Notifications, n)

		// Persist the terminal quarantine state (with AgentEvidence
		// already attached above) before waiting on any Discord endpoint
		// below, not after: a crash or forced shutdown during a
		// slow/unreachable webhook's wait must not leave run.json stuck
		// at its previously saved state instead of "quarantined". This is
		// now the only save in this function — see loadAgentEvidence's
		// call above for why a second one after the Discord wait would
		// reopen a lost-update window against a concurrent override.
		if err := save(dp, r, dataDir); err != nil {
			rollbackIsolatedWorkspaceOnSaveFailure(r)
			return fmt.Errorf("persist quarantine state: %w", err)
		}

		// Cross-run attribution (gap 3's other half) — see
		// invalidatePriorRunOnFullSuiteRegression's own doc comment for why
		// this is placed here (after r's own terminal save above) and why
		// it is called once, here. Writes a different run's file (r.PriorRunID's), never
		// r's own, so this does not reopen the "only one save of r in this
		// function" window the comment above protects.
		invalidatePriorRunOnFullSuiteRegression(dp, dataDir, id, r.PriorRunID, failedChecks)

		// Found via review: this Temporal-routed path only ever reached
		// the durable LogNotifier above, never the same best-effort
		// external paging channels for the
		// same event — an operator relying on Discord/Slack/desktop
		// paging for quarantines got silently weaker coverage depending
		// on which execution engine happened to run a given ticket.
		// DispatchExternal (found via Codex review round 2 on PR #88:
		// this called DiscordNotifier directly, bypassing Slack/desktop
		// entirely) dispatches on its own goroutine and returns
		// immediately.
		dispatchRunNotification(dp, dataDir, r.RequestID, n)
	} else if err := save(dp, r, dataDir); err != nil {
		rollbackIsolatedWorkspaceOnSaveFailure(r)
		return err
	} else {
		// recordReleaseDecision keys the decision by release.ProjectOf(r)
		// -- r.Project, else the legacy derivation from r.ProjectPath --
		// never by the workspacePath parameter (found via Codex review of
		// PR #45): for an isolated run reconcileReclaimedRun applies,
		// workspacePath here is fresh.WorkspacePath, the isolated worktree
		// under .../workspaces/<run-id>, whose parent's basename would
		// derive "workspaces" as the project and store the decision under
		// the wrong project, consulting the wrong kill switch. r.Project
		// and r.ProjectPath are set once before submission and never
		// reassigned by this function (see its own doc comment on that).
		// decisionPolicy falls back to r's own persisted ReleasePolicy
		// (mergePolicyFromRun) before falling back further to the
		// always-deny release.MergePolicy{} zero value -- centralized
		// here, not just in reconcileReclaimedRun's own call site, so
		// ANY caller that reaches this function with releasePolicy == nil
		// (today, only reconcileReclaimedRun; a future caller need not
		// remember to recover it explicitly) gets the run's own real
		// policy back instead of always denying (this fallback used to
		// live only in reclaim.go, duplicating
		// mergePolicyFromRun's own ok=false legacy handling instead of
		// sharing it). A legacy run predating run.Run.ReleasePolicy still
		// gets mergePolicyFromRun's ok=false and keeps today's fail-closed
		// release.MergePolicy{} behavior, logged here so an operator
		// auditing a denied run can tell "denied by a real policy" apart
		// from "denied because no policy could be recovered".
		decisionPolicy := release.MergePolicy{}
		prPolicy := releasePolicy
		if releasePolicy != nil {
			decisionPolicy = *releasePolicy
		} else if recovered, ok := mergePolicyFromRun(r); ok {
			decisionPolicy = recovered
			prPolicy = &decisionPolicy
		} else {
			log.Printf("run %s: no release policy passed and none persisted on this run record (started before run.Run.ReleasePolicy existed) -- evaluating release decision against the fail-closed release.MergePolicy{} default", id)
		}
		decision := recordReleaseDecision(dataDir, id, r, decisionPolicy)
		// r.OpenPullRequest, not a new parameter threaded through every
		// caller: every run (runViaTemporal, runViaRepositoryOwner, and
		// reconcileReclaimedRun's own delayed reconciliation) completes
		// through this function. isolatedWorkspacePath,
		// not workspacePath, for the same reason recordReleaseDecision
		// above uses r.ProjectPath and not workspacePath -- it is this
		// run's own real checkout (the isolated worktree when one
		// exists), the directory forge.PullRequestOpener needs to push
		// the branch it already contains.
		//
		// Gated on decision.Allowed, mirroring run_ticket.go's own
		// identical gate -- see recordReleaseDecision's doc comment for
		// why a nil decision is treated the same as denied.
		var prURL, prWithheldReason string
		if r.OpenPullRequest {
			if decision != nil && decision.Allowed {
				prURL = openEvidencePullRequest(dataDir, isolatedWorkspacePath, id, r, prPolicy, opener)
			} else {
				prWithheldReason = releasePullRequestWithheldReason(decision)
				log.Printf("run %s: -open-pull-request set but release decision denies it: %s", id, prWithheldReason)
			}
		}
		n := notifyAcceptedRun(dp, dataDir, id, ticket, r.RequestID, prURL, prWithheldReason)
		// underRunLock: true for runViaRepositoryOwner's and
		// reconcileReclaimedRun's own callers, which already hold id's
		// run.WithLock for this entire call -- recordAcceptedRunSideEffects
		// must not take it again (see its own doc comment on why that
		// would deadlock). false for runViaTemporal's caller, which holds
		// no lock here at all.
		recordAcceptedRunSideEffects(dp, dataDir, r, prURL, n, underRunLock)
	}
	fmt.Printf("run %s: FINAL state=%s (via Temporal, task_queue=%s)\n", id, r.State, taskQueue)
	fmt.Printf("  base_sha=%s result_sha=%s\n", baseSHA, r.ResultSHA)

	if r.State != run.StateAccepted {
		return fmt.Errorf("run quarantined: %s", r.Notifications[len(r.Notifications)-1].Reason)
	}
	return nil
}

// repositoryOwnerPollInterval bounds how often runViaRepositoryOwner
// re-queries RepositoryOwnerWorkflow's durable result while waiting for
// this run's own request to complete. Short enough that a fast run
// doesn't visibly wait on it, long enough not to hammer the server with
// query traffic while a slower one (or one queued behind another
// repository run) is still in flight.
const repositoryOwnerPollInterval = 500 * time.Millisecond

// workerStopTimeout bounds how long this invocation's own process exit
// can be delayed waiting for a borrowed Activity/Workflow Task to finish
// gracefully. See the doc comment on WorkerStopTimeout's use below for
// why this is a fixed, short duration rather than
// workflow.ActivityStartToCloseTimeout.
const workerStopTimeout = 30 * time.Second
