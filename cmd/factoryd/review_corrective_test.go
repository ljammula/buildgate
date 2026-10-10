package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// reviewQuarantinedBuildRunner returns a stub ticketRunner mimicking a
// ticket's first build quarantining with spec_conformity as the only
// failed gate (extraGates, if any, are additionally recorded as passed).
func reviewQuarantinedBuildRunner(t *testing.T, dataDir, branch, baseSHA string, extraFailedGate string) requestdriver.TicketRunner {
	t.Helper()
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		gates := []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "diff_scope", Passed: true},
		}
		if extraFailedGate != "" {
			gates = append(gates, run.GateResult{Check: extraFailedGate, Passed: false})
		}
		gates = append(gates, run.GateResult{Check: "spec_conformity", Passed: false})
		rr := &run.Run{
			ID:          ticket,
			State:       run.StateQuarantined,
			HaltError:   "gate failed: spec_conformity",
			Branch:      branch,
			BaseSHA:     baseSHA,
			GateResults: gates,
			SpecConformityVerdicts: []run.ReviewVerdict{
				{Criterion: "1. handles empty input", Verdict: "clean"},
				{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "no test covers a negative amount"},
			},
		}
		return rr.Save(dataDir)
	}
}

// stubReviewCorrectiveRunner installs a package-var override for the
// duration of the test (mirroring stubPRReviewDeps' own swap-and-restore
// shape) that records the args it was called with and its call count, and
// finishes the round according to finish.
func stubReviewCorrectiveRunner(t *testing.T, dataDir string, finish func(dataDir, roundRunID string) *run.Run) (calls *int, lastArgs *[]string) {
	t.Helper()
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	calls = new(int)
	lastArgs = new([]string)
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		*calls++
		*lastArgs = args
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		rr := finish(dataDir, roundRunID)
		return rr.Save(dataDir)
	}
	return calls, lastArgs
}

// TestAdvanceBuildingReviewCorrectiveRoundAcceptedProceedsAsAccepted
// covers the corrective round's own "accepted -> the request proceeds exactly as a normally
// accepted ticket" done-when item: the corrective round's own Branch/PRURL
// land on the ticket, and a single-ticket request moves on to pr_review
// exactly as an ordinary first-build acceptance would.
func TestAdvanceBuildingReviewCorrectiveRoundAcceptedProceedsAsAccepted(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")

	calls, lastArgs := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 1", *calls)
	}
	if got := requestdrivertest.ArgValue(*lastArgs, "-on-branch"); got != branch {
		t.Errorf("-on-branch = %q, want %q", got, branch)
	}
	if got := requestdrivertest.ArgValue(*lastArgs, "-diff-base"); got != baseSHA {
		t.Errorf("-diff-base = %q, want %q", got, baseSHA)
	}
	if !requestdrivertest.HasFlag(*lastArgs, "-open-pull-request") {
		t.Error("-open-pull-request not forwarded, want it left at cfg's own value (unlike a PR-review round's forced-off)")
	}
	addendumPath := requestdrivertest.ArgValue(*lastArgs, "-spec")
	addendum, err := os.ReadFile(addendumPath)
	if err != nil {
		t.Fatalf("read addendum %s: %v", addendumPath, err)
	}
	if !strings.Contains(string(addendum), "rejects a negative amount") || !strings.Contains(string(addendum), "no test covers a negative amount") {
		t.Errorf("addendum = %q, want the flagged criterion and its detail", addendum)
	}
	if strings.Contains(string(addendum), "handles empty input") {
		t.Errorf("addendum = %q, want the clean criterion omitted", addendum)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (accepted corrective round advances exactly like an ordinary accepted build)", loaded.State, request.StatePRReview)
	}
	tk := loaded.Tickets[0]
	if tk.Branch != branch {
		t.Errorf("Tickets[0].Branch = %q, want %q", tk.Branch, branch)
	}
	if tk.PRURL != "https://github.com/acme/app/pull/999" {
		t.Errorf("Tickets[0].PRURL = %q, want the corrective round's own PR", tk.PRURL)
	}
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.ConformityRoundKind || tk.Rounds[0].Outcome != request.RoundAccepted {
		t.Fatalf("Tickets[0].Rounds = %+v, want one accepted conformity round", tk.Rounds)
	}
	if tk.RunID != tk.Rounds[0].RunID {
		t.Errorf("Tickets[0].RunID = %q, want it repointed at the corrective round's own run %q", tk.RunID, tk.Rounds[0].RunID)
	}
}

