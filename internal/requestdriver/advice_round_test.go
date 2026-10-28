package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/release"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/unresolved"
)

// The accepted run's review: one high finding, one medium, one low.
var adviceFindings = []run.CodeReviewFinding{
	{Severity: "high", File: "src/patch.ts", Line: 42, Summary: "frozen arrays lose their length on patch", FailureScenario: "produce(frozen, d => { d.length = 0 }) throws"},
	{Severity: "medium", File: "src/util.ts", Summary: "a medium concern about naming"},
	{Severity: "low", Summary: "a low concern about comments"},
}

// A later round's review, still with a high finding.
var stillHigh = []run.CodeReviewFinding{{Severity: "high", Summary: "nested drafts still leak a proxy"}}

// adviceFixture is a request in building whose ticket builds are accepted
// with a pull request and the review findings given, plus the advice round's
// stand-in runner and the pull request's push, branch and comment seams.
type adviceFixture struct {
	dp                 *fakeDeps
	dataDir, id, prURL string
	baseSHA, resultSHA string
	buildRunner        requestdriver.TicketRunner
	buildArgs          [][]string
	cfg                requestdriver.WorkerConfig
	pushed, restored   []string
	comments           []string
	// roundFindings is the review of advice round k's run.
	roundFindings map[int][]run.CodeReviewFinding
}

func newAdviceFixture(t *testing.T, findings []run.CodeReviewFinding) *adviceFixture {
	return newAdviceFixtureOf(t, 1, findings)
}

func newAdviceFixtureOf(t *testing.T, tickets int, findings []run.CodeReviewFinding) *adviceFixture {
	t.Helper()
	f := &adviceFixture{dp: newFakeDeps(t), baseSHA: fmt.Sprintf("%040d", 1), resultSHA: fmt.Sprintf("%040d", 2), roundFindings: map[int][]run.CodeReviewFinding{}}
	f.dataDir, f.id = requestdrivertest.BuildingFixture(f.dp, t, tickets)
	f.prURL = "https://github.com/acme/app/pull/7"
	f.buildRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		f.buildArgs = append(f.buildArgs, args)
		ticket := requestdrivertest.ArgValue(args, "-ticket")
		onReady(&run.Run{ID: ticket, State: run.StateReady})
		rr := &run.Run{
			ID: ticket, Ticket: ticket, Project: "app", RequestID: f.id, State: run.StateAccepted, Branch: "factoryd/" + ticket, BaseSHA: f.baseSHA, ResultSHA: f.resultSHA,
			PullRequestURL: f.prURL, CodeReview: &run.CodeReviewResult{Policy: "required", Available: true, Findings: findings},
			Attempts: []run.Attempt{{RelayConsumedInputTokens: 700, RelayConsumedOutputTokens: 100}},
		}
		return rr.Save(f.dataDir)
	}
	f.cfg = requestdriver.WorkerConfig{ReviewCorrectiveRounds: 1, OpenPullRequest: true}
	f.cfg.Settings.ReviewAdviceRounds = 1
	stubPRReviewDeps(f.dp, t, forge.ReviewState{State: forge.PRStateOpen, IsDraft: true}, nil)
	// Origin has ticket 1's branch at its accepted run's commit, as the
	// build that opened the pull request pushed it.
	requestdrivertest.LastPushedSHA[f.branch()] = f.resultSHA
	f.dp.pushExistingBranchFn = func(ctx context.Context, workspaceDir, sha, branch string) error {
		f.pushed = append(f.pushed, branch+"@"+sha)
		requestdrivertest.LastPushedSHA[branch] = sha
		return nil
	}
	f.dp.restoreBranchFn = func(dir, branch, sha string) (bool, error) {
		f.restored = append(f.restored, branch+"@"+sha)
		return false, nil
	}
	f.dp.commentOnPullRequestFn = func(ctx context.Context, prURL, body string) error {
		f.comments = append(f.comments, body)
		return nil
	}
	return f
}

func (f *adviceFixture) branch() string { return "factoryd/" + requestdrivertest.TicketRunID(f.id, 1) }

// roundSHA is advice round k's result commit.
func roundSHA(k int) string { return fmt.Sprintf("%040d", 100+k) }

// roundOf is k of a run id ending in -advice<k>, 0 for any other.
func roundOf(id string) int {
	i := strings.LastIndex(id, "-advice")
	if i < 0 {
		return 0
	}
	k, _ := strconv.Atoi(id[i+len("-advice"):])
	return k
}

