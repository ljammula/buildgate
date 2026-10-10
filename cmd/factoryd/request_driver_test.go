package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/store"
)

// TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet is the
// plan's own required test ("argv the request driver hands to the run
// path is parsed through the REAL flag set"), mirroring
// TestBuildTicketRunArgsIsAcceptedByRunMainWithReadysOwnFlagSet: the argv
// buildRequestBuildArgs produces for one ticket must parse cleanly
// through runMainWithReady's real flag.FlagSet, stopping only at the
// required-flags check (empty -workspace here), never at "flag provided
// but not defined".
func TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet(t *testing.T) {
	dp := newTestDeps(t)
	isolateSessionConfig(t)
	workspace := t.TempDir()
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("Verify-Command: make verify\n\n## Goal\n\ndo the thing\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := &request.Request{ID: "req-1", Workspace: workspace, Project: "payments"}
	ticket := request.Ticket{Index: 1, SpecPath: specPath}
	cfg := requestdriver.WorkerConfig{
		BuildAppScript: "/harness/build_app.py",
		SandboxImage:   "registry.example/org/img@sha256:deadbeef",
	}

	args, err := requestdriver.BuildRequestBuildArgs(t.TempDir(), r, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}

	err = runMainWithReady(dp, context.Background(), args, nil)
	if err == nil {
		t.Fatal("runMainWithReady(buildRequestBuildArgs(...)) = nil, want the required-flags error it stops at")
	}
	if strings.Contains(err.Error(), "not defined") {
		t.Fatalf("runMainWithReady rejected an argv buildRequestBuildArgs produced: %v", err)
	}
}

// TestHaltRequestNotificationHasRetryNextAndConsoleLink proves a halted
// request's notification points the operator at retrying this request
// and, when FACTORYD_CONSOLE_URL is configured, at its console page.
func TestHaltRequestNotificationHasRetryNextAndConsoleLink(t *testing.T) {
	t.Setenv(consoleLinkEnvVar, "https://console.example")
	dataDir := t.TempDir()
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	if err := requestdriver.HaltRequest(dataDir, r, "spec drafting failed", base); err != nil {
		t.Fatalf("haltRequest: %v", err)
	}

	b, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	var got run.NotificationRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if want := "factoryd retry req-1"; got.Next != want {
		t.Errorf("Next = %q, want %q", got.Next, want)
	}
	if want := "https://console.example/requests/req-1"; got.Link != want {
		t.Errorf("Link = %q, want %q", got.Link, want)
	}
}

// TestRemindRequestNotificationHasApproveNextAndConsoleLink proves a
// spec/plan review reminder points the operator at approving (or
// rejecting) this request and, when configured, its console page.
func TestRemindRequestNotificationHasApproveNextAndConsoleLink(t *testing.T) {
	t.Setenv(consoleLinkEnvVar, "https://console.example")
	dataDir := t.TempDir()
	base := time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC)
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, base)
	r.State = request.StateSpecReview

	requestdriver.RemindRequest(dataDir, r, base)

	b, err := os.ReadFile(requestdriver.RequestNotificationLogPath(dataDir, "req-1"))
	if err != nil {
		t.Fatalf("read notifications.log: %v", err)
	}
	var got run.NotificationRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal notification: %v", err)
	}
	if !strings.Contains(got.Next, "factoryd approve req-1") {
		t.Errorf("Next = %q, want it to include factoryd approve req-1", got.Next)
	}
	if want := "https://console.example/requests/req-1"; got.Link != want {
		t.Errorf("Link = %q, want %q", got.Link, want)
	}
}

