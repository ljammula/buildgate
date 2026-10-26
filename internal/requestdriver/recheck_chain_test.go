package requestdriver_test

import (
	"context"
	"errors"
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
	"buildgate/internal/unresolved"
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
	dropped := r.DroppedReproductions
	if len(dropped) != 1 || dropped[0].Path != overridden.Path || dropped[0].By != "bob" || !strings.Contains(dropped[0].Why, "o-run") {
		t.Errorf("dropped = %+v, want the overridden one, by bob, naming o-run", dropped)
	}

	// The rest of the list refused only overridden runs too: no recheck,
	// and no refusal.
	r.Tickets[0].Reproductions[0].RefusedRuns = []string{"o-run"}
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
	if len(argvs) != 0 || loaded.State != request.StateHalted || loaded.HaltKind != request.HaltRecheckUnheld || !strings.Contains(loaded.Error, "factoryd retry "+r.ID) || !strings.Contains(loaded.NextAction(), "factoryd retry "+r.ID) {
		t.Fatalf("builds %d, state %s kind %q error %q; want halted before any build, naming the retry", len(argvs), loaded.State, loaded.HaltKind, loaded.Error)
	}
	retried, err := request.Retry(dataDir, r.ID, "alice", "", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(retried.Tickets[0].Reproductions) != 0 || len(retried.DroppedReproductions) != 1 || retried.DroppedReproductions[0].By != "alice" || !strings.Contains(retried.DroppedReproductions[0].Why, "held copy") {
		t.Fatalf("after the retry: list %+v, dropped %+v; want it dropped by alice", retried.Tickets[0].Reproductions, retried.DroppedReproductions)
	}
	if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, retried, cfg, time.Now().Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if len(argvs) != 1 || requestdrivertest.HasFlag(argvs[0], "-recheck-reproductions") {
		t.Errorf("builds %v; want the round, with no recheck", argvs)
	}
}

// manifestHas reports whether args' -recheck-reproductions manifest holds e.
func manifestHas(t *testing.T, args []string, e run.RecheckEntry) bool {
	t.Helper()
	if !requestdrivertest.HasFlag(args, "-recheck-reproductions") {
		return false
	}
	return slices.ContainsFunc(readRecheckManifest(t, args).Reproductions, func(x run.RecheckEntry) bool { return fmt.Sprint(x) == fmt.Sprint(e) })
}

// diskListHas reports whether ticket 1's list on disk holds e.
func diskListHas(t *testing.T, dataDir, id string, e run.RecheckEntry) bool {
	t.Helper()
	return slices.ContainsFunc(requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].Reproductions, func(rp request.TicketReproduction) bool {
		return request.ReproductionKey(rp.RecheckEntry) == request.ReproductionKey(e)
	})
}

// SC-022: what refused a build is on the ticket's list on disk as soon as
// the build ends, so a worker that stops before the request records the
// outcome (here, inside the automatic corrective round that follows) loses
// nothing: the build that resumes the ticket rechecks it.
func TestARefusalSurvivesAWorkerStoppedInTheCorrectiveRound(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := requestdrivertest.ReviewQuarantinedBuildRunner(t, dataDir, "factoryd/"+id+"-001", sha40(1), "")
	ctx, cancel := context.WithCancel(context.Background())
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(rctx context.Context, args []string, onReady func(*run.Run)) error {
		onReady(&run.Run{ID: requestdrivertest.ArgValue(args, "-ticket")})
		cancel()
		return errors.New("context canceled")
	}
	cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
	if err := requestdrivertest.DriveRequests(dp, ctx, dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err == nil {
		t.Fatal("driveRequests: want the stopped round's error")
	}
	e := requestdrivertest.HeldEntry(t, dataDir, id+"-001")
	if !diskListHas(t, dataDir, id, e) {
		t.Fatalf("the list on disk after the stop = %+v, want the first build's refusal", requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].Reproductions)
	}
	var builds [][]string
	next := func(c context.Context, args []string, onReady func(*run.Run)) error {
		builds = append(builds, args)
		return requestdrivertest.AcceptingBuildRunner(t, dataDir)(c, args, onReady)
	}
	requestdriver.ReviewCorrectiveRunner = next
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), next); err != nil {
		t.Fatalf("driveRequests after the restart: %v", err)
	}
	if len(builds) == 0 || !manifestHas(t, builds[0], e) {
		t.Errorf("builds after the restart = %v, want the next one to recheck %+v", builds, e)
	}
}

