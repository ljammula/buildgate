package requestdriver_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

var orderGroups = [][]string{{"db/migrations/"}}

const (
	orderBase   = "1111111111111111111111111111111111111111"
	orderResult = "2222222222222222222222222222222222222222"
)

// mixedTicketRunner ends the ticket's run accepted with a change touching
// the migrations group and the files no group lists, its release decision
// denied for pr_order alone.
func mixedTicketRunner(t *testing.T, dataDir, requestID, runID string) requestdriver.TicketRunner {
	t.Helper()
	return func(_ context.Context, _ []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: runID, State: run.StateReady})
		record := &run.Run{
			ID: runID, RequestID: requestID, TicketIndex: 1, Project: "app", State: run.StateAccepted,
			BaseSHA: orderBase, ResultSHA: orderResult,
			ChangedFiles:  []string{"db/migrations/001.sql", "app/main.go"},
			ChangeSize:    &run.ChangeSize{SourceLines: 20, SourceFiles: 2, Functions: 1},
			ReleasePolicy: &run.ReleasePolicy{MaxChangedLines: 1000, MaxFilesChanged: 25},
			PROrder:       &run.PROrder{Groups: orderGroups},
		}
		if err := record.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		reasons := release.PROrderReasons(*record)
		if len(reasons) != 1 {
			t.Fatalf("PROrderReasons = %v, want one", reasons)
		}
		if err := release.SaveDecision(dataDir, release.Decision{RunID: runID, Project: "app", Allowed: false, Reasons: reasons}); err != nil {
			t.Fatal(err)
		}
		return nil
	}
}

// orderDeps is a fake whose git calls carve the two files of mixedTicketRunner.
func orderDeps(t *testing.T) (dp *fakeDeps, commits *[]string) {
	dp = newFakeDeps(t)
	commits = &[]string{}
	dp.changedPathsFn = func(_, base, result string) ([]string, error) {
		if base != orderBase || result != orderResult {
			t.Errorf("ChangedPaths(%s, %s), want the run's diff base and result", base, result)
		}
		return []string{"app/main.go", "db/migrations/001.sql"}, nil
	}
	dp.carveChainFn = func(context.Context, string, string, string, [][]string) error { return nil }
	dp.carveCommitFn = func(_ context.Context, _, parent, _ string, paths []string, _ string) (string, error) {
		sha := fmt.Sprintf("%039d%d", 0, len(*commits)+3)
		*commits = append(*commits, parent+" "+strings.Join(paths, ","))
		return sha, nil
	}
	dp.moveBranchFn = func(string, string, string) error { return nil }
	return dp, commits
}

func orderRequest(t *testing.T) (dataDir, id string) {
	t.Helper()
	dataDir, id = requestdrivertest.ApprovedPlanningFixture(t, requestdrivertest.TwoCriteriaSpec, "make verify")
	requestdrivertest.PlanReviewOff(t, dataDir, id)
	driveWithSpecPlanner(t, dataDir, requestdrivertest.FailingPlanTicketsRunner(t))
	return dataDir, id
}

func advanceOrder(t *testing.T, dp requestdriver.Deps, dataDir, id string, runner requestdriver.TicketRunner) *request.Request {
	t.Helper()
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, runner, time.Now()); err != nil {
		t.Fatal(err)
	}
	return requestdrivertest.LoadRequest(t, dataDir, id)
}

func TestMixedRunIsCarvedIntoTicketsInGroupOrder(t *testing.T) {
	dp, commits := orderDeps(t)
	dataDir, id := orderRequest(t)
	r := advanceOrder(t, dp, dataDir, id, mixedTicketRunner(t, dataDir, id, "mixed-1"))
	if r.State != request.StateBuilding || r.TicketCount != 2 || len(r.Tickets) != 2 || r.TicketIndex != 1 {
		t.Fatalf("state %s, %d tickets, index %d; want building with two carved tickets", r.State, len(r.Tickets), r.TicketIndex)
	}
	if want := []string{orderBase + " db/migrations/001.sql", "0000000000000000000000000000000000000003 app/main.go"}; fmt.Sprint(*commits) != fmt.Sprint(want) {
		t.Fatalf("carved commits %v, want %v: group order, each on the one before", *commits, want)
	}
	for i, group := range []int{1, 2} {
		o := r.Tickets[i].Order
		if o == nil || o.Group != group || o.SourceRunID != "mixed-1" || o.First != 1 || o.Commit == "" || o.Branch == "" {
			t.Fatalf("ticket %d order = %+v, want group %d of run mixed-1", i+1, o, group)
		}
	}
	if r.Tickets[1].Order.Onto != r.Tickets[0].Order.Commit || r.Tickets[0].Order.Onto != orderBase {
		t.Fatalf("carved tickets sit on %s and %s, want the diff base then the first carved commit", r.Tickets[0].Order.Onto, r.Tickets[1].Order.Onto)
	}
	if err := requestdriver.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("carved tickets are not pinned: %v", err)
	}
}