// adviceRun is an advice round's run as its stand-in saves it: on the
// accepted run's branch, ending in state with result roundSHA(k), the
// review f.roundFindings[k], and a release decision allowed or not.
func (f *adviceFixture) adviceRun(t *testing.T, id string, state run.State, allowed bool) *run.Run {
	t.Helper()
	k := roundOf(id)
	rr := &run.Run{
		ID: id, Ticket: id, Project: "app", State: state, Branch: f.branch(), BaseSHA: roundSHA(k - 1), DiffBaseSHA: f.baseSHA, ResultSHA: roundSHA(k),
		WorkspacePath: t.TempDir(), Attempts: []run.Attempt{{}},
		CodeReview: &run.CodeReviewResult{Policy: "required", Available: true, Findings: f.roundFindings[k]},
	}
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

// acceptingRounds installs a stand-in for the advice rounds' builds that
// accepts each with an allowed decision, and returns its call count and
// last args.
func (f *adviceFixture) acceptingRounds(t *testing.T) (*int, *[]string) {
	t.Helper()
	return requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.adviceRun(t, id, run.StateAccepted, true)
	})
}

// TestAdviceRoundAcceptedIsPushedAndBecomesTheTicketsRun: an accepted run
// whose review recorded a high finding is followed by one advice round on its
// branch, given that finding (and no medium or low one) as the record of the
// attempt, opening no pull request; accepted again, its result is pushed to
// the pull request's branch, its run is the ticket's, the pull request gets
// one comment saying so, and the request moves on as it would have.
func TestAdviceRoundAcceptedIsPushedAndBecomesTheTicketsRun(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.roundFindings[1] = []run.CodeReviewFinding{{Severity: "low", Summary: "a leftover low note"}}
	calls, lastArgs := f.acceptingRounds(t)
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("advice round builds = %d, want 1", *calls)
	}
	f.assertAdviceArgs(t, *lastArgs)
	if want := []string{f.branch() + "@" + roundSHA(1)}; fmt.Sprint(f.pushed) != fmt.Sprint(want) {
		t.Errorf("pushed %v, want %v", f.pushed, want)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (%s)", loaded.State, request.StatePRReview, loaded.Error)
	}
	tk := loaded.Tickets[0]
	roundRunID := requestdrivertest.TicketRunID(f.id, 1) + "-advice1"
	if len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.AdviceRoundKind || tk.Rounds[0].Outcome != request.RoundAccepted || !tk.Rounds[0].Pushed || tk.Rounds[0].Index != 1 {
		t.Fatalf("Rounds = %+v, want one accepted, pushed advice round", tk.Rounds)
	}
	if tk.RunID != roundRunID || tk.PRHeadRunID() != roundRunID || tk.Advice != nil {
		t.Errorf("ticket run %q, PR head %q, in flight %+v; want the advice round's run %q and none in flight", tk.RunID, tk.PRHeadRunID(), tk.Advice, roundRunID)
	}
	if tk.PRURL != f.prURL || tk.Branch != f.branch() {
		t.Errorf("ticket PR %q branch %q, want the accepted run's %q %q", tk.PRURL, tk.Branch, f.prURL, f.branch())
	}
	if len(f.restored) != 0 {
		t.Errorf("a pushed round restored the branch: %v", f.restored)
	}
	if len(f.comments) != 1 {
		t.Fatalf("comments = %d, want one after the push", len(f.comments))
	}
	for _, want := range []string{"**high** frozen arrays lose their length on patch", roundRunID, roundSHA(1), "passed every gate and a fresh review again", "**low** a leftover low note"} {
		if !strings.Contains(f.comments[0], want) {
			t.Errorf("the comment lacks %q:\n%s", want, f.comments[0])
		}
	}
	for _, unwanted := range []string{"a medium concern", "src/patch.ts", "produce(frozen"} {
		if strings.Contains(f.comments[0], unwanted) {
			t.Errorf("the comment carries %q:\n%s", unwanted, f.comments[0])
		}
	}
}

