package main

import (
	"context"

	"buildgate/internal/requestdriver"
	"buildgate/internal/runner"
	"buildgate/internal/workspace"
)

// The real round-ref and round-branch methods of the forge boundary. The refs
// live in the target repository's local .git and are never pushed.

func (impl realForge) createRef(dir, ref, sha string) error {
	return workspace.CreateRef(dir, ref, sha)
}
func (impl realForge) pruneRoundRefs(dir, id string, keepPrefixes []string) error {
	return workspace.PruneRoundRefs(dir, id, keepPrefixes)
}
func (impl realForge) createBranchAt(dir, branch, sha string) error {
	return workspace.CreateBranchAt(dir, branch, sha)
}

// The restack's git calls: the local ones in internal/workspace and
// internal/runner, the remote ones beside the request driver's other pushes.
func (impl realForge) fetchBranchCommit(ctx context.Context, dir, branch, sha string) error {
	return requestdriver.RealFetchBranchCommit(ctx, dir, branch, sha)
}
func (impl realForge) revListRange(dir, from, to string) ([]string, []string, error) {
	return runner.GitRevListRange(context.Background(), dir, from, to)
}
func (impl realForge) patchID(dir, sha string) (string, error) {
	return runner.GitPatchID(context.Background(), dir, sha)
}
func (impl realForge) replayCommits(ctx context.Context, dir, worktreePath, branch, onto string, commits []string) ([]string, error) {
	return workspace.ReplayCommits(ctx, dir, worktreePath, branch, onto, commits)
}
func (impl realForge) pushWithLease(ctx context.Context, dir, sha, branch, expected string) error {
	return requestdriver.RealPushWithLease(ctx, dir, sha, branch, expected)
}
func (impl realForge) moveBranch(dir, branch, sha string) error {
	return workspace.MoveBranch(dir, branch, sha)
}
func (impl realForge) changedPaths(dir, base, result string) ([]string, error) {
	return runner.GitDiffNameOnly(dir, base, result)
}
func (impl realForge) carveChain(ctx context.Context, dir, base, result string, parts [][]string) error {
	_, err := workspace.CarveChain(ctx, dir, base, result, parts)
	return err
}
func (impl realForge) carveCommit(ctx context.Context, dir, parent, result string, paths []string, message string) (string, error) {
	return workspace.CarveCommit(ctx, dir, parent, result, paths, message)
}
func (impl realForge) restoreBranch(dir, branch, sha string) (bool, error) {
	return workspace.RestoreBranch(dir, branch, sha)
}
func (impl realForge) hostGitVersion(ctx context.Context) (string, error) {
	return workspace.HostGitVersion(ctx)
}
