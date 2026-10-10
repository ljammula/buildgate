package requestdrivertest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
)

// StubPRReviewTestFixture builds a minimal request in pr_review with one
// ticket whose PR is already open, plus that ticket's own spec file on
// disk (needed only by the corrective-round path, which reads
// ticket.SpecPath via ticketspec.ParseVerifyCommand) -- returns the
// request and dataDir.
// TestTicketRunResultSHA is StubPRReviewTestFixture's own ticket i's
// ResultSHA -- a package-level helper (not just a literal inline) so
// every test constructing its own forge.ReviewState{HeadSHA: ...} to
// exercise a "marked ready"/"approved" path can compute the exact value
// advancePRReadyOrApproved's own state.HeadSHA-vs-ResultSHA check (found
// via review, GitHub Codex App, PR #154 round 5) requires them to match,
// without duplicating the literal or risking it drifting from the
// fixture that sets it.
func TestTicketRunResultSHA(i int) string {
	return fmt.Sprintf("%040d", 900+i)
}

// TestLastPushedSHA is stubPRReviewDeps' own default forge.pushExistingBranch/
// forge.remoteBranchHeadSHA pairing's shared state -- see stubPRReviewDeps'
// own doc comment. Reset at the start of every stubPRReviewDeps call, so
// tests never leak state into one another despite this package's
// non-parallel convention.
var TestLastPushedSHA map[string]string

func StubPRReviewTestFixture(t *testing.T, ticketCount int) (*request.Request, string) {
	t.Helper()
	dataDir := t.TempDir()
	workspace := t.TempDir()
	tickets := make([]request.Ticket, 0, ticketCount)
	for i := 1; i <= ticketCount; i++ {
		specPath := filepath.Join(dataDir, fmt.Sprintf("ticket-%d.spec.md", i))
		if err := os.WriteFile(specPath, []byte("Verify-Command: make verify\n\n## Goal\n\nDo the thing.\n"), 0o600); err != nil {
			t.Fatalf("write ticket spec: %v", err)
		}
		runID := fmt.Sprintf("run-%d", i)
		tickets = append(tickets, request.Ticket{
			Index:    i,
			SpecPath: specPath,
			RunID:    runID,
			PRURL:    fmt.Sprintf("https://github.com/acme/widget/pull/%d", 100+i),
		})
		// runCorrectiveRound loads this record (run.Load(dataDir,
		// ticket.RunID)) to read its BaseSHA for -diff-base -- without it
		// on disk, every corrective-round test below would fail at that
		// load, not at whatever it's actually testing. Project: "widget"
		// (matching the request's own Project field below) so
		// release.ProjectOf resolves to the same project the recorded
		// decision just below is saved under.
		ticketRun := &run.Run{ID: runID, Project: "widget", BaseSHA: fmt.Sprintf("%040d", i), ResultSHA: TestTicketRunResultSHA(i)}
		if err := ticketRun.Save(dataDir); err != nil {
			t.Fatalf("save ticket run fixture: %v", err)
		}
		// advancePRReadyOrApproved's own release-decision gate (added
		// alongside the "check the decision before opening a PR" fix)
		// requires an Allowed decision on file before it will ever call
		// `gh pr ready` -- without this, every "marked ready" case in
		// TestAdvancePRReviewStateTable below would silently stop calling
		// it. Tests that specifically want a denied/missing decision seed
		// their own fixture instead of using this default.
		if err := release.SaveDecision(dataDir, release.Decision{RunID: runID, Project: "widget", Allowed: true, Evaluated: time.Now().UTC().Format(time.RFC3339)}); err != nil {
			t.Fatalf("save ticket run's allowed release decision fixture: %v", err)
		}
	}
	r := &request.Request{
		ID:          "req-1",
		Workspace:   workspace,
		Project:     "widget",
		State:       request.StatePRReview,
		TicketIndex: 1,
		TicketCount: ticketCount,
		Tickets:     tickets,
		SubmittedAt: time.Now().UTC().Format(time.RFC3339Nano),
		UpdatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
		EnteredAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save fixture request: %v", err)
	}
	return r, dataDir
}

