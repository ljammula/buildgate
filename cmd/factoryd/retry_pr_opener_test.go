package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/store"
)

// stubBranchTip overrides forge.branchTip for one test's
// duration, returning tip for every (workspaceDir, branch) pair -- a
// fake, not a real git binary/repo, since retryPullRequestOpener's own
// WorkspacePath fixtures here are bare t.TempDir()s.
func stubBranchTip(dp *deps, t *testing.T, tip string, err error) {
	t.Helper()
	old := fakeForgeOf(dp).branchTipFn
	fakeForgeOf(dp).branchTipFn = func(context.Context, string, string) (string, error) { return tip, err }
	t.Cleanup(func() { fakeForgeOf(dp).branchTipFn = old })
}

// acceptedRunFixture builds a run.Run that MergePolicyCheck will allow
// (the ReleasePolicy every "success path" test here shares), saved under
// dataDir. Factored out since each of the regression tests below needs
// a full, allow-able run record to reach the actual behavior under test.
func acceptedRunFixture(t *testing.T, dataDir, runID string) *run.Run {
	t.Helper()
	r := &run.Run{
		ID: runID, Ticket: "001", Project: "widgets",
		State: run.StateAccepted, BaseSHA: "base", ResultSHA: "result",
		Branch:                     "factoryd/" + runID,
		WorkspacePath:              t.TempDir(),
		ChangedFiles:               []string{"lib/app.go"},
		DependencyLockfilesTouched: []string{},
		DiffStat:                   &run.DiffStat{FilesChanged: 1, Insertions: 2},
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "full_suite_verify", Passed: true},
		},
		ReleasePolicy: runReleasePolicy(release.MergePolicy{
			MaxFilesChanged:                10,
			MaxInsertions:                  100,
			RollbackPlan:                   "revert the PR",
			RequiredGates:                  []string{"canonical_verify", "full_suite_verify"},
			AllowUnsandboxed:               true,
			AllowDependencyLockfileChanges: true,
		}),
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestRetryPullRequestOpenerOpensWithoutRebuilding is the success-path
// test: retryPullRequestOpener re-attempts only the push/PR-open for an
// already-accepted run, using its own recorded ReleasePolicy -- nothing
// here ever starts a build (there is no build-runner call in this
// function at all, unlike the request.Retry "rebuild" branch it replaces
// for this shape; internal/request's own TestRetryAcceptedNoPRReopensWithoutRebuilding
// covers that the state machine never reaches StateBuilding for this
// case).
func TestRetryPullRequestOpenerOpensWithoutRebuilding(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	acceptedRunFixture(t, dataDir, "run-1")
	stubBranchTip(dp, t, "result", nil) // matches ResultSHA -- the branch-tip check below passes

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/9"}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err != nil || outcome.WithheldReason != "" {
		t.Fatalf("outcome = %+v, want a clean success", outcome)
	}
	if outcome.PRURL != "https://github.com/acme/widgets/pull/9" {
		t.Errorf("PRURL = %q, want the opener's URL", outcome.PRURL)
	}
	if !opener.called {
		t.Error("opener was never called")
	}
	if opener.calledHeadSHA != "result" {
		t.Errorf("calledHeadSHA = %q, want the run's own ResultSHA pinned into the push", opener.calledHeadSHA)
	}

	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PullRequestURL != outcome.PRURL {
		t.Errorf("run's own PullRequestURL = %q, want %q persisted", reloaded.PullRequestURL, outcome.PRURL)
	}
}

// TestRetryPullRequestOpenerRefusesWhenReleaseDenies covers the other
// half: a run whose OWN recorded ReleasePolicy denies it (e.g. an
// unconfigured RollbackPlan) reports WithheldReason and never calls the
// opener -- no push/PR-create attempt at all for a denied run.
func TestRetryPullRequestOpenerRefusesWhenReleaseDenies(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "001", Project: "widgets",
		State: run.StateAccepted, Branch: "factoryd/run-1",
		WorkspacePath: t.TempDir(), ResultSHA: "result",
		ReleasePolicy: runReleasePolicy(release.MergePolicy{}), // RollbackPlan == "" denies unconditionally
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	stubBranchTip(dp, t, "result", nil)

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/9"}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.WithheldReason == "" {
		t.Fatal("WithheldReason is empty, want a denial reason")
	}
	if opener.called {
		t.Error("opener was called, want it never invoked for a denied release decision")
	}
}

// TestRetryPullRequestOpenerRefusesNonAcceptedRun guards against opening
// a PR for a run that isn't (or is no longer) accepted.
func TestRetryPullRequestOpenerRefusesNonAcceptedRun(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	r := &run.Run{ID: "run-1", State: run.StateQuarantined, Branch: "factoryd/run-1"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "not accepted") {
		t.Errorf("outcome.Err = %v, want a not-accepted error", outcome.Err)
	}
}

