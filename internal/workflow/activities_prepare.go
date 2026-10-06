package workflow

import (
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/ticketspec"
	wsisolation "buildgate/internal/workspace"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// CaptureBaseSHAActivity reads the workspace's current git HEAD, fresh,
// at the moment this Activity actually runs. Two deliberate exceptions:
// an isolated chained run uses the accepted predecessor's ResultSHA, after
// independently validating that result and resolving it in the shared
// project repository, because the predecessor's private branch never
// advances the shared checkout; a -on-branch run (input.OnBranch set)
// resolves that EXISTING branch's own tip instead of HEAD, because
// WorkspacePath is the shared checkout (its HEAD is whatever ordinary
// branch, e.g. main, that repo happens to be on) and the run must be
// based at the named branch's own tip regardless. Found live (a Flutter + Go app repo run
// 3, 2026-09-28, PR #331): before this exception existed, a
// Temporal-routed -on-branch round silently captured the shared
// workspace's HEAD (main's tip) as its base instead of the PR branch's
// tip, so PrepareIsolatedWorkspaceActivity built its worktree from main —
// the round's result then had main, not the PR head, as its parent, and
// the subsequent `git push origin <PR branch>` from that worktree found
// its local ref unchanged and reported "Everything up-to-date" without
// ever pushing the fix.
//
// No checkpoint needed: this is a read-only check with no side effect,
// trivially safe to redo on redispatch — same reasoning as
// PreflightActivity below.
func (a *Activities) CaptureBaseSHAActivity(_ context.Context, input CaptureBaseSHAInput) (string, error) {
	if input.OnBranch != "" {
		sha, err := runner.GitRevParseRef(input.WorkspacePath, "refs/heads/"+input.OnBranch)
		if err != nil {
			return "", temporal.NewApplicationErrorWithCause("capture base sha: resolve -on-branch tip", InfrastructureFailureType, err)
		}
		return sha, nil
	}
	if input.UsePriorResultSHA {
		if input.PriorRunID == "" {
			return "", temporal.NewApplicationError("isolated chain requires a predecessor", SliceChainFailureType)
		}
		if input.ProjectPath == "" {
			return "", temporal.NewApplicationError("slice-chain validation requires a shared project path", SliceChainFailureType)
		}
		prior := &run.Run{
			ID:          input.PriorRunID,
			State:       input.PriorRunState,
			ProjectPath: input.PriorRunProjectPath,
			ResultSHA:   input.PriorRunResultSHA,
		}
		if err := run.ValidateSliceChain(prior, input.PriorRunResultSHA, input.ProjectPath); err != nil {
			return "", temporal.NewApplicationErrorWithCause("validate slice-chain predecessor", SliceChainFailureType, err)
		}
		resolved, err := runner.GitResolveCommit(input.ProjectPath, input.PriorRunResultSHA)
		if err != nil {
			return "", temporal.NewApplicationErrorWithCause("verify prior run result sha", SliceChainFailureType, err)
		}
		return resolved, nil
	}
	sha, err := runner.GitRevParseHEAD(input.WorkspacePath)
	if err != nil {
		return "", temporal.NewApplicationErrorWithCause("capture base sha", InfrastructureFailureType, err)
	}
	return sha, nil
}

// CheckChainSuccessorActivity runs the run.FindChainSuccessor staleness check — see
// CheckChainSuccessorActivityName's doc comment in workflow.go for why an
// isolated chain needs this in addition to CaptureBaseSHAActivity/
// ValidateSliceChainActivity's own (otherwise tautological, for this one
// case) baseSHA comparison.
//
// No checkpoint needed: this is a read-only check with no side effect,
// trivially safe to redo on redispatch — same reasoning as
// PreflightActivity/CaptureBaseSHAActivity above.
func (a *Activities) CheckChainSuccessorActivity(_ context.Context, input CheckChainSuccessorInput) error {
	dataDir := input.DataDir
	if dataDir == "" {
		dataDir = a.DataDir
	}
	if dataDir == "" {
		return temporal.NewApplicationError("slice-chain successor check requires a data directory", SliceChainFailureType)
	}
	successorID, err := run.FindChainSuccessor(dataDir, input.ProjectPath, input.PriorRunID)
	if err != nil {
		return temporal.NewApplicationErrorWithCause("check whether prior run has already been superseded", SliceChainFailureType, err)
	}
	if successorID != "" {
		return temporal.NewApplicationError(
			fmt.Sprintf("slice-chain validation: prior run %q has already been superseded by run %q — declare that as the prior run instead", input.PriorRunID, successorID),
			SliceChainFailureType,
		)
	}
	return nil
}

// PreflightActivity runs before RunBuildActivity ever starts. When supplied,
// it first validates the pi-harness-native ticket structure, then checks
// whether the ticket's declared Allowed-Files/Required-Changed-Files name a
// path missing a real subdirectory prefix (e.g. a monorepo whose real
// application code lives under backend/, not the checkout root -- see
// ticketspec.MisprefixedWorkspacePaths's own doc comment for the real
// incident that found this and why it's checked here, before the build,
// rather than left to diff_scope/required_files_changed after it), then
// checks whether any of the ticket's declared Required-Changed-Files
// already has uncommitted changes in the workspace. required_files_changed
// (evaluated after the run, against a diff from base_sha) can't tell an
// edit the agent actually made apart from one that was already sitting
// uncommitted before the run started — base_sha only captures HEAD, and the
// safety-net commits PostBuildActivity/CollectEvidenceActivity make sweep
// up any uncommitted change regardless of who made it. A required file
// that's already dirty here would let that gate pass on unattributable
// evidence, recreating the exact false-accept condition it exists to
// close, so the run must not proceed at all. These checks run here (not only in cmd/factoryd) so a
// caller that submits a run straight to RepositoryOwnerWorkflow, bypassing
// cmd/factoryd, gets the same protection (found via adversarial review,
// 2026-09-11, of the misprefixed-path check's own first version: it landed
// in realMain only, silently leaving this path -- the one this doc comment
// already exists to keep in sync with realMain -- without it).
//
// No checkpoint needed: this is a read-only check with no side effect,
// trivially safe to redo on redispatch.
func (a *Activities) PreflightActivity(ctx context.Context, input PreflightInput) (err error) {
	progressMark(ctx, input.LogDir, "preflight", "start", "", "")
	defer func() {
		outcome, detail := "pass", ""
		if err != nil {
			outcome, detail = "fail", err.Error()
		}
		progressMark(ctx, input.LogDir, "preflight", "end", outcome, detail)
	}()
	// Ticket-header strictness: refuse to start on a known header present
	// but malformed, or an unrecognized key that near-matches a known one
	// (e.g. "Verify-command:"/"Allowed_Files:") -- same check as cmd/factoryd (right after the ticket spec snapshot) so a caller that submits straight to RunWorkflow,
	// bypassing cmd/factoryd, gets the same protection. An absent
	// optional header is never a problem here -- see
	// ticketspec.HeaderStrictnessProblems's own doc comment.
	//
	// A SpecPath that doesn't exist on disk is deliberately not an error
	// here: this Activity only validates header *content*, and plenty of
	// this package's own fixture RunWorkflowInputs (fixtureInputForTicket)
	// set SpecPath to a placeholder that's never actually read -- a real
	// run's snapshot always exists by the time Preflight runs (it's
	// written before submission), and RunBuildActivity's own staging step
	// already fails closed if it's genuinely missing.
	if input.SpecPath != "" {
		if _, statErr := os.Stat(input.SpecPath); statErr == nil {
			problems, err := ticketspec.HeaderStrictnessProblems(input.SpecPath)
			if err != nil {
				return temporal.NewApplicationErrorWithCause("check ticket header strictness", InfrastructureFailureType, err)
			}
			if len(problems) > 0 {
				return temporal.NewApplicationError(
					fmt.Sprintf("ticket header preflight failed for %s:\n%s\n\nrun `factoryd check-ticket %s`", input.SpecPath, strings.Join(problems, "\n"), input.SpecPath),
					PreflightFailureType,
				)
			}
		} else if !os.IsNotExist(statErr) {
			return temporal.NewApplicationErrorWithCause("check ticket header strictness", InfrastructureFailureType, statErr)
		}
	}
	if input.TicketPath != "" {
		content, err := os.ReadFile(input.TicketPath)
		if err != nil {
			return temporal.NewApplicationErrorWithCause("read pi-harness ticket for structure preflight", PreflightFailureType, err)
		}
		// RequestTicket: TicketPath is a request-pipeline ticketspec-format
		// ticket (the run's own -spec snapshot), not a repo-native
		// pi-harness one -- checked against the same brownfield-ticket
		// checker cmd/factoryd's uses for a -request-ticket run
		// (see QueueEntry.RequestTicket's doc comment), so the two paths
		// agree instead of this Activity hard-failing every request-driven
		// Temporal run on a format mismatch.
		var passed bool
		var reasons []string
		if input.RequestTicket {
			passed, reasons = policy.TicketStructureBrownfield(string(content))
		} else {
			passed, reasons = policy.TicketStructure(string(content), input.TicketNumber)
		}
		if !passed {
			return temporal.NewApplicationError(
				fmt.Sprintf("pi-harness ticket structure preflight failed for %q: %s", input.TicketPath, strings.Join(reasons, "; ")),
				PreflightFailureType,
			)
		}
	}
	if scopePaths := dedupStrings(append(append([]string{}, input.AllowedFiles...), input.RequiredChangedFiles...)); len(scopePaths) > 0 {
		corrections, err := ticketspec.MisprefixedWorkspacePaths(input.WorkspacePath, scopePaths)
		if err != nil {
			return temporal.NewApplicationErrorWithCause("check ticket Allowed-Files/Required-Changed-Files against the workspace", InfrastructureFailureType, err)
		}
		if len(corrections) > 0 {
			names := make([]string, 0, len(corrections))
			for declared := range corrections {
				names = append(names, declared)
			}
			sort.Strings(names)
			lines := make([]string, 0, len(names))
			for _, declared := range names {
				lines = append(lines, fmt.Sprintf("%q -> found only at %q", declared, corrections[declared]))
			}
			return temporal.NewApplicationError(
				fmt.Sprintf("ticket declares a path that doesn't exist at the workspace's own root, but does exist under exactly one subdirectory of its own -- likely missing that prefix: %s", strings.Join(lines, "; ")),
				PreflightFailureType,
			)
		}
	}
	if len(input.RequiredChangedFiles) == 0 || input.Resumed {
		return nil
	}
	initialDirty, err := runner.GitStatusPaths(input.WorkspacePath)
	if err != nil {
		return temporal.NewApplicationErrorWithCause("check workspace for pre-existing uncommitted changes to required files", InfrastructureFailureType, err)
	}
	if alreadyDirty := policy.RequiredFilesPreDirty(initialDirty, input.RequiredChangedFiles); len(alreadyDirty) > 0 {
		return temporal.NewApplicationError(
			fmt.Sprintf("required file(s) already had uncommitted changes before this run started, so required_files_changed could not honestly attribute a change to this run: %v", alreadyDirty),
			PreflightFailureType,
		)
	}
	return nil
}

// dedupStrings returns ss with duplicates removed, preserving first-seen
// order.
func dedupStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ValidateSliceChainActivity ports cmd/factoryd's multi-slice
// chain validation (-prior-run) to the Temporal path. This validates against
// input.BaseSHA as captured by CaptureBaseSHAActivity — this run's real
// execution-time starting point, not whatever the submitting process
// observed before it was ever queued (see RunWorkflowInput's PriorRun*
// fields for why that distinction matters for -repository). A no-op when
// no chain was declared.
//
// No checkpoint needed: this is a read-only check with no side effect,
// trivially safe to redo on redispatch — same reasoning as
// PreflightActivity/CaptureBaseSHAActivity above.
func (a *Activities) ValidateSliceChainActivity(_ context.Context, input ValidateSliceChainInput) error {
	if input.PriorRunID == "" {
		return nil
	}
	// ValidateSliceChain only compares base_sha against the prior slice's
	// result_sha — it says nothing about uncommitted changes sitting in
	// the workspace on top of that commit. Mirrors cmd/factoryd's direct
	// path's own matching GitIsClean check (see its doc comment there):
	// without it, uncommitted dirt left after the prior slice would pass
	// chain validation untouched, then get swept into this slice's own
	// result by PostBuildActivity's/CollectEvidenceActivity's safety-net
	// commit — this slice's evidence would then describe a change neither
	// slice's own accepted evidence actually accounts for.
	clean, err := runner.GitIsClean(input.WorkspacePath)
	if err != nil {
		return temporal.NewApplicationErrorWithCause("check workspace cleanliness for slice-chain validation", InfrastructureFailureType, err)
	}
	if !clean {
		return temporal.NewApplicationError(
			fmt.Sprintf("slice-chain validation: workspace has uncommitted changes on top of prior run %q's result_sha; a declared chain must start from a clean, exact copy of the prior slice's result", input.PriorRunID),
			SliceChainFailureType,
		)
	}
	prior := &run.Run{
		ID:          input.PriorRunID,
		State:       input.PriorRunState,
		ProjectPath: input.PriorRunProjectPath,
		ResultSHA:   input.PriorRunResultSHA,
	}
	projectPath := input.ProjectPath
	if projectPath == "" {
		// ValidateSliceChainInput predates the separate shared-project field.
		// Replaying that history supplies only WorkspacePath, which was the
		// shared checkout at the pre-prepare validation point; retain that
		// compatibility while still rejecting an actually empty path.
		projectPath = input.WorkspacePath
	}
	if projectPath == "" {
		return temporal.NewApplicationError("slice-chain validation requires a shared project path", SliceChainFailureType)
	}
	if err := run.ValidateSliceChain(prior, input.BaseSHA, projectPath); err != nil {
		return temporal.NewApplicationErrorWithCause("slice-chain validation", SliceChainFailureType, err)
	}
	if _, err := runner.GitResolveCommit(projectPath, input.PriorRunResultSHA); err != nil {
		return temporal.NewApplicationErrorWithCause("verify prior run result sha", SliceChainFailureType, err)
	}
	return nil
}

// PrepareIsolatedWorkspaceActivity creates a per-run git worktree
// (internal/workspace.Prepare) so this run's own git operations land there
// instead of the shared checkout at input.RepoDir. Checkpointed, with the
// same two-phase intent protocol RunBuildActivity/RunVerifyActivity use
// (see their own doc comments): a crash between `git worktree add`
// succeeding and this checkpoint persisting halts a redispatch as
// AmbiguousPriorAttemptType rather than risk a duplicate worktree/branch
// or silently losing track of one that already exists — an accepted residual limitation.
func (a *Activities) PrepareIsolatedWorkspaceActivity(ctx context.Context, input PrepareIsolatedWorkspaceInput) (progressResult PrepareIsolatedWorkspaceResult, err error) {
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
		progressMark(ctx, input.LogDir, "prepare_workspace", "end", outcome, "")
	}()
	checkpointDir := a.resolveCheckpointDir(input.CheckpointDir, input.LogDir)
	if err := a.fenceAttempt(ctx, checkpointDir); err != nil {
		return PrepareIsolatedWorkspaceResult{}, err
	}
	checkpoint, path, found, err := loadActivityCheckpoint[PrepareIsolatedWorkspaceResult](ctx, checkpointDir)
	if err != nil {
		return PrepareIsolatedWorkspaceResult{}, checkpointLoadError("load isolate-workspace-prepare Activity checkpoint", err)
	}
	if found {
		if checkpoint.Error != "" {
			// attachIsolatedWorkspaceDetail, not a plain
			// NewApplicationError (found via a second GitHub Codex App
			// review round): checkpoint.Result already carries the real
			// worktree path/branch whenever the original failure happened
			// after wsisolation.Prepare succeeded (the fresh-failure path
			// below attaches them for exactly that reason) — but replaying
			// a cached failure on redispatch (e.g. a worker crash and
			// restart after the checkpoint was written) previously
			// discarded them again by returning a brand-new error with no
			// Details, leaving RunWorkflow with nothing to recover via
			// IsolatedWorkspaceFromError and never scheduling rollback a
			// second time either.
			return checkpoint.Result, attachIsolatedWorkspaceDetail(checkpoint.Error, IsolationFailureType, errors.New(checkpoint.Error), checkpoint.Result.WorktreePath, checkpoint.Result.Branch)
		}
		return checkpoint.Result, nil
	}
	progressStarted = true
	progressMark(ctx, input.LogDir, "prepare_workspace", "start", "", "")

	intentFound, err := priorActivityIntent(ctx, checkpointDir)
	if err != nil {
		return PrepareIsolatedWorkspaceResult{}, temporal.NewApplicationErrorWithCause("load isolate-workspace-prepare Activity intent", InfrastructureFailureType, err)
	}
	if intentFound {
		return PrepareIsolatedWorkspaceResult{}, temporal.NewApplicationError(
			"a prior attempt of this isolate-workspace-prepare Activity recorded intent to create the isolated worktree but crashed before reaching a durable checkpoint — halting rather than risk a duplicate or silently-abandoned worktree",
			AmbiguousPriorAttemptType,
		)
	}
	if input.Resume != nil {
		// A resume adopts the halted run's kept worktree instead of creating
		// one (see adoptKeptWorktree). No intent record: adoption deletes
		// nothing, and a redispatch is answered by the checkpoint saved below.
		// A failure carries no worktree details, so RunWorkflow never rolls
		// back the worktree it refused to adopt.
		adopted, adoptErr := a.adoptKeptWorktree(ctx, input, checkpointDir, activity.GetInfo(ctx).WorkflowExecution.RunID, activity.GetInfo(ctx).ActivityID)
		var activityErr error
		if adoptErr != nil {
			if wp, _ := IsolatedWorkspaceFromError(adoptErr); wp != "" {
				// Failed after the marker moved: the error already carries
				// the worktree, so the caller can flag the new run kept.
				activityErr = adoptErr
			} else {
				activityErr = temporal.NewApplicationErrorWithCause("adopt the kept isolated workspace", IsolationFailureType, adoptErr)
			}
			checkpoint.Error = activityErr.Error()
		} else {
			if markErr := recordIsolatedWorkspaceMarker(checkpointDir, input.RepoDir, adopted.WorktreePath, adopted.Branch); markErr != nil {
				activity.GetLogger(ctx).Warn("failed to record isolated workspace recovery marker", "error", markErr)
			}
			if rmErr := wsisolation.RemoveStaleEvidence(adopted.WorktreePath); rmErr != nil {
				activityErr = attachIsolatedWorkspaceDetail(rmErr.Error(), IsolationFailureType, rmErr, adopted.WorktreePath, adopted.Branch)
				checkpoint.Error = activityErr.Error()
			}
		}
		checkpoint.Result = adopted
		if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
			return adopted, attachIsolatedWorkspaceDetail("save isolate-workspace-prepare Activity checkpoint", InfrastructureFailureType, err, adopted.WorktreePath, adopted.Branch)
		}
		return adopted, activityErr
	}
	branch := "factoryd/" + input.RunID
	if input.OnBranch != "" {
		branch = input.OnBranch
	}
	worktreePath := filepath.Join(input.ParentDir, input.RunID)
	markerPath := ""
	if input.DataDir != "" && input.DurableRunID != "" {
		if err := wsisolation.ValidateIsolationRunID(input.DurableRunID); err != nil {
			return PrepareIsolatedWorkspaceResult{}, temporal.NewApplicationErrorWithCause("validate isolation marker run ID", InfrastructureFailureType, err)
		}
		commonDir, commonErr := wsisolation.GitCommonDir(input.RepoDir)
		if commonErr != nil {
			return PrepareIsolatedWorkspaceResult{}, temporal.NewApplicationErrorWithCause("resolve isolation marker repository", InfrastructureFailureType, commonErr)
		}
		markerPath = wsisolation.IsolationMarkerPath(input.DataDir, input.DurableRunID)
		if markerErr := wsisolation.WriteIsolationMarker(markerPath, wsisolation.IsolationMarker{
			Version:       wsisolation.IsolationMarkerVersion,
			RunID:         input.DurableRunID,
			WorktreeID:    input.RunID,
			Mode:          "temporal",
			RepoDir:       input.RepoDir,
			CommonDir:     commonDir,
			DataDir:       input.DataDir,
			ParentDir:     input.ParentDir,
			WorktreePath:  worktreePath,
			Branch:        branch,
			WorkflowID:    input.WorkflowID,
			ActivityRunID: activity.GetInfo(ctx).WorkflowExecution.RunID,
			ActivityID:    activity.GetInfo(ctx).ActivityID,
			CheckpointDir: checkpointDir,
		}); markerErr != nil {
			return PrepareIsolatedWorkspaceResult{}, temporal.NewApplicationErrorWithCause("record isolation ownership marker", InfrastructureFailureType, markerErr)
		}
	}
	intentArgs := []string{"git", "-C", input.RepoDir, "worktree", "add", "-b", branch, worktreePath, input.BaseSHA}
	if input.OnBranch != "" {
		intentArgs = []string{"git", "-C", input.RepoDir, "worktree", "add", "--force", worktreePath, branch}
	}
	if _, err := recordActivityIntent(ctx, checkpointDir, "isolate-workspace-prepare", intentArgs); err != nil {
		return PrepareIsolatedWorkspaceResult{}, temporal.NewApplicationErrorWithCause("record isolate-workspace-prepare Activity intent", InfrastructureFailureType, err)
	}

	var prepErr error
	if input.OnBranch != "" {
		worktreePath, prepErr = wsisolation.PrepareOnBranch(input.RepoDir, input.ParentDir, input.RunID, branch)
	} else {
		worktreePath, branch, prepErr = wsisolation.Prepare(input.RepoDir, input.ParentDir, input.RunID, input.BaseSHA)
	}
	result := PrepareIsolatedWorkspaceResult{WorktreePath: worktreePath, Branch: branch}
	var activityErr error
	if prepErr != nil {
		activityErr = temporal.NewApplicationErrorWithCause("prepare isolated workspace", IsolationFailureType, prepErr)
		checkpoint.Error = activityErr.Error()
	} else if markerPath != "" {
		marker, markerErr := wsisolation.LoadIsolationMarker(markerPath)
		if markerErr == nil {
			marker.Prepared = true
			markerErr = wsisolation.WriteIsolationMarker(markerPath, marker)
		}
		if markerErr != nil {
			activityErr = attachIsolatedWorkspaceDetail("mark isolated workspace prepared", InfrastructureFailureType, markerErr, worktreePath, branch)
			checkpoint.Error = activityErr.Error()
		}
	}
	if prepErr == nil {
		if markErr := recordIsolatedWorkspaceMarker(checkpointDir, input.RepoDir, worktreePath, branch); markErr != nil {
			// Best-effort, logged rather than failing the Activity (found via
			// review): a caller's own hard TerminateWorkflow (used by every
			// give-up/timeout path in cmd/factoryd) closes the Workflow
			// Execution without running another workflow task at all, so
			// RunWorkflow's own deferred rollback is never actually scheduled
			// regardless of whether the Go defer statement executes
			// in-process. cmd/factoryd's own caller-side rollback for that
			// case reads this marker directly off disk, independent of any
			// error-Details round trip (there is no error to recover Details
			// from — a client-side ctx.DeadlineExceeded from a timed-out wait
			// carries none). A failure to write it here shouldn't fail an
			// otherwise-successful Prepare — it only means that specific
			// caller-side safety net has nothing to read; the primary
			// mechanisms (the workflow's own defer, and Details attached to
			// any later Activity failure) are unaffected.
			activity.GetLogger(ctx).Warn("failed to record isolated workspace recovery marker", "error", markErr)
		}
	}
	if activityErr == nil {
		if rmErr := wsisolation.RemoveStaleEvidence(worktreePath); rmErr != nil {
			// attachIsolatedWorkspaceDetail, not a plain
			// NewApplicationErrorWithCause (found via review): by this point
			// wsisolation.Prepare above has already created a real
			// worktree/branch on disk — unlike prepErr's own branch above,
			// where Prepare never creates anything to leak. RunWorkflow's
			// Get(ctx, &prep) never populates prep on a failed Activity, so
			// without this Details attachment, its rollback defer (only ever
			// registered *after* this whole Activity call succeeds) would have
			// no way to learn this worktree/branch exist at all — the exact
			// crash cmd/factoryd's already hit once and fixed
			// by moving its equivalent cleanup to run after its own rollback
			// defer was registered (see
			// TestIntegrationIsolateWorkspaceRollsBackWhenStaleEvidenceCleanupFails).
			activityErr = attachIsolatedWorkspaceDetail(rmErr.Error(), IsolationFailureType, rmErr, worktreePath, branch)
			checkpoint.Error = activityErr.Error()
		}
	}
	if activityErr == nil {
		// Mirrors cmd/factoryd's equivalent, run right after
		// its own wsisolation.Prepare succeeds -- see
		// internal/workspace.EnableWorkerGroupWrite's own doc comment for
		// the full mechanism and why this, applied unconditionally
		// regardless of which sandbox identity this run ends up using, is
		// the actual worker-UID-separation setup step (Phase 6).
		if grantErr := wsisolation.EnableWorkerGroupWrite(worktreePath, os.Getgid()); grantErr != nil {
			activityErr = attachIsolatedWorkspaceDetail(grantErr.Error(), IsolationFailureType, grantErr, worktreePath, branch)
			checkpoint.Error = activityErr.Error()
		}
	}
	checkpoint.Result = result
	if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
		// Same reasoning as the evidence-cleanup failure above: a
		// checkpoint-save failure here can only happen after
		// wsisolation.Prepare (and, if it ran, the evidence cleanup) have
		// already succeeded, so worktreePath/branch are always real by
		// this point too.
		return result, attachIsolatedWorkspaceDetail("save isolate-workspace-prepare Activity checkpoint", InfrastructureFailureType, err, worktreePath, branch)
	}
	return result, activityErr
}

