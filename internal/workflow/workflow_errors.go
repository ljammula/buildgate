package workflow

import (
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"errors"

	"go.temporal.io/sdk/temporal"
)

// AttemptsFromError recovers per-attempt build/verify evidence a failed
// RunBuildActivity/RunVerifyActivity/RunWorkflow attached to its returned
// error as ApplicationError Details (see RunBuildActivity's doc comment
// on its failure returns for why: Temporal does not deliver an Activity's
// or Workflow's return value to its caller alongside a non-nil error,
// only the error itself, so a failure's Details are the only channel left
// to carry this evidence across that boundary). Returns nil, not an
// error, when err carries no such Details — every caller treats missing
// attempt evidence on a failure as "none recovered", not fatal, since a
// failure early enough (e.g. PreflightActivity, or loading a checkpoint)
// has no attempts to have recovered in the first place.
func AttemptsFromError(err error) []run.Attempt {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return nil
	}
	var attempts []run.Attempt
	if detailsErr := appErr.Details(&attempts); detailsErr != nil {
		return nil
	}
	return attempts
}

// CleanupUnconfirmedFromError preserves the sandbox cleanup safety signal
// through Temporal's ApplicationError wrappers.
func CleanupUnconfirmedFromError(err error) bool {
	if errors.Is(err, sandbox.ErrCleanupUnconfirmed) {
		return true
	}
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == CleanupUnconfirmedFailureType
}

// RelayCeilingExceededFromError preserves the relay-ceiling-exceeded signal
// through Temporal's ApplicationError wrappers, mirroring
// CleanupUnconfirmedFromError above exactly (see its own doc comment).
func RelayCeilingExceededFromError(err error) bool {
	if errors.Is(err, sandbox.ErrRelayCeilingExceeded) {
		return true
	}
	var appErr *temporal.ApplicationError
	return errors.As(err, &appErr) && appErr.Type() == RelayCeilingExceededFailureType
}

// HaltReasonCodeFromError maps err to run.Run.HaltReasonCode's value,
// through Temporal's ApplicationError wrappers like
// RelayCeilingExceededFromError, or "" for a halt cause with no code.
func HaltReasonCodeFromError(err error) string {
	switch {
	case RelayCeilingExceededFromError(err):
		return run.HaltReasonRelayCeilingExceeded
	case errors.Is(err, sandbox.ErrComposeServicesRejected):
		return run.HaltReasonComposeServicesRejected
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return ""
	}
	switch appErr.Type() {
	case ComposeServicesRejectedFailureType:
		return run.HaltReasonComposeServicesRejected
	case BaselineVerifyFailureType:
		return run.HaltReasonBaselineVerifyFailed
	}
	return ""
}

// BaseSHAFromError recovers the execution-time BaseSHA wrapActivityFailure
// attaches as a second Details value alongside Attempts (see its own doc
// comment) — the same error-Details round trip AttemptsFromError already
// uses for per-attempt evidence. Requesting two Details values against an
// ApplicationError that was only ever given one (or zero) — every other
// error construction in this codebase that predates this field, e.g. a
// bare runner-level infrastructure failure — fails outright ("too many
// arguments"), not merely leaves the second pointer unset, so this
// deliberately falls back to a one-value decode first: only a
// wrapActivityFailure-produced error (or one explicitly given a second
// detail value) ever successfully decodes the second pointer at all.
// Returns "" if err carries none — either it isn't such an error, or the
// failure happened before CaptureBaseSHAActivity ever ran (so there was no
// captured value yet to attach).
func BaseSHAFromError(err error) string {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return ""
	}
	var attempts []run.Attempt
	var baseSHA string
	if detailsErr := appErr.Details(&attempts, &baseSHA); detailsErr != nil {
		return ""
	}
	return baseSHA
}

// CommittedFromError recovers the committed flag wrapActivityFailure
// attaches as a third Details value (see its own doc comment) — true means
// the failed child's own safety-net commit already advanced the shared
// workspace's HEAD before it failed, leaving committed-but-unverified
// content behind. Same safe-fallback convention as BaseSHAFromError:
// requesting three Details values against an error given fewer fails
// outright rather than partially decoding, so this returns false — not
// "unknown" — for any error that isn't a wrapActivityFailure-produced one,
// or one that predates this field. False is the conservative-but-wrong
// direction for an old error shape, but every call site that matters
// (RunWorkflow's own Activity failures) always attaches this now; a
// mismatch here would mean the workspace genuinely wasn't touched by this
// specific failure, not that this function is guessing.
func CommittedFromError(err error) bool {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return false
	}
	var attempts []run.Attempt
	var baseSHA string
	var committed bool
	if detailsErr := appErr.Details(&attempts, &baseSHA, &committed); detailsErr != nil {
		return false
	}
	return committed
}

