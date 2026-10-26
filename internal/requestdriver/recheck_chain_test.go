package requestdriver_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

// originRun is the run whose review held the reproduction seedTicketList
// puts on a ticket's list. It has no run record of its own: only the held
// copy under its review-repro directory.
const originRun = "run-origin"

// seedTicketList puts one reproduction on ticket index's list, as the request
// driver records a refusal, with its held copy on disk, and saves r.
func seedTicketList(t *testing.T, dataDir string, r *request.Request, index int) {
	t.Helper()
	requestdrivertest.ReproductionRefusedOn(t, dataDir, originRun, "", "", "")
	r.Tickets[index-1].NoteReproductionRefusal(requestdrivertest.HeldEntry(t, dataDir, originRun), originRun)
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

// assertRechecksTheTicketList checks that args' -recheck-reproductions
// manifest is exactly ticket index's list on the request on disk.
func assertRechecksTheTicketList(t *testing.T, dataDir, id string, index int, args []string) {
	t.Helper()
	list := requestdrivertest.LoadRequest(t, dataDir, id).Tickets[index-1].Reproductions
	if len(list) == 0 {
		t.Fatal("the ticket's list is empty")
	}
	m := readRecheckManifest(t, args)
	var want []string
	for _, rp := range list {
		want = append(want, fmt.Sprint(rp.RecheckEntry))
	}
	var got []string
	for _, e := range m.Reproductions {
		got = append(got, fmt.Sprint(e))
	}
	if !slices.Equal(got, want) {
		t.Errorf("manifest = %v, want the ticket's list %v", got, want)
	}
}

// prRoundAfter runs one PR-review round of a one-ticket request whose ticket
// has a reproduction on its list and whose last round is last (none when
// nil), and returns the round's args.
func prRoundAfter(t *testing.T, last *request.Round, lastRun *run.Run) (dataDir, id string, args []string) {
	t.Helper()
	dp := newFakeDeps(t)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "alice", Body: "fix", CommentID: 5}}
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	approveRequestSpec(t, dataDir, r)
	if lastRun != nil {
		if err := saveAcceptedRoundRun(t, dataDir, lastRun); err != nil {
			t.Fatal(err)
		}
	}
	if last != nil {
		r.Tickets[0].Rounds = []request.Round{*last}
	}
	seedTicketList(t, dataDir, r, 1)
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		return (&run.Run{ID: requestdrivertest.ArgValue(a, "-ticket"), State: run.StateQuarantined, GateResults: []run.GateResult{{Check: "lint", Passed: false}}}).Save(dataDir)
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 5}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if args == nil {
		t.Fatal("no round ran")
	}
	return dataDir, r.ID, args
}

// retryFromRoundOne rebuilds a request quarantined after one round, with a
// reproduction on its ticket's list, from round 1 (`retry -from 1`); with
// unreadable, round 1's run record is gone first.
func retryFromRoundOne(t *testing.T, unreadable bool) (dataDir, id string, args []string) {
	t.Helper()
	dp := newFakeDeps(t)
	dataDir, id, _ = quarantinedAfterOneRound(t, dp)
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if unreadable {
		// The ledger names a run whose record cannot be read.
		r.Tickets[0].RoundTags[0].RunID = "gone-run"
	}
	seedTicketList(t, dataDir, r, 1)
	if _, err := request.RetryFromRound(dataDir, id, "alice", "", 1, time.Now(), nil); err != nil {
		t.Fatalf("RetryFromRound: %v", err)
	}
	buildRunner := func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		args = a
		runID := requestdrivertest.ArgValue(a, "-ticket") + "-rebuilt"
		onReady(&run.Run{ID: runID})
		return quarantinedRun(t, dataDir, runID, sha40(0xa1), sha40(0xa3), "canonical_verify")
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	return dataDir, id, args
}