// PrReviewHooks points at the fields of a Deps fake that StubPRReviewHooks
// sets: each test package's own fake has them under its own type.
type PrReviewHooks struct {
	ReadReviewState             *func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error)
	MarkPullRequestReady        *func(ctx context.Context, prURL string) error
	UndoMarkPullRequestReady    *func(ctx context.Context, prURL string) error
	PushExistingBranch          *func(ctx context.Context, workspaceDir string, sha string, branch string) error
	RemoteBranchHeadSHA         *func(ctx context.Context, workspaceDir string, branch string) (string, error)
	RoundResultDescendsFromHead *func(dir string, ancestor string, descendant string) (bool, error)
	ReplyToReviewComment        *func(ctx context.Context, prURL string, commentID int64, body string) error
}

// StubPRReviewHooks overrides every seam of the PR-review driver: the fields
// of the caller's Deps fake that h points at and the package-level ones, restoring each to its production value on test cleanup -- the
// same "swap and defer-restore" shape worker_config_test.go's own
// stubWorkerDoctorChecks helper uses.
func StubPRReviewHooks(h PrReviewHooks, t *testing.T, state forge.ReviewState, readErr error) {
	t.Helper()
	origRead := *h.ReadReviewState
	origRunner := requestdriver.PrReviewCorrectiveRunner
	origReady := *h.MarkPullRequestReady
	origUndoReady := *h.UndoMarkPullRequestReady
	origPush := *h.PushExistingBranch
	origRemoteHead := *h.RemoteBranchHeadSHA
	origDescendsFromHead := *h.RoundResultDescendsFromHead
	origReply := *h.ReplyToReviewComment
	origStartNext := requestdriver.ContinueAfterPRApproval
	t.Cleanup(func() {
		*h.ReadReviewState = origRead
		requestdriver.PrReviewCorrectiveRunner = origRunner
		*h.MarkPullRequestReady = origReady
		*h.UndoMarkPullRequestReady = origUndoReady
		*h.PushExistingBranch = origPush
		*h.RemoteBranchHeadSHA = origRemoteHead
		*h.RoundResultDescendsFromHead = origDescendsFromHead
		*h.ReplyToReviewComment = origReply
		requestdriver.ContinueAfterPRApproval = origStartNext
	})
	// Defaults to a no-op success: every case that doesn't specifically
	// exercise the post-ready-flip revert (decisionStillAllows still true
	// between the pre-flip check and forge.markPullRequestReady) never
	// reaches this seam at all, but stubbing it here rather than leaving
	// it at its production real-`gh`-binary value means a future test
	// that DOES reach it without overriding this default fails loudly
	// (a real `gh` binary invocation in a unit test) rather than
	// silently depending on one being installed.
	*h.UndoMarkPullRequestReady = func(ctx context.Context, prURL string) error { return nil }
	*h.ReadReviewState = func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
		return state, readErr
	}
	// TestLastPushedSHA/forge.remoteBranchHeadSHA/forge.roundResultDescendsFromHead
	// default to a consistent, always-succeeding trio for
	// pushAcceptedRoundAndReply's own pre-push branch/ancestry guards and
	// its post-push remote-verification read (see that function's own
	// doc comments, pr_review_driver.go): none of this package's fixture
	// workspaces are real git repositories, so these seams exist
	// precisely so a test never needs one just to reach an accepted
	// round's push. A test that specifically exercises one of these
	// guards overrides forge.roundResultDescendsFromHead or
	// forge.remoteBranchHeadSHA directly instead of using this default.
	TestLastPushedSHA = map[string]string{}
	*h.RemoteBranchHeadSHA = func(ctx context.Context, workspaceDir, branch string) (string, error) {
		if sha, ok := TestLastPushedSHA[branch]; ok {
			return sha, nil
		}
		return strings.Repeat("0", 40), nil
	}
	*h.RoundResultDescendsFromHead = func(dir, ancestor, descendant string) (bool, error) { return true, nil }
}

// AcceptAndReview makes ticket i's fixture run an accepted one whose
// code_review gate passed: the build the ready-to-merge bar wants behind a
// pull request's head.
func AcceptAndReview(t *testing.T, dataDir string, i int, gates ...run.GateResult) {
	t.Helper()
	loaded, err := run.Load(dataDir, fmt.Sprintf("run-%d", i))
	if err != nil {
		t.Fatal(err)
	}
	loaded.State = run.StateAccepted
	loaded.GateResults = gates
	if err := loaded.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}
