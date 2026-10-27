package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/unresolved"
)

// The accepted run's review: one high finding, one medium, one low.
var adviceFindings = []run.CodeReviewFinding{
	{Severity: "high", File: "src/patch.ts", Line: 42, Summary: "frozen arrays lose their length on patch", FailureScenario: "produce(frozen, d => { d.length = 0 }) throws"},
	{Severity: "medium", File: "src/util.ts", Summary: "a medium concern about naming"},
	{Severity: "low", Summary: "a low concern about comments"},
}

// adviceFixture is a one-ticket request in building whose build is accepted
// with a pull request and the review findings given, plus the advice round's
// stand-in runner and the pull request's push seams.
type adviceFixture struct {
	dp                         *fakeDeps
	dataDir, id, branch, prURL string
	baseSHA, resultSHA         string
	buildRunner                requestdriver.TicketRunner
	cfg                        requestdriver.WorkerConfig
	pushed                     []string
}

func newAdviceFixture(t *testing.T, findings []run.CodeReviewFinding) *adviceFixture {
	t.Helper()
	f := &adviceFixture{dp: newFakeDeps(t), baseSHA: fmt.Sprintf("%040d", 1), resultSHA: fmt.Sprintf("%040d", 2)}
	f.dataDir, f.id = requestdrivertest.BuildingFixture(f.dp, t, 1)
	f.branch = "factoryd/" + f.id + "-001"
	f.prURL = "https://github.com/acme/app/pull/7"
	f.buildRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: ticket, State: run.StateReady})
		rr := &run.Run{
			ID: ticket, Ticket: ticket, Project: "app", State: run.StateAccepted, Branch: f.branch, BaseSHA: f.baseSHA, ResultSHA: f.resultSHA,
			PullRequestURL: f.prURL, CodeReview: &run.CodeReviewResult{Policy: "required", Available: true, Findings: findings},
		}
		return rr.Save(f.dataDir)
	}
	f.cfg = requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	f.cfg.Settings.ReviewAdviceRound = true
	stubPRReviewDeps(f.dp, t, forge.ReviewState{State: forge.PRStateOpen, IsDraft: true}, nil)
	f.dp.pushExistingBranchFn = func(ctx context.Context, workspaceDir, sha, branch string) error {
		f.pushed = append(f.pushed, branch+"@"+sha)
		requestdrivertest.LastPushedSHA[branch] = sha
		return nil
	}
	return f
}

// adviceRun is the advice round's run as its stand-in saves it: on the
// accepted run's branch, ending in state, with a release decision allowed or
// not.
func (f *adviceFixture) adviceRun(t *testing.T, id string, state run.State, allowed bool) *run.Run {
	t.Helper()
	rr := &run.Run{ID: id, Ticket: id, Project: "app", State: state, Branch: f.branch, BaseSHA: f.resultSHA, DiffBaseSHA: f.baseSHA, ResultSHA: fmt.Sprintf("%040d", 3), WorkspacePath: t.TempDir(), Attempts: []run.Attempt{{}}}
	if state == run.StateAccepted {
		decision := release.Decision{RunID: id, Project: "app", Allowed: allowed, Evaluated: time.Now().UTC().Format(time.RFC3339)}
		if !allowed {
			decision.Reasons = []string{"touches a protected path"}
		}
		if err := release.SaveDecision(f.dataDir, decision); err != nil {
			t.Fatal(err)
		}
	}
	return rr
}

func (f *adviceFixture) drive(t *testing.T, ctx context.Context, buildRunner requestdriver.TicketRunner) error {
	t.Helper()
	return requestdrivertest.DriveRequests(f.dp, ctx, f.dataDir, f.cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner)
}