// TestAdvanceBuildingBuildsThreeTicketsInOrderWithPriorRunChaining covers
// the plan's own "a three-ticket fixture builds 1, 2, 3 in order with
// chained -prior-run" done-when item, argv proven through the real flag
// set the same way TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet
// does.
func TestAdvanceBuildingBuildsThreeTicketsInOrderWithPriorRunChaining(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 3)

	var gotArgs [][]string
	base := requestdrivertest.AcceptingBuildRunner(t, dataDir)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		gotArgs = append(gotArgs, append([]string(nil), args...))
		return base(ctx, args, onReady)
	}

	for i := 1; i <= 3; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{OpenPullRequest: true}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	if len(gotArgs) != 3 {
		t.Fatalf("build runner called %d times, want 3", len(gotArgs))
	}
	for i, args := range gotArgs {
		wantTicket := requestdrivertest.TicketRunID(id, i+1)
		if got := requestdrivertest.ArgValue(args, "-ticket"); got != wantTicket {
			t.Errorf("call %d: -ticket = %q, want %q", i, got, wantTicket)
		}
		if !requestdrivertest.HasFlag(args, "-open-pull-request") {
			t.Errorf("call %d: missing -open-pull-request", i)
		}
		if i == 0 {
			if got := requestdrivertest.ArgValue(args, "-prior-run"); got != "" {
				t.Errorf("call %d: -prior-run = %q, want none on the first ticket", i, got)
			}
		} else {
			wantPrior := requestdrivertest.TicketRunID(id, i)
			if got := requestdrivertest.ArgValue(args, "-prior-run"); got != wantPrior {
				t.Errorf("call %d: -prior-run = %q, want %q", i, got, wantPrior)
			}
		}
	}

	// Argv for the per-ticket run is parsed through the real flag set
	// (the plan's own Ground Rules requirement), mirroring
	// TestBuildRequestBuildArgsIsAcceptedByRunMainWithReadysOwnFlagSet:
	// checked on the first (no -prior-run) and last (-prior-run present)
	// calls, so -prior-run's own presence is proven accepted too.
	for _, args := range []([]string){gotArgs[0], gotArgs[2]} {
		err := runMainWithReady(dp, context.Background(), args, nil)
		if err == nil {
			t.Fatal("runMainWithReady(driveRequests' own argv) = nil, want the required-flags error it stops at")
		}
		if strings.Contains(err.Error(), "not defined") {
			t.Fatalf("runMainWithReady rejected an argv driveRequests produced: %v", err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q", loaded.State, request.StatePRReview)
	}
	if loaded.TicketIndex != 3 {
		t.Errorf("TicketIndex = %d, want 3", loaded.TicketIndex)
	}
	for i, tk := range loaded.Tickets {
		wantRunID := requestdrivertest.TicketRunID(id, i+1)
		if tk.RunID != wantRunID {
			t.Errorf("Tickets[%d].RunID = %q, want %q", i, tk.RunID, wantRunID)
		}
		if tk.PRURL == "" || tk.PRState != "draft" {
			t.Errorf("Tickets[%d] = %+v, want a PR URL and PRState %q", i, tk, "draft")
		}
	}

	// M4-K1 regression: advanceBuilding's own onReady callback used to
	// call startedRun.Save directly, bypassing RecordEvent entirely, so
	// a ticket's run never appended to events.db the moment it started.
	// It now goes through startedRun.Persist instead.
	s, err := store.Open(run.EventsDBPath(dataDir))
	if err != nil {
		t.Fatalf("open event store: %v", err)
	}
	defer s.Close()
	for i := 1; i <= 3; i++ {
		events, err := s.List(context.Background(), requestdrivertest.TicketRunID(id, i))
		if err != nil {
			t.Fatalf("list events for ticket %d's run: %v", i, err)
		}
		if len(events) == 0 {
			t.Errorf("ticket %d's run: events = [], want at least one durable event recorded when its run started", i)
		}
	}
}

// TestAdvanceBuildingQuarantineOfTicket2LeavesTicket1IntactAndRetryable
// covers the plan's own "quarantine of ticket 2 leaves ticket 1's PR and
// run intact and the request retryable from ticket 2" done-when item.
func TestAdvanceBuildingQuarantineOfTicket2LeavesTicket1IntactAndRetryable(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 3)
	accept := requestdrivertest.AcceptingBuildRunner(t, dataDir)
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if ticket != requestdrivertest.TicketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateQuarantined, HaltError: "gate failed: tests_added"}
		return rr.Save(dataDir)
	}

	for i := 1; i <= 2; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if !strings.Contains(loaded.Error, "ticket 2/3") || !strings.Contains(loaded.Error, "quarantined") || !strings.Contains(loaded.Error, "gate failed: tests_added") {
		t.Errorf("Error = %q, want it to name ticket 2/3, quarantined, and the run's own reason", loaded.Error)
	}
	if got := loaded.Tickets[0].RunID; got != requestdrivertest.TicketRunID(id, 1) {
		t.Errorf("Tickets[0].RunID = %q, want %q (ticket 1 untouched)", got, requestdrivertest.TicketRunID(id, 1))
	}
	if loaded.Tickets[0].PRURL == "" {
		t.Errorf("Tickets[0].PRURL is empty, want ticket 1's PR left intact")
	}
	if got := loaded.Tickets[1].RunID; got != requestdrivertest.TicketRunID(id, 2) {
		t.Errorf("Tickets[1].RunID = %q, want %q (recorded even though ticket 2 quarantined)", got, requestdrivertest.TicketRunID(id, 2))
	}

	handled, err := retryRequest(dp, dataDir, loaded, "", time.Now())
	if !handled {
		t.Fatal("retryRequest: handled = false, want true for a quarantined request with TicketIndex within range")
	}
	if err != nil {
		t.Fatalf("retryRequest: %v", err)
	}
	retried, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != request.StateBuilding {
		t.Fatalf("State after retry = %q, want %q", retried.State, request.StateBuilding)
	}
	if retried.TicketIndex != 2 {
		t.Errorf("TicketIndex after retry = %d, want 2 (retries from the failed ticket)", retried.TicketIndex)
	}
	if got := retried.Tickets[0].RunID; got != requestdrivertest.TicketRunID(id, 1) {
		t.Errorf("Tickets[0].RunID after retry = %q, want %q (still untouched)", got, requestdrivertest.TicketRunID(id, 1))
	}
}

