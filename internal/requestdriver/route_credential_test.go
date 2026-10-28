package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/unresolved"
)

// errExpiredCredential is what the worker's credential resolver returns for
// a chatgpt-codex token inside its expiry margin, as route selection wraps
// it when every route failed on its credential.
var errExpiredCredential = fmt.Errorf("plan drafting: %w", sandbox.RouteCredentialError(errors.New("modelrole: no configured route is usable for this role's model: roles.planning.model \"gpt-6-luna\", routes tried [{codex route's credential did not resolve}]")))

// wantRouteCredentialWait checks that the request waits in resume_review
// from `from` with unresolved.RouteCredential, its clause, the job's detail,
// and the one reminder.
func wantRouteCredentialWait(t *testing.T, dataDir, id string, from request.State) *request.Request {
	t.Helper()
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.RouteCredential || got.Resume.FromState != from {
		t.Fatalf("state %s resume %+v (error %q), want resume_review from %s with cause route_credential", got.State, got.Resume, got.Error, from)
	}
	if !strings.Contains(got.Error, unresolved.RouteCredential.What(string(from))) || !strings.Contains(got.Error, "codex login") {
		t.Errorf("Error = %q, want the cause's clause", got.Error)
	}
	if got.NotifyCount != 1 {
		t.Errorf("NotifyCount = %d, want the one reminder", got.NotifyCount)
	}
	return got
}

// A ticket build whose run halted because its route credential could not be
// used (the workflow's RouteCredential failure, recorded as the run's
// HaltReasonCode) is no verdict on the work: the request waits in
// resume_review with the cause and the kept run, not halted.
func TestATicketRunHaltedOnItsRouteCredentialWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(_ context.Context, args []string, onReady func(*run.Run)) error {
		rid := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: rid, State: run.StateReady})
		saved := &run.Run{ID: rid, State: run.StateHalted, KeptForResume: true, RequestID: id,
			HaltReasonCode: string(unresolved.RouteCredential), HaltError: "workflow execution error (type: RunWorkflow, ...): codex access token expires within the required 6h0m0s margin"}
		if err := saved.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return errors.New("temporal workflow did not complete")
	}
	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	got := wantRouteCredentialWait(t, dataDir, id, request.StateBuilding)
	if got.Resume.LostRunID != requestdrivertest.TicketRunID(id, 1) {
		t.Errorf("LostRunID = %q, want the kept run", got.Resume.LostRunID)
	}
}

// A ticket build refused before its run existed, because route selection
// found no usable credential, waits in resume_review too, with nothing kept.
func TestATicketBuildThatCouldNotStartForItsRouteCredentialWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	runner := func(context.Context, []string, func(*run.Run)) error {
		return fmt.Errorf("roles.execution: %w", errExpiredCredential)
	}
	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, runner)
	got := wantRouteCredentialWait(t, dataDir, id, request.StateBuilding)
	if got.Resume.LostRunID != "" || !strings.Contains(got.Error, "resume -from scratch") {
		t.Errorf("Resume %+v, Error %q: want no kept run and a rebuild offered", got.Resume, got.Error)
	}

	// Any other start failure still halts.
	dp = newFakeDeps(t)
	dataDir, id = requestdrivertest.BuildingFixture(dp, t, 1)
	requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{}, func(context.Context, []string, func(*run.Run)) error {
		return errors.New("roles.execution: modelrole: roles.execution.model is not configured")
	})
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StateHalted {
		t.Errorf("a configuration fault: state %s, want halted", got.State)
	}
}

func TestASpecDraftingJobWithNoUsableRouteCredentialWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	runner, _ := stubSpecDraftRunner("", nil, errExpiredCredential)
	if err := requestdriver.AdvanceSpecDrafting(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantRouteCredentialWait(t, dataDir, "req-1", request.StateSpecDrafting)
}

