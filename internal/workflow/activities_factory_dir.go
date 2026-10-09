package workflow

import (
	"buildgate/internal/projectconfig"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/sanitize"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Every sandbox of a run that executes a repository command (the build, the
// baseline and canonical verify, the full suite, every named and repository
// gate, the oracle canary and the reruns after an oracle commit) sees
// /workspace/.factory as the commit .factory.yml was read from holds it,
// mounted read-only (SC-012). runSandboxWithRetries is the one place that
// attaches the mount, before every launch; a review's launch, which runs no
// repository command, is marked with forReviewLaunch and gets none.

// maxFactoryDirErrorBytes caps the cleaned refusal an attempt records.
const maxFactoryDirErrorBytes = 500

var factoryDirFullCommit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

type reviewLaunchKey struct{}

// forReviewLaunch marks ctx as a model review's launch: its sandbox runs no
// repository command and carries the review's instruction masks only.
func forReviewLaunch(ctx context.Context) context.Context {
	return context.WithValue(ctx, reviewLaunchKey{}, true)
}

func isReviewLaunch(ctx context.Context) bool {
	marked, _ := ctx.Value(reviewLaunchKey{}).(bool)
	return marked
}

// factoryDirCommit is the commit whose `.factory/` the run's sandboxes see:
// the commit .factory.yml was read from at dispatch, else (a run dispatched
// before that input existed) the commit its reviews read instruction files
// from.
func (input RunWorkflowInput) factoryDirCommit() string {
	if input.ProjectConfigCommitSHA != "" {
		return input.ProjectConfigCommitSHA
	}
	return input.instructionBase()
}

// prepareFactoryDir stages the `.factory/` mount of one launch on workspace
// under a new directory beside the launch's log, never inside the workspace.
// A review launch gets the zero mount. An error that wraps
// sandbox.ErrCommitDirMount is a refusal: the commit's or the worktree's shape
// causes it, the launch must not happen and is not retried. Any other error
// (git could not run, the context ended, a directory could not be written) is
// an ordinary infrastructure failure.
func prepareFactoryDir(ctx context.Context, input RunWorkflowInput, workspace, logDir string) (sandbox.CommitDirMount, error) {
	if isReviewLaunch(ctx) {
		return sandbox.CommitDirMount{}, nil
	}
	commit := input.factoryDirCommit()
	if commit != "" && !factoryDirFullCommit.MatchString(commit) {
		resolved, err := runner.GitRevParseRef(workspace, commit+"^{commit}")
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			// git ran and does not know the commit: a rerun repeats it.
			return sandbox.CommitDirMount{}, fmt.Errorf("%w: resolve the commit %s/ is read from: %w", sandbox.ErrCommitDirMount, projectconfig.DirName, err)
		}
		if err != nil {
			return sandbox.CommitDirMount{}, fmt.Errorf("resolve the commit %s/ is read from: %w", projectconfig.DirName, err)
		}
		commit = resolved
	}
	return sandbox.PrepareCommitDirMount(ctx, workspace, commit, projectconfig.DirName, sandbox.CommitDirStagingPath(logDir))
}

// refusedFactoryDirAttempt records the attempt whose launch prepareFactoryDir
// did not allow and returns what runSandboxWithRetries hands back: the
// attempt never started a process, so it has exit code -1. A refusal also
// records its cleaned reason; a failure that is not one records none and is
// returned as the infrastructure failure it is, for the Activity's retry.
func refusedFactoryDirAttempt(afterAttempt func(int, runner.Result, error) error, attempt int, command []string, cause error) (runner.Result, error) {
	text := ""
	if errors.Is(cause, sandbox.ErrCommitDirMount) {
		// The two sentinels' own words say nothing the halt reason does not.
		text = sanitize.Line(cause.Error())
		text = strings.TrimPrefix(text, sandbox.ErrCommitDirMount.Error()+": ")
		text = strings.TrimPrefix(text, sandbox.ErrCommitDirSnapshot.Error()+": ")
	}
	if len(text) > maxFactoryDirErrorBytes {
		text = strings.ToValidUTF8(text[:maxFactoryDirErrorBytes], "")
	}
	now := time.Now()
	last := runner.Result{ExitCode: -1, Command: command, StartedAt: now, FinishedAt: now, FactoryDirError: text}
	err := fmt.Errorf("mount %s/ read-only: %w", projectconfig.DirName, cause)
	if afterAttempt != nil {
		if hookErr := afterAttempt(attempt, last, err); hookErr != nil {
			return last, errors.Join(err, fmt.Errorf("after attempt %d hook: %w", attempt, hookErr))
		}
	}
	return last, err
}

// launchErrorType is the ApplicationError type of an Activity whose launch
// returned runErr: an unconfirmed teardown and a refused `.factory/` mount
// keep their own non-retryable types, anything else is an infrastructure
// failure.
func launchErrorType(runErr error) string {
	switch {
	case errors.Is(runErr, sandbox.ErrCleanupUnconfirmed):
		return CleanupUnconfirmedFailureType
	case errors.Is(runErr, sandbox.ErrCommitDirMount):
		return FactoryDirFailureType
	}
	return InfrastructureFailureType
}

// checkpointErrorType is launchErrorType as a checkpoint records it: empty
// for an infrastructure failure, which is what a replayed checkpoint with no
// type is read as.
func checkpointErrorType(runErr error) string {
	if t := launchErrorType(runErr); t != InfrastructureFailureType {
		return t
	}
	return ""
}

// joinCleanup adds a cleanup's error to a launch's, leaving the launch's
// error as it is when the cleanup had none.
func joinCleanup(runErr, cleanupErr error) error {
	if cleanupErr == nil {
		return runErr
	}
	return errors.Join(runErr, cleanupErr)
}
