package workflow

import (
	"context"
	"os"
	"path/filepath"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"buildgate/internal/evidence"
	"buildgate/internal/reviewstep"
)

// The harness session folders a launch's scripts save prompts in
// (agent/pi/scripts/saved_prompts.py), relative to the worktree. A build
// launch may also run the conformity review in its own process, so its list
// has that review's folder too.
const conformitySessionDir = ".pi-conformity-session"

var buildPromptSessions = []string{buildSessionDir, conformitySessionDir}

// reviewPromptSession is the session folder step's script saves its prompt in.
func reviewPromptSession(stepName string) string {
	switch stepName {
	case reviewstep.CodeReview:
		return ".pi-code-review-session"
	case reviewstep.Combined:
		return ".pi-combined-review-session"
	}
	return conformitySessionDir
}

// retainLaunchPrompts copies the prompts a launch saved in sessions into the
// run's own directory, under prompts/<kind>-<attempt>/ (SC-018: for the
// operator only). What is copied is what the session folders held: see
// evidence.PromptsDirName. Best-effort: the operator loses the prompts, the
// step is not failed.
func (a *Activities) retainLaunchPrompts(ctx context.Context, input RunWorkflowInput, kind string, attempt int32, sessions []string) {
	logDir := a.logDirFor(input)
	if logDir == "" || input.WorkspacePath == "" {
		return
	}
	dst := filepath.Join(logDir, evidence.PromptsDirName, evidence.PromptAttemptDir(kind, int(attempt)))
	if _, err := evidence.RetainPrompts(input.WorkspacePath, sessions, dst); err != nil {
		activity.GetLogger(ctx).Warn("failed to retain every prompt the launch saved", "kind", kind, "error", err)
	}
}

// prepareReviewLaunch is prepareReviewInstructions, then the removal of what
// the worktree holds in step's prompts folder before the launch: not saved by
// it, so the build agent wrote it. A folder left read-only is made removable
// first; when it still cannot be shown gone the step fails as an
// infrastructure error, and no review is launched beside it. The returned
// preparation is finished by the caller in either case.
func (a *Activities) prepareReviewLaunch(ctx context.Context, input ReviewStepInput, step reviewstep.Step, dst string) (reviewInstructions, error) {
	prep, err := a.prepareReviewInstructions(ctx, input, step.Name, dst)
	if err != nil {
		return prep, err
	}
	if err := evidence.DropSavedPrompts(input.WorkspacePath, []string{reviewPromptSession(step.Name)}); err != nil {
		return prep, temporal.NewApplicationErrorWithCause("remove the prompts found in the review's session folder before its launch", InfrastructureFailureType, err)
	}
	return prep, nil
}

// clearBeforeBuild removes what a build launch must not find in the worktree:
// an earlier build's evidence file, and any prompt in the session folders the
// launch saves its own in (not saved by it, so an earlier step or the
// repository wrote it). A failure is an infrastructure error.
func clearBeforeBuild(workspace string) error {
	if err := os.Remove(filepath.Join(workspace, "BUILD_EVIDENCE.json")); err != nil && !os.IsNotExist(err) {
		return temporal.NewApplicationErrorWithCause("remove stale build evidence", InfrastructureFailureType, err)
	}
	if err := evidence.DropSavedPrompts(workspace, buildPromptSessions); err != nil {
		return temporal.NewApplicationErrorWithCause("remove the prompts found in the build's session folders before its launch", InfrastructureFailureType, err)
	}
	return nil
}

// retainReviewPrompts copies the prompt a review launch saved into the run's
// directory and removes it from the worktree, which the next review shares.
func (a *Activities) retainReviewPrompts(ctx context.Context, input ReviewStepInput, step reviewstep.Step) {
	session := []string{reviewPromptSession(step.Name)}
	a.retainLaunchPrompts(ctx, input.RunWorkflowInput, step.Name, activity.GetInfo(ctx).Attempt, session)
	if err := evidence.DropSavedPrompts(input.WorkspacePath, session); err != nil {
		activity.GetLogger(ctx).Warn("failed to remove the review's saved prompts from the worktree", "error", err)
	}
}