func TestAPlanDraftingJobWithNoUsableRouteCredentialWaitsInResumeReview(t *testing.T) {
	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	runner := func(context.Context, string, *request.Request, requestdriver.WorkerConfig, string) ([]requestdriver.DraftedTicket, *request.PlanEvidence, error) {
		return nil, nil, errExpiredCredential
	}
	if err := requestdriver.AdvancePlanning(newFakeDeps(t), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantRouteCredentialWait(t, dataDir, id, request.StatePlanning)
}

// The read-back otherwise never stops a request; one whose route credential
// could not be used read nothing that a fresh credential would not, so the
// request waits for it instead of reaching spec_review without its read-back.
func TestAReadBackWithNoUsableRouteCredentialWaitsInResumeReview(t *testing.T) {
	dataDir := t.TempDir()
	r := requestdrivertest.HandedOverRequest(t, dataDir, requestdrivertest.CanonicalValidSpec)
	runner := func(context.Context, string, *request.Request, string, requestdriver.WorkerConfig) (*request.ReadBack, *request.SpecEvidence, error) {
		return nil, nil, errExpiredCredential
	}
	if err := requestdriver.AdvanceSpecDrafting(newFakeDeps(t), context.Background(), dataDir, r, readBackConfig(runner), requestdrivertest.FailingSpecDraftRunner(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	wantRouteCredentialWait(t, dataDir, r.ID, request.StateSpecDrafting)
}

// errRoundCredential is a round's runner refused before its run record
// existed: runMainWithReady resolved the route's credential first.
var errRoundCredential = fmt.Errorf("roles.execution: %w", sandbox.RouteCredentialError(errors.New("codex access token expires within the required 6h0m0s margin")))

// A PR-review round whose credential was refused before its run existed
// used no max_review_rounds slot and waits for a human: before, it started
// again on every poll, with a notification each time.
func TestAPRReviewRoundRefusedForItsRouteCredentialWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	ticket := &r.Tickets[0]
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}
	replies := 0
	dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { replies++; return nil }
	calls := 0
	requestdriver.PrReviewCorrectiveRunner = func(context.Context, []string, func(*run.Run)) error {
		calls++
		return errRoundCredential
	}
	cfg := requestdriver.WorkerConfig{MaxReviewRounds: 1}
	if err := requestdriver.RunCorrectiveRound(dp, context.Background(), dataDir, r, ticket, threads, cfg, time.Now()); err != nil {
		t.Fatalf("RunCorrectiveRound: %v", err)
	}
	if r.State != request.StateResumeReview || r.Resume == nil || r.Resume.Cause != unresolved.RouteCredential || r.Resume.FromState != request.StatePRReview {
		t.Fatalf("state %s resume %+v (%s), want resume_review from pr_review with route_credential", r.State, r.Resume, r.Error)
	}
	if len(ticket.Rounds) != 1 || ticket.Rounds[0].Counted() || ticket.Rounds[0].Unresolved != unresolved.RouteCredential {
		t.Fatalf("Rounds = %+v, want one uncounted round carrying the cause", ticket.Rounds)
	}
	if calls != 1 || replies != 0 {
		t.Errorf("runner calls %d, replies %d: want one launch and no reply", calls, replies)
	}
}

// A corrective round of a quarantined ticket build, refused for its
// credential before its run existed, is no quarantine: before, it
// quarantined the request as a start failure.
func TestACorrectiveRoundRefusedForItsRouteCredentialWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	buildRunner := checkQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "lint")
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	calls := 0
	requestdriver.ReviewCorrectiveRunner = func(context.Context, []string, func(*run.Run)) error {
		calls++
		return errRoundCredential
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.RouteCredential || got.Resume.FromState != request.StateBuilding {
		t.Fatalf("state %s resume %+v (%s), want resume_review from building with route_credential", got.State, got.Resume, got.Error)
	}
	if rounds := got.Tickets[0].Rounds; len(rounds) != 1 || rounds[0].Counted() || rounds[0].Unresolved != unresolved.RouteCredential || got.QuarantineCheck != "" {
		t.Fatalf("Rounds %+v, check %q: want one uncounted round with the cause and no quarantine", rounds, got.QuarantineCheck)
	}
	if calls != 1 {
		t.Errorf("corrective runner calls = %d, want 1", calls)
	}
}

// A restack whose replay run was refused for its credential waits for a
// human at once: before, it was tried every poll and halted after three.
func TestARestackRefusedForItsRouteCredentialWaitsInResumeReview(t *testing.T) {
	f := newRestackFixture(t, 2)
	f.dp.runTicketFn = func(context.Context, []string, func(*run.Run)) error {
		f.event("run (credential refused)")
		return errRoundCredential
	}
	f.pass()
	if f.r.State != request.StateResumeReview || f.r.Resume == nil || f.r.Resume.Cause != unresolved.RouteCredential {
		t.Fatalf("state %s resume %+v (%s), want resume_review with route_credential", f.r.State, f.r.Resume, f.r.Error)
	}
	if rounds := f.r.Tickets[1].Rounds; len(rounds) == 0 || rounds[len(rounds)-1].Unresolved != unresolved.RouteCredential || rounds[len(rounds)-1].Counted() {
		t.Fatalf("Rounds = %+v, want the restack round uncounted with the cause", rounds)
	}
}

// The operator's resume of a kept build that is refused for its credential
// before the run starts keeps that build: the request waits again with the
// same kept run, and `resume` continues it once the credential is fresh.
func TestAResumeOfAKeptBuildRefusedForItsRouteCredentialKeepsTheBuild(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
	gate := requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}
	got := requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, gate, func(context.Context, []string, func(*run.Run)) error {
		return errRoundCredential
	})
	if got.State != request.StateResumeReview || got.Resume == nil || got.Resume.Cause != unresolved.RouteCredential || got.Resume.LostRunID != "lost-run" {
		t.Fatalf("state %s resume %+v (%s), want resume_review with route_credential and the kept run", got.State, got.Resume, got.Error)
	}
	if !strings.Contains(got.Error, "factoryd resume "+id+"` continues it") {
		t.Errorf("Error = %q, want the resume of the kept build offered", got.Error)
	}
	if rec, err := run.Load(dataDir, "lost-run"); err != nil || !rec.KeptForResume {
		t.Fatalf("kept run: %+v (%v), want still kept", rec, err)
	}
	if _, err := got.ResumeDecide(request.ResumeRound, "alice", time.Now()); err != nil {
		t.Fatalf("resume after the refusal: %v", err)
	}
}