// TestAdvanceBuildingRetryStartFailureDoesNotReplayStaleRunID covers a
// Codex review finding on PR #135: retryRequest rebuilds a
// quarantined/halted ticket without clearing its previous RunID, so a
// fresh runner call that fails *before* onReady fires this time must not
// let ticket.RunID's leftover value from the prior attempt cause
// advanceBuilding to load and replay that old (already-superseded)
// outcome instead of reporting the new start failure.
func TestAdvanceBuildingRetryStartFailureDoesNotReplayStaleRunID(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 3)
	accept := requestdrivertest.AcceptingBuildRunner(t, dataDir)
	quarantineTicket2 := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if ticket != requestdrivertest.TicketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		if onReady != nil {
			onReady(&run.Run{ID: ticket, State: run.StateReady})
		}
		rr := &run.Run{ID: ticket, State: run.StateQuarantined, HaltError: "gate failed: tests_added"}
		if err := rr.Save(dataDir); err != nil {
			return err
		}
		return fmt.Errorf("run quarantined: gate failed: tests_added")
	}

	for i := 1; i <= 2; i++ {
		if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), quarantineTicket2); err != nil {
			t.Fatalf("driveRequests (ticket %d): %v", i, err)
		}
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}

	if _, err := retryRequest(dp, dataDir, loaded, "", time.Now()); err != nil {
		t.Fatalf("retryRequest: %v", err)
	}
	retried, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != request.StateBuilding {
		t.Fatalf("State after retry = %q, want %q", retried.State, request.StateBuilding)
	}
	if got := retried.Tickets[1].RunID; got != requestdrivertest.TicketRunID(id, 2) {
		t.Fatalf("Tickets[1].RunID after retry = %q, want %q (retryRequest leaves the prior attempt's RunID in place)", got, requestdrivertest.TicketRunID(id, 2))
	}

	// The next runner call for ticket 2 fails before onReady fires at
	// all -- a fresh start failure, unrelated to the prior quarantine.
	startFailure := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if ticket != requestdrivertest.TicketRunID(id, 2) {
			return accept(ctx, args, onReady)
		}
		return errors.New("sandbox image pull failed")
	}
	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), startFailure); err != nil {
		t.Fatalf("driveRequests after retry: %v", err)
	}

	final, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != request.StateHalted {
		t.Fatalf("State after retry+start-failure = %q, want %q (must not replay the stale quarantine)", final.State, request.StateHalted)
	}
	if !strings.Contains(final.Error, "start failed") || !strings.Contains(final.Error, "sandbox image pull failed") {
		t.Errorf("Error = %q, want it to name the new start failure", final.Error)
	}
	if strings.Contains(final.Error, "tests_added") {
		t.Errorf("Error = %q, must not replay the stale quarantine's own reason", final.Error)
	}
}