// TestRetryPullRequestOpenerIsIdempotentWhenAlreadyRecorded covers a run
// that already has PullRequestURL recorded (an earlier attempt's
// PR-open succeeded, but the request-side save then failed or crashed):
// it must be linked directly -- no gh call at all -- rather than trying
// to open a second PR.
func TestRetryPullRequestOpenerIsIdempotentWhenAlreadyRecorded(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "001", State: run.StateAccepted,
		Branch: "factoryd/run-1", WorkspacePath: t.TempDir(),
		PullRequestURL: "https://github.com/acme/widgets/pull/3",
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/999"}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err != nil || outcome.WithheldReason != "" {
		t.Fatalf("outcome = %+v, want a clean success", outcome)
	}
	if outcome.PRURL != "https://github.com/acme/widgets/pull/3" {
		t.Errorf("PRURL = %q, want the already-recorded URL, not a new one", outcome.PRURL)
	}
	if opener.called {
		t.Error("opener.OpenDraftPullRequest was called, want no gh call for an already-recorded PR")
	}
	if opener.verifyCalled {
		t.Error("opener.VerifyExistingPullRequest was called, want no gh call for an already-recorded PR")
	}
}

// TestRetryPullRequestOpenerLinksExistingPRWhenGHReportsAlreadyExists
// covers gh itself reporting "a pull request already exists" (a crash
// between THIS attempt's own successful gh pr create and factoryd
// recording its URL): the URL gh's own error text names must be
// recovered and VERIFIED (open, same-repository, headRefOid ==
// ResultSHA) before linking it -- not treated as a hard failure, but not
// trusted blindly either.
func TestRetryPullRequestOpenerLinksExistingPRWhenGHReportsAlreadyExists(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	acceptedRunFixture(t, dataDir, "run-1")
	stubBranchTip(dp, t, "result", nil)

	opener := &fakePullRequestOpener{
		err:              errors.New("gh pr create: exit status 1: GraphQL: A pull request already exists for acme:factoryd/run-1. (createPullRequest): https://github.com/acme/widgets/pull/42"),
		verifyState:      "OPEN",
		verifyHeadRefOid: "result",
	}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err != nil || outcome.WithheldReason != "" {
		t.Fatalf("outcome = %+v, want a clean success recovering the existing PR", outcome)
	}
	if outcome.PRURL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("PRURL = %q, want the recovered, verified PR", outcome.PRURL)
	}
	if !opener.verifyCalled || opener.verifyURL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("VerifyExistingPullRequest called=%v url=%q, want it called with the URL parsed from gh's own error", opener.verifyCalled, opener.verifyURL)
	}

	reloaded, err := run.Load(dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PullRequestURL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("run's own PullRequestURL = %q, want the recovered URL persisted", reloaded.PullRequestURL)
	}
	if len(reloaded.Notifications) != 0 {
		t.Errorf("Notifications = %d, want 0: linking an already-existing PR must not send a second \"accepted\" notification", len(reloaded.Notifications))
	}

	// M4-K1 regression: recoverExistingPullRequest used to call
	// fresh.Save directly, bypassing RecordEvent entirely. It now goes
	// through fresh.Persist instead.
	s, err := store.Open(run.EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	events, err := s.List(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Error("events = [], want at least one durable event recorded by recovering the existing PR")
	}
}

// TestRetryPullRequestOpenerRefusesRecoveredPRWithNoURLInError covers
// gh's "already exists" error carrying no parseable URL at all: nothing
// to verify or link, so the original error is reported rather than
// guessing.
func TestRetryPullRequestOpenerRefusesRecoveredPRWithNoURLInError(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	acceptedRunFixture(t, dataDir, "run-1")
	stubBranchTip(dp, t, "result", nil)

	opener := &fakePullRequestOpener{
		err: errors.New("gh pr create: exit status 1: GraphQL: A pull request already exists for acme:factoryd/run-1. (createPullRequest)"),
	}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err == nil {
		t.Fatal("outcome.Err is nil, want the original opener error since no URL could be recovered")
	}
	if opener.verifyCalled {
		t.Error("VerifyExistingPullRequest was called with no URL to verify")
	}
}