// TestAdviceRoundAcceptedIsPushedAndBecomesTheTicketsRun: an accepted run
// whose review recorded a high finding is followed by one advice round on its
// branch, given that finding (and no medium or low one) as the record of the
// attempt, opening no pull request; accepted again, its result is pushed to
// the pull request's branch and its run is the ticket's, which a later
// ticket stacks on, and the request moves on as it would have.
func TestAdviceRoundAcceptedIsPushedAndBecomesTheTicketsRun(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	var roundRunID string
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		roundRunID = id
		return f.adviceRun(t, id, run.StateAccepted, true)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("advice round builds = %d, want 1", *calls)
	}
	f.assertAdviceArgs(t, *lastArgs)
	if want := []string{f.branch + "@" + fmt.Sprintf("%040d", 3)}; fmt.Sprint(f.pushed) != fmt.Sprint(want) {
		t.Errorf("pushed %v, want %v", f.pushed, want)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (%s)", loaded.State, request.StatePRReview, loaded.Error)
	}
	tk := loaded.Tickets[0]
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.AdviceRoundKind || tk.Rounds[0].Outcome != request.RoundAccepted || !tk.Rounds[0].Pushed {
		t.Fatalf("Rounds = %+v, want one accepted, pushed advice round", tk.Rounds)
	}
	if tk.RunID != roundRunID || tk.PRHeadRunID() != roundRunID || tk.Advice != nil {
		t.Errorf("ticket run %q, PR head %q, in flight %+v; want the advice round's run %q and none in flight", tk.RunID, tk.PRHeadRunID(), tk.Advice, roundRunID)
	}
	if tk.PRURL != f.prURL || tk.Branch != f.branch {
		t.Errorf("ticket PR %q branch %q, want the accepted run's %q %q", tk.PRURL, tk.Branch, f.prURL, f.branch)
	}
}

// assertAdviceArgs checks the advice round's build argv: its run id, the
// accepted run's branch and diff base, no pull request of its own, and the
// record holding the high finding and no other.
func (f *adviceFixture) assertAdviceArgs(t *testing.T, args []string) {
	t.Helper()
	original := requestdrivertest.TicketRunID(f.id, 1)
	if got := requestdrivertest.ArgValue(args, "-ticket"); got != original+"-advice1" {
		t.Errorf("-ticket = %q, want %s-advice1", got, original)
	}
	if got := requestdrivertest.ArgValue(args, "-on-branch"); got != f.branch {
		t.Errorf("-on-branch = %q, want %q", got, f.branch)
	}
	if got := requestdrivertest.ArgValue(args, "-diff-base"); got != f.baseSHA {
		t.Errorf("-diff-base = %q, want %q", got, f.baseSHA)
	}
	for _, flag := range []string{"-open-pull-request", "-pr-base", "-pr-closes-issue"} {
		if requestdrivertest.HasFlag(args, flag) {
			t.Errorf("the advice round's build has %s: its pull request is already open", flag)
		}
	}
	record, err := os.ReadFile(requestdrivertest.ArgValue(args, "-earlier-attempt"))
	if err != nil {
		t.Fatalf("read -earlier-attempt: %v", err)
	}
	for _, want := range []string{"That attempt was accepted", "frozen arrays lose their length on patch", "`src/patch.ts:42`", "produce(frozen, d => { d.length = 0 }) throws"} {
		if !strings.Contains(string(record), want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	for _, unwanted := range []string{"a medium concern", "a low concern"} {
		if strings.Contains(string(record), unwanted) {
			t.Errorf("the record carries %q:\n%s", unwanted, record)
		}
	}
	spec, err := os.ReadFile(requestdrivertest.ArgValue(args, "-spec"))
	if err != nil || strings.Contains(string(spec), "frozen arrays") {
		t.Errorf("the advice round's spec carries the findings (err %v):\n%s", err, spec)
	}

}

// TestAdviceRoundThatIsNotAcceptedLeavesTheAcceptedRunStanding: whatever
// else the advice round ends in, nothing is pushed, the accepted run stays
// the ticket's, and the request moves on exactly as without the round: it is
// never quarantined, halted or sent to resume_review for it.
func TestAdviceRoundThatIsNotAcceptedLeavesTheAcceptedRunStanding(t *testing.T) {
	cases := []struct {
		name    string
		outcome request.RoundOutcome
		runner  func(f *adviceFixture, t *testing.T) requestdriver.TicketRunner
	}{
		{"quarantined", request.RoundQuarantined, func(f *adviceFixture, t *testing.T) requestdriver.TicketRunner {
			return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
				id := requestdrivertest.ArgValue(args, "-ticket")
				onReady(&run.Run{ID: id})
				rr := f.adviceRun(t, id, run.StateQuarantined, false)
				rr.GateResults = []run.GateResult{{Check: "lint", ExitCode: 2}}
				return rr.Save(f.dataDir)
			}
		}},
		{"never started", request.RoundHalted, func(f *adviceFixture, t *testing.T) requestdriver.TicketRunner {
			return func(context.Context, []string, func(*run.Run)) error { return errors.New("sandbox launch refused") }
		}},
		{"no verdict on the work", request.RoundHalted, func(f *adviceFixture, t *testing.T) requestdriver.TicketRunner {
			return func(ctx context.Context, args []string, onReady func(*run.Run)) error {
				id := requestdrivertest.ArgValue(args, "-ticket")
				onReady(&run.Run{ID: id})
				rr := f.adviceRun(t, id, run.StateHalted, false)
				rr.HaltReasonCode = string(unresolved.WorkerLost)
				return rr.Save(f.dataDir)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, adviceFindings)
			orig := requestdriver.ReviewCorrectiveRunner
			t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
			requestdriver.ReviewCorrectiveRunner = tc.runner(f, t)
			if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
				t.Fatalf("drive: %v", err)
			}
			f.assertOriginalStands(t, tc.outcome)
		})
	}
}

