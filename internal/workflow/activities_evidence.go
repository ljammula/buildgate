package workflow

import (
	"buildgate/internal/evidence"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// CollectEvidenceActivity gathers the ticket's declared scope/required-
// file/required-content keys and the real git evidence needed to
// evaluate them, run after RunVerifyActivity so it sees anything
// verification itself left behind (a formatter, codegen) — mirroring
// cmd/factoryd's own documented reasoning for that ordering.
//
// Checkpointed for the same reason PostBuildActivity is (see its own doc
// comment): on redispatch after a prior attempt's own safety-net commit
// already ran, the workspace being clean now would otherwise make this
// Activity silently report Committed=false, even though the factory
// itself made that commit.
func (a *Activities) CollectEvidenceActivity(ctx context.Context, input CollectEvidenceInput) (progressResult CollectedEvidence, err error) {
	collectEvidenceLogDir := input.LogDir
	if collectEvidenceLogDir == "" {
		collectEvidenceLogDir = a.LogDir
	}
	// The stage "start" mark is deferred until past the checkpoint
	// short-circuit below: a redispatch of an already-completed Activity
	// must not add a spurious near-zero start/end pair to the feed.
	progressStarted := false
	defer func() {
		if !progressStarted {
			return
		}
		outcome := "pass"
		if err != nil {
			outcome = "fail"
		}
		progressMark(ctx, collectEvidenceLogDir, "evidence", "end", outcome, "")
	}()
	checkpointDir := a.resolveCheckpointDir(input.CheckpointDir, input.LogDir)
	if err := a.fenceAttempt(ctx, checkpointDir); err != nil {
		return CollectedEvidence{}, err
	}
	checkpoint, path, found, err := loadActivityCheckpoint[CollectedEvidence](ctx, checkpointDir)
	if err != nil {
		return CollectedEvidence{}, checkpointLoadError("load collect-evidence Activity checkpoint", err)
	}
	if found {
		if checkpoint.Error != "" {
			return checkpoint.Result, attachCommittedDetail(temporal.NewApplicationError(checkpoint.Error, InfrastructureFailureType), checkpoint.Result.Committed)
		}
		return checkpoint.Result, nil
	}
	progressStarted = true
	progressMark(ctx, collectEvidenceLogDir, "evidence", "start", "", "")

	// Same two-phase intent protocol as PostBuildActivity — see its doc
	// comment for why: closes the crash window between this Activity's own
	// safety-net commit succeeding and the checkpoint below persisting.
	intentFound, err := priorActivityIntent(ctx, checkpointDir)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("load collect-evidence Activity intent", InfrastructureFailureType, err)
	}
	if intentFound {
		return CollectedEvidence{}, temporal.NewApplicationError(
			"a prior attempt of this collect-evidence Activity recorded intent to commit verification output but crashed before reaching a durable checkpoint — halting rather than risk misreporting Committed",
			AmbiguousPriorAttemptType,
		)
	}

	result, activityErr := a.runCollectEvidence(ctx, checkpointDir, input)
	checkpoint.Result = result
	if activityErr != nil {
		// Same convention as PostBuildActivity's own checkpoint.Error: see
		// its comment for why the original error's exact type doesn't
		// need preserving here.
		checkpoint.Error = activityErr.Error()
	}
	if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		return result, attachCommittedDetail(temporal.NewApplicationErrorWithCause("save collect-evidence Activity checkpoint", InfrastructureFailureType, err), result.Committed)
	}
	return result, attachCommittedDetail(activityErr, result.Committed)
}

