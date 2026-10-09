package workflow

import (
	"buildgate/internal/release"
	"buildgate/internal/run"
	wsisolation "buildgate/internal/workspace"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// RollbackIsolatedWorkspaceActivity discards the worktree/branch
// PrepareIsolatedWorkspaceActivity created (internal/release.Rollback),
// for a run that did not reach run.StateAccepted — mirrors
// cmd/factoryd's rollback defer. Checkpointed with the same
// two-phase intent protocol as PrepareIsolatedWorkspaceActivity, for the
// same reason: internal/workspace.Remove's branch delete is not
// idempotent (a second call against an already-deleted branch errors), so
// a redispatch after a crash between Remove succeeding and this
// checkpoint persisting must not blindly retry it.
// RollbackIsolatedWorkspaceActivity's own checkpoint/intent bookkeeping is
// deliberately best-effort, unlike every other checkpointed Activity in
// this file (found via review) — logged on failure, never blocking the
// actual rollback attempt below. RunBuildActivity/PrepareIsolatedWorkspaceActivity's
// strict halt-on-ambiguity protocol exists to protect against re-running
// a side effect that risks real duplication (a second build_app.py
// invocation, a second worktree); this Activity doesn't have that
// problem — internal/workspace.Remove already tolerates a worktree
// that's already gone (it prunes instead), and only its branch delete can
// meaningfully fail on a redundant retry, which is a harmless, visible
// error, not a duplicated side effect. Given that, gating the real git
// cleanup on successfully writing bookkeeping *first* is the wrong
// trade-off: on a checkpoint volume that's full or read-only — precisely
// the scenario that can make PrepareIsolatedWorkspaceActivity's own
// checkpoint save fail and dispatch this Activity in the first place —
// refusing to even attempt the actual cleanup would leak the worktree and
// branch permanently over a bookkeeping failure with nothing to do with
// whether the cleanup itself could succeed.
func (a *Activities) RollbackIsolatedWorkspaceActivity(ctx context.Context, input RollbackIsolatedWorkspaceInput) error {
	checkpointDir := a.resolveCheckpointDir(input.CheckpointDir, input.LogDir)
	if err := a.fenceAttempt(ctx, checkpointDir); err != nil {
		return err
	}
	checkpoint, path, found, loadErr := loadActivityCheckpoint[struct{}](ctx, checkpointDir)
	if loadErr == nil && found {
		if checkpoint.Error != "" {
			return temporal.NewApplicationError(checkpoint.Error, IsolationFailureType)
		}
		return nil
	}
	if loadErr != nil {
		// Same reasoning as the doc comment above: a checkpoint-load
		// failure (not just "no checkpoint yet") still falls through to
		// attempting the rollback, rather than halting like
		// checkpointLoadError's callers elsewhere in this file do.
		activity.GetLogger(ctx).Warn("failed to load isolate-workspace-rollback Activity checkpoint; attempting rollback anyway", "error", loadErr)
	}

	if _, err := recordActivityIntent(ctx, checkpointDir, "isolate-workspace-rollback", []string{"git", "-C", input.RepoDir, "worktree", "remove", "--force", input.WorktreePath}); err != nil {
		activity.GetLogger(ctx).Warn("failed to record isolate-workspace-rollback Activity intent; attempting rollback anyway", "error", err)
	}

	// Retained here, before the worktree is actually removed below, not
	// left to cmd/factoryd's own loadAgentEvidence (found via a real
	// GitHub Codex App review of this PR): a run that halts partway
	// through a Temporal-routed slice -- after build_app.py wrote this
	// report but before the workflow ever reaches a state
	// applyRunWorkflowResult's own attributeWorkspaceEvidence branch
	// covers -- never calls loadAgentEvidence at all, and this Activity's
	// own deferred rollback (registered unconditionally once
	// PrepareIsolatedWorkspaceActivity succeeds, per RunWorkflow's own
	// comment) deletes the one place that report still exists before
	// returning control to any caller that might have retained it later.
	// Best-effort and non-fatal, like every other evidence-retention
	// warning in this codebase: a run whose build never got far enough to
	// write a report simply has nothing here to retain, which
	// evidence.RetainFile's own os.IsNotExist return already
	// distinguishes from a real retention failure.
	// An OnBranch run (a PR-review round) loses its worktree the same way,
	// so this comes before both removals. The rounds' saved output goes
	// with the report (run.RetainBuildArtifacts).
	if input.LogDir != "" {
		if err := run.RetainBuildArtifacts(input.WorktreePath, input.LogDir); err != nil {
			activity.GetLogger(ctx).Warn("failed to retain the build's report and round logs before isolated-workspace rollback", "error", err)
		}
	}

	// OnBranch: this run only checked out an existing PR branch (never
	// created it) — RemoveWorktreeOnly, as cmd/factoryd's rollbackIsolatedWorkspace does,
	// leaves it untouched. release.Rollback would otherwise `branch -D`
	// it once its worktree registration is gone.
	if input.OnBranch {
		rollbackErr := wsisolation.RemoveWorktreeOnly(input.RepoDir, input.WorktreePath)
		var activityErr error
		if rollbackErr != nil {
			activityErr = temporal.NewApplicationErrorWithCause("rollback isolated workspace", IsolationFailureType, rollbackErr)
			checkpoint.Error = activityErr.Error()
		}
		if path != "" {
			if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
				activity.GetLogger(ctx).Warn("failed to save isolate-workspace-rollback Activity checkpoint", "error", err)
			}
		}
		return activityErr
	}

	rollbackErr := release.Rollback(input.RepoDir, input.WorktreePath, input.Branch)
	var activityErr error
	if rollbackErr != nil {
		activityErr = temporal.NewApplicationErrorWithCause("rollback isolated workspace", IsolationFailureType, rollbackErr)
		checkpoint.Error = activityErr.Error()
	}
	if path != "" {
		if err := saveActivityCheckpoint(path, checkpoint, activity.GetInfo(ctx).Attempt); err != nil {
			activity.GetLogger(ctx).Warn("failed to save isolate-workspace-rollback Activity checkpoint", "error", err)
		}
	}
	return activityErr
}

