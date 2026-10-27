package workflow

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/temporal"

	"buildgate/internal/reproduction"
	"buildgate/internal/run"
)

// The tests_red_on_base proof runs the test files a change added or changed,
// once on the result commit and once on the base commit, through the same
// gate-sandbox launch a held reviewer's reproduction uses
// (RunReviewReproductionActivity, SC-022): a scratch worktree of the commit,
// the held copy mounted read-only over the file's path, the toolchain runner's
// command, its report classified on the host. Only the hold differs: the copy
// is the changed file's content at the result commit (HoldChangedTestsActivity)
// rather than a reviewer's text.

// HoldChangedTestsActivityName holds the changed test files of a run
// (HoldChangedTestsActivity).
const HoldChangedTestsActivityName = "HoldChangedTestsActivity"

// testsRedAttemptKind is the attempt kind, progress kind and stage of a
// changed test file's launch.
const testsRedAttemptKind = "tests_red_on_base"

// TestsRedHoldInput is HoldChangedTestsActivity's input: the run's input, the
// result commit and the changed test files (policy.TestFiles) to hold.
type TestsRedHoldInput struct {
	RunWorkflowInput
	Commit string   `json:"commit"`
	Files  []string `json:"files"`
}

// TestsRedHoldResult is the files the proof will run, one HeldReproduction
// each (Finding is the file's index), and one line saying why the rest are
// not run. At most reproduction.MaxReproductions are held.
type TestsRedHoldResult struct {
	Held   []run.HeldReproduction `json:"held,omitempty"`
	Reason string                 `json:"reason,omitempty"`
}

// HoldChangedTestsActivity reads each changed test file from the result
// commit, keeps those a runner takes (reproduction.CheckChangedTest), up to
// reproduction.MaxReproductions, and copies each to the run's own directory
// (run.TestsRedOnBaseDir), mode 0444, recording its SHA-256. The content never
// reaches the workflow, its history or the run record. A copy that cannot be
// written is ReproductionUnheldFailureType: no verdict on the work.
func (a *Activities) HoldChangedTestsActivity(ctx context.Context, input TestsRedHoldInput) (TestsRedHoldResult, error) {
	tracked, err := treePaths(ctx, input.WorkspacePath, input.Commit)
	if err != nil {
		return TestsRedHoldResult{}, temporal.NewApplicationErrorWithCause("list the result commit's paths", InfrastructureFailureType, err)
	}
	root, err := rootFactsAt(ctx, input.WorkspacePath, input.Commit, tracked)
	if err != nil {
		return TestsRedHoldResult{}, temporal.NewApplicationErrorWithCause("read the result commit's root files", InfrastructureFailureType, err)
	}
	dir := run.TestsRedOnBaseDir(a.logDirFor(input.RunWorkflowInput))
	var res TestsRedHoldResult
	for _, p := range input.Files {
		if len(res.Held) >= reproduction.MaxReproductions {
			break
		}
		content, err := readCommitFile(ctx, input.WorkspacePath, input.Commit, p)
		if err != nil || content == "" {
			continue
		}
		accepted, err := reproduction.CheckChangedTest(p, []byte(content), root)
		if err != nil {
			continue
		}
		h, err := holdOne(dir, len(res.Held), accepted, []byte(content), false)
		if err != nil {
			return TestsRedHoldResult{}, temporal.NewNonRetryableApplicationError(fmt.Sprintf("hold the changed tests: %v", err), ReproductionUnheldFailureType, err)
		}
		res.Held = append(res.Held, h)
	}
	if len(res.Held) == 0 {
		res.Reason = "no changed test file has a test runner the factory can run"
	}
	return res, nil
}
