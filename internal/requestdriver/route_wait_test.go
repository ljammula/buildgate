package requestdriver_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/unresolved"
)

// routeWaitDeps is a fake whose RouteWait answers reason for every need and
// records the needs it was asked about.
func routeWaitDeps(t *testing.T, reason *string, asked *[]requestdriver.RouteNeed) *fakeDeps {
	dp := newFakeDeps(t)
	dp.routeWaitFn = func(_ string, _ sessionconfig.Settings, need requestdriver.RouteNeed) string {
		*asked = append(*asked, need)
		return *reason
	}
	return dp
}

// A spec drafting job whose route cannot be used does not launch: the
// request stays in spec_drafting with the route wait, and the next pass with
// a usable route ends the wait and launches the job.
func TestASpecDraftingJobWaitsForItsRouteThenLaunches(t *testing.T) {
	dataDir := savedSpecDraftingRequest(t)
	reason, asked := "waiting for roles.planning's model route: route codex: expired", []requestdriver.RouteNeed{}
	dp := routeWaitDeps(t, &reason, &asked)
	launches := 0
	runner := func(context.Context, string, *request.Request, requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		launches++
		return "", nil, errors.New("stop after the launch")
	}
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if err := requestdriver.AdvanceSpecDrafting(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if launches != 0 || got.State != request.StateSpecDrafting || got.RouteWait == nil || got.RouteWait.On != reason {
		t.Fatalf("launches %d, state %s, RouteWait %+v; want no launch and the wait recorded in spec_drafting", launches, got.State, got.RouteWait)
	}
	if len(asked) != 1 || asked[0].Role != "planning" {
		t.Fatalf("asked %+v, want the planning role", asked)
	}

	reason = ""
	_ = requestdriver.AdvanceSpecDrafting(dp, context.Background(), dataDir, got, requestdriver.WorkerConfig{}, runner, time.Now())
	if launches != 1 {
		t.Fatalf("launches = %d once the route was usable, want 1", launches)
	}
	if got = requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.RouteWait != nil {
		t.Fatalf("RouteWait = %+v after the launch, want it cleared", got.RouteWait)
	}
}

// A handed-over spec launches no drafting model, so it never waits for a
// route: it reaches spec_review even when no route is usable.
func TestAHandedOverSpecNeverWaitsForARoute(t *testing.T) {
	dataDir := t.TempDir()
	requestdrivertest.HandedOverRequest(t, dataDir, idlessSpec)
	reason, asked := "no route", []requestdriver.RouteNeed{}
	dp := routeWaitDeps(t, &reason, &asked)
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.State != request.StateSpecReview || got.RouteWait != nil || len(asked) != 0 {
		t.Fatalf("state %s, RouteWait %+v, asked %+v; want spec_review with no route asked", got.State, got.RouteWait, asked)
	}
}

// Planning that writes its one ticket without a model never waits for a
// route; a planning job that launches one does.
func TestOnlyAPlanningJobWaitsForItsRoute(t *testing.T) {
	reason := "no route"
	failing := requestdrivertest.FailingPlanTicketsRunner(t)

	dataDir, id := requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	asked := []requestdriver.RouteNeed{}
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvancePlanning(routeWaitDeps(t, &reason, &asked), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, failing, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State == request.StatePlanning || got.RouteWait != nil || len(asked) != 0 {
		t.Fatalf("model-free planning: state %s, RouteWait %+v, asked %+v; want it planned with no route asked", got.State, got.RouteWait, asked)
	}

	dataDir, id = requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	usePlanningJob(t, dataDir, id)
	asked = nil
	r = requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvancePlanning(routeWaitDeps(t, &reason, &asked), context.Background(), dataDir, r, requestdriver.WorkerConfig{}, failing, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StatePlanning || got.RouteWait == nil || len(asked) != 1 {
		t.Fatalf("planning job: state %s, RouteWait %+v, asked %+v; want it waiting in planning", got.State, got.RouteWait, asked)
	}
}

// A ticket build waits before anything of it starts, asking for the
// execution role with the request's picks and for the configured review role.
func TestATicketBuildWaitsForItsRoutesBeforeStarting(t *testing.T) {
	reason, asked := "", []requestdriver.RouteNeed{}
	dp := routeWaitDeps(t, &reason, &asked)
	// The fixture plans through the same fake: the route is usable until the
	// build is about to start.
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	reason, asked = "no route", nil
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.Models = map[string]string{"execution": "luna"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{Settings: sessionconfig.Settings{Roles: &sessionconfig.Roles{
		Execution: &sessionconfig.RoleConfig{Model: "luna"}, Review: &sessionconfig.RoleConfig{Model: "luna"},
	}}}
	started := false
	runner := func(context.Context, []string, func(*run.Run)) error { started = true; return errors.New("stop") }
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, requestdrivertest.LoadRequest(t, dataDir, id), cfg, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := requestdrivertest.LoadRequest(t, dataDir, id)
	if started || got.State != request.StateBuilding || got.RouteWait == nil {
		t.Fatalf("started %v, state %s (%s), RouteWait %+v; want no build and the wait in building", started, got.State, got.Error, got.RouteWait)
	}
	if len(asked) != 1 || asked[0] != (requestdriver.RouteNeed{Role: "execution", Model: "luna"}) {
		t.Fatalf("asked %+v, want execution with the request's model first (the first role that waits ends the check)", asked)
	}
	reason, asked = "", nil
	_ = requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, requestdrivertest.LoadRequest(t, dataDir, id), cfg, runner, time.Now())
	if !started || len(asked) != 2 || asked[1].Role != "review" {
		t.Fatalf("started %v, asked %+v; want the build started after both roles were checked", started, asked)
	}
}

// A route wait reminds at the reminder interval, as a review gate does, and
// starting or ending it never resets the reminders of a stacked wait that is
// still running.
func TestARouteWaitRemindsAndLeavesAStackedWaitsRemindersAlone(t *testing.T) {
	dataDir := savedSpecDraftingRequest(t)
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	start := time.Now().Add(-2 * time.Hour)
	if err := requestdriver.RecordRouteWait(newFakeDeps(t), dataDir, r, "waiting for roles.planning's model route", time.Hour, start); err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.RecordRouteWait(newFakeDeps(t), dataDir, r, "waiting for roles.planning's model route", time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.NotifyCount != 1 {
		t.Fatalf("NotifyCount = %d after the wait outlasted the interval, want 1", got.NotifyCount)
	}

	r = requestdrivertest.LoadRequest(t, dataDir, "req-1")
	r.StackedWait = &request.StepWait{Since: start.UTC().Format(time.RFC3339Nano), On: "ticket 2/2 waits"}
	notified := r.LastNotifiedAt
	if err := requestdriver.EndRouteWait(dataDir, r); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, "req-1"); got.RouteWait != nil || got.LastNotifiedAt != notified {
		t.Fatalf("after ending the route wait: RouteWait %+v, LastNotifiedAt %q; want it gone and the stacked wait's reminder time %q kept", got.RouteWait, got.LastNotifiedAt, notified)
	}
}

// A handed-over plan reads its tickets and launches no model, so it never
// waits for a route.
func TestAHandedOverPlanNeverWaitsForARoute(t *testing.T) {
	reason, asked := "no route", []requestdriver.RouteNeed{}
	dp := routeWaitDeps(t, &reason, &asked)
	ticket := requestdrivertest.ValidBrownfieldTicket(requestdrivertest.HandoverVerify, 1, 2)
	dataDir, id := requestdrivertest.HandedOverPlanFixture(t, map[string]string{"001.spec.md": ticket})
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatal(err)
	}
	if got := requestdrivertest.LoadRequest(t, dataDir, id); got.State != request.StatePlanReview || got.RouteWait != nil || len(asked) != 0 {
		t.Fatalf("state %s, RouteWait %+v, asked %+v; want plan_review with no route asked", got.State, got.RouteWait, asked)
	}
}

// A redraft in the same pass (a drafted spec refused for its criteria) is not
// held: the route was checked before the pass's first launch, and a wait
// there would lose the pass's attempt count.
func TestASpecRedraftInTheSamePassIsNotHeld(t *testing.T) {
	dataDir := savedSpecDraftingRequest(t)
	reason, asked := "", []requestdriver.RouteNeed{}
	dp := routeWaitDeps(t, &reason, &asked)
	var seen []string
	inner := sequenceSpecRunner(dataDir, []string{idlessSpec, labelledSpec}, &seen)
	runner := func(ctx context.Context, dd string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		reason = "no route" // the route goes after the first launch
		return inner(ctx, dd, r, cfg)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, "req-1")
	if err := requestdriver.AdvanceSpecDrafting(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || len(asked) != 1 {
		t.Fatalf("launches %d, route checks %d; want both drafts launched and one check", len(seen), len(asked))
	}
}

// A build the operator resumed after the provider refused it with its own
// limit tells the route check when it was resumed, so a refusal at or before
// that resume does not hold it.
func TestAResumedQuotaBuildTellsTheRouteCheckWhenItWasResumed(t *testing.T) {
	reason, asked := "", []requestdriver.RouteNeed{}
	dp := routeWaitDeps(t, &reason, &asked)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	at := time.Now().UTC().Format(time.RFC3339Nano)
	r.Resume = &request.ResumeInfo{FromState: request.StateBuilding, Cause: unresolved.ProviderQuota, Routes: []string{"codex"}, Generation: 1, At: at}
	r.ResumeDecision = &request.ResumeDecision{Verb: request.ResumeScratch, Generation: 1, By: "alice", At: at}
	reason, asked = "held", nil
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, requestdrivertest.FailingBuildRunner(t), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(asked) == 0 || asked[0].ResumedAt != at {
		t.Fatalf("asked %+v, want the resume's time %s passed to the route check", asked, at)
	}
}