// isolatedWorkspaceMarker is recordIsolatedWorkspaceMarker's on-disk
// payload — deliberately not keyed by Workflow/Run/Activity ID like
// activityCheckpoint/activityIntent, since exactly one isolated workspace
// ever exists per run (checkpointDir is already scoped to one run — see
// Activities.CheckpointDir's own doc comment).
type isolatedWorkspaceMarker struct {
	RepoDir      string `json:"repo_dir"`
	WorktreePath string `json:"worktree_path"`
	Branch       string `json:"branch"`
}

func isolatedWorkspaceMarkerPath(checkpointDir string) string {
	return filepath.Join(checkpointDir, "isolated-workspace.json")
}

// recordIsolatedWorkspaceMarker durably records the isolated worktree's
// repoDir/path/branch immediately after wsisolation.Prepare succeeds —
// before any later step (stale-evidence cleanup, this Activity's own
// checkpoint save) that could itself fail. Follows the same
// write-temp-then-rename discipline as saveActivityCheckpoint/
// recordActivityIntent.
//
// This exists specifically for cmd/factoryd's own caller-side rollback on
// a hard TerminateWorkflow (found via review): every give-up/timeout path
// in cmd/factoryd calls TerminateWorkflow, which closes the Workflow
// Execution without running another workflow task at all — so
// RunWorkflow's own deferred RollbackIsolatedWorkspaceActivity call is
// never actually scheduled, regardless of whether the Go defer statement
// itself executes in-process (a terminated execution accepts no new
// Activity dispatch). The caller in that position has no ApplicationError
// to recover Details from either — a client-side context.DeadlineExceeded
// carries none — so this is the only channel left to learn where a real,
// already-created worktree/branch lives.
func recordIsolatedWorkspaceMarker(checkpointDir, repoDir, worktreePath, branch string) error {
	path := isolatedWorkspaceMarkerPath(checkpointDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create isolated workspace marker dir: %w", err)
	}
	b, err := json.MarshalIndent(isolatedWorkspaceMarker{RepoDir: repoDir, WorktreePath: worktreePath, Branch: branch}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal isolated workspace marker: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write isolated workspace marker: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename isolated workspace marker: %w", err)
	}
	return nil
}

// RecoverIsolatedWorkspaceFromCheckpointDir best-effort reads the marker
// recordIsolatedWorkspaceMarker writes — see that function's own doc
// comment for why a caller needs this independent of any error object at
// all. Returns empty strings if the marker was never written, can't be
// read, or can't be parsed; this is best-effort recovery, never the
// source of truth for whether isolation was used.
func RecoverIsolatedWorkspaceFromCheckpointDir(checkpointDir string) (repoDir, worktreePath, branch string) {
	b, err := os.ReadFile(isolatedWorkspaceMarkerPath(checkpointDir))
	if err != nil {
		return "", "", ""
	}
	var m isolatedWorkspaceMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return "", "", ""
	}
	return m.RepoDir, m.WorktreePath, m.Branch
}