// SC-022: the ticket's list is the one home of its recheck chain. Every
// build of the ticket, whatever led to it and whatever the run before it
// recorded, is given -recheck-reproductions with exactly that list.
func TestEveryBuildOfATicketRechecksItsList(t *testing.T) {
	cases := map[string]func(t *testing.T) (dataDir, id string, index int, args []string){
		"an automatic corrective round": func(t *testing.T) (string, string, int, []string) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, "factoryd/"+id+"-001", sha40(1), "")
			_, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
				return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: "factoryd/" + id + "-001", PullRequestURL: "https://github.com/acme/app/pull/9"}
			})
			if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
				t.Fatalf("driveRequests: %v", err)
			}
			return dataDir, id, 1, *lastArgs
		},
		"a retry": func(t *testing.T) (string, string, int, []string) {
			dp := newFakeDeps(t)
			dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
			var builds [][]string
			buildRunner := refusedBuildRunner(t, dataDir, id, &builds)
			drive := func() {
				if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
					t.Fatalf("driveRequests: %v", err)
				}
			}
			drive()
			if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
				t.Fatal(err)
			}
			drive()
			if len(builds) != 2 {
				t.Fatalf("builds = %d, want the retry", len(builds))
			}
			return dataDir, id, 1, builds[1]
		},
		"a retry -from 1": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := retryFromRoundOne(t, false)
			return dataDir, id, 1, args
		},
		"a retry -from 1 whose round record is unreadable": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := retryFromRoundOne(t, true)
			return dataDir, id, 1, args
		},
		"a resume of a lost build with no record": func(t *testing.T) (string, string, int, []string) {
			dp := newFakeDeps(t)
			dataDir, id, _ := requestdrivertest.LostBuildFixture(dp, t, request.ResumeRound)
			r := requestdrivertest.LoadRequest(t, dataDir, id)
			seedTicketList(t, dataDir, r, 2)
			var calls [][]string
			requestdrivertest.AdvanceBuildingOnce(dp, t, dataDir, id, requestdriver.ResumeGate{Preconditions: &requestdrivertest.FakeResumePreconditions{T: t, OK: true}}, capturingBuildRunner(t, dataDir, &calls))
			if len(calls) != 1 || requestdrivertest.ArgValue(calls[0], "-resume-worktree-of") != "lost-run" || requestdrivertest.HasFlag(calls[0], "-earlier-attempt") {
				t.Fatalf("builds = %v, want the resume with no record", calls)
			}
			return dataDir, id, 2, calls[0]
		},
		"a PR round with no round before it": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := prRoundAfter(t, nil, nil)
			return dataDir, id, 1, args
		},
		"a PR round after a quarantined round": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := prRoundAfter(t, &request.Round{Index: 1, RunID: "q-run", Outcome: request.RoundQuarantined}, nil)
			return dataDir, id, 1, args
		},
		"a PR round after an accepted round": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := prRoundAfter(t, &request.Round{Index: 1, RunID: "a-run", Outcome: request.RoundAccepted, Pushed: true},
				&run.Run{ID: "a-run", State: run.StateAccepted, BaseSHA: sha40(1), ResultSHA: sha40(2)})
			return dataDir, id, 1, args
		},
		"a PR round after a halted round": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := prRoundAfter(t, &request.Round{Index: 1, RunID: "h-run", Outcome: request.RoundHalted}, nil)
			return dataDir, id, 1, args
		},
		"a PR round after a restack": func(t *testing.T) (string, string, int, []string) {
			dataDir, id, args := prRoundAfter(t, &request.Round{Index: 1, Kind: request.RestackRoundKind, RunID: "replay-run", Outcome: request.RoundAccepted, Pushed: true},
				&run.Run{ID: "replay-run", State: run.StateAccepted, BaseSHA: sha40(2), ResultSHA: sha40(3), Replay: &run.Replay{OfRunID: "run-1", OntoSHA: sha40(1), HeadSHA: sha40(3)}})
			return dataDir, id, 1, args
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			dataDir, id, index, args := build(t)
			assertRechecksTheTicketList(t, dataDir, id, index, args)
		})
	}
}

