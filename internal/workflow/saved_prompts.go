package workflow

import (
	"context"
	"path/filepath"

	"go.temporal.io/sdk/activity"

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
// operator only). Best-effort: the operator loses the prompts, the step is
// not failed.
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

// dropStaleReviewPrompts removes what the worktree holds in step's prompts
// folder before the launch: not saved by it, so the build agent wrote it.
func (a *Activities) dropStaleReviewPrompts(ctx context.Context, input ReviewStepInput, step reviewstep.Step) {
	if err := evidence.DropSavedPrompts(input.WorkspacePath, []string{reviewPromptSession(step.Name)}); err != nil {
		activity.GetLogger(ctx).Warn("failed to remove prompts found before the review launched", "error", err)
	}
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