// TestRetryPullRequestOpenerRefusesRecoveredPRThatIsClosed covers the
// core case of another finding: a PR gh names in its "already exists"
// error that turns out to be closed must never be silently linked as a
// success.
func TestRetryPullRequestOpenerRefusesRecoveredPRThatIsClosed(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	acceptedRunFixture(t, dataDir, "run-1")
	stubBranchTip(dp, t, "result", nil)

	opener := &fakePullRequestOpener{
		err:              errors.New("already exists: https://github.com/acme/widgets/pull/42"),
		verifyState:      "CLOSED",
		verifyHeadRefOid: "result",
	}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "CLOSED") {
		t.Errorf("outcome.Err = %v, want a refusal naming the closed state", outcome.Err)
	}
}

// TestRetryPullRequestOpenerRefusesRecoveredPRThatIsCrossRepository
// covers a same-named branch on a fork -- gh's own "already exists" text
// names a branch, not a repository, so the URL it points to could belong
// to an entirely different repository.
func TestRetryPullRequestOpenerRefusesRecoveredPRThatIsCrossRepository(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	acceptedRunFixture(t, dataDir, "run-1")
	stubBranchTip(dp, t, "result", nil)

	opener := &fakePullRequestOpener{
		err:                     errors.New("already exists: https://github.com/acme/widgets/pull/42"),
		verifyState:             "OPEN",
		verifyHeadRefOid:        "result",
		verifyIsCrossRepository: true,
	}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "cross-repository") {
		t.Errorf("outcome.Err = %v, want a refusal naming cross-repository", outcome.Err)
	}
}

// TestRetryPullRequestOpenerRefusesRecoveredPRWithWrongHeadSHA covers
// gh's "already exists" naming an open, same-repository PR that just
// happens to point at a different commit than this run's own ResultSHA
// (e.g. a stale, unrelated PR against the same branch name).
func TestRetryPullRequestOpenerRefusesRecoveredPRWithWrongHeadSHA(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	acceptedRunFixture(t, dataDir, "run-1")
	stubBranchTip(dp, t, "result", nil)

	opener := &fakePullRequestOpener{
		err:              errors.New("already exists: https://github.com/acme/widgets/pull/42"),
		verifyState:      "OPEN",
		verifyHeadRefOid: "somethingelse",
	}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "somethingelse") || !strings.Contains(outcome.Err.Error(), "result") {
		t.Errorf("outcome.Err = %v, want a refusal naming both SHAs", outcome.Err)
	}
}

// TestRetryPullRequestOpenerRefusesWhenBranchTipMovedPastResultSHA is
// the regression test for an adversarial-review finding: the branch's
// own current tip is not what the release decision/evidence were
// computed from (a corrective round, a maintainer's own push, or a
// reused/reclaimed worktree moved it) -- retry must refuse to push it,
// not silently land unevaluated code on the PR.
func TestRetryPullRequestOpenerRefusesWhenBranchTipMovedPastResultSHA(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "001", State: run.StateAccepted,
		Branch: "factoryd/run-1", WorkspacePath: t.TempDir(),
		ResultSHA: "deadbeef",
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	stubBranchTip(dp, t, "somethingelse", nil) // the branch ref has moved on

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/9"}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.Err == nil {
		t.Fatal("outcome.Err is nil, want a refusal naming the branch/ResultSHA mismatch")
	}
	if !strings.Contains(outcome.Err.Error(), "deadbeef") || !strings.Contains(outcome.Err.Error(), "somethingelse") {
		t.Errorf("outcome.Err = %v, want it to name both the recorded ResultSHA and the branch's real tip", outcome.Err)
	}
	if opener.called {
		t.Error("opener was called, want no push/PR-open attempt for a moved branch")
	}
}

// TestRetryPullRequestOpenerRefusesEmptyResultSHA is the round-2
// regression test for that same adversarial-review finding: an empty
// ResultSHA used to silently skip the branch-tip check entirely rather
// than refusing -- must now report
// WithheldReason (rebuild-recoverable, subject to request.Retry's own
// same-ticket rule) instead of attempting a push nothing can verify.
func TestRetryPullRequestOpenerRefusesEmptyResultSHA(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	r := &run.Run{
		ID: "run-1", Ticket: "001", State: run.StateAccepted,
		Branch: "factoryd/run-1", WorkspacePath: t.TempDir(),
		// ResultSHA deliberately empty.
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	opener := &fakePullRequestOpener{url: "https://github.com/acme/widgets/pull/9"}
	old := dp.forge.pullRequestOpener()
	fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return opener }
	defer func() { fakeForgeOf(dp).pullRequestOpenerFn = func() forge.PullRequestOpener { return old } }()

	outcome := retryPullRequestOpener(dp, dataDir, "run-1")
	if outcome.WithheldReason == "" {
		t.Fatalf("outcome = %+v, want WithheldReason for a run with no recorded ResultSHA", outcome)
	}
	if opener.called {
		t.Error("opener was called, want no push/PR-open attempt with no ResultSHA to verify against")
	}
}
