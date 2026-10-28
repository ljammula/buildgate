package requestdriver_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"buildgate/internal/consolelink"
	"buildgate/internal/forge"
	"buildgate/internal/notify"
	"buildgate/internal/request"
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
	readJobLogFn                  func(ctx context.Context, prURL string, jobID int64) (string, error)
	readReviewStateFn             func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error)
	remoteBranchHeadSHAFn         func(ctx context.Context, workspaceDir string, branch string) (string, error)
	replyToReviewCommentFn        func(ctx context.Context, prURL string, commentID int64, body string) error
	retargetPullRequestBaseFn     func(ctx context.Context, prURL string, base string) error
	roundResultDescendsFromHeadFn func(dir string, ancestor string, descendant string) (bool, error)
	undoMarkPullRequestReadyFn    func(ctx context.Context, prURL string) error
	createRefFn                   func(dir string, ref string, sha string) error
	createBranchAtFn              func(dir string, branch string, sha string) error
	pruneRoundRefsFn              func(dir string, id string, keepPrefixes []string) error
	runTicketFn                   func(ctx context.Context, args []string, onReady func(*run.Run)) error
	saveReviewEntryFn             func(dataDir string, r *request.Request) error
	fetchBranchCommitFn           func(ctx context.Context, dir, branch, sha string) error
	revListRangeFn                func(dir, from, to string) ([]string, []string, error)
	patchIDFn                     func(dir, sha string) (string, error)
	replayCommitsFn               func(ctx context.Context, dir, worktreePath, branch, onto string, commits []string) ([]string, error)
	pushWithLeaseFn               func(ctx context.Context, dir, sha, branch, expected string) error
	moveBranchFn                  func(dir, branch, sha string) error
	hostGitVersionFn              func(ctx context.Context) (string, error)
	restoreBranchFn               func(dir, branch, sha string) (bool, error)
	commentOnPullRequestFn        func(ctx context.Context, prURL, body string) error
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
func (f *fakeDeps) CreateRef(dir string, ref string, sha string) error {
	return f.createRefFn(dir, ref, sha)
}
func (f *fakeDeps) PruneRoundRefs(dir string, id string, keepPrefixes []string) error {
	return f.pruneRoundRefsFn(dir, id, keepPrefixes)
}
func (f *fakeDeps) CreateBranchAt(dir string, branch string, sha string) error {
	return f.createBranchAtFn(dir, branch, sha)
}
func (f *fakeDeps) RunTicket(ctx context.Context, args []string, onReady func(*run.Run)) error {
	return f.runTicketFn(ctx, args, onReady)
}

func (f *fakeDeps) SaveReviewEntry(dataDir string, r *request.Request) error {
	return f.saveReviewEntryFn(dataDir, r)
}

// afterReviewSave makes the save of a request entering a review state run
// after right after the record is written: the point where a test lands an
// operator's write before the reminder.
func (f *fakeDeps) afterReviewSave(after func(dataDir, id string)) {
	f.saveReviewEntryFn = func(dataDir string, r *request.Request) error {
		if err := r.Save(dataDir); err != nil {
			return err
		}
		after(dataDir, r.ID)
		return nil
	}
}
func (f *fakeDeps) FetchBranchCommit(ctx context.Context, dir, branch, sha string) error {
	return f.fetchBranchCommitFn(ctx, dir, branch, sha)
}
func (f *fakeDeps) RevListRange(dir, from, to string) ([]string, []string, error) {
	return f.revListRangeFn(dir, from, to)
}
func (f *fakeDeps) PatchID(dir, sha string) (string, error) { return f.patchIDFn(dir, sha) }
func (f *fakeDeps) ReplayCommits(ctx context.Context, dir, worktreePath, branch, onto string, commits []string) ([]string, error) {
	return f.replayCommitsFn(ctx, dir, worktreePath, branch, onto, commits)
}
func (f *fakeDeps) PushWithLease(ctx context.Context, dir, sha, branch, expected string) error {
	return f.pushWithLeaseFn(ctx, dir, sha, branch, expected)
}
func (f *fakeDeps) MoveBranch(dir, branch, sha string) error { return f.moveBranchFn(dir, branch, sha) }
func (f *fakeDeps) RestoreBranch(dir, branch, sha string) (bool, error) {
	return f.restoreBranchFn(dir, branch, sha)
}
func (f *fakeDeps) CommentOnPullRequest(ctx context.Context, prURL, body string) error {
	return f.commentOnPullRequestFn(ctx, prURL, body)
}
func (f *fakeDeps) ReadJobLog(ctx context.Context, prURL string, jobID int64) (string, error) {
	return f.readJobLogFn(ctx, prURL, jobID)
}
func (f *fakeDeps) HostGitVersion(ctx context.Context) (string, error) {
	return f.hostGitVersionFn(ctx)
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
		createRefFn:                func(string, string, string) error { return refused("CreateRef") },
		createBranchAtFn:           func(string, string, string) error { return refused("CreateBranchAt") },
		pruneRoundRefsFn:           func(string, string, []string) error { return refused("PruneRoundRefs") },
		runTicketFn: func(context.Context, []string, func(*run.Run)) error {
			return refused("RunTicket")
		},
		saveReviewEntryFn:   func(dataDir string, r *request.Request) error { return r.Save(dataDir) },
		fetchBranchCommitFn: func(context.Context, string, string, string) error { return refused("FetchBranchCommit") },
		revListRangeFn: func(string, string, string) ([]string, []string, error) {
			return nil, nil, refused("RevListRange")
		},
		patchIDFn: func(string, string) (string, error) { return "", refused("PatchID") },
		replayCommitsFn: func(context.Context, string, string, string, string, []string) ([]string, error) {
			return nil, refused("ReplayCommits")
		},
		pushWithLeaseFn: func(context.Context, string, string, string, string) error { return refused("PushWithLease") },
		moveBranchFn:    func(string, string, string) error { return refused("MoveBranch") },
		restoreBranchFn: func(string, string, string) (bool, error) { return false, refused("RestoreBranch") },
		commentOnPullRequestFn: func(context.Context, string, string) error {
			return refused("CommentOnPullRequest")
		},
		readJobLogFn: func(context.Context, string, int64) (string, error) {
			return "", refused("ReadJobLog")
		},
		hostGitVersionFn: func(context.Context) (string, error) {
			return "", refused("HostGitVersion")
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
