package requestdriver_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"buildgate/internal/consolelink"
	"buildgate/internal/forge"
	"buildgate/internal/notify"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// TestMain keeps this package's tests inside the test process: no desktop
// notification, no console link to a serve that happens to run on this
// machine, and no session config or gh login of the operator's.
func TestMain(m *testing.M) {
	os.Exit(runTests(m))
}

func runTests(m *testing.M) int {
	home, err := os.MkdirTemp("", "requestdriver-test-home-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(home)
	os.Setenv("HOME", home)
	os.Setenv("XDG_CONFIG_HOME", home+"/xdg")
	os.Setenv(notify.DesktopNotificationsEnvironmentVariable, "0")
	consolelink.Listening = func(string) bool { return false }
	return m.Run()
}

// fakeDeps is a requestdriver.Deps whose every method is a field. newFakeDeps
// sets each to a refusal, so a test reaches no pull request, pushes nothing
// and launches no build unless it sets the field itself.
type fakeDeps struct {
	listReviewCommentsFn          func(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error)
	markPullRequestReadyFn        func(ctx context.Context, prURL string) error
	pushExistingBranchFn          func(ctx context.Context, workspaceDir string, sha string, branch string) error
	readReviewStateFn             func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error)
	remoteBranchHeadSHAFn         func(ctx context.Context, workspaceDir string, branch string) (string, error)
	replyToReviewCommentFn        func(ctx context.Context, prURL string, commentID int64, body string) error
	retargetPullRequestBaseFn     func(ctx context.Context, prURL string, base string) error
	roundResultDescendsFromHeadFn func(dir string, ancestor string, descendant string) (bool, error)
	undoMarkPullRequestReadyFn    func(ctx context.Context, prURL string) error
	runTicketFn                   func(ctx context.Context, args []string, onReady func(*run.Run)) error
}

var _ requestdriver.Deps = (*fakeDeps)(nil)

func (f *fakeDeps) ListReviewComments(ctx context.Context, prURL string) ([]requestdriver.ReviewComment, error) {
	return f.listReviewCommentsFn(ctx, prURL)
}
func (f *fakeDeps) MarkPullRequestReady(ctx context.Context, prURL string) error {
	return f.markPullRequestReadyFn(ctx, prURL)
}
func (f *fakeDeps) PushExistingBranch(ctx context.Context, workspaceDir string, sha string, branch string) error {
	return f.pushExistingBranchFn(ctx, workspaceDir, sha, branch)
}
func (f *fakeDeps) ReadReviewState(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
	return f.readReviewStateFn(ctx, prURL, policy)
}
func (f *fakeDeps) RemoteBranchHeadSHA(ctx context.Context, workspaceDir string, branch string) (string, error) {
	return f.remoteBranchHeadSHAFn(ctx, workspaceDir, branch)
}
func (f *fakeDeps) ReplyToReviewComment(ctx context.Context, prURL string, commentID int64, body string) error {
	return f.replyToReviewCommentFn(ctx, prURL, commentID, body)
}
func (f *fakeDeps) RetargetPullRequestBase(ctx context.Context, prURL string, base string) error {
	return f.retargetPullRequestBaseFn(ctx, prURL, base)
}
func (f *fakeDeps) RoundResultDescendsFromHead(dir string, ancestor string, descendant string) (bool, error) {
	return f.roundResultDescendsFromHeadFn(dir, ancestor, descendant)
}
func (f *fakeDeps) UndoMarkPullRequestReady(ctx context.Context, prURL string) error {
	return f.undoMarkPullRequestReadyFn(ctx, prURL)
}
func (f *fakeDeps) RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error {
	return f.runTicketFn(ctx, args, onReady)
}

func newFakeDeps(testing.TB) *fakeDeps {
	refused := func(what string) error { return errors.New("test deps: " + what + " is not set by this test") }
	return &fakeDeps{
		listReviewCommentsFn: func(context.Context, string) ([]requestdriver.ReviewComment, error) {
			return nil, refused("ListReviewComments")
		},
		markPullRequestReadyFn: func(context.Context, string) error { return refused("MarkPullRequestReady") },
		pushExistingBranchFn: func(context.Context, string, string, string) error {
			return refused("PushExistingBranch")
		},
		readReviewStateFn: func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
			return forge.ReviewState{}, refused("ReadReviewState")
		},
		remoteBranchHeadSHAFn: func(context.Context, string, string) (string, error) {
			return "", refused("RemoteBranchHeadSHA")
		},
		replyToReviewCommentFn: func(context.Context, string, int64, string) error {
			return refused("ReplyToReviewComment")
		},
		retargetPullRequestBaseFn: func(context.Context, string, string) error {
			return refused("RetargetPullRequestBase")
		},
		roundResultDescendsFromHeadFn: func(string, string, string) (bool, error) {
			return false, refused("RoundResultDescendsFromHead")
		},
		undoMarkPullRequestReadyFn: func(context.Context, string) error { return refused("UndoMarkPullRequestReady") },
		runTicketFn: func(context.Context, []string, func(*run.Run)) error {
			return refused("RunTicket")
		},
	}
}

// stubPRReviewDeps is requestdrivertest.StubPRReviewHooks over this package's
// fake.
func stubPRReviewDeps(dp *fakeDeps, t *testing.T, state forge.ReviewState, readErr error) {
	t.Helper()
	requestdrivertest.StubPRReviewHooks(requestdrivertest.PrReviewHooks{
		ReadReviewState: &dp.readReviewStateFn, MarkPullRequestReady: &dp.markPullRequestReadyFn,
		UndoMarkPullRequestReady: &dp.undoMarkPullRequestReadyFn, PushExistingBranch: &dp.pushExistingBranchFn,
		RemoteBranchHeadSHA: &dp.remoteBranchHeadSHAFn, RoundResultDescendsFromHead: &dp.roundResultDescendsFromHeadFn,
		ReplyToReviewComment: &dp.replyToReviewCommentFn,
	}, t, state, readErr)
}