// TestAdvanceBuildingReviewCorrectiveRoundQuarantinedAgainQuarantines
// covers the corrective round's own "quarantined/halted again -> the request quarantines as
// today" done-when item, and that Retry/SendBack keep working: ticket.
// Branch/PRURL stay empty (AnyTicketAccepted must stay false), and exactly
// one corrective round runs (the budget is consumed, not retried again in
// the same pass).
func TestAdvanceBuildingReviewCorrectiveRoundQuarantinedAgainQuarantines(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")

	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: spec_conformity", Branch: branch, BaseSHA: baseSHA,
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "spec_conformity", Passed: false},
			},
			SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "still not fixed"}},
		}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want exactly 1 (budget is 1 and must not be retried again in the same pass)", *calls)
	}

	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	// The exhausted-rounds message quotes the last round's own reason, so
	// the operator sees why without opening each run.
	if !strings.Contains(loaded.Error, "rounds exhausted") || !strings.Contains(loaded.Error, "gate failed: spec_conformity") {
		t.Errorf("Error = %q, want the exhausted message quoting the last round's reason", loaded.Error)
	}
	tk := loaded.Tickets[0]
	if tk.Branch != "" || tk.PRURL != "" {
		t.Errorf("Tickets[0] = %+v, want Branch/PRURL left empty (AnyTicketAccepted must stay false)", tk)
	}
	if loaded.AnyTicketAccepted() {
		t.Error("AnyTicketAccepted() = true, want false after a quarantined corrective round")
	}
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.ConformityRoundKind || tk.Rounds[0].Outcome != request.RoundQuarantined {
		t.Fatalf("Tickets[0].Rounds = %+v, want one quarantined conformity round", tk.Rounds)
	}
	if tk.RunID != tk.Rounds[0].RunID {
		t.Errorf("Tickets[0].RunID = %q, want it repointed at the corrective round's own run %q", tk.RunID, tk.Rounds[0].RunID)
	}

	// Retry must still work against this quarantined request (the corrective round's own
	// "does not break Retry" guard).
	handled, err := retryRequest(dp, dataDir, loaded, "", time.Now())
	if !handled || err != nil {
		t.Fatalf("retryRequest: handled=%v err=%v, want handled=true err=nil", handled, err)
	}
}

// TestAdvanceBuildingReviewCorrectiveRoundNotTriggeredByAnotherGateFailure
// covers the corrective round's own "never triggered when any deterministic gate failed"
// guard: a diff_scope failure alongside spec_conformity must quarantine
// immediately, with no corrective round launched at all.
func TestAdvanceBuildingReviewCorrectiveRoundNotTriggeredByAnotherGateFailure(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	// "some_other_gate" (not diff_scope): stands in for an arbitrary
	// unrelated gate failure, not the diff_scope-specific quarantine cause
	// (request.QuarantineCheckDiffScope) -- see
	// TestAdvanceBuildingSetsQuarantineCheckDiffScope for that one.
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), "some_other_gate")
	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 0 (some_other_gate also failed)", *calls)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if len(loaded.Tickets[0].Rounds) != 0 {
		t.Errorf("Tickets[0].Rounds = %+v, want none", loaded.Tickets[0].Rounds)
	}
	// Follow-up B: some_other_gate also failed, so this is NOT a
	// spec_conformity-only quarantine -- QuarantineCheck must stay empty
	// (some_other_gate is not diff_scope, so the new diff_scope-specific
	// branch doesn't apply either).
	if loaded.QuarantineCheck != "" {
		t.Errorf("QuarantineCheck = %q, want empty (some_other_gate also failed)", loaded.QuarantineCheck)
	}
}

// TestAdvanceBuildingSetsQuarantineCheckDiffScope covers advanceBuilding's
// own StateQuarantined case (request_driver.go): a run that quarantines
// with diff_scope as its only failed gate (no spec_conformity/code_review
// alongside it, so reviewShapeOnly is false) must set
// request.QuarantineCheckDiffScope, so NextAction leads with
// `factoryd amend-scope` instead of a generic "fix the cause, then retry".
func TestAdvanceBuildingSetsQuarantineCheckDiffScope(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		rr := &run.Run{
			ID:        ticket,
			State:     run.StateQuarantined,
			HaltError: "gate failed: diff_scope",
			Branch:    "factoryd/" + id + "-001",
			BaseSHA:   fmt.Sprintf("%040d", 1),
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "diff_scope", Passed: false},
			},
		}
		return rr.Save(dataDir)
	}

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != request.QuarantineCheckDiffScope {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckDiffScope)
	}
	if next := loaded.NextAction(); !strings.Contains(next, "factoryd amend-scope") {
		t.Errorf("NextAction() = %q, want it to name `factoryd amend-scope`", next)
	}
}

// TestAdvanceBuildingReviewCorrectiveRoundNotTriggeredWhenBudgetZero
// covers the corrective round's own "0 disables" default-off case: workerConfig{}'s zero
// value for conformityCorrectiveRounds must behave exactly as before this
// feature existed.
func TestAdvanceBuildingReviewCorrectiveRoundNotTriggeredWhenBudgetZero(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, "factoryd/"+id+"-001", fmt.Sprintf("%040d", 1), "")
	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	if err := driveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 0 (conformityCorrectiveRounds is 0)", *calls)
	}
	loaded, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	// Follow-up B: spec_conformity was the only gate that failed here
	// (reviewQuarantinedBuildRunner's extraFailedGate is ""), so this
	// quarantine is conformity-only even though the corrective round never
	// ran (budget 0) -- QuarantineCheck must still be set, and NextAction
	// must lead with the spec send-back, not a doomed retry.
	if loaded.QuarantineCheck != request.QuarantineCheckSpecConformity {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckSpecConformity)
	}
	if next := loaded.NextAction(); !strings.Contains(next, "factoryd reject -to spec") {
		t.Errorf("NextAction() = %q, want it to lead with the spec send-back", next)
	}
}