// assertOriginalStands checks the request moved on with the accepted run as
// the ticket's, nothing pushed, and one advice round recorded as outcome.
func (f *adviceFixture) assertOriginalStands(t *testing.T, outcome request.RoundOutcome) {
	t.Helper()
	if len(f.pushed) != 0 {
		t.Errorf("pushed %v, want nothing", f.pushed)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q as without the round (%s)", loaded.State, request.StatePRReview, loaded.Error)
	}
	tk := loaded.Tickets[0]
	original := requestdrivertest.TicketRunID(f.id, 1)
	if tk.RunID != original || tk.PRHeadRunID() != original || tk.Advice != nil {
		t.Errorf("ticket run %q, PR head %q, in flight %+v; want the accepted run %q", tk.RunID, tk.PRHeadRunID(), tk.Advice, original)
	}
	if tk.PRURL != f.prURL {
		t.Errorf("ticket PR %q, want %q", tk.PRURL, f.prURL)
	}
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.AdviceRoundKind || tk.Rounds[0].Outcome != outcome || tk.Rounds[0].Pushed {
		t.Errorf("Rounds = %+v, want one advice round %s, not pushed", tk.Rounds, outcome)
	}
}

// TestAdviceRoundPushChecksThatRefuseLeaveTheAcceptedRunStanding: an
// accepted advice round whose release decision is not allowed, or whose
// result does not descend from the pull request's head, is not pushed; the
// accepted run stands and the request moves on, with the reason on the
// round.
func TestAdviceRoundPushChecksThatRefuseLeaveTheAcceptedRunStanding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		setup   func(f *adviceFixture)
		want    string
	}{
		{"decision denied", false, func(*adviceFixture) {}, "denied by release policy"},
		{"head not an ancestor", true, func(f *adviceFixture) {
			f.dp.roundResultDescendsFromHeadFn = func(string, string, string) (bool, error) { return false, nil }
		}, "is not an ancestor of round result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, adviceFindings)
			tc.setup(f)
			requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
				return f.adviceRun(t, id, run.StateAccepted, tc.allowed)
			})
			if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
				t.Fatalf("drive: %v", err)
			}
			f.assertOriginalStands(t, request.RoundAccepted)
			if got := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0].Rounds[0].Error; !strings.Contains(got, tc.want) || !strings.Contains(got, "advice round") {
				t.Errorf("round error %q, want it to name the advice round and %q", got, tc.want)
			}
		})
	}
}