// assertAdviceArgs checks the advice round's build argv: its run id, the
// accepted run's branch and diff base, no pull request of its own, the
// execution role's model, and the record holding the high finding and no
// other.
func (f *adviceFixture) assertAdviceArgs(t *testing.T, args []string) {
	t.Helper()
	original := requestdrivertest.TicketRunID(f.id, 1)
	if got := requestdrivertest.ArgValue(args, "-ticket"); got != original+"-advice1" {
		t.Errorf("-ticket = %q, want %s-advice1", got, original)
	}
	if got := requestdrivertest.ArgValue(args, "-on-branch"); got != f.branch() {
		t.Errorf("-on-branch = %q, want %q", got, f.branch())
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
// else the advice round ends in, nothing is pushed, the local branch is put
// back on the accepted run's commit, the accepted run stays the ticket's,
// and the request moves on exactly as without the round: it is never
// quarantined, halted or sent to resume_review for it.
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
// the ticket's, nothing pushed or commented, the branch put back on the
// accepted run's commit, and one advice round recorded as outcome.
func (f *adviceFixture) assertOriginalStands(t *testing.T, outcome request.RoundOutcome) {
	t.Helper()
	if len(f.pushed) != 0 || len(f.comments) != 0 {
		t.Errorf("pushed %v, commented %d times; want nothing", f.pushed, len(f.comments))
	}
	if want := []string{f.branch() + "@" + f.resultSHA}; fmt.Sprint(f.restored) != fmt.Sprint(want) {
		t.Errorf("branch restored %v, want %v", f.restored, want)
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
// accepted advice round whose release decision is not allowed, whose result
// does not descend from the pull request's head, or whose pull request
// cannot be turned back to draft, is not pushed; the accepted run stands and
// the request moves on, with the reason on the round.
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
		{"draft conversion failed", true, func(f *adviceFixture) {
			f.dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) {
				return forge.ReviewState{State: forge.PRStateOpen}, nil
			}
			f.dp.undoMarkPullRequestReadyFn = func(context.Context, string) error { return errors.New("gh refused") }
		}, "nothing was pushed"},
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

// TestAdviceRoundBranchThatCannotBePutBack: a branch that could not be put
// back halts the request only while it still points past the accepted run's
// commit; otherwise the failure is on the round and the request moves on.
func TestAdviceRoundBranchThatCannotBePutBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ahead     bool
		wantState request.State
	}{
		{"still ahead: halts", true, request.StateHalted},
		{"moved elsewhere: noted", false, request.StatePRReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, adviceFindings)
			f.dp.restoreBranchFn = func(string, string, string) (bool, error) { return tc.ahead, errors.New("cannot lock ref") }
			requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
				return f.adviceRun(t, id, run.StateAccepted, false)
			})
			if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
				t.Fatalf("drive: %v", err)
			}
			loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			if loaded.State != tc.wantState {
				t.Fatalf("State %q (%s), want %q", loaded.State, loaded.Error, tc.wantState)
			}
			tk := loaded.Tickets[0]
			if !strings.Contains(tk.Rounds[0].Error, "was not put back") || tk.RunID != requestdrivertest.TicketRunID(f.id, 1) {
				t.Errorf("round %+v run %q, want the failure on the round and the accepted run standing", tk.Rounds[0], tk.RunID)
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
	f.acceptingRounds(t)
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
// exactly as before: the key at 0, no high finding, pull requests off, a
// budget with less left than the accepted run spent.
func TestNoAdviceRound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		findings []run.CodeReviewFinding
		setup    func(f *adviceFixture)
	}{
		{"the key is 0", adviceFindings, func(f *adviceFixture) { f.cfg.Settings.ReviewAdviceRounds = 0 }},
		{"only medium and low findings", adviceFindings[1:], func(*adviceFixture) {}},
		{"no review", nil, func(*adviceFixture) {}},
		{"pull requests off", adviceFindings, func(f *adviceFixture) { f.cfg.OpenPullRequest = false; f.prURL = "" }},
		// The accepted run spent 800 tokens: 1000 leaves 200.
		{"the request budget has less left than the run spent", adviceFindings, func(f *adviceFixture) { f.cfg.Settings.RequestTokenBudget = 1000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, tc.findings)
			tc.setup(f)
			calls, _ := f.acceptingRounds(t)
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

// TestAdviceRoundRunsWithinABudgetThatCoversIt: a request budget with at
// least the accepted run's spend left runs the round.
func TestAdviceRoundRunsWithinABudgetThatCoversIt(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.cfg.Settings.RequestTokenBudget = 1600
	calls, _ := f.acceptingRounds(t)
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("advice round builds = %d, want 1: 1600 leaves the 800 the run spent", *calls)
	}
}

// TestAdviceRoundRunsOnTheExecutionModel: with an escalation configured, the
// advice round still runs on the request's execution model, and its run is
// not recorded as escalated.
func TestAdviceRoundRunsOnTheExecutionModel(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	requestPickingModel(t, f.dataDir, f.id)
	settings := escalationSettings(&sessionconfig.Escalation{Model: "sol", Thinking: "high"})
	settings.ReviewAdviceRounds = 1
	f.cfg.Settings = settings
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		if started, err := run.Load(dataDir, id); err != nil || started.Escalated {
			t.Errorf("the started advice run: %v, escalated %v, want not escalated", err, started != nil && started.Escalated)
		}
		return f.adviceRun(t, id, run.StateAccepted, true)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("advice round builds = %d, want 1", *calls)
	}
	if got := requestdrivertest.ArgValue(*lastArgs, "-execution-model"); got != "picked" {
		t.Errorf("-execution-model = %q, want the request's pick, not the escalation", got)
	}
	if got := requestdrivertest.ArgValue(*lastArgs, "-execution-thinking"); got != "" {
		t.Errorf("-execution-thinking = %q, want none", got)
	}
}