func TestCarvedTicketBuildsAsAReplayRunAndARefusalGoesToPlanningOnce(t *testing.T) {
	dp, _ := orderDeps(t)
	dataDir, id := orderRequest(t)
	advanceOrder(t, dp, dataDir, id, mixedTicketRunner(t, dataDir, id, "mixed-1"))
	var args []string
	refused := func(_ context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		onReady(&run.Run{ID: "carved-1", State: run.StateReady})
		record := &run.Run{ID: "carved-1", RequestID: id, TicketIndex: 1, Project: "app", State: run.StateQuarantined}
		if err := record.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	r := advanceOrder(t, dp, dataDir, id, func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		err := refused(ctx, a, onReady)
		record, loadErr := run.Load(dataDir, "carved-1")
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		record.Replay = &run.Replay{Carve: &run.Carve{Groups: orderGroups, Group: 1}}
		if saveErr := record.Save(dataDir); saveErr != nil {
			t.Fatal(saveErr)
		}
		return err
	})
	joined := " " + strings.Join(args, " ") + " "
	for _, want := range []string{" -order-group 1 ", " -on-branch buildgate/" + id + "/t1-carve ", " -diff-base " + orderBase + " ", " -replay ", " -scope-approval none "} {
		if !strings.Contains(joined, want) {
			t.Errorf("carved build argv lacks %q: %v", want, args)
		}
	}
	if strings.Contains(joined, " -prior-run ") {
		t.Errorf("carved build argv chains -prior-run: %v", args)
	}
	if r.State != request.StatePlanning || r.SizeReplan == nil || r.SizeReplan.RunID != "mixed-1" || len(r.SizeReplan.Groups) != 1 || r.SizeReplan.Measurement.SourceFiles != 2 {
		t.Fatalf("state %s, replan %+v; want planning with the source run's measurement and the groups", r.State, r.SizeReplan)
	}
	if err := r.ReturnToPlanningForOrder("mixed-1", r.SizeReplan.Measurement, orderGroups, "again", time.Now()); err == nil {
		t.Fatal("a second move to planning for order was accepted")
	}
}

func TestMixedRunWhoseCarveChainDoesNotMatchGoesToPlanning(t *testing.T) {
	dp, _ := orderDeps(t)
	dp.carveChainFn = func(context.Context, string, string, string, [][]string) error { return fmt.Errorf("tree mismatch") }
	dataDir, id := orderRequest(t)
	r := advanceOrder(t, dp, dataDir, id, mixedTicketRunner(t, dataDir, id, "mixed-1"))
	if r.State != request.StatePlanning || r.SizeReplan == nil || len(r.SizeReplan.Groups) != 1 || !strings.Contains(r.History[len(r.History)-1].Reason, "tree mismatch") {
		t.Fatalf("state %s, replan %+v; want planning with the groups and the mismatch", r.State, r.SizeReplan)
	}
}

func TestMixedRunOfAHandedOverOrReplannedPlanHaltsWithoutCarving(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(*request.Request)
	}{
		{"handed over", func(r *request.Request) { r.PlanImported = true }},
		{"replanned", func(r *request.Request) { r.SizeReplan = &request.SizeReplan{RunID: "earlier"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dp := newFakeDeps(t) // every carve call refuses
			dataDir, id := orderRequest(t)
			r := requestdrivertest.LoadRequest(t, dataDir, id)
			tc.set(r)
			if err := r.Save(dataDir); err != nil {
				t.Fatal(err)
			}
			r = advanceOrder(t, dp, dataDir, id, mixedTicketRunner(t, dataDir, id, "mixed-1"))
			if r.State != request.StateHalted || !strings.Contains(r.Error, "pr_order group") || len(r.Tickets) != 1 {
				t.Fatalf("state %s, error %q, %d tickets; want a halt naming the groups, no carve", r.State, r.Error, len(r.Tickets))
			}
		})
	}
}

func TestRunWithoutPROrderIsNotCarved(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := orderRequest(t)
	runner := func(_ context.Context, _ []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: "plain-1", State: run.StateReady})
		record := &run.Run{ID: "plain-1", RequestID: id, Project: "app", State: run.StateAccepted, ChangedFiles: []string{"db/migrations/001.sql", "app/main.go"}}
		if err := record.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		return nil
	}
	r := advanceOrder(t, dp, dataDir, id, runner)
	if len(r.Tickets) != 1 || r.SizeReplan != nil {
		t.Fatalf("%d tickets, replan %+v; a run with no pr_order is neither carved nor re-planned", len(r.Tickets), r.SizeReplan)
	}
}