// TestAdviceRoundPushThatFailsHalts: a push that failed once sent may have
// reached the remote, so the request halts for a human, as for a PR-review
// round, and the accepted run stays the ticket's.
func TestAdviceRoundPushThatFailsHalts(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.dp.pushExistingBranchFn = func(context.Context, string, string, string) error { return errors.New("remote hung up") }
	requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.adviceRun(t, id, run.StateAccepted, true)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State != request.StateHalted || !strings.Contains(loaded.Error, "accepted advice round") || !strings.Contains(loaded.Error, "remote hung up") {
		t.Fatalf("State %q error %q, want halted naming the advice round's push", loaded.State, loaded.Error)
	}
	tk := loaded.Tickets[0]
	if tk.RunID != requestdrivertest.TicketRunID(f.id, 1) || len(tk.Rounds) != 1 || tk.Rounds[0].Pushed || tk.Advice != nil {
		t.Errorf("ticket run %q rounds %+v in flight %+v, want the accepted run and one unpushed advice round", tk.RunID, tk.Rounds, tk.Advice)
	}
}

// TestNoAdviceRound: no round where none applies, and the request moves on
// exactly as before: the key off, no high finding, pull requests off.
func TestNoAdviceRound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings []run.CodeReviewFinding
		setup    func(f *adviceFixture)
	}{
		{"the key is off", adviceFindings, func(f *adviceFixture) { f.cfg.Settings.ReviewAdviceRound = false }},
		{"only medium and low findings", adviceFindings[1:], func(*adviceFixture) {}},
		{"no review", nil, func(*adviceFixture) {}},
		{"pull requests off", adviceFindings, func(f *adviceFixture) { f.cfg.OpenPullRequest = false; f.prURL = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, tc.findings)
			tc.setup(f)
			calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
				return f.adviceRun(t, id, run.StateAccepted, true)
			})
			if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
				t.Fatalf("drive: %v", err)
			}
			if *calls != 0 || len(f.pushed) != 0 {
				t.Fatalf("advice round builds %d, pushes %v; want none", *calls, f.pushed)
			}
			loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			tk := loaded.Tickets[0]
			if loaded.State != request.StatePRReview || tk.RunID != requestdrivertest.TicketRunID(f.id, 1) || len(tk.Rounds) != 0 {
				t.Errorf("State %q run %q rounds %+v, want pr_review on the accepted run with no round", loaded.State, tk.RunID, tk.Rounds)
			}
		})
	}
}

// TestAdviceRoundRunsOncePerTicket: a ticket that already had its advice
// round gets no second one, whatever its next accepted run's review says.
func TestAdviceRoundRunsOncePerTicket(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	r.Tickets[0].Rounds = []request.Round{{Index: 1, Kind: request.AdviceRoundKind, RunID: "earlier-advice", Outcome: request.RoundQuarantined, At: time.Now().UTC().Format(time.RFC3339Nano)}}
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.adviceRun(t, id, run.StateAccepted, true)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("advice round builds = %d, want none: the ticket had its round", *calls)
	}
	if loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id); loaded.State != request.StatePRReview || len(loaded.Tickets[0].Rounds) != 1 {
		t.Errorf("State %q rounds %+v, want pr_review with the one earlier round", loaded.State, loaded.Tickets[0].Rounds)
	}
}