// SC-022: an operator's override of a run a reproduction refused takes that
// reproduction off the ticket's list, recorded with the override's author;
// the next build rechecks the rest of the list, and with nothing left it is
// given no recheck and is not refused.
func TestAnOverrideTakesTheReproductionOffTheTicketsList(t *testing.T) {
	dp := newFakeDeps(t)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "alice", Body: "fix", CommentID: 5}}
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	approveRequestSpec(t, dataDir, r)
	seedTicketList(t, dataDir, r, 1)
	// A second reproduction, which refused run o-run; the operator then
	// accepted o-run by override.
	overridden := requestdrivertest.HeldEntry(t, dataDir, originRun)
	overridden.Path, overridden.SHA256 = "internal/sum/overridden_repro_test.go", strings.Repeat("b", 64)
	r.Tickets[0].NoteReproductionRefusal(overridden, "o-run")
	o := &run.Run{ID: "o-run", State: run.StateQuarantined, BaseSHA: sha40(1), ResultSHA: sha40(2)}
	if err := o.ApplyOverride("bob", "the finding tests behaviour the spec does not ask for", run.StateAccepted, func() string { return time.Now().UTC().Format(time.RFC3339) }); err != nil {
		t.Fatal(err)
	}
	if err := o.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	var argvs [][]string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		argvs = append(argvs, a)
		return (&run.Run{ID: requestdrivertest.ArgValue(a, "-ticket"), State: run.StateQuarantined, GateResults: []run.GateResult{{Check: "lint", Passed: false}}}).Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 5}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 1 {
		t.Fatalf("builds = %d, want the round", len(argvs))
	}
	m := readRecheckManifest(t, argvs[0])
	if want := requestdrivertest.HeldEntry(t, dataDir, originRun); len(m.Reproductions) != 1 || fmt.Sprint(m.Reproductions[0]) != fmt.Sprint(want) {
		t.Errorf("manifest = %+v, want only %+v", m.Reproductions, want)
	}
	dropped := r.Tickets[0].DroppedReproductions
	if len(dropped) != 1 || dropped[0].Path != overridden.Path || dropped[0].By != "bob" || !strings.Contains(dropped[0].Why, "o-run") {
		t.Errorf("dropped = %+v, want the overridden one, by bob, naming o-run", dropped)
	}

	// The rest of the list overridden too: no recheck, and no refusal.
	r.Tickets[0].Reproductions[0].RefusedRuns = append(r.Tickets[0].Reproductions[0].RefusedRuns, "o-run")
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 2 || requestdrivertest.HasFlag(argvs[1], "-recheck-reproductions") || len(r.Tickets[0].Reproductions) != 0 {
		t.Errorf("builds = %d, list %+v; want a round with no recheck and an empty list", len(argvs), r.Tickets[0].Reproductions)
	}
}

// SC-022: a held copy that is gone halts the request before any build, with
// the operator's way out in its reason; `factoryd retry` takes that
// reproduction off the list, recorded on the ticket, and the round then runs.
func TestAMissingHeldCopyHaltsAndARetryDropsIt(t *testing.T) {
	dp := newFakeDeps(t)
	threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "alice", Body: "fix", CommentID: 5}}
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	approveRequestSpec(t, dataDir, r)
	seedTicketList(t, dataDir, r, 1)
	if err := os.Remove(requestdrivertest.HeldEntry(t, dataDir, originRun).HostPath); err != nil {
		t.Fatal(err)
	}
	stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: threads, ActionableThreads: threads}, nil)
	var argvs [][]string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		argvs = append(argvs, a)
		return (&run.Run{ID: requestdrivertest.ArgValue(a, "-ticket"), State: run.StateQuarantined, GateResults: []run.GateResult{{Check: "lint", Passed: false}}}).Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 5}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, r.ID)
	if len(argvs) != 0 || loaded.State != request.StateHalted || loaded.HaltKind != request.HaltReproductionUnheld || !strings.Contains(loaded.Error, "factoryd retry "+r.ID) || !strings.Contains(loaded.NextAction(), "factoryd retry "+r.ID) {
		t.Fatalf("builds %d, state %s kind %q error %q; want halted before any build, naming the retry", len(argvs), loaded.State, loaded.HaltKind, loaded.Error)
	}
	retried, err := request.Retry(dataDir, r.ID, "alice", "", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried.Tickets[0].Reproductions) != 0 || len(retried.Tickets[0].DroppedReproductions) != 1 || retried.Tickets[0].DroppedReproductions[0].By != "alice" || !strings.Contains(retried.Tickets[0].DroppedReproductions[0].Why, "held copy") {
		t.Fatalf("after the retry: list %+v, dropped %+v; want it dropped by alice", retried.Tickets[0].Reproductions, retried.Tickets[0].DroppedReproductions)
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, retried, cfg, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 1 || requestdrivertest.HasFlag(argvs[0], "-recheck-reproductions") {
		t.Errorf("builds %v; want the round, with no recheck", argvs)
	}
}
