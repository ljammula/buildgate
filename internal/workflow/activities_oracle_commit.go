package workflow

import (
	"buildgate/internal/evidence"
	"buildgate/internal/oraclecommit"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"fmt"
	"path/filepath"

	"go.temporal.io/sdk/temporal"
)

// CommitOraclesActivity is the Temporal path's host-side oracle commit (see
// CommitOraclesActivityName and internal/oraclecommit). Idempotent rather
// than checkpointed: re-running it after a crash between the commit and the
// Activity result finds the exact blobs already committed, commits nothing,
// re-verifies them against the pinned hashes and returns the same evidence,
// so Committed reports "the factory authored the oracles", not "this
// attempt made the commit".
func (a *Activities) CommitOraclesActivity(ctx context.Context, input CommitOraclesInput) (CommittedOracles, error) {
	fail := func(msg string, err error) (CommittedOracles, error) {
		return CommittedOracles{}, temporal.NewApplicationErrorWithCause(msg, InfrastructureFailureType, err)
	}
	logDir := a.logDirFor(input.RunWorkflowInput)
	snapshotDir, err := filepath.Abs(filepath.Join(logDir, "reference-oracle-commit-snapshot"))
	if err != nil {
		return fail("resolve oracle-commit snapshot path", err)
	}
	plan, err := oraclecommit.SnapshotPlan(input.WorkspacePath, input.ReferenceOracleDir, snapshotDir, input.PinnedOracleSHA256, input.ReleaseProtectedPaths)
	if err != nil {
		return fail("load committed-oracle plan", err)
	}
	if plan == nil {
		return CommittedOracles{}, nil
	}
	if input.ValidateOnly {
		if err := oraclecommit.Validate(input.WorkspacePath, plan, input.BaseSHA); err != nil {
			return fail("validate oracle plan (commit opted out)", err)
		}
		return CommittedOracles{}, nil
	}
	// The host commit changes HEAD after every gate ran; RunWorkflow always
	// follows a Committed result with RunPostOracleCommitVerifyActivity, which
	// re-verifies the resulting tree (Codex review of #202).
	// Progress marks only when there is something to commit, so a run with no
	// committed oracle keeps showing this stage as skipped (Codex review of #203).
	progressMark(ctx, logDir, "commit_oracles", "start", "", "")
	commitOutcome := "fail"
	defer func() { progressMark(ctx, logDir, "commit_oracles", "end", commitOutcome, "") }()
	requestID := input.RunID
	if requestID == "" {
		requestID = input.Ticket
	}
	msg := fmt.Sprintf("ticket(%s): commit accepted acceptance oracles\n\nAuto-committed by the Temporal Worker: the reference oracle passed against this\nchange and its manifest declares a target_path, so the host commits the\nverified oracle bytes (and .buildgate/oracles.json) into this result.", input.Ticket)
	applied, err := oraclecommit.Apply(input.WorkspacePath, plan, input.BaseSHA, requestID, msg)
	if err != nil {
		return fail("commit accepted oracles", err)
	}
	resultSHA, err := runner.GitRevParseHEAD(input.WorkspacePath)
	if err != nil {
		return fail("capture result SHA after oracle commit", err)
	}
	committedChanged, err := runner.GitDiffNameOnly(input.WorkspacePath, input.BaseSHA, resultSHA)
	if err != nil {
		return fail("collect committed changed files after oracle commit", err)
	}
	uncommittedChanged, err := runner.GitStatusPaths(input.WorkspacePath)
	if err != nil {
		return fail("collect uncommitted changed files after oracle commit", err)
	}
	changed := evidence.UnionSorted(committedChanged, uncommittedChanged)
	ev, err := oraclecommit.CollectEvidence(input.WorkspacePath, input.BaseSHA, resultSHA, changed, applied.Authored, applied.Deleted)
	if err != nil {
		return fail("collect committed-oracle evidence", err)
	}
	filesChanged, insertions, deletions, err := runner.GitDiffShortStatIncludingWorktree(input.WorkspacePath, input.BaseSHA)
	if err != nil {
		return fail("compute diff stat after oracle commit", err)
	}
	out := CommittedOracles{
		Active:       true,
		Committed:    len(applied.Authored) > 0,
		ResultSHA:    resultSHA,
		ChangedFiles: changed,
		DiffStat:     &run.DiffStat{FilesChanged: filesChanged, Insertions: insertions, Deletions: deletions},
		Oracles:      ev,
	}
	if input.RequiredContent != nil {
		// Same convention as CollectEvidenceActivity: final content is read
		// from the workspace (refusing a symlink), here from the tree that now
		// carries the factory's commit.
		out.RequiredContentFinalFiles = make(map[string]string, len(input.RequiredChangedFiles))
		for _, f := range input.RequiredChangedFiles {
			finalContent, _, err := runner.ReadRegularFile(filepath.Join(input.WorkspacePath, f))
			if err != nil {
				return fail(fmt.Sprintf("read final content of required file %q after oracle commit", f), err)
			}
			out.RequiredContentFinalFiles[f] = finalContent
		}
	}

	if logDir != "" {
		truncated, err := runner.GitDiffIncludingWorktreeToFile(input.WorkspacePath, input.BaseSHA, filepath.Join(logDir, run.DiffFileName), runner.MaxStoredDiffBytes)
		if err != nil {
			return fail("compute diff text after oracle commit", err)
		}
		out.DiffAvailable = true
		out.DiffTruncated = truncated
	}
	commitOutcome = "pass"
	return out, nil
}
