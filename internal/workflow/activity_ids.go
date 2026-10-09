package workflow

import (
	"errors"
	"fmt"

	enumspb "go.temporal.io/api/enums/v1"

	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// explicitActivityIDsChange is the GetVersion change ID for RunWorkflow's
// explicit ActivityIDs. Version 1 names each Activity; DefaultVersion keeps
// the SDK's schedule-order IDs, so a workflow started before this change
// replays with the IDs its history already holds.
const explicitActivityIDsChange = "explicit-activity-ids"

// keepWorktreeWhenLostChange is the GetVersion change ID for RunWorkflow's
// deferred rollback. Version 1 skips the isolated-worktree rollback when the
// run halts because an Activity was lost (see lostActivity); DefaultVersion
// keeps the unconditional rollback, so a workflow started before this change
// replays with the rollback Activity its history already holds.
const keepWorktreeWhenLostChange = "keep-worktree-when-lost"

// heartbeatLost reports whether err means an Activity's worker went away: a
// heartbeat timeout. A start-to-close timeout is deliberately not a loss
// here (unlike RequestWorkflow's lostActivity): a hang past StartToClose
// with a live worker is a judged timeout, and its work is not kept for a
// resume.
func heartbeatLost(err error) bool {
	var timeoutErr *temporal.TimeoutError
	return errors.As(err, &timeoutErr) && timeoutErr.TimeoutType() == enumspb.TIMEOUT_TYPE_HEARTBEAT
}

// stepLost reports whether err means a step ended without a verdict on its
// work, so the worktree is kept for a resume decision: the Activity's worker
// went away (heartbeatLost), or the sandbox runtime started the build's
// command a second time and killed the first. A history recorded before the
// second case existed cannot hold its failure type, so it replays unchanged.
func stepLost(err error) bool {
	if heartbeatLost(err) {
		return true
	}
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == SandboxRerunFailureType
}

// StepLostFromError is stepLost for a RunWorkflow failure that crossed the
// Temporal boundary. cmd/factoryd uses it to mark the run's kept worktree.
func StepLostFromError(err error) bool {
	return stepLost(err)
}

// activityIDs hands out a stable ActivityID for each Activity RunWorkflow
// schedules. The Activity checkpoint, intent and journal records are keyed
// by ActivityID; with the SDK default (the schedule sequence number) adding
// or reordering an Activity silently shifts every later key. A name
// repeated within one execution (a second rollback) gets a -2, -3 suffix;
// the count advances in workflow code order, so it is deterministic on
// replay.
type activityIDs struct {
	explicit bool
	// retries is true when the run takes the activity-retries version: the
	// Activities named in withRetries get retriedActivityOptions' policy.
	retries bool
	seen    map[string]int
}

// activityRetriesChange is the GetVersion change ID for RunWorkflow's
// Activity retries. Version 1 retries the Activities listed in
// retriedActivityNames once; DefaultVersion and version 2 keep
// MaximumAttempts 1. A lost Activity is never rerun by Temporal on its own:
// the request waits in resume_review and a human decides (resume from the
// kept worktree, rebuild, or cancel). New executions record version 2; a
// history that recorded version 1 replays with the retry it holds, and one
// that recorded none replays with the single attempt.
const activityRetriesChange = "activity-retries"

// activityRetriesOn is the one version of activityRetriesChange whose
// Activities retry.
const activityRetriesOn = 1

// retriedActivityMaximumAttempts is the total attempt count (the first plus
// one retry). Why once: a worker stopped by a laptop sleep or a lost
// heartbeat is the case this exists for, and one rerun on the kept work
// covers it; a second loss in a row signals a systemic fault (a dead Docker
// daemon, a full disk) that more attempts would only spend model budget
// against, so the run halts for a human instead.
const retriedActivityMaximumAttempts = 2

// nonRetryableActivityFailureTypes is every application failure type except
// InfrastructureFailureType. Only an infrastructure failure (and a Temporal
// timeout, which carries no application type) says "this attempt was lost,
// the work is sound"; every other type is a verdict a rerun would repeat or
// a safety stop that must not be retried: an unconfirmed teardown, a
// refused precondition, an exhausted relay budget, a rejected compose file,
// an ambiguous prior attempt, or a superseded one.
var nonRetryableActivityFailureTypes = []string{
	CleanupUnconfirmedFailureType,
	PreflightFailureType,
	BaselineVerifyFailureType,
	SliceChainFailureType,
	RelayConfigurationFailureType,
	IsolationFailureType,
	RelayCeilingExceededFailureType,
	SandboxRerunFailureType,
	ComposeServicesRejectedFailureType,
	AmbiguousPriorAttemptType,
	ActivitySupersededType,
}

// retriedActivityOptions is options with the retrying policy: the second
// attempt keeps the first's work (see activity_handoff.go). Because Temporal
// retries inside the Activity, RunWorkflow sees a failure once, after the
// final attempt, so the isolated worktree's rollback on failure runs only
// then and never deletes the work a retry is about to resume.
func retriedActivityOptions(options temporalworkflow.ActivityOptions) temporalworkflow.ActivityOptions {
	options.RetryPolicy = &temporal.RetryPolicy{
		MaximumAttempts:        retriedActivityMaximumAttempts,
		NonRetryableErrorTypes: append([]string(nil), nonRetryableActivityFailureTypes...),
	}
	return options
}

// newActivityIDs must run at the same point in RunWorkflow on every
// execution, before its first ExecuteActivity, as GetVersion requires.
func newActivityIDs(ctx temporalworkflow.Context) *activityIDs {
	version := temporalworkflow.GetVersion(ctx, explicitActivityIDsChange, temporalworkflow.DefaultVersion, 1)
	retries := temporalworkflow.GetVersion(ctx, activityRetriesChange, temporalworkflow.DefaultVersion, 2)
	return &activityIDs{explicit: version == 1, retries: retries == activityRetriesOn, seen: map[string]int{}}
}

// with returns ctx carrying the next ActivityID for name, or ctx unchanged on
// a workflow replaying from before explicitActivityIDsChange.
func (ids *activityIDs) with(ctx temporalworkflow.Context, name string) temporalworkflow.Context {
	if !ids.explicit {
		return ctx
	}
	ids.seen[name]++
	options := temporalworkflow.GetActivityOptions(ctx)
	options.ActivityID = nextActivityID(name, ids.seen[name])
	return temporalworkflow.WithActivityOptions(ctx, options)
}

// withRetries is with for an Activity that Temporal may retry (build,
// verify, full-suite, named gates, review steps and the post-oracle verify).
// The Activities that must not repeat -- workspace prepare/rollback,
// base-sha capture, chain checks, preflight, oracle commit, worker-group
// write, evaluate -- use with and keep a single attempt, and so do
// post-build and collect-evidence: their safety-net commit intent is how a
// lost attempt's commit is detected (CommittedByWorker/Committed), which a
// retry that skipped the commit would misreport.
func (ids *activityIDs) withRetries(ctx temporalworkflow.Context, name string) temporalworkflow.Context {
	ctx = ids.with(ctx, name)
	if !ids.retries {
		return ctx
	}
	return temporalworkflow.WithActivityOptions(ctx, retriedActivityOptions(temporalworkflow.GetActivityOptions(ctx)))
}

// nextActivityID is the ActivityID of the nth Activity named name in one
// execution: name itself, then name-2, name-3.
func nextActivityID(name string, n int) string {
	if n > 1 {
		return fmt.Sprintf("%s-%d", name, n)
	}
	return name
}