// SC-022: a build refused and never noted (the worker stopped between the
// run's end and the note) is still rechecked by the next build: the build
// notes again what refused every run recorded on the ticket first.
func TestARefusalNeverNotedIsRecheckedByTheNextBuild(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds [][]string
	buildRunner := refusedBuildRunner(t, dataDir, id, &builds)
	drive := func() {
		t.Helper()
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive()
	// The note never happened.
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	r.Tickets[0].Reproductions = nil
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	drive()
	if e := requestdrivertest.HeldEntry(t, dataDir, id+"-001"); len(builds) != 2 || !manifestHas(t, builds[1], e) {
		t.Errorf("builds = %d; want the retry to recheck %+v", len(builds), e)
	}
}

// SC-022: a request that left building while its build ran (here, moved to
// resume_review) keeps what refused the build on the ticket's list.
func TestARefusalIsNotedWhenTheRequestLeftBuilding(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: ticket})
		unlock, err := request.Lock(dataDir, id)
		if err != nil {
			return err
		}
		cur, err := request.Load(dataDir, id)
		if err == nil {
			err = cur.EnterResumeReview(request.StateBuilding, request.LostStep{RunID: ticket, Cause: unresolved.WorkerLost}, time.Now())
		}
		if err == nil {
			err = cur.Save(dataDir)
		}
		unlock()
		if err != nil {
			return err
		}
		rr := requestdrivertest.ReproductionRefusedOn(t, dataDir, ticket, "factoryd/"+ticket, sha40(1), sha40(2))
		return saveAsBuiltFrom(t, dataDir, id, args, rr)
	}
	if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
		t.Fatalf("driveRequests: %v", err)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if loaded.State != request.StateResumeReview || !diskListHas(t, dataDir, id, requestdrivertest.HeldEntry(t, dataDir, id+"-001")) {
		t.Errorf("state %s, list %+v; want resume_review with the refusal on the list", loaded.State, loaded.Tickets[0].Reproductions)
	}
}