// TestAdvanceBuildingReviewCorrectiveRoundNotTriggeredWhenBudgetExhausted
// covers the corrective round's own budget-exhaustion guard directly against
// tryReviewCorrectiveRound: a ticket that already has one non-start-
// failure conformity round on record must not get a second when the budget
// is 1.
func TestAdvanceBuildingReviewCorrectiveRoundNotTriggeredWhenBudgetExhausted(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)

	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.TicketIndex = 1
	ticket := &r.Tickets[0]
	ticket.Rounds = append(ticket.Rounds, request.Round{
		Index: 1, Kind: request.ConformityRoundKind, Outcome: request.RoundQuarantined,
		RunID: "prior-conformity-run", At: time.Now().UTC().Format(time.RFC3339Nano),
	})
	runRecord := &run.Run{
		ID: "run-under-test", State: run.StateQuarantined, Branch: branch, BaseSHA: baseSHA,
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "spec_conformity", Passed: false},
		},
		SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "still not fixed"}},
	}

	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	handled, err := requestdriver.TryReviewCorrectiveRound(dp, context.Background(), dataDir, r, ticket, runRecord, requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}, time.Now())
	if handled {
		t.Fatal("tryReviewCorrectiveRound: handled = true, want false (budget already exhausted)")
	}
	if err != nil {
		t.Fatalf("tryReviewCorrectiveRound: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 0", *calls)
	}
	if len(ticket.Rounds) != 1 {
		t.Fatalf("ticket.Rounds = %+v, want still just the one pre-existing round", ticket.Rounds)
	}
}

// TestTryReviewCorrectiveRoundExhaustedBudgetSetsQuarantineCheck is the
// regression test for Follow-up B's other quarantineRequestWithCheck call
// site: a ticket that stays conformity-only-eligible through every round
// but never gets accepted before the budget runs out must still record
// QuarantineCheck (the loop only ever exits this way after a round whose
// own reviewOnlyFlagged was true), so NextAction leads with the
// spec send-back instead of a doomed retry.
func TestTryReviewCorrectiveRoundExhaustedBudgetSetsQuarantineCheck(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")

	origRunner := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = origRunner })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		rr := &run.Run{
			ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: spec_conformity",
			Branch: branch, BaseSHA: baseSHA,
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "spec_conformity", Passed: false},
			},
			SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "still not fixed"}},
		}
		return rr.Save(dataDir)
	}

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != request.QuarantineCheckSpecConformity {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckSpecConformity)
	}
	if next := loaded.NextAction(); !strings.Contains(next, "factoryd reject -to spec") {
		t.Errorf("NextAction() = %q, want it to lead with the spec send-back", next)
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

// TestTryReviewCorrectiveRoundCancelledMidRoundDoesNotResurrectRequest
// pins a review finding (HIGH): a request cancelled WHILE the corrective round's
// own build runs must not be resurrected by this function's own r.Save
// calls once that build returns -- mirrors advanceBuilding's identical
// stillInState re-check after the ticket's own first build.
func TestTryReviewCorrectiveRoundCancelledMidRoundDoesNotResurrectRequest(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")

	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		cancelRequestForTest(t, dataDir, id)
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 1", *calls)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateCancelled {
		t.Fatalf("State = %q, want %q (a cancel mid-round must survive the round's own save)", loaded.State, request.StateCancelled)
	}
	if len(loaded.Tickets[0].Rounds) != 0 {
		t.Errorf("Tickets[0].Rounds = %+v, want none recorded (the round's result was discarded, not saved)", loaded.Tickets[0].Rounds)
	}
}

// TestTryReviewCorrectiveRoundCtxCancelledLeavesRequestBuilding pins
// a review finding (MEDIUM): a shutdown mid-round (the runner returns an
// error and the context it was given is Done) must leave the request in
// building with no round recorded and no budget consumed, exactly like
// advanceBuilding's own identical ctx.Err() handling for a ticket's first
// build -- so the next worker poll retries the same round rather than
// treating a shutdown as a quarantine.
func TestTryReviewCorrectiveRoundCtxCancelledLeavesRequestBuilding(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, "")

	ctx, cancel := context.WithCancel(context.Background())
	origRunner := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = origRunner })
	requestdriver.ReviewCorrectiveRunner = func(rctx context.Context, args []string, onReady func(*run.Run)) error {
		cancel()
		return errors.New("context canceled")
	}

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := driveRequests(dp, ctx, dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err == nil {
		t.Fatal("driveRequests: want the cancelled round's own error propagated")
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateBuilding {
		t.Fatalf("State = %q, want %q (shutdown mid-round leaves the request for the next poll to retry)", loaded.State, request.StateBuilding)
	}
	if len(loaded.Tickets[0].Rounds) != 0 {
		t.Errorf("Tickets[0].Rounds = %+v, want none (no budget consumed on shutdown)", loaded.Tickets[0].Rounds)
	}
}

// TestBuildReviewCorrectiveArgsCarriesHarnessLikeFirstBuild pins a
// review finding (MEDIUM): the corrective round's argv must carry the
// same -execution-harness flag the ticket's ordinary first build gets -- proving
// both are now built from the one shared ticketQueueEntry, not two
// independently hand-built QueueEntry literals that can drift apart.
func TestBuildReviewCorrectiveArgsCarriesHarnessLikeFirstBuild(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	r.Harnesses = map[string]string{"execution": "pifork"}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	ticket := r.Tickets[0]

	firstArgs, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, cfg)
	if err != nil {
		t.Fatalf("buildRequestBuildArgs: %v", err)
	}
	correctiveArgs, err := requestdriver.BuildReviewCorrectiveArgs(dataDir, r, ticket, cfg, ticket.SpecPath, "round-1", "some-branch", "0000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("buildReviewCorrectiveArgs: %v", err)
	}

	want := requestdrivertest.ArgValue(firstArgs, "-execution-harness")
	got := requestdrivertest.ArgValue(correctiveArgs, "-execution-harness")
	if want != "pifork" || got != want {
		t.Errorf("-execution-harness = %q, want %q (same as the first build's own argv, pifork)", got, want)
	}
}

