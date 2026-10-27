package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

// An oracle-drafting job that returned the resolver's error (no
// OracleDraftJobFailure: it never launched) is a job failure with the cause,
// not a failed draft.
func TestAnOracleDraftingJobWithNoUsableRouteCredentialWaitsInResumeReview(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.OracleStageFixture(t, true)
	runner, _ := requestdrivertest.StubOracleDraftRunner(request.OracleDraft{}, errExpiredCredential)
	if err := driveOracleDrafting(t, dp, context.Background(), dataDir, runner); err != nil {
		t.Fatal(err)
	}
	got := wantRouteCredentialWait(t, dataDir, id, request.StateOracleDrafting)
	if got.OracleDraft != nil {
		t.Errorf("OracleDraft = %+v, want none", got.OracleDraft)
	}
}
