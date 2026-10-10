package requestdriver_test

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

// stubSpecDraftRunner returns a fixed (specMD, evidence, err) for every
// call, recording how many times (and with which request) it was
// invoked -- the same "inject a stub instead of a real subprocess" shape
// worker_config_test.go's own stub ticketRunner uses for the worker.
func stubSpecDraftRunner(specMD string, evidence *request.SpecEvidence, err error) (requestdriver.SpecDraftRunner, *int) {
	calls := 0
	runner := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		calls++
		return specMD, evidence, err
	}
	return runner, &calls
}

// failingSpecDraftRunner fails the test outright if ever called -- used
// where driveRequests must not touch the spec-drafting job at all (e.g. a
// request already past spec_drafting, or the pure submitted->
// spec_drafting move which needs no job).
func failingSpecDraftRunner(t *testing.T) requestdriver.SpecDraftRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		t.Fatal("specDraftRunner must not be called")
		return "", nil, nil
	}
}

// failingPlanTicketsRunner is failingSpecDraftRunner's own sibling for
// the plan-drafting job -- used everywhere driveRequests must not touch
// planning at all.
func failingPlanTicketsRunner(t *testing.T) requestdriver.PlanTicketsRunner {
	return func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig, verifyCommand string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		t.Fatal("planTicketsRunner must not be called")
		return nil, nil, nil
	}
}

// failingOracleDraftRunner is failingSpecDraftRunner's sibling for the oracle
// drafting job -- used everywhere driveRequests must not touch
// oracle_drafting (every pre-existing test: no request there sets
// -draft-oracles).
func failingOracleDraftRunner(t *testing.T) requestdriver.OracleDraftRunner {
	return func(ctx context.Context, in requestdriver.OracleDraftInput) (request.OracleDraft, error) {
		t.Fatal("oracleDraftRunner must not be called")
		return request.OracleDraft{}, nil
	}
}

// failingBuildRunner is failingSpecDraftRunner's own sibling for a
// ticket build (ticketRunner) -- used everywhere driveRequests must not
// touch building at all.
func failingBuildRunner(t *testing.T) requestdriver.TicketRunner {
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		t.Fatal("ticketRunner (ticket build) must not be called")
		return nil
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

func containsArg(args []string, name, value string) bool {
	for i, a := range args {
		if a == name && i+1 < len(args) && args[i+1] == value {
			return true
		}
	}
	return false
}

// stubPRReviewTestFixture builds a minimal request in pr_review with one
// ticket whose PR is already open, plus that ticket's own spec file on
// disk (needed only by the corrective-round path, which reads
// ticket.SpecPath via ticketspec.ParseVerifyCommand) -- returns the
// request and dataDir.
// testTicketRunResultSHA is stubPRReviewTestFixture's own ticket i's
// ResultSHA -- a package-level helper (not just a literal inline) so
// every test constructing its own forge.ReviewState{HeadSHA: ...} to
// exercise a "marked ready"/"approved" path can compute the exact value
// advancePRReadyOrApproved's own state.HeadSHA-vs-ResultSHA check (found
// via review, GitHub Codex App, PR #154 round 5) requires them to match,
// without duplicating the literal or risking it drifting from the
// fixture that sets it.
func testTicketRunResultSHA(i int) string {
	return fmt.Sprintf("%040d", 900+i)
}

// testLastPushedSHA is stubPRReviewDeps' own default forge.pushExistingBranch/
// forge.remoteBranchHeadSHA pairing's shared state -- see stubPRReviewDeps'
// own doc comment. Reset at the start of every stubPRReviewDeps call, so
// tests never leak state into one another despite this package's
// non-parallel convention.
var testLastPushedSHA map[string]string

func stubPRReviewTestFixture(t *testing.T, ticketCount int) (*request.Request, string) {
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
		ticketRun := &run.Run{ID: runID, Project: "widget", BaseSHA: fmt.Sprintf("%040d", i), ResultSHA: testTicketRunResultSHA(i)}
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

// stubPRReviewDeps overrides every package-level seam pr_review_driver.go
// exposes, restoring each to its production value on test cleanup -- the
// same "swap and defer-restore" shape worker_config_test.go's own
// stubWorkerDoctorChecks helper uses.
func stubPRReviewDeps(dp *fakeDeps, t *testing.T, state forge.ReviewState, readErr error) {
	t.Helper()
	origRead := dp.readReviewStateFn
	origRunner := requestdriver.PrReviewCorrectiveRunner
	origReady := dp.markPullRequestReadyFn
	origUndoReady := dp.undoMarkPullRequestReadyFn
	origPush := dp.pushExistingBranchFn
	origRemoteHead := dp.remoteBranchHeadSHAFn
	origDescendsFromHead := dp.roundResultDescendsFromHeadFn
	origReply := dp.replyToReviewCommentFn
	origStartNext := requestdriver.ContinueAfterPRApproval
	t.Cleanup(func() {
		dp.readReviewStateFn = origRead
		requestdriver.PrReviewCorrectiveRunner = origRunner
		dp.markPullRequestReadyFn = origReady
		dp.undoMarkPullRequestReadyFn = origUndoReady
		dp.pushExistingBranchFn = origPush
		dp.remoteBranchHeadSHAFn = origRemoteHead
		dp.roundResultDescendsFromHeadFn = origDescendsFromHead
		dp.replyToReviewCommentFn = origReply
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
	dp.undoMarkPullRequestReadyFn = func(ctx context.Context, prURL string) error { return nil }
	dp.readReviewStateFn = func(ctx context.Context, prURL string, policy forge.AuthorPolicy) (forge.ReviewState, error) {
		return state, readErr
	}
	// testLastPushedSHA/forge.remoteBranchHeadSHA/forge.roundResultDescendsFromHead
	// default to a consistent, always-succeeding trio for
	// pushAcceptedRoundAndReply's own pre-push branch/ancestry guards and
	// its post-push remote-verification read (see that function's own
	// doc comments, pr_review_driver.go): none of this package's fixture
	// workspaces are real git repositories, so these seams exist
	// precisely so a test never needs one just to reach an accepted
	// round's push. A test that specifically exercises one of these
	// guards overrides forge.roundResultDescendsFromHead or
	// forge.remoteBranchHeadSHA directly instead of using this default.
	testLastPushedSHA = map[string]string{}
	dp.remoteBranchHeadSHAFn = func(ctx context.Context, workspaceDir, branch string) (string, error) {
		if sha, ok := testLastPushedSHA[branch]; ok {
			return sha, nil
		}
		return strings.Repeat("0", 40), nil
	}
	dp.roundResultDescendsFromHeadFn = func(dir, ancestor, descendant string) (bool, error) { return true, nil }
}

// acceptAndReview makes ticket i's fixture run an accepted one whose
// code_review gate passed: the build the ready-to-merge bar wants behind a
// pull request's head.
func acceptAndReview(t *testing.T, dataDir string, i int, gates ...run.GateResult) {
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

// argValue returns the value following flag in args, or "" if flag is
// absent or has no following value.
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// hasFlag reports whether the bare flag (a boolean flag with no value,
// e.g. -open-pull-request) is present in args.
func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