// TestBuildReviewCorrectiveArgsForwardsTemporalAddressAndDiffBase is
// the argv-level proof that routing flags are forwarded: -temporal-address, -on-branch and
// -diff-base are all still forwarded on the corrective round's own argv
// regardless of routing -- run_ticket.go itself is what currently honours
// -on-branch/-diff-base only on the direct execution path (documented on
// buildReviewCorrectiveArgs' own doc comment and USAGE_REFERENCE.md;
// the same pre-existing limitation the PR-review corrective round has).
func TestBuildReviewCorrectiveArgsForwardsTemporalAddressAndDiffBase(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	ticket := r.Tickets[0]
	cfg := requestdriver.WorkerConfig{TemporalAddress: "localhost:7233"}

	args, err := requestdriver.BuildReviewCorrectiveArgs(dataDir, r, ticket, cfg, ticket.SpecPath, "round-1", "some-branch", "0000000000000000000000000000000000000001")
	if err != nil {
		t.Fatalf("buildReviewCorrectiveArgs: %v", err)
	}
	if got := requestdrivertest.ArgValue(args, "-temporal-address"); got != "localhost:7233" {
		t.Errorf("-temporal-address = %q, want %q", got, "localhost:7233")
	}
	if got := requestdrivertest.ArgValue(args, "-on-branch"); got != "some-branch" {
		t.Errorf("-on-branch = %q, want %q carried through regardless of routing", got, "some-branch")
	}
	if got := requestdrivertest.ArgValue(args, "-diff-base"); got != "0000000000000000000000000000000000000001" {
		t.Errorf("-diff-base = %q, want it carried through regardless of routing", got)
	}
}

// TestTryReviewCorrectiveRoundLoopsWithinBudgetOnRepeatedConformityFailure
// pins a review finding: a budget of 2 must actually allow two CONSECUTIVE
// rounds when the first corrective round quarantines again with the
// identical conformity-only shape, not quarantine the request after
// exactly one round regardless of budget. It also proves round 2's own
// -diff-base is round 1's DiffBaseSHA (the ticket's true original base),
// not round 1's own BaseSHA (which is only the branch tip at round 1's own
// start, distinct here on purpose) -- the original-base fix threaded through
// this loop.
func TestTryReviewCorrectiveRoundLoopsWithinBudgetOnRepeatedConformityFailure(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	originalBase := fmt.Sprintf("%040d", 1)
	round1OwnTip := fmt.Sprintf("%040d", 2)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, originalBase, "")

	callCount := 0
	var round2Args []string
	origRunner := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = origRunner })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		callCount++
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		if callCount == 1 {
			round2Args = nil
			rr := &run.Run{
				ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: spec_conformity",
				Branch: branch, BaseSHA: round1OwnTip, DiffBaseSHA: originalBase,
				GateResults: []run.GateResult{
					{Check: "canonical_verify", Passed: true},
					{Check: "spec_conformity", Passed: false},
				},
				SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "still not fixed"}},
			}
			return rr.Save(dataDir)
		}
		round2Args = args
		rr := &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
		return rr.Save(dataDir)
	}

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 2, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 2 (budget 2, round 1 conformity-only again must trigger round 2)", callCount)
	}
	if got := requestdrivertest.ArgValue(round2Args, "-diff-base"); got != originalBase {
		t.Errorf("round 2's -diff-base = %q, want the ticket's original base %q (not round 1's own tip %q)", got, originalBase, round1OwnTip)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (round 2 accepted)", loaded.State, request.StatePRReview)
	}
	tk := loaded.Tickets[0]
	if len(tk.Rounds) != 2 || tk.Rounds[0].Outcome != request.RoundQuarantined || tk.Rounds[1].Outcome != request.RoundAccepted {
		t.Fatalf("Tickets[0].Rounds = %+v, want [quarantined, accepted]", tk.Rounds)
	}
}

