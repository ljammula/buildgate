package main

import (
	"buildgate/internal/forge"
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
func (dp *deps) UndoMarkPullRequestReady(ctx context.Context, prURL string) error {
	return dp.forge.undoMarkPullRequestReady(ctx, prURL)
}

func (dp *deps) RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error {
	return runMainWithReady(dp, ctx, args, onReady)
}

// The real pull-request and git-push methods whose bodies live in requestdriver.

func (impl realForge) readReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
	return requestdriver.RealReadReviewState(ctx, prURL, policy)
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
func (impl realForge) replyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	return requestdriver.RealReplyToReviewComment(ctx, prURL, commentID, body)
}
func (impl realForge) listReviewComments(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error) {
	return requestdriver.RealListReviewComments(ctx, prURL)
}
