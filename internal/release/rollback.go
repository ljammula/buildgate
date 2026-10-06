package release

import (
	"fmt"

	"buildgate/internal/workspace"
)

// Rollback discards an isolated per-run git worktree and its branch,
// created by internal/workspace.Prepare, when a run does not reach
// run.StateAccepted. This is the plan's named "automatic rollback to
// last-known-good" substitute for the removed human merge gate, made
// real by workspace isolation: a rejected run's changes were never made
// against the shared repository at repoDir in the first place, so
// "rollback" here means discarding the isolated branch entirely rather
// than resetting shared history anything else might depend on.
func Rollback(repoDir, worktreePath, branch string) error {
	if err := workspace.Remove(repoDir, worktreePath, branch); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}