// TestRetryRequestResumesReviewWhenTicketHasAPR: a request halted out of
// pr_review (rounds exhausted, push rejected) goes back to pr_review on
// retry, not to a fresh build that would open a second PR on the branch.
func TestRetryRequestResumesReviewWhenTicketHasAPR(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, now)
	r.State = request.StateHalted
	r.Error = "review rounds exhausted for ticket 1/1"
	r.TicketIndex, r.TicketCount = 1, 1
	r.Tickets = []request.Ticket{{Index: 1, SpecPath: "/x/001.spec.md", RunID: "run-1", PRURL: "https://github.com/acme/w/pull/1"}}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	handled, err := retryRequest(dp, dataDir, r, "", now)
	if !handled || err != nil {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview || loaded.Error != "" {
		t.Fatalf("State = %q, Error = %q; want pr_review with the error cleared", loaded.State, loaded.Error)
	}
}

// TestRetryRequestResumesSpecDraftingWhenNoTicketExists: a request halted
// during spec drafting goes back to spec_drafting on retry.
func TestRetryRequestResumesSpecDraftingWhenNoTicketExists(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, now)
	r.State = request.StateHalted
	r.Error = "spec drafting failed"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	handled, err := retryRequest(dp, dataDir, r, "", now)
	if !handled || err != nil {
		t.Fatalf("retryRequest: handled=%v err=%v", handled, err)
	}
	loaded, err := request.Load(dataDir, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateSpecDrafting || loaded.Error != "" {
		t.Fatalf("State = %q, Error = %q; want spec_drafting with the error cleared", loaded.State, loaded.Error)
	}
}

// A request halted at the very start of building (TicketIndex still 0 --
// e.g. an approved-oracle hash mismatch, refused before ticket 1 is
// marked started) must be retryable: it used to fall through to the
// legacy queue-entry path and fail with "no queue entry" (found live,
// 2026-09-19).
func TestRetryRequestHaltedBeforeFirstTicketStarts(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 0
	r.State = request.StateHalted
	r.Error = "tickets/001.oracle/x_test.go has changed since the operator approved it -- re-approve before continuing"
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	handled, err := retryRequest(dp, dataDir, loaded, "", time.Now())
	if err != nil || !handled {
		t.Fatalf("retryRequest = handled %v, err %v; want handled with no error", handled, err)
	}
	retried, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if retried.State != request.StateBuilding || retried.Error != "" {
		t.Errorf("after retry: state %q, error %q; want building with the error cleared", retried.State, retried.Error)
	}
}

// --- An adversarial-review finding (2026-09-24): a cancel that lands
// while a job the driver is already running (spec/oracle/plan drafting,
// a ticket build) must not be resurrected by that job's own r.Save once
// it returns. Each test below cancels the request *from inside the stub
// runner*, mimicking cancelRequest (internal/api/server.go) or `factoryd
// cancel` taking request.Lock concurrently with driveRequests holding its
// own unlocked *request.Request from request.List -- see stillInState's
// doc comment (request_driver.go) for the mechanism these tests pin.

// TestDriveRequestsPublishesTheActiveRequestDuringItsJob: the request whose
// job is in flight is published (for the worker heartbeat) while the job
// runs and cleared once it returns.
func TestDriveRequestsPublishesTheActiveRequestDuringItsJob(t *testing.T) {
	dp := newTestDeps(t)
	dataDir := t.TempDir()
	if err := request.SaveText(dataDir, "req-1", "text"); err != nil {
		t.Fatal(err)
	}
	r := request.New("req-1", "/repos/app", "app", request.Source{Kind: request.SourceText}, time.Now())
	r.State = request.StateSpecDrafting
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var during []string
	spec := func(ctx context.Context, dataDir string, r *request.Request, cfg requestdriver.WorkerConfig) (string, *request.SpecEvidence, error) {
		during = currentActiveRequests()
		return "", nil, errors.New("stop here")
	}
	_ = driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, spec, requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), requestdrivertest.FailingBuildRunner(t))
	if len(during) != 1 || during[0] != "req-1" {
		t.Errorf("active requests during the job = %v, want [req-1]", during)
	}
	if got := currentActiveRequests(); len(got) != 0 {
		t.Errorf("active requests after the job = %v, want none", got)
	}
}

// no ## Scope heading