// TestAdviceRoundsCount: review_advice_rounds bounds the rounds, and each
// after the first follows only a pushed round whose own review still
// recorded a high finding, given that round's findings.
func TestAdviceRoundsCount(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int
		round1     []run.CodeReviewFinding
		allowed1   bool
		wantBuilds int
		wantRun    string
	}{
		{"round 1 pushed and still high: round 2 runs", 2, stillHigh, true, 2, "-advice2"},
		{"round 1 pushed with no high finding: stops", 2, nil, true, 1, "-advice1"},
		{"round 1 not pushed: stops", 2, stillHigh, false, 1, ""},
		{"the count reached: stops", 1, stillHigh, true, 1, "-advice1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixture(t, adviceFindings)
			f.cfg.Settings.ReviewAdviceRounds = tc.count
			f.roundFindings[1] = tc.round1
			var args [][]string
			requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
				return f.adviceRun(t, id, run.StateAccepted, roundOf(id) != 1 || tc.allowed1)
			})
			calls := 0
			stub := requestdriver.ReviewCorrectiveRunner
			requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
				calls++
				args = append(args, a)
				return stub(ctx, a, onReady)
			}
			if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
				t.Fatalf("drive: %v", err)
			}
			if calls != tc.wantBuilds {
				t.Fatalf("advice round builds = %d, want %d", calls, tc.wantBuilds)
			}
			loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			tk := loaded.Tickets[0]
			if loaded.State != request.StatePRReview || len(tk.Rounds) != tc.wantBuilds {
				t.Fatalf("State %q rounds %+v, want pr_review with %d rounds", loaded.State, tk.Rounds, tc.wantBuilds)
			}
			if want := requestdrivertest.TicketRunID(f.id, 1) + tc.wantRun; tk.RunID != want {
				t.Errorf("ticket run %q, want %q", tk.RunID, want)
			}
			if calls == 2 {
				record, err := os.ReadFile(requestdrivertest.ArgValue(args[1], "-earlier-attempt"))
				if err != nil || !strings.Contains(string(record), "nested drafts still leak a proxy") || strings.Contains(string(record), "frozen arrays") {
					t.Errorf("round 2's record (err %v) is not round 1's findings:\n%s", err, record)
				}
				if got := requestdrivertest.ArgValue(args[1], "-ticket"); got != requestdrivertest.TicketRunID(f.id, 1)+"-advice2" {
					t.Errorf("round 2's -ticket = %q", got)
				}
				if tk.Rounds[1].Index != 2 || len(f.comments) != 2 {
					t.Errorf("round 2 index %d, comments %d; want 2 and one per push", tk.Rounds[1].Index, len(f.comments))
				}
			}
		})
	}
}