// SC-022: what a retry dropped (a held copy that stayed missing) is not
// added back by the build-time safety net, though the run it refused is
// still recorded on the ticket.
func TestARetryDropIsNotAddedBack(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds [][]string
	refused := refusedBuildRunner(t, dataDir, id, &builds)
	drive := func(runner requestdriver.TicketRunner) {
		t.Helper()
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive(refused)
	e := requestdrivertest.HeldEntry(t, dataDir, id+"-001")
	if err := os.Remove(e.HostPath); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	drive(refused)
	if got := requestdrivertest.LoadRequest(t, dataDir, id); len(builds) != 1 || got.HaltKind != request.HaltRecheckUnheld {
		t.Fatalf("builds %d, halt kind %q; want no build and the unheld halt", len(builds), got.HaltKind)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var after [][]string
	drive(func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		after = append(after, args)
		return requestdrivertest.AcceptingBuildRunner(t, dataDir)(ctx, args, onReady)
	})
	loaded := requestdrivertest.LoadRequest(t, dataDir, id)
	if len(after) != 1 || requestdrivertest.HasFlag(after[0], "-recheck-reproductions") || len(loaded.Tickets[0].Reproductions) != 0 || !loaded.RefusalDropped(1, e, id+"-001") {
		t.Errorf("builds %v, list %+v, dropped %+v; want a build with no recheck and the drop kept", after, loaded.Tickets[0].Reproductions, loaded.DroppedReproductions)
	}
}

// SC-022: what an override dropped is not added back by the build-time
// safety net, round after round, though the overridden run is still the
// ticket's run.
func TestAnOverrideDropIsNotAddedBack(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	approveRequestSpec(t, dataDir, r)
	head, err := run.Load(dataDir, r.Tickets[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	refused := requestdrivertest.ReproductionRefusedOn(t, dataDir, head.ID, "", head.BaseSHA, head.ResultSHA)
	refused.Project, refused.RequestID, refused.TicketIndex = head.Project, r.ID, 1
	if err := refused.ApplyOverride("bob", "not what the spec asks", run.StateAccepted, func() string { return time.Now().UTC().Format(time.RFC3339) }); err != nil {
		t.Fatal(err)
	}
	if err := refused.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var argvs [][]string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		argvs = append(argvs, a)
		return (&run.Run{ID: requestdrivertest.ArgValue(a, "-ticket"), State: run.StateQuarantined, GateResults: []run.GateResult{{Check: "lint", Passed: false}}}).Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 5}
	for i, thread := range []forge.Thread{
		{ID: "thread-1", Path: "a.go", Line: 1, Author: "alice", Body: "fix", CommentID: 5},
		{ID: "thread-2", Path: "b.go", Line: 2, Author: "alice", Body: "and this", CommentID: 6},
	} {
		stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: []forge.Thread{thread}, ActionableThreads: []forge.Thread{thread}}, nil)
		if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now().Add(time.Duration(i)*2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, r.ID)
	e := requestdrivertest.HeldEntry(t, dataDir, head.ID)
	if len(argvs) != 2 || requestdrivertest.HasFlag(argvs[0], "-recheck-reproductions") || requestdrivertest.HasFlag(argvs[1], "-recheck-reproductions") || len(loaded.Tickets[0].Reproductions) != 0 || !loaded.RefusalDropped(1, e, head.ID) {
		t.Errorf("builds %d, list %+v, dropped %+v; want two rounds with no recheck and the drop kept", len(argvs), loaded.Tickets[0].Reproductions, loaded.DroppedReproductions)
	}
}

// SC-022: a drop covers only the refusals it was made for. After an
// override dropped a reproduction for run X, a later run Z refused by the
// identical reproduction puts it back on the list, for Z alone: the safety
// net reading run X again does not add X's refusal back, and the next round
// rechecks Z's copy.
func TestADropCoversOnlyTheRefusalsItWasMadeFor(t *testing.T) {
	dp := newFakeDeps(t)
	r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
	approveRequestSpec(t, dataDir, r)
	head, err := run.Load(dataDir, r.Tickets[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	x := requestdrivertest.ReproductionRefusedOn(t, dataDir, head.ID, "", head.BaseSHA, head.ResultSHA)
	x.Project, x.RequestID, x.TicketIndex = head.Project, r.ID, 1
	if err := x.ApplyOverride("bob", "not what the spec asks", run.StateAccepted, func() string { return time.Now().UTC().Format(time.RFC3339) }); err != nil {
		t.Fatal(err)
	}
	if err := x.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	var argvs [][]string
	var z string
	requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		argvs = append(argvs, a)
		id := requestdrivertest.ArgValue(a, "-ticket")
		if len(argvs) == 1 {
			z = id
			return requestdrivertest.ReproductionRefusedOn(t, dataDir, id, "", "", "").Save(dataDir)
		}
		return (&run.Run{ID: id, State: run.StateQuarantined, GateResults: []run.GateResult{{Check: "lint", Passed: false}}}).Save(dataDir)
	}
	cfg := requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 5}
	for i, thread := range []forge.Thread{
		{ID: "thread-1", Path: "a.go", Line: 1, Author: "alice", Body: "fix", CommentID: 5},
		{ID: "thread-2", Path: "b.go", Line: 2, Author: "alice", Body: "and this", CommentID: 6},
	} {
		stubPRReviewDeps(dp, t, forge.ReviewState{State: "OPEN", BlocksReadyThreads: []forge.Thread{thread}, ActionableThreads: []forge.Thread{thread}}, nil)
		if err := requestdriver.AdvancePRReview(dp, context.Background(), dataDir, r, cfg, time.Now().Add(time.Duration(i)*2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if len(argvs) < 2 {
		t.Fatalf("builds = %d, want two rounds", len(argvs))
	}
	if requestdrivertest.HasFlag(argvs[0], "-recheck-reproductions") {
		t.Errorf("round 1 rechecks the overridden reproduction: %v", argvs[0])
	}
	if want := requestdrivertest.HeldEntry(t, dataDir, z); !manifestHas(t, argvs[1], want) || len(readRecheckManifest(t, argvs[1]).Reproductions) != 1 {
		t.Errorf("round 2's manifest = %+v, want only Z's copy %+v", readRecheckManifest(t, argvs[1]).Reproductions, want)
	}
	loaded := requestdrivertest.LoadRequest(t, dataDir, r.ID)
	if list := loaded.Tickets[0].Reproductions; len(list) != 1 || !slices.Equal(list[0].RefusedRuns, []string{z}) {
		t.Errorf("list = %+v, want the reproduction back for run %s alone", list, z)
	}
}

// SC-022: a send-back's drops are keyed by the old tickets' runs, so the
// same reproduction refusing a run of the new ticket 1 is noted.
func TestASendBackDropDoesNotCoverTheNewTicketsRefusals(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	var builds [][]string
	drive := func(runner requestdriver.TicketRunner) {
		t.Helper()
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, requestdriver.WorkerConfig{}, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), runner); err != nil {
			t.Fatalf("driveRequests: %v", err)
		}
	}
	drive(refusedBuildRunner(t, dataDir, id, &builds))
	before := requestdrivertest.LoadRequest(t, dataDir, id)
	sent, err := request.SendBack(dataDir, id, "alice", "a different plan", request.SendBackToPlan, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(sent.DroppedReproductions) != 1 || !slices.Equal(sent.DroppedReproductions[0].RefusedRuns, []string{id + "-001"}) {
		t.Fatalf("drops after the send-back = %+v, want one for run %s-001", sent.DroppedReproductions, id)
	}
	// The replan's new ticket 1, built again (stood in for by the old
	// ticket with its list and run cleared).
	before.DroppedReproductions = sent.DroppedReproductions
	before.Tickets[0].Reproductions, before.Tickets[0].RunID = nil, ""
	if err := before.Save(dataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := request.Retry(dataDir, id, "alice", "", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	drive(func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		runID := requestdrivertest.ArgValue(args, "-ticket") + "-new"
		onReady(&run.Run{ID: runID})
		rr := requestdrivertest.ReproductionRefusedOn(t, dataDir, runID, "factoryd/"+runID, sha40(1), sha40(2))
		return saveAsBuiltFrom(t, dataDir, id, args, rr)
	})
	list := requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].Reproductions
	if len(list) != 1 || !slices.Equal(list[0].RefusedRuns, []string{id + "-001-new"}) {
		t.Errorf("list = %+v, want the new ticket's refusal noted", list)
	}
}

// SC-022: a refusal stays on the plan that judged it. A run that started
// before the request was sent back and planned again ends refused: nothing
// is noted on the new plan's ticket 1.
func TestARefusalOfARunFromBeforeASendBackIsNotNoted(t *testing.T) {
	dp := newFakeDeps(t)
	dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
	buildRunner := func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		runID := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: runID})
		started := time.Now().Add(-time.Minute)
		// While the run builds: the request is quarantined, sent back to
		// plan and planned again, its new ticket 1 building.
		old := requestdrivertest.LoadRequest(t, dataDir, id)
		old.State = request.StateQuarantined
		if err := old.Save(dataDir); err != nil {
			return err
		}
		if _, err := request.SendBack(dataDir, id, "alice", "a different plan", request.SendBackToPlan, time.Now()); err != nil {
			return err
		}
		replanned := requestdrivertest.LoadRequest(t, dataDir, id)
		replanned.State, replanned.TicketCount, replanned.TicketIndex = request.StateBuilding, 1, 1
		replanned.Tickets = []request.Ticket{{Index: 1, SpecPath: old.Tickets[0].SpecPath}}
		if err := replanned.Save(dataDir); err != nil {
			return err
		}
		rr := requestdrivertest.ReproductionRefusedOn(t, dataDir, runID, "factoryd/"+runID, sha40(1), sha40(2))
		rr.CreatedAt = started.Format(time.RFC3339Nano)
		return saveAsBuiltFrom(t, dataDir, id, args, rr)
	}
	r := requestdrivertest.LoadRequest(t, dataDir, id)
	if err := requestdriver.AdvanceBuilding(dp, context.Background(), dataDir, r, requestdriver.WorkerConfig{}, buildRunner, time.Now()); err != nil {
		t.Fatalf("AdvanceBuilding: %v", err)
	}
	if list := requestdrivertest.LoadRequest(t, dataDir, id).Tickets[0].Reproductions; len(list) != 0 {
		t.Errorf("new ticket 1's list = %+v, want nothing from a run of the old plan", list)
	}
}