// TestTryReviewCorrectiveRoundStopsAfterNonConformityFailureEvenWithBudgetRemaining
// is the negative case of the budget-loop test above: budget 2, but round 1's own failure
// is NOT conformity-only (diff_scope also failed) -- the request must
// quarantine immediately, never attempting a second round just because
// budget remains.
func TestTryReviewCorrectiveRoundStopsAfterNonConformityFailureEvenWithBudgetRemaining(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	originalBase := fmt.Sprintf("%040d", 1)
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, originalBase, "")

	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{
			ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: diff_scope",
			Branch: branch, BaseSHA: originalBase,
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "diff_scope", Passed: false},
				{Check: "spec_conformity", Passed: false},
			},
			SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. rejects a negative amount", Verdict: "flagged", Detail: "still not fixed"}},
		}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 2}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 1 (round 1's own failure wasn't conformity-only, so no round 2)", *calls)
	}
	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if len(loaded.Tickets[0].Rounds) != 1 {
		t.Errorf("Tickets[0].Rounds = %+v, want exactly 1", loaded.Tickets[0].Rounds)
	}
}

// Round-2 review: under -temporal-address the round would rebuild
// from the repo's current HEAD, dropping earlier tickets' work for ticket
// 2+, so it is skipped there (ticket 1 still gets it).
// TestTryReviewCorrectiveRoundRunsForLaterTicketsUnderTemporal: since #341
// the Temporal path honours -on-branch, so ticket 2+ gets its corrective
// round on its own branch (live Flutter + Go app M-E1 run, 2026-09-28: ticket 2 of 3
// quarantined on spec_conformity with no round, because of the old skip).
func TestTryReviewCorrectiveRoundRunsForLaterTicketsUnderTemporal(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	ticket := &r.Tickets[0]
	ticket.Index = 2
	ticket.RunID = "ticket-one-run"
	if err := (&run.Run{ID: ticket.RunID, BaseSHA: fmt.Sprintf("%040d", 1)}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	runRecord := &run.Run{
		ID: "run-under-test", State: run.StateQuarantined, Branch: "factoryd/" + id + "-002", BaseSHA: fmt.Sprintf("%040d", 1),
		GateResults: []run.GateResult{
			{Check: "canonical_verify", Passed: true},
			{Check: "spec_conformity", Passed: false},
		},
		SpecConformityVerdicts: []run.ReviewVerdict{{Criterion: "2. x", Verdict: "flagged", Detail: "missing"}},
	}
	calls, lastArgs := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	handled, err := requestdriver.TryReviewCorrectiveRound(dp, context.Background(), dataDir, r, ticket, runRecord, requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, TemporalAddress: "localhost:7233"}, time.Now())
	if !handled || err != nil || *calls != 1 {
		t.Fatalf("handled=%v err=%v calls=%d, want true/nil/1 (a round for ticket 2 under Temporal)", handled, err, *calls)
	}
	if got := requestdrivertest.ArgValue(*lastArgs, "-on-branch"); got != runRecord.Branch {
		t.Fatalf("-on-branch = %q, want the quarantined run's branch %q", got, runRecord.Branch)
	}
}

// TestReviewCorrectiveRoundRecordsItsRunWhileRunning: while the round
// runs, the request on disk already names the round's run, so the console
// shows the round building rather than the quarantined first run.
func TestReviewCorrectiveRoundRecordsItsRunWhileRunning(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	buildRunner := reviewQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), "")

	var onDiskDuringRound, requestIDOnRun string
	stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		loaded, err := request.Load(dataDir, id)
		if err != nil {
			t.Fatal(err)
		}
		onDiskDuringRound = loaded.Tickets[0].RunID
		if rr, err := run.Load(dataDir, roundRunID); err == nil {
			requestIDOnRun = rr.RequestID
		}
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, RequestID: id, PullRequestURL: "https://github.com/acme/app/pull/1"}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if want := id + "-001-conformity1"; onDiskDuringRound != want {
		t.Errorf("ticket RunID on disk during the round = %q, want the round's run %q", onDiskDuringRound, want)
	}
	if requestIDOnRun != id {
		t.Errorf("round run RequestID = %q, want %q", requestIDOnRun, id)
	}
}

// ---- code_review coverage (M2-D) ----------------------------------------

// codeReviewQuarantinedBuildRunner returns a stub ticketRunner mimicking a
// ticket's first build quarantining with code_review as a failed gate
// (and, if conformityAlsoFlagged, spec_conformity too) -- extraFailedGate,
// if any, is an additional failed gate that must make the quarantine
// ineligible for a review corrective round.
func codeReviewQuarantinedBuildRunner(t *testing.T, dataDir, branch, baseSHA string, codeReview *run.CodeReviewResult, conformityAlsoFlagged bool, extraFailedGate string) requestdriver.TicketRunner {
	t.Helper()
	return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: ticket})
		}
		gates := []run.GateResult{{Check: "canonical_verify", Passed: true}}
		if extraFailedGate != "" {
			gates = append(gates, run.GateResult{Check: extraFailedGate, Passed: false})
		}
		var verdicts []run.ReviewVerdict
		if conformityAlsoFlagged {
			gates = append(gates, run.GateResult{Check: "spec_conformity", Passed: false})
			verdicts = []run.ReviewVerdict{{Criterion: "1. x", Verdict: "flagged", Detail: "y"}}
		}
		gates = append(gates, run.GateResult{Check: "code_review", Passed: false})
		rr := &run.Run{
			ID: ticket, State: run.StateQuarantined, HaltError: "gate failed: code_review",
			Branch: branch, BaseSHA: baseSHA,
			GateResults:            gates,
			SpecConformityVerdicts: verdicts,
			CodeReview:             codeReview,
		}
		return rr.Save(dataDir)
	}
}

