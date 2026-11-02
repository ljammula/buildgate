package main

import (
	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"context"
)

// The methods that make *deps a requestdriver dependency set.

func (dp *deps) ListReviewComments(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error) {
	return dp.forge.listReviewComments(ctx, prURL)
}
func (dp *deps) MarkPullRequestReady(ctx context.Context, prURL string) error {
	return dp.forge.markPullRequestReady(ctx, prURL)
}
func (dp *deps) PushExistingBranch(ctx context.Context, workspaceDir string, sha string, branch string) error {
	return dp.forge.pushExistingBranch(ctx, workspaceDir, sha, branch)
}
func (dp *deps) ReadReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
	return dp.forge.readReviewState(ctx, prURL, policy)
}
func (dp *deps) RemoteBranchHeadSHA(ctx context.Context, workspaceDir string, branch string) (string, error) {
	return dp.forge.remoteBranchHeadSHA(ctx, workspaceDir, branch)
}
func (dp *deps) ReplyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	return dp.forge.replyToReviewComment(ctx, prURL, commentID, body)
}
func (dp *deps) RetargetPullRequestBase(ctx context.Context, prURL string, base string) error {
	return dp.forge.retargetPullRequestBase(ctx, prURL, base)
}
func (dp *deps) RoundResultDescendsFromHead(dir string, ancestor string, descendant string) (bool, error) {
	return dp.forge.roundResultDescendsFromHead(dir, ancestor, descendant)
}
func (dp *deps) ChangedFileStats(ctx context.Context, dir string, base string, head string) ([]requestdriver.FileStat, error) {
	return dp.forge.changedFileStats(ctx, dir, base, head)
}
func (dp *deps) CreateRef(dir string, ref string, sha string) error {
	return dp.forge.createRef(dir, ref, sha)
}
func (dp *deps) CreateBranchAt(dir string, branch string, sha string) error {
	return dp.forge.createBranchAt(dir, branch, sha)
}
func (dp *deps) PruneRoundRefs(dir string, id string, keepPrefixes []string) error {
	return dp.forge.pruneRoundRefs(dir, id, keepPrefixes)
}
func (dp *deps) UndoMarkPullRequestReady(ctx context.Context, prURL string) error {
	return dp.forge.undoMarkPullRequestReady(ctx, prURL)
}

func (dp *deps) SaveReviewEntry(dataDir string, r *request.Request) error {
	return r.Save(dataDir)
}

func (dp *deps) FetchBranchCommit(ctx context.Context, dir string, branch string, sha string) error {
	return dp.forge.fetchBranchCommit(ctx, dir, branch, sha)
}
func (dp *deps) RevListRange(dir string, from string, to string) ([]string, []string, error) {
	return dp.forge.revListRange(dir, from, to)
}
func (dp *deps) PatchID(dir string, sha string) (string, error) { return dp.forge.patchID(dir, sha) }
func (dp *deps) ReplayCommits(ctx context.Context, dir string, worktreePath string, branch string, onto string, commits []string) ([]string, error) {
	return dp.forge.replayCommits(ctx, dir, worktreePath, branch, onto, commits)
}
func (dp *deps) PushWithLease(ctx context.Context, dir string, sha string, branch string, expected string) error {
	return dp.forge.pushWithLease(ctx, dir, sha, branch, expected)
}
func (dp *deps) MoveBranch(dir string, branch string, sha string) error {
	return dp.forge.moveBranch(dir, branch, sha)
}
func (dp *deps) ChangedPaths(dir string, base string, result string) ([]string, error) {
	return dp.forge.changedPaths(dir, base, result)
}
func (dp *deps) CarveChain(ctx context.Context, dir string, base string, result string, parts [][]string) error {
	return dp.forge.carveChain(ctx, dir, base, result, parts)
}
func (dp *deps) CarveCommit(ctx context.Context, dir string, parent string, result string, paths []string, message string) (string, error) {
	return dp.forge.carveCommit(ctx, dir, parent, result, paths, message)
}
func (dp *deps) RestoreBranch(dir string, branch string, sha string) (bool, error) {
	return dp.forge.restoreBranch(dir, branch, sha)
}
func (dp *deps) CommentOnPullRequest(ctx context.Context, prURL string, body string) error {
	return dp.forge.commentOnPullRequest(ctx, prURL, body)
}
func (dp *deps) ReadJobLog(ctx context.Context, prURL string, jobID int64) (string, error) {
	return dp.forge.readJobLog(ctx, prURL, jobID)
}
func (dp *deps) HostGitVersion(ctx context.Context) (string, error) {
	return dp.forge.hostGitVersion(ctx)
}

func (dp *deps) RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error {
	return runMainWithReady(dp, ctx, args, onReady)
}

// The real pull-request and git-push methods whose bodies live in requestdriver.

func (impl realForge) readReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
	return requestdriver.RealReadReviewState(ctx, prURL, policy)
}
func (impl realForge) readJobLog(ctx context.Context, prURL string, jobID int64) (string, error) {
	return requestdriver.RealReadJobLog(ctx, prURL, jobID)
}
func (impl realForge) markPullRequestReady(ctx context.Context, prURL string) error {
	return requestdriver.RealMarkPullRequestReady(ctx, prURL)
}
func (impl realForge) undoMarkPullRequestReady(ctx context.Context, prURL string) error {
	return requestdriver.RealUndoMarkPullRequestReady(ctx, prURL)
}
func (impl realForge) retargetPullRequestBase(ctx context.Context, prURL, base string) error {
	return requestdriver.RealRetargetPullRequestBase(ctx, prURL, base)
}
func (impl realForge) pushExistingBranch(ctx context.Context, workspaceDir, sha, branch string) error {
	return requestdriver.RealPushExistingBranch(ctx, workspaceDir, sha, branch)
}
func (impl realForge) remoteBranchHeadSHA(ctx context.Context, workspaceDir, branch string) (string, error) {
	return requestdriver.RealRemoteBranchHeadSHA(ctx, workspaceDir, branch)
}
func (impl realForge) roundResultDescendsFromHead(dir, ancestor, descendant string) (bool, error) {
	return requestdriver.RealRoundResultDescendsFromHead(dir, ancestor, descendant)
}
func (impl realForge) changedFileStats(ctx context.Context, dir, base, head string) ([]requestdriver.FileStat, error) {
	return requestdriver.RealChangedFileStats(ctx, dir, base, head)
}
func (impl realForge) commentOnPullRequest(ctx context.Context, prURL string, body string) error {
	return requestdriver.RealCommentOnPullRequest(ctx, prURL, body)
}
func (impl realForge) replyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	return requestdriver.RealReplyToReviewComment(ctx, prURL, commentID, body)
}
func (impl realForge) listReviewComments(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error) {
	return requestdriver.RealListReviewComments(ctx, prURL)
}