// IsolatedWorkspaceFromError recovers the isolated worktree path/branch
// wrapActivityFailure attaches as a fourth/fifth Details value alongside
// Attempts/BaseSHA/Committed (see its own doc comment) — the same
// error-Details round trip AttemptsFromError/BaseSHAFromError/
// CommittedFromError already use. Needed for the same reason those exist:
// a later Activity failing (RunBuildActivity onward) after
// PrepareIsolatedWorkspaceActivity already succeeded means
// RunWorkflowResult.WorkspacePath/Branch — set only on RunWorkflow's
// successful return path — would otherwise never reach a caller that only
// ever sees this error, leaving a real, just-created worktree/branch with
// no record of its existence anywhere once RollbackIsolatedWorkspaceActivity
// (which reads its own path/branch straight from RunWorkflow's own local
// variables, not from this recovery path) discards it. Returns ("", "")
// if err carries none — either it isn't such an error, isolation was never
// requested for this run, or the failure happened before
// PrepareIsolatedWorkspaceActivity ever succeeded.
func IsolatedWorkspaceFromError(err error) (workspacePath, branch string) {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return "", ""
	}
	var attempts []run.Attempt
	var baseSHA string
	var committed bool
	if detailsErr := appErr.Details(&attempts, &baseSHA, &committed, &workspacePath, &branch); detailsErr != nil {
		return "", ""
	}
	return workspacePath, branch
}

// applicationErrorType returns the innermost Temporal application-error
// type carried by err. ChildWorkflowExecutionError and the workflow wrappers
// both preserve the original application error through Unwrap, so this keeps
// stop-line classification tied to the typed failure rather than brittle
// string matching.
func applicationErrorType(err error) string {
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) {
		return ""
	}
	return appErr.Type()
}

// wrapActivityFailure wraps a failed Activity's error with context
// (matching RunWorkflow's pre-existing "run build activity"/"run verify
// activity"-style messages), re-attaching whatever AttemptsFromError
// recovers from it as the new error's own Details so that evidence
// survives one more hop up the call chain (RepositoryOwnerWorkflow,
// cmd/factoryd), and preserving the original error's ApplicationError
// type (e.g. AmbiguousPriorAttemptType) instead of collapsing every
// failure to InfrastructureFailureType.
// priorAttempts is evidence already accumulated by an earlier step in the
// same RunWorkflow execution (e.g. build's attempts, when verify is the
// one that failed) — merged in ahead of whatever AttemptsFromError
// recovers from err itself, since Get never populates a failed Activity's
// own return value and that accumulated result would otherwise be lost
// along with it.
// baseSHA is the execution-time value RunWorkflow's own CaptureBaseSHAActivity
// captured (possibly "" if that Activity itself hasn't succeeded yet) —
// attached as a second Details value so a caller no longer has to fall
// back to its own stale, pre-submission BaseSHA when reporting this
// failure (see BaseSHAFromError's doc comment for why that mattered).
// committed is result.CommittedByWorker at the moment of failure — true
// when PostBuildActivity's or CollectEvidenceActivity's own safety-net
// commit already advanced the workspace's HEAD past baseSHA before this
// later Activity failed. Attached as a third Details value, recoverable
// via CommittedFromError, so RepositoryOwnerWorkflow can tell a run that
// merely failed to build/verify (workspace unchanged, safe to move on)
// apart from one that left committed-but-never-canonically-verified
// content sitting in the shared workspace for whatever request the owner
// dispatches next to unknowingly inherit as its own base.
// workspacePath/branch are the isolated worktree's path and branch (both
// "" when RunWorkflowInput.IsolateWorkspace was false, or the failure
// happened before PrepareIsolatedWorkspaceActivity succeeded) — attached
// as a fourth/fifth Details value, recoverable via
// IsolatedWorkspaceFromError, so a caller that only ever sees this error
// (Get never populates a failed Workflow's return value) still learns
// where the just-created worktree/branch RollbackIsolatedWorkspaceActivity
// is about to discard actually live, for its own durable record.
func wrapActivityFailure(context string, err error, priorAttempts []run.Attempt, baseSHA string, committed bool, workspacePath, branch string) error {
	errType := InfrastructureFailureType
	var appErr *temporal.ApplicationError
	if errors.As(err, &appErr) {
		errType = appErr.Type()
	}
	attempts := append(append([]run.Attempt{}, priorAttempts...), AttemptsFromError(err)...)
	return temporal.NewApplicationErrorWithCause(context+": "+err.Error(), errType, err, attempts, baseSHA, committed, workspacePath, branch)
}