// runCollectEvidence is CollectEvidenceActivity's actual logic, factored
// out so the checkpoint wrapper above can save its result (success or
// failure) uniformly regardless of which path returned.
func (a *Activities) runCollectEvidence(ctx context.Context, checkpointDir string, input CollectEvidenceInput) (result CollectedEvidence, err error) {
	// committed and this defer, not a per-return-site edit, are what
	// preserve Committed=true across every failure return below the
	// commit at "committed = true" — found via review: every one of those
	// returns a bare CollectedEvidence{}, discarding a commit this same
	// Activity invocation already made moments earlier (e.g. diff
	// collection or a required-content read failing right after the
	// verification-output commit above succeeded). Since result/err are
	// named returns, an explicit `return CollectedEvidence{}, someErr`
	// still populates them before this deferred func runs, so it can
	// retroactively patch Committed onto the exact value CollectEvidenceActivity's
	// own checkpoint and the Activity-boundary error both end up carrying,
	// without editing every individual return site.
	var committed bool
	defer func() {
		if err != nil {
			result.Committed = committed
		}
	}()
	// Canonical verification can itself leave the workspace dirty (a
	// formatter, codegen) without committing its own output. Found live
	// (review): recording ResultSHA at an earlier commit while still
	// folding that worktree dirt into ChangedFiles/DiffStat below let an
	// accepted run's evidence describe changes its own result_sha commit
	// couldn't reproduce or be merged from. Same safety-net-commit
	// guarantee PostBuildActivity already makes for build-time dirt,
	// extended to verification's own output, before ResultSHA is
	// captured — mirrors cmd/factoryd.
	//
	// Gated on both exit codes being 0: found live (a second review
	// round), committing unconditionally advanced HEAD even when build
	// or verification had already failed and this run would quarantine
	// regardless — a later accepted run's base_sha would then silently
	// include a prior quarantined run's partial/failed output, even
	// though that run's own changed-file gates excluded it. Extended to
	// FullSuiteExitCode when FullSuiteRan (found via a later review
	// round, same class of bug): see FullSuiteRan's own doc comment.
	if input.BuildExitCode == 0 && input.VerifyExitCode == 0 && (!input.FullSuiteRan || input.FullSuiteExitCode == 0) && !input.RepoGatesFailed {
		verifyClean, err := runner.GitIsClean(input.WorkspacePath)
		if err != nil {
			return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("check workspace cleanliness after verification", InfrastructureFailureType, err)
		}
		if !verifyClean {
			msg := "ticket: commit verification output\n\nAuto-committed by the Temporal Worker: canonical verification left\nthe workspace dirty (e.g. a formatter or code generator) without\ncommitting its own output."
			if _, err := recordActivityIntent(ctx, checkpointDir, "collect-evidence-commit", []string{"git", "-C", input.WorkspacePath, "-c", "core.hooksPath=/dev/null", "commit", "-m", msg}); err != nil {
				return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("record collect-evidence-commit Activity intent", InfrastructureFailureType, err)
			}
			if err := runner.GitCommitAll(input.WorkspacePath, msg); err != nil {
				return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("commit verification output", InfrastructureFailureType, err)
			}
			committed = true
		}
	}

	// Captured fresh here, not trusted from an earlier step: canonical
	// verification can itself create a commit (a formatter, codegen), so
	// the pre-verification SHA PostBuildActivity returned could already
	// be stale by the time this runs. See CollectedEvidence's doc comment.
	resultSHA, err := runner.GitRevParseHEAD(input.WorkspacePath)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("capture post-verification result SHA", InfrastructureFailureType, err)
	}

	committedChanged, err := runner.GitDiffNameOnly(input.WorkspacePath, input.BaseSHA, resultSHA)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect committed changed files", InfrastructureFailureType, err)
	}
	// The workspace was just confirmed clean (or made clean) above, so
	// this is expected to always be empty — kept as a second, direct
	// source of truth rather than trusted to be a no-op, and because
	// GitDiffShortStatIncludingWorktree below still reads the worktree
	// directly rather than the commit alone.
	uncommittedChanged, err := runner.GitStatusPaths(input.WorkspacePath)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect uncommitted changed files", InfrastructureFailureType, err)
	}

	result = CollectedEvidence{
		ResultSHA:    resultSHA,
		ChangedFiles: setAsideChallengeFiles(input.WorkspacePath, resultSHA, evidence.UnionSorted(committedChanged, uncommittedChanged), input.SetAside),
		Committed:    committed,
	}
	packageLockChanges, err := runner.PackageLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic package-lock dependency changes", InfrastructureFailureType, err)
	}
	composerLockChanges, err := runner.ComposerLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic composer.lock dependency changes", InfrastructureFailureType, err)
	}
	pubspecLockChanges, err := runner.PubspecLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic pubspec.lock dependency changes", InfrastructureFailureType, err)
	}
	goSumChanges, err := runner.GoSumDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic go.sum dependency changes", InfrastructureFailureType, err)
	}
	yarnChanges, err := runner.YarnLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic yarn.lock dependency changes", InfrastructureFailureType, err)
	}
	pnpmChanges, err := runner.PnpmLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic pnpm-lock dependency changes", InfrastructureFailureType, err)
	}
	gemChanges, err := runner.GemfileLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic Gemfile.lock dependency changes", InfrastructureFailureType, err)
	}
	poetryChanges, err := runner.PoetryLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic poetry.lock dependency changes", InfrastructureFailureType, err)
	}
	cargoChanges, err := runner.CargoLockDependencyChanges(input.WorkspacePath, input.BaseSHA, result.ChangedFiles)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("collect semantic Cargo.lock dependency changes", InfrastructureFailureType, err)
	}
	result.DependencyChanges = append(append(append(append(append(packageLockChanges, composerLockChanges...), pubspecLockChanges...), goSumChanges...), yarnChanges...), pnpmChanges...)
	result.DependencyChanges = append(append(append(result.DependencyChanges, gemChanges...), poetryChanges...), cargoChanges...)
	evidence.SortDependencyChanges(result.DependencyChanges)
	// Worktree-inclusive, not a plain two-commit diff stat, for
	// consistency with ChangedFiles above — though by this point the
	// worktree is clean, so this reduces to the same thing a plain
	// base..resultSHA diff stat would report.
	filesChanged, insertions, deletions, err := runner.GitDiffShortStatIncludingWorktree(input.WorkspacePath, input.BaseSHA)
	if err != nil {
		return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("compute diff stat", InfrastructureFailureType, err)
	}
	result.DiffStat = &run.DiffStat{FilesChanged: filesChanged, Insertions: insertions, Deletions: deletions}

	// Snapshotted once, here, from the workspace at its true final state —
	// not recomputed later from a live workspace a diff-reading API request
	// might otherwise touch well after this run's workspace has moved on to
	// a later run. Written straight to this run's own durable directory,
	// not returned through this Activity's result — see run.Run.
	// DiffAvailable's and CollectedEvidence's own doc comments for why.
	diffLogDir := input.LogDir
	if diffLogDir == "" {
		diffLogDir = a.LogDir
	}
	if diffLogDir != "" {
		truncated, err := runner.GitDiffIncludingWorktreeToFile(input.WorkspacePath, input.BaseSHA, filepath.Join(diffLogDir, run.DiffFileName), runner.MaxStoredDiffBytes)
		if err != nil {
			return CollectedEvidence{}, temporal.NewApplicationErrorWithCause("compute diff text", InfrastructureFailureType, err)
		}
		result.DiffAvailable = true
		result.DiffTruncated = truncated
	}

	return result, nil
}

// setAsideChallengeFiles drops from changed each file of set that the result
// commit holds with exactly the pinned SHA-256: the factory's own committed
// challenge files (SC-022), set aside by hash and never by path, so a file
// of the same path with any other content is judged like any changed file.
func setAsideChallengeFiles(dir, resultSHA string, changed []string, set []run.ChallengeFile) []string {
	if len(set) == 0 {
		return changed
	}
	pinned := make(map[string]string, len(set))
	for _, f := range set {
		pinned[f.Path] = f.SHA256
	}
	kept := make([]string, 0, len(changed))
	for _, p := range changed {
		if want, ok := pinned[p]; ok && want != "" {
			if content, existed, err := runner.GitShowFile(dir, resultSHA, p); err == nil && existed {
				sum := sha256.Sum256([]byte(content))
				if hex.EncodeToString(sum[:]) == want {
					continue
				}
			}
		}
		kept = append(kept, p)
	}
	return kept
}