// TestAdviceRoundRunsOncePerCount: a ticket that already had its advice
// rounds gets no more, whatever its next accepted run's review says.
func TestAdviceRoundRunsOncePerCount(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	r.Tickets[0].Rounds = []request.Round{{Index: 4, Kind: request.AdviceRoundKind, RunID: "earlier-advice", Outcome: request.RoundAccepted, Pushed: true, At: time.Now().UTC().Format(time.RFC3339Nano)}}
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	calls, _ := f.acceptingRounds(t)
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

// stopMidRound drives the first pass with the worker told to stop while
// advice round stopAt builds (rounds before it are accepted and pushed): the
// round is left in flight. started says whether its run had started.
func (f *adviceFixture) stopMidRound(t *testing.T, stopAt int, started bool) (calls *int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	orig := requestdriver.ReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
	requestdriver.ReviewCorrectiveRunner = func(_ context.Context, args []string, onReady func(*run.Run)) error {
		n++
		id := requestdrivertest.ArgValue(args, "-ticket")
		if roundOf(id) < stopAt {
			onReady(&run.Run{ID: id})
			return f.adviceRun(t, id, run.StateAccepted, true).Save(f.dataDir)
		}
		defer cancel()
		if !started {
			return errors.New("stopping")
		}
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
	return &n
}

// TestAdviceRoundInterruptedIsSettledNotRelaunched: a worker that stops
// mid-round leaves the round in flight on the ticket, with the ticket's run
// still the one it follows; the next pass settles it from its run, never
// building it or the ticket again: an accepted round is pushed, one whose
// run never started leaves the accepted run standing, and one whose run has
// not ended is waited for.
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
			calls := f.stopMidRound(t, 1, tc.started)
			original := requestdrivertest.TicketRunID(f.id, 1)
			stopped := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			tk := stopped.Tickets[0]
			if stopped.State != request.StateBuilding || tk.Advice == nil || len(tk.Rounds) != 0 || tk.RunID != original {
				t.Fatalf("after the stop: State %q in flight %+v rounds %+v run %q, want building with the round in flight and the accepted run the ticket's", stopped.State, tk.Advice, tk.Rounds, tk.RunID)
			}
			roundRunID := tk.Advice.RunID
			if tc.started {
				if roundRunID != original+"-advice1" {
					t.Fatalf("in-flight round run %q, want %s-advice1", roundRunID, original)
				}
				// While its run has not ended the pass waits, changing nothing.
				if err := f.drive(t, context.Background(), requestdrivertest.FailingBuildRunner(t)); err == nil || !strings.Contains(err.Error(), "has not ended") {
					t.Fatalf("a pass while the round's run is going = %v, want it to wait", err)
				}
				if err := f.adviceRun(t, roundRunID, tc.state, true).Save(f.dataDir); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.drive(t, context.Background(), requestdrivertest.FailingBuildRunner(t)); err != nil {
				t.Fatalf("drive after the restart: %v", err)
			}
			if *calls != 1 {
				t.Errorf("advice round builds = %d, want the one before the stop", *calls)
			}
			loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			tk = loaded.Tickets[0]
			if loaded.State != request.StatePRReview || tk.Advice != nil || len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.AdviceRoundKind || tk.Rounds[0].Outcome != tc.wantRound {
				t.Fatalf("State %q in flight %+v rounds %+v, want pr_review with one %s advice round", loaded.State, tk.Advice, tk.Rounds, tc.wantRound)
			}
			wantRun := original
			if tc.wantPush {
				wantRun = roundRunID
			}
			if tk.RunID != wantRun || (len(f.pushed) == 1) != tc.wantPush {
				t.Errorf("ticket run %q pushed %v, want run %q and a push %t", tk.RunID, f.pushed, wantRun, tc.wantPush)
			}
		})
	}
}

// TestAdviceRoundInterruptedInRoundTwo: a restart mid-round 2 relaunches
// neither round: round 1 stays as recorded, round 2 is settled from its run.
func TestAdviceRoundInterruptedInRoundTwo(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.cfg.Settings.ReviewAdviceRounds = 2
	f.roundFindings[1] = stillHigh
	calls := f.stopMidRound(t, 2, true)
	stopped := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0]
	round1 := requestdrivertest.TicketRunID(f.id, 1) + "-advice1"
	if len(stopped.Rounds) != 1 || !stopped.Rounds[0].Pushed || stopped.RunID != round1 || stopped.Advice == nil || stopped.Advice.Of != round1 {
		t.Fatalf("after the stop: rounds %+v run %q in flight %+v, want round 1 pushed and round 2 in flight after it", stopped.Rounds, stopped.RunID, stopped.Advice)
	}
	if err := f.adviceRun(t, stopped.Advice.RunID, run.StateAccepted, true).Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	if err := f.drive(t, context.Background(), requestdrivertest.FailingBuildRunner(t)); err != nil {
		t.Fatalf("drive after the restart: %v", err)
	}
	if *calls != 2 {
		t.Errorf("advice round builds = %d, want the two before the stop", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	tk := loaded.Tickets[0]
	if loaded.State != request.StatePRReview || len(tk.Rounds) != 2 || !tk.Rounds[1].Pushed || tk.RunID != requestdrivertest.TicketRunID(f.id, 1)+"-advice2" {
		t.Fatalf("State %q rounds %+v run %q, want round 2 settled, pushed and the ticket's", loaded.State, tk.Rounds, tk.RunID)
	}
}

// TestAdviceRoundInFlightTooLongIsSettledAsHalted: a round whose run never
// ends is waited for only until a ticket build's own time limit and an hour
// have passed since it started, then settled as halted: the accepted run
// stands and the request moves on.
func TestAdviceRoundInFlightTooLongIsSettledAsHalted(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.stopMidRound(t, 1, true)
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	since, err := time.Parse(time.RFC3339Nano, r.Tickets[0].Advice.Since)
	if err != nil {
		t.Fatal(err)
	}
	if err := requestdriver.AdvanceBuilding(f.dp, context.Background(), f.dataDir, r, f.cfg, requestdrivertest.FailingBuildRunner(t), since.Add(47*time.Hour)); err == nil {
		t.Fatal("a pass within the bound did not wait")
	}
	r = requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if err := requestdriver.AdvanceBuilding(f.dp, context.Background(), f.dataDir, r, f.cfg, requestdrivertest.FailingBuildRunner(t), since.Add(50*time.Hour)); err != nil {
		t.Fatalf("a pass past the bound: %v", err)
	}
	f.restored = f.restored[:1]
	f.assertOriginalStands(t, request.RoundHalted)
	if got := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0].Rounds[0].Error; !strings.Contains(got, "has not ended") || !strings.Contains(got, "settled as halted") {
		t.Errorf("round error %q, want it to say the run never ended", got)
	}
}