// TestReviewCorrectiveRoundCodeReviewOnlyHighFindingLaunchesAndAccepts
// covers M2-D's core new case: a code_review-only quarantine with a
// "high"-severity finding launches a corrective round whose addendum
// names the finding's file:line, summary and failure scenario; an
// accepted round proceeds exactly like an ordinary accepted build.
func TestReviewCorrectiveRoundCodeReviewOnlyHighFindingLaunchesAndAccepts(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	codeReview := &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{
			{Severity: "high", File: "internal/foo.go", Line: 42, Summary: "unchecked error swallows a write failure", FailureScenario: "a full disk silently drops data"},
			{Severity: "low", File: "internal/bar.go", Summary: "nit"},
		},
	}
	buildRunner := codeReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, codeReview, false, "")

	calls, lastArgs := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 1", *calls)
	}

	addendumPath := requestdrivertest.ArgValue(*lastArgs, "-spec")
	addendum := string(mustReadFile(t, addendumPath))
	if !strings.Contains(addendum, "## Code review findings to address") {
		t.Errorf("addendum = %q, want the code-review section", addendum)
	}
	if !strings.Contains(addendum, "internal/foo.go:42") || !strings.Contains(addendum, "unchecked error swallows a write failure") || !strings.Contains(addendum, "a full disk silently drops data") {
		t.Errorf("addendum = %q, want the high finding's file:line, summary and failure scenario", addendum)
	}
	if strings.Contains(addendum, "internal/bar.go") {
		t.Errorf("addendum = %q, want the low-severity finding omitted", addendum)
	}
	if strings.Contains(addendum, "## Spec conformity review to address") {
		t.Errorf("addendum = %q, want no conformity section (spec_conformity never failed)", addendum)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (round accepted)", loaded.State, request.StatePRReview)
	}
}

// TestReviewCorrectiveRoundAddendumEmbedsBuildSpecCriteriaOnce covers the
// fix's own "embed the build spec instead of both" requirement:
// writeReviewAddendum used to embed the raw ticket spec; it now embeds
// ticketBuildSpecContent (ticket spec + covered-criteria text), so the
// corrective round's own -spec must carry exactly one criteria section,
// not the ticket spec plus a second, separately-appended one.
func TestReviewCorrectiveRoundAddendumEmbedsBuildSpecCriteriaOnce(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	codeReview := &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{
			{Severity: "high", File: "internal/foo.go", Line: 42, Summary: "bug", FailureScenario: "crash"},
		},
	}
	buildRunner := codeReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, codeReview, false, "")

	_, lastArgs := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	addendumPath := requestdrivertest.ArgValue(*lastArgs, "-spec")
	addendum := string(mustReadFile(t, addendumPath))
	if got := strings.Count(addendum, requestdriver.BuildSpecCriteriaHeading); got != 1 {
		t.Fatalf("addendum contains %q %d times, want exactly 1:\n%s", requestdriver.BuildSpecCriteriaHeading, got, addendum)
	}
	// buildingFixture's own tickets each claim both of twoCriteriaSpec's
	// criteria (validBrownfieldTicket("make verify", 1, 2)) -- both
	// criteria's own text must be present.
	if !strings.Contains(addendum, "A retried POST /refunds with the same idempotency key returns the original result.") {
		t.Errorf("addendum = %q, want criterion 1's text from the build spec", addendum)
	}
}

// TestReviewCorrectiveRoundBothGatesFailedOneRoundBothSections covers the
// "both failed -> one round, addendum has both sections" case: spec_conformity
// and code_review both flagged actionable content, and a single corrective
// round's addendum carries both.
func TestReviewCorrectiveRoundBothGatesFailedOneRoundBothSections(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	codeReview := &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{{Severity: "high", File: "a.go", Line: 5, Summary: "bug", FailureScenario: "crash"}},
	}
	buildRunner := codeReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, codeReview, true, "")

	calls, lastArgs := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch, PullRequestURL: "https://github.com/acme/app/pull/999"}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 1", *calls)
	}
	addendum := string(mustReadFile(t, requestdrivertest.ArgValue(*lastArgs, "-spec")))
	if !strings.Contains(addendum, "## Spec conformity review to address") {
		t.Errorf("addendum = %q, want the conformity section", addendum)
	}
	if !strings.Contains(addendum, "## Code review findings to address") {
		t.Errorf("addendum = %q, want the code-review section", addendum)
	}
}