// TestAdviceRoundInterruptedIsSettledNotRelaunched: a worker that stops
// mid-round leaves the round in flight on the ticket; the next pass settles
// it from its run, never building it or the ticket again: an accepted round
// is pushed, one whose run never started leaves the accepted run standing,
// and one whose run has not ended is waited for.
func TestAdviceRoundInterruptedIsSettledNotRelaunched(t *testing.T) {
	for _, tc := range []struct {
		name      string
		started   bool
		state     run.State
		wantRound request.RoundOutcome
		wantPush  bool
	}{
		{"the round's run was accepted", true, run.StateAccepted, request.RoundAccepted, true},
		{"the round's run was quarantined", true, run.StateQuarantined, request.RoundQuarantined, false},
		{"the round's run never started", false, "", request.RoundHalted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, adviceFindings)
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			orig := requestdriver.ReviewCorrectiveRunner
			t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
			requestdriver.ReviewCorrectiveRunner = func(_ context.Context, args []string, onReady func(*run.Run)) error {
				calls++
				defer cancel()
				if !tc.started {
					return errors.New("stopping")
				}
				id := requestdrivertest.ArgValue(args, "-ticket")
				onReady(&run.Run{ID: id})
				// The run is still going when the worker is told to stop.
				if err := f.adviceRun(t, id, run.StateVerifying, true).Save(f.dataDir); err != nil {
					t.Fatal(err)
				}
				return errors.New("stopping")
			}
			if err := f.drive(t, ctx, f.buildRunner); err == nil {
				t.Fatal("drive with the worker stopping mid-round returned no error")
			}
			stopped := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			if stopped.State != request.StateBuilding || stopped.Tickets[0].Advice == nil || len(stopped.Tickets[0].Rounds) != 0 {
				t.Fatalf("after the stop: State %q in flight %+v rounds %+v, want building with the round in flight", stopped.State, stopped.Tickets[0].Advice, stopped.Tickets[0].Rounds)
			}
			roundRunID := stopped.Tickets[0].RunID
			if tc.started {
				// While its run has not ended the pass waits, changing nothing.
				if err := f.drive(t, context.Background(), requestdrivertest.FailingBuildRunner(t)); err == nil || !strings.Contains(err.Error(), "has not ended") {
					t.Fatalf("a pass while the round's run is going = %v, want it to wait", err)
				}
				rr := f.adviceRun(t, roundRunID, tc.state, true)
				if err := rr.Save(f.dataDir); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.drive(t, context.Background(), requestdrivertest.FailingBuildRunner(t)); err != nil {
				t.Fatalf("drive after the restart: %v", err)
			}
			if calls != 1 {
				t.Errorf("advice round builds = %d, want the one before the stop", calls)
			}
			loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			tk := loaded.Tickets[0]
			if loaded.State != request.StatePRReview || tk.Advice != nil || len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.AdviceRoundKind || tk.Rounds[0].Outcome != tc.wantRound {
				t.Fatalf("State %q in flight %+v rounds %+v, want pr_review with one %s advice round", loaded.State, tk.Advice, tk.Rounds, tc.wantRound)
			}
			wantRun := requestdrivertest.TicketRunID(f.id, 1)
			if tc.wantPush {
				wantRun = roundRunID
			}
			if tk.RunID != wantRun || (len(f.pushed) == 1) != tc.wantPush {
				t.Errorf("ticket run %q pushed %v, want run %q and a push %t", tk.RunID, f.pushed, wantRun, tc.wantPush)
			}
		})
	}
}