// TestAHaltMidRoundThenRetryUsesTheAcceptedRun: a request halted while its
// advice round was in flight and retried into pr_review records the round
// as not pushed, puts the branch back, and reads the accepted run as the
// pull request's head.
func TestAHaltMidRoundThenRetryUsesTheAcceptedRun(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.stopMidRound(t, 1, true)
	now := time.Now()
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if err := requestdriver.HaltRequest(f.dataDir, r, "operator stop", now); err != nil {
		t.Fatal(err)
	}
	r, err := request.Retry(f.dataDir, f.id, "alice", "", now, nil)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if r.State != request.StatePRReview {
		t.Fatalf("retry moved to %q, want pr_review", r.State)
	}
	if err := requestdriver.AdvancePRReview(f.dp, context.Background(), f.dataDir, r, f.cfg, now); err != nil {
		t.Fatalf("AdvancePRReview: %v", err)
	}
	tk := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0]
	original := requestdrivertest.TicketRunID(f.id, 1)
	if tk.Advice != nil || len(tk.Rounds) != 1 || tk.Rounds[0].Pushed || tk.RunID != original || tk.PRHeadRunID() != original {
		t.Fatalf("in flight %+v rounds %+v run %q head %q, want the round recorded unpushed and the accepted run the head", tk.Advice, tk.Rounds, tk.RunID, tk.PRHeadRunID())
	}
	if want := []string{f.branch() + "@" + f.resultSHA}; fmt.Sprint(f.restored) != fmt.Sprint(want) {
		t.Errorf("branch restored %v, want %v", f.restored, want)
	}
}

// TestALaterTicketChainsFromTheTicketsPushedRun: ticket 2 is built on
// ticket 1's advice round when it was pushed, and on ticket 1's accepted run
// when it was not.
func TestALaterTicketChainsFromTheTicketsPushedRun(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed bool
		suffix  string
	}{
		{"pushed", true, "-advice1"},
		{"not pushed", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAdviceFixtureOf(t, 2, adviceFindings)
			requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
				return f.adviceRun(t, id, run.StateAccepted, tc.allowed)
			})
			for pass := 0; pass < 2; pass++ {
				if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
					t.Fatalf("drive %d: %v", pass, err)
				}
			}
			if len(f.buildArgs) != 2 {
				t.Fatalf("ticket builds = %d, want 2", len(f.buildArgs))
			}
			want := requestdrivertest.TicketRunID(f.id, 1) + tc.suffix
			if got := requestdrivertest.ArgValue(f.buildArgs[1], "-prior-run"); got != want {
				t.Errorf("ticket 2's -prior-run = %q, want %q", got, want)
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
		return requestdrivertest.ReproductionRefusedOn(t, f.dataDir, id, f.branch(), f.resultSHA, roundSHA(1)).Save(f.dataDir)
	}
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	f.assertOriginalStands(t, request.RoundQuarantined)
	if got := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0].Reproductions; len(got) != 0 {
		t.Errorf("the ticket's list = %+v, want empty: the advice round refuses nothing", got)
	}
}