// TestReviewCorrectiveRoundCodeReviewUnavailableNoRoundQuarantinesAsReviewUnavailable
// covers "code_review failed but no high finding (reviewer unavailable) ->
// no round, quarantine named review_unavailable": the code_review check's
// own advice ("the reviewer reported blocking defects") is wrong for a
// reviewer that reported nothing.
func TestReviewCorrectiveRoundCodeReviewUnavailableNoRoundQuarantinesAsReviewUnavailable(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	codeReview := &run.CodeReviewResult{Policy: "required", Available: false}
	buildRunner := codeReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, codeReview, false, "")

	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 0 (reviewer unavailable, nothing actionable)", *calls)
	}
	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != request.QuarantineCheckReviewUnavailable {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckReviewUnavailable)
	}
	if !strings.Contains(loaded.Error, "the review gave no verdict") {
		t.Errorf("Error = %q, want it to say the review gave no verdict", loaded.Error)
	}
}

// TestReviewCorrectiveRoundCodeReviewPlusAnotherGateNoRound covers
// "code_review + another gate (e.g. canonical_verify) failed -> no round".
func TestReviewCorrectiveRoundCodeReviewPlusAnotherGateNoRound(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	codeReview := &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{{Severity: "high", File: "a.go", Line: 1, Summary: "bug"}},
	}
	buildRunner := codeReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, codeReview, false, "canonical_verify")
	calls, _ := stubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
		return &run.Run{ID: roundRunID, State: run.StateAccepted}
	})

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("reviewCorrectiveRunner calls = %d, want 0 (canonical_verify also failed)", *calls)
	}
	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != "" {
		t.Errorf("QuarantineCheck = %q, want empty (canonical_verify also failed)", loaded.QuarantineCheck)
	}
}

// TestReviewCorrectiveRoundBudgetExhaustedQuarantineCheckCodeReview covers
// "budget exhausted -> quarantine check code_review": a code_review-only
// eligible quarantine that keeps failing the same way through its whole
// budget must quarantine with QuarantineCheckCodeReview, not
// QuarantineCheckSpecConformity, once the budget runs out.
func TestReviewCorrectiveRoundBudgetExhaustedQuarantineCheckCodeReview(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 1)
	branch := "factoryd/" + id + "-001"
	baseSHA := fmt.Sprintf("%040d", 1)
	codeReview := &run.CodeReviewResult{
		Policy: "required", Available: true,
		Findings: []run.CodeReviewFinding{{Severity: "high", File: "a.go", Line: 1, Summary: "still broken"}},
	}
	buildRunner := codeReviewQuarantinedBuildRunner(t, dataDir, branch, baseSHA, codeReview, false, "")

	origRunner := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = origRunner })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		roundRunID := requestdrivertest.ArgValue(args, "-ticket")
		if onReady != nil {
			onReady(&run.Run{ID: roundRunID})
		}
		rr := &run.Run{
			ID: roundRunID, State: run.StateQuarantined, HaltError: "gate failed: code_review",
			Branch: branch, BaseSHA: baseSHA,
			GateResults: []run.GateResult{
				{Check: "canonical_verify", Passed: true},
				{Check: "code_review", Passed: false},
			},
			CodeReview: codeReview,
		}
		return rr.Save(dataDir)
	}

	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	if err := driveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}

	loaded := loadRequest(t, dataDir, id)
	if loaded.State != request.StateQuarantined {
		t.Fatalf("State = %q, want %q", loaded.State, request.StateQuarantined)
	}
	if loaded.QuarantineCheck != request.QuarantineCheckCodeReview {
		t.Errorf("QuarantineCheck = %q, want %q", loaded.QuarantineCheck, request.QuarantineCheckCodeReview)
	}
}

