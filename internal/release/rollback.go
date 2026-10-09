package release

import (
	"fmt"

	"buildgate/internal/workspace"
)

// Rollback discards an isolated per-run git worktree, created by
// internal/workspace.Prepare or PrepareOnBranch, when a run does not reach
// run.StateAccepted. This is the plan's named "automatic rollback to
// last-known-good" substitute for the removed human merge gate, made
// real by workspace isolation: a rejected run's changes were never made
// against the shared repository at repoDir in the first place, so
// "rollback" here means discarding the isolated branch entirely rather
// than resetting shared history anything else might depend on.
//
// onBranch is true for a run that checked out an existing branch
// (-on-branch): the branch is not the factory's to delete, so only the
// worktree goes. Every caller states it, so none can forget.
func Rollback(repoDir, worktreePath, branch string, onBranch bool) error {
	if onBranch {
		if err := workspace.RemoveWorktreeOnly(repoDir, worktreePath); err != nil {
			return fmt.Errorf("rollback: %w", err)
		}
		return nil
	}
	if err := workspace.Remove(repoDir, worktreePath, branch); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
