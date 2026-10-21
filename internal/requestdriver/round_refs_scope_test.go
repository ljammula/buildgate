package requestdriver_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// A `retry -from` choice is for the ticket it was made on: once that ticket is
// accepted, the next ticket builds as any first build does.
func TestRetryFromRoundDoesNotLeakToTheNextTicket(t *testing.T) {
	dp := newFakeDeps(t)
	recordRefs(dp)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 2)
	argsOf := map[string][]string{}
	phase := "quarantine"
	runner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		runID := ticket + "-" + phase
		onReady(&run.Run{ID: runID})
		argsOf[ticket] = args
		if phase == "quarantine" {
			return quarantinedRun(t, dataDir, runID, sha40(1), sha40(0xa1), "canonical_verify")
		}
		return (&run.Run{ID: runID, State: run.StateAccepted, Branch: "factoryd/" + runID, BaseSHA: sha40(1), ResultSHA: sha40(0xb1)}).Save(dataDir)
	}
	drive := func() {
		t.Helper()
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive()
	if _, err := request.RetryFromRound(dataDir, id, "alice", "", 1, time.Now(), nil); err != nil {
		t.Fatalf("RetryFromRound: %v", err)
	}
	phase = "accept"
	drive() // ticket 1, on the round-1 branch, accepted
	if got := requestdrivertest.ArgValue(argsOf[requestdrivertest.TicketRunID(id, 1)], "-on-branch"); got != "buildgate/"+id+"/t1-from-r1" {
		t.Fatalf("ticket 1 -on-branch = %q, want the round-1 branch", got)
	}
	mid := requestdrivertest.LoadRequest(t, dataDir, id)
	if mid.RetryFromRound != 0 || mid.RetryTicket != 0 {
		t.Errorf("after ticket 1 started: RetryFromRound %d RetryTicket %d, want the choice consumed", mid.RetryFromRound, mid.RetryTicket)
	}
	drive() // ticket 2
	second := argsOf[requestdrivertest.TicketRunID(id, 2)]
	if second == nil {
		t.Fatal("ticket 2 never built")
	}
	if requestdrivertest.HasFlag(second, "-on-branch") || requestdrivertest.HasFlag(second, "-diff-base") {
		t.Errorf("ticket 2 argv %v carries ticket 1's retry-from choice", second)
	}
}

func TestRetryChoiceAppliesOnlyToItsTicket(t *testing.T) {
	r := &request.Request{TicketIndex: 2}
	r.RetryFromScratch, r.RetryFromRound, r.RetryTicket = true, 3, 1
	if s, n := r.RetryChoiceFor(2); s || n != 0 {
		t.Errorf("ticket 2 got scratch %v round %d from a choice made on ticket 1", s, n)
	}
	if s, n := r.RetryChoiceFor(1); !s || n != 3 {
		t.Errorf("ticket 1 got scratch %v round %d, want its own choice", s, n)
	}
}

// A send-back re-plans the tickets, so a ticket's ledger starts again at round
// 1; the refs of the plan before it keep their commits.
func TestRoundRefsOfAnEarlierPlanSurviveASendBack(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id, rec := quarantinedAfterOneRound(t, dp)
	oldRef := request.RoundRef(id, 1, 1)
	if rec.refs[oldRef] != sha40(0xa1) {
		t.Fatalf("fixture: %s = %q", oldRef, rec.refs[oldRef])
	}
	// What re-planning leaves: the same ticket index, an empty ledger.
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.Tickets[0].RoundTags, r.Tickets[0].RunID = nil, ""
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	buildRunner := func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		runID := requestdrivertest.ArgValue(a, "-ticket") + "-replanned"
		onReady(&run.Run{ID: runID})
		return quarantinedRun(t, dataDir, runID, sha40(1), sha40(0xc1), "canonical_verify")
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	if rec.refs[oldRef] != sha40(0xa1) {
		t.Errorf("%s = %q after the re-plan, want the earlier plan's commit %s kept", oldRef, rec.refs[oldRef], sha40(0xa1))
	}
	tags := requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].RoundTags
	if len(tags) != 1 || tags[0].N != 2 || tags[0].Ref != request.RoundRef(id, 1, 2) || rec.refs[tags[0].Ref] != sha40(0xc1) {
		t.Fatalf("RoundTags = %+v refs %v, want the new round at the next free number, r2", tags, rec.refs)
	}
	if tags[0].Repo == "" {
		t.Error("the tag records no repository")
	}
}

func TestPruneRoundRefsKeepsThePushedTicketsBranches(t *testing.T) {
	dp := newFakeDeps(t)
	type call struct {
		dir, id string
		keep    []string
	}
	var calls []call
	dp.pruneRoundRefsFn = func(dir, id string, keep []string) error {
		calls = append(calls, call{dir, id, keep})
		return nil
	}
	r := &request.Request{ID: "req-1", Tickets: []request.Ticket{
		{Index: 1, RoundTags: []request.RoundTag{{N: 1, Repo: "/repo/a"}}},
		{Index: 2, PRURL: "https://github.com/acme/app/pull/2", RoundTags: []request.RoundTag{{N: 1, Repo: "/repo/a"}}},
		{Index: 3},
	}}
	requestdriver.PruneRoundRefs(dp, r)
	if len(calls) != 1 || calls[0].dir != "/repo/a" || calls[0].id != "req-1" || !reflect.DeepEqual(calls[0].keep, []string{"buildgate/req-1/t2-"}) {
		t.Errorf("prune calls = %+v, want one for /repo/a keeping ticket 2's branches", calls)
	}
}