// DisableWorkerGroupWriteActivity revokes the group-write permission
// PrepareIsolatedWorkspaceActivity granted (internal/workspace.
// EnableWorkerGroupWrite) once a run's own isolated worktree will no
// longer be touched by any further sandboxed attempt. Called only from
// RunWorkflow's own deferred cleanup, on the branch that *keeps* the
// worktree (StateAccepted/StateQuarantined) rather than discarding it via
// RollbackIsolatedWorkspaceActivity — a worktree that's about to be
// deleted outright has no permission state left to revoke.
//
// Deliberately uncheckpointed, unlike every strict-halt-on-ambiguity
// Activity in this file: internal/workspace.DisableWorkerGroupWrite is
// itself idempotent (chmod to a computed target mode, not a stateful
// mutation that compounds on retry) and best-effort by design (see its
// own doc comment) — there is no duplicated side effect a redispatch here
// could cause that checkpointing would need to prevent.
func (a *Activities) DisableWorkerGroupWriteActivity(_ context.Context, input DisableWorkerGroupWriteInput) error {
	if input.WorktreePath == "" {
		return nil
	}
	if err := wsisolation.DisableWorkerGroupWrite(input.WorktreePath); err != nil {
		return temporal.NewApplicationErrorWithCause("disable worker group write", InfrastructureFailureType, err)
	}
	return nil
}