func TestCarvedReplayWhoseReviewGaveNoVerdictWaitsInResumeReview(t *testing.T) {
	dp, _ := orderDeps(t)
	dataDir, id := orderRequest(t)
	advanceOrder(t, dp, dataDir, id, mixedTicketRunner(t, dataDir, id, "mixed-1"))
	r := advanceOrder(t, dp, dataDir, id, func(_ context.Context, _ []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: "carved-1", State: run.StateReady})
		record := &run.Run{
			ID: "carved-1", RequestID: id, TicketIndex: 1, Project: "app", State: run.StateQuarantined,
			Replay:      &run.Replay{Carve: &run.Carve{Groups: orderGroups, Group: 1}},
			GateResults: []run.GateResult{{Check: "code_review", Passed: false}},
			CodeReview:  &run.CodeReviewResult{Policy: "required", Available: false},
		}
		return record.Save(dataDir)
	})
	if r.State == request.StatePlanning || r.SizeReplan != nil {
		t.Fatalf("state %s, replan %+v: a review with no verdict used up the move to planning", r.State, r.SizeReplan)
	}
	if r.State != request.StateResumeReview {
		t.Fatalf("state %s, want resume_review", r.State)
	}
}

// A branch that cannot be made leaves the request's tickets and their files
// as they were: the move to planning saves the record that describes the files.
func TestCarveThatFailsAfterTheCommitsLeavesTheTicketFilesAlone(t *testing.T) {
	dp, _ := orderDeps(t)
	dp.moveBranchFn = func(string, string, string) error { return fmt.Errorf("ref locked") }
	dataDir, id := orderRequest(t)
	before := requestdrivertest.LoadRequest(t, dataDir, id)
	r := advanceOrder(t, dp, dataDir, id, mixedTicketRunner(t, dataDir, id, "mixed-1"))
	if r.State != request.StatePlanning || len(r.Tickets) != len(before.Tickets) {
		t.Fatalf("state %s, %d tickets (was %d); want planning with the tickets unchanged", r.State, len(r.Tickets), len(before.Tickets))
	}
	for _, tk := range r.Tickets {
		if tk.Order != nil {
			t.Errorf("ticket %d is carved after a failed carve", tk.Index)
		}
	}
	if err := requestdriver.VerifyApprovedHashes(dataDir, r); err != nil {
		t.Fatalf("hashes no longer describe the files: %v", err)
	}
}

// A mixed run's red and broken challenge files get no round; the carve record
// of the first carved ticket carries a line for each, for its pull request,
// and the later tickets' records carry none.
func TestCarvedFirstTicketCarriesTheMixedRunsChallengeFiles(t *testing.T) {
	dp, _ := orderDeps(t)
	dataDir, id := orderRequest(t)
	mixed := mixedTicketRunner(t, dataDir, id, "mixed-1")
	advanceOrder(t, dp, dataDir, id, func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		if err := mixed(ctx, a, onReady); err != nil {
			return err
		}
		record, err := run.Load(dataDir, "mixed-1")
		if err != nil {
			t.Fatal(err)
		}
		record.Challenge = &run.ChallengeRecord{Verdict: run.ChallengeRed, Files: []run.ChallengeFile{
			{Path: "app/edge_test.go", Criterion: "AC-2", Verdict: run.ChallengeRed},
			{Path: "app/odd_test.go", Criterion: "AC-1", Verdict: "broken"},
			{Path: "app/ok_test.go", Criterion: "AC-1", Verdict: run.ChallengeGreen},
		}}
		return record.Save(dataDir)
	})
	var args []string
	advanceOrder(t, dp, dataDir, id, func(_ context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		onReady(&run.Run{ID: "carved-1", State: run.StateReady})
		return (&run.Run{ID: "carved-1", RequestID: id, TicketIndex: 1, Project: "app", State: run.StateQuarantined}).Save(dataDir)
	})
	rp, err := run.ReadReplayFile(requestdrivertest.ArgValue(args, "-replay"))
	if err != nil {
		t.Fatal(err)
	}
	first := rp.Carve.Notes
	if len(first) != 2 || !strings.Contains(first[0], "app/edge_test.go") || !strings.Contains(first[0], "no challenge round ran") || !strings.Contains(first[1], "broken") {
		t.Errorf("first ticket's carve notes = %v, want one line for the red file and one for the broken file", first)
	}
}