// TestAdviceRoundCountsAgainstNeitherBudget: a ticket's advice round uses
// none of review_corrective_rounds (a quarantined build still gets its
// corrective round) and none of max_review_rounds (a PR-review round still
// runs).
func TestAdviceRoundCountsAgainstNeitherBudget(t *testing.T) {
	// A round that ran and was judged: Counted, so only its Kind keeps it out
	// of both budgets.
	adviceRound := request.Round{Index: 1, Kind: request.AdviceRoundKind, RunID: "advice-run", Outcome: request.RoundQuarantined, At: time.Now().UTC().Format(time.RFC3339Nano)}

	t.Run("review_corrective_rounds", func(t *testing.T) {
		dp := newFakeDeps(t)
		dataDir, id := requestdrivertest.BuildingFixture(dp, t, 1)
		r := requestdrivertest.LoadRequest(t, dataDir, id)
		r.Tickets[0].Rounds = []request.Round{adviceRound}
		if err := r.Save(dataDir); err != nil {
			t.Fatal(err)
		}
		branch := "factoryd/" + id + "-001"
		buildRunner := checkQuarantinedBuildRunner(t, dataDir, branch, fmt.Sprintf("%040d", 1), fmt.Sprintf("%040d", 2), "lint")
		calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, dataDir, func(dataDir, roundRunID string) *run.Run {
			return &run.Run{ID: roundRunID, State: run.StateAccepted, Branch: branch}
		})
		cfg := requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1}
		if err := requestdrivertest.DriveRequests(dp, context.Background(), dataDir, cfg, requestdrivertest.FailingSpecDraftRunner(t), requestdrivertest.FailingPlanTicketsRunner(t), requestdrivertest.FailingOracleDraftRunner(t), buildRunner); err != nil {
			t.Fatalf("drive: %v", err)
		}
		if *calls != 1 {
			t.Fatalf("corrective round builds = %d, want 1: the advice round is not in its budget", *calls)
		}
	})

	t.Run("max_review_rounds", func(t *testing.T) {
		dp := newFakeDeps(t)
		draftPullRequest(dp)
		r, dataDir := requestdrivertest.StubPRReviewTestFixture(t, 1)
		ticket := &r.Tickets[0]
		ticket.Rounds = []request.Round{adviceRound}
		stubPRReviewDeps(dp, t, forge.ReviewState{State: forge.PRStateOpen, IsDraft: true}, nil)
		dp.pushExistingBranchFn = func(ctx context.Context, workspaceDir, sha, branch string) error {
			requestdrivertest.LastPushedSHA[branch] = sha
			return nil
		}
		dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }
		requestdriver.PrReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
			id := requestdrivertest.ArgValue(args, "-ticket")
			return saveAcceptedRoundRun(t, dataDir, &run.Run{ID: id, State: run.StateAccepted, WorkspacePath: t.TempDir(), ResultSHA: "cafef00d", Branch: requestdrivertest.ArgValue(args, "-on-branch")})
		}
		threads := []forge.Thread{{ID: "thread-1", Path: "a.go", Line: 1, Author: "dave", Body: "needs work", CommentID: 444}}
		if err := requestdriver.RunCorrectiveRound(dp, context.Background(), dataDir, r, ticket, threads, requestdriver.WorkerConfig{MaxReviewRounds: 1}, time.Now()); err != nil {
			t.Fatalf("RunCorrectiveRound: %v", err)
		}
		if r.State == request.StateHalted {
			t.Fatalf("halted (the advice round was counted against max_review_rounds): %s", r.Error)
		}
		if len(ticket.Rounds) != 2 || ticket.Rounds[1].Kind != "" {
			t.Errorf("Rounds = %+v, want the advice round and a PR-review round", ticket.Rounds)
		}
	})
}

// TestAReproductionThatRefusedTheAdviceRoundGoesOnNoList: the advice round
// refuses nothing, so a reviewer's reproduction that refused its run is not
// added to the ticket's list, which every later build rechecks.
func TestAReproductionThatRefusedTheAdviceRoundGoesOnNoList(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		id := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: id})
		return requestdrivertest.ReproductionRefusedOn(t, f.dataDir, id, f.branch, f.resultSHA, fmt.Sprintf("%040d", 3)).Save(f.dataDir)
	}
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	f.assertOriginalStands(t, request.RoundQuarantined)
	if got := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0].Reproductions; len(got) != 0 {
		t.Errorf("the ticket's list = %+v, want empty: the advice round refuses nothing", got)
	}
}