// Every build that follows an earlier run of a request carries the commit the
// request started from as -instruction-base, so a review does not take an
// earlier build's unmerged instruction text as genuine: ticket N>1 gets the
// value recorded on ticket 1's run, ticket 1 the value recorded on its own
// earlier run. A ticket's very first build carries none.
func TestStackedTicketsCarryTheInstructionBase(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	ticketOneBase, ticketOneDiffBase := fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 3)
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	argsOf := func(ticket request.Ticket) (first, corrective []string) {
		t.Helper()
		first, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, cfg)
		if err != nil {
			t.Fatalf("BuildRequestBuildArgs ticket %d: %v", ticket.Index, err)
		}
		corrective, err = requestdriver.BuildReviewCorrectiveArgs(dataDir, r, ticket, cfg, ticket.SpecPath, "round-1", "some-branch", fmt.Sprintf("%040d", 2))
		if err != nil {
			t.Fatalf("BuildReviewCorrectiveArgs ticket %d: %v", ticket.Index, err)
		}
		return first, corrective
	}

	// Ticket 1's very first build has no earlier run: nothing to pass.
	if first, corrective := argsOf(r.Tickets[0]); requestdrivertest.HasFlag(first, "-instruction-base") || requestdrivertest.HasFlag(corrective, "-instruction-base") {
		t.Errorf("ticket 1 before any run carries -instruction-base: %v / %v", first, corrective)
	}

	ticketOne := &run.Run{ID: ticketRunID(id, 1), BaseSHA: ticketOneBase, InstructionBaseSHA: ticketOneBase}
	if err := ticketOne.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = ticketOne.ID
	for name, ticket := range map[string]request.Ticket{"ticket 1 (a retry)": r.Tickets[0], "ticket 2": r.Tickets[1]} {
		first, corrective := argsOf(ticket)
		for what, args := range map[string][]string{"first build and retry": first, "corrective round": corrective} {
			if got := requestdrivertest.ArgValue(args, "-instruction-base"); got != ticketOneBase {
				t.Errorf("%s %s: -instruction-base = %q, want ticket 1's base %q", name, what, got, ticketOneBase)
			}
		}
	}

	// A record from before the field existed: its diff base outranks its base.
	ticketOne.InstructionBaseSHA, ticketOne.DiffBaseSHA = "", ticketOneDiffBase
	if err := ticketOne.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if first, _ := argsOf(r.Tickets[1]); requestdrivertest.ArgValue(first, "-instruction-base") != ticketOneDiffBase {
		t.Errorf("-instruction-base = %q, want ticket 1's diff base %q", requestdrivertest.ArgValue(first, "-instruction-base"), ticketOneDiffBase)
	}
	ticketOne.InstructionBaseSHA = fmt.Sprintf("%040d", 4)
	if err := ticketOne.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if first, _ := argsOf(r.Tickets[1]); requestdrivertest.ArgValue(first, "-instruction-base") != ticketOne.InstructionBaseSHA {
		t.Errorf("-instruction-base = %q, want ticket 1's recorded instruction base", requestdrivertest.ArgValue(first, "-instruction-base"))
	}
}

// An earlier run that cannot be loaded or records no base halts the build
// with a reason, instead of reviewing against the diff base.
func TestInstructionBaseUnknownRefusesTheBuild(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	want := func(ticket request.Ticket, runID string) {
		t.Helper()
		_, err := requestdriver.BuildRequestBuildArgs(dataDir, r, ticket, cfg)
		prefix := fmt.Sprintf("ticket %d: cannot determine the commit the request started from (run %s ", ticket.Index, runID)
		if err == nil || !strings.Contains(err.Error(), prefix) || !strings.Contains(err.Error(), "a review would trust an earlier build's instruction files") {
			t.Errorf("ticket %d: err = %v, want a refusal beginning %q", ticket.Index, err, prefix)
		}
	}
	// Ticket 2 with no ticket 1 run, and with one that cannot be loaded.
	want(r.Tickets[1], "")
	r.Tickets[0].RunID = "no-such-run"
	want(r.Tickets[1], "no-such-run")
	// Ticket 1's own earlier run, unloadable.
	want(r.Tickets[0], "no-such-run")
	// A run that records no base at all.
	empty := &run.Run{ID: ticketRunID(id, 1)}
	if err := empty.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = empty.ID
	want(r.Tickets[1], empty.ID)
	// Ticket 1's own earlier run with a result but no base: something was
	// built, and what it was built on is unknown. (With no result either
	// it built nothing: TestARetryAfterARunThatRecordedNoBaseIsAFirstBuild.)
	empty.ResultSHA = fmt.Sprintf("%040d", 7)
	if err := empty.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	want(r.Tickets[0], empty.ID)
}

// TestARetryAfterARunThatRecordedNoBaseIsAFirstBuild: a ticket's run that
// halted before it recorded the commit it started from built nothing, so
// the build that follows it is given no -instruction-base (its own base is
// the request's) instead of the request being left with no retry that
// works. A later ticket still refuses: its base is an earlier build's output.
func TestARetryAfterARunThatRecordedNoBaseIsAFirstBuild(t *testing.T) {
	dp := newTestDeps(t)
	dataDir, id := buildingFixture(dp, t, 2)
	r, err := request.Load(dataDir, id)
	if err != nil {
		t.Fatal(err)
	}
	halted := &run.Run{ID: ticketRunID(id, 1), State: run.StateHalted}
	if err := halted.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	r.Tickets[0].RunID = halted.ID
	args, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], requestdriver.WorkerConfig{})
	if err != nil || requestdrivertest.HasFlag(args, "-instruction-base") {
		t.Fatalf("ticket 1 after a run with no base: err %v, args carry -instruction-base: %v; want a plain first build", err, requestdrivertest.HasFlag(args, "-instruction-base"))
	}
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[1], requestdriver.WorkerConfig{}); err == nil || !strings.Contains(err.Error(), "cannot determine the commit the request started from") {
		t.Fatalf("ticket 2 with a base-less ticket 1 run: err = %v, want the refusal", err)
	}
	// A run that recorded a result but no base is not "nothing built".
	halted.ResultSHA = fmt.Sprintf("%040d", 7)
	if err := halted.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := requestdriver.BuildRequestBuildArgs(dataDir, r, r.Tickets[0], requestdriver.WorkerConfig{}); err == nil {
		t.Fatal("ticket 1 after a run with a result and no base built without a refusal")
	}
}
