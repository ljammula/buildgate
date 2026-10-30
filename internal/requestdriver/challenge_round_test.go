package requestdriver_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

const heldContent = "package sum\n\n// CONTENT-NEVER-TOLD\nfunc TestEmpty(t *testing.T) {}\n"

// redChallenge is the record of a first build whose held file failed.
func redChallenge(t *testing.T, dataDir, runID string) *run.ChallengeRecord {
	t.Helper()
	host := filepath.Join(run.Dir(dataDir, runID), run.ChallengeDirName, "0", "internal", "sum", "sum_edge_test.go")
	if err := os.MkdirAll(filepath.Dir(host), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(host, []byte(heldContent), 0o444); err != nil {
		t.Fatal(err)
	}
	return &run.ChallengeRecord{Verdict: run.ChallengeRed, Files: []run.ChallengeFile{{
		Path: "internal/sum/sum_edge_test.go", SHA256: strings.Repeat("a", 64), HostPath: host, Criterion: "AC-2", Verdict: run.ChallengeRed,
		Failures: []run.ChallengeFailure{{Test: "TestEmptySum", Assertion: "fail"}},
	}}}
}

// challengeFixture is an advice fixture whose first build records a challenge
// with the given verdict, with findings as its review's, and whose request has
// an approved spec; review_advice_rounds is 0, so the one round a red
// challenge always has is the only one that may run.
func challengeFixture(t *testing.T, verdict string, findings ...run.CodeReviewFinding) *adviceFixture {
	f := newAdviceFixture(t, findings)
	f.cfg.Settings.ReviewAdviceRounds = 0
	r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	sum, err := request.HashFile(f.dataDir, r.ID, "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if r.ApprovedSHA256 == nil {
		r.ApprovedSHA256 = map[string]string{}
	}
	r.ApprovedSHA256["spec.md"] = sum
	if err := r.Save(f.dataDir); err != nil {
		t.Fatal(err)
	}
	first := f.buildRunner
	f.buildRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		if err := first(ctx, args, onReady); err != nil {
			return err
		}
		id := requestdrivertest.ArgValue(args, "-ticket")
		rec, err := run.Load(f.dataDir, id)
		if err != nil {
			return err
		}
		rec.Challenge = redChallenge(t, f.dataDir, id)
		rec.Challenge.Verdict = verdict
		return rec.Save(f.dataDir)
	}
	return f
}

// roundChallengeRun is round run id's run as its stand-in saves it: accepted
// on the first run's branch with an allowed decision, the review
// f.roundFindings[k], and a re-run challenge with the given verdict for the
// held file, whose green result is committed.
func (f *adviceFixture) roundChallengeRun(t *testing.T, id, verdict string, others ...run.ChallengeFile) *run.Run {
	t.Helper()
	rr := f.adviceRun(t, id, run.StateAccepted, true)
	committed := ""
	if verdict == run.ChallengeGreen {
		committed = rr.ResultSHA
	}
	files := append([]run.ChallengeFile{{Path: "internal/sum/sum_edge_test.go", Criterion: "AC-2", Verdict: verdict}}, others...)
	rr.Challenge = &run.ChallengeRecord{Verdict: verdict, Rerun: true, CommittedSHA: committed, Files: files}
	return rr
}

// SC-022: the first build of a request's ticket is given the challenge inputs
// (the approved spec with the answers, the counts a header may cite); a red
// challenge starts one round, given the failing files' paths, criteria and
// failing tests as its record and the held files read-only, never a file's
// content; an accepted round with a green challenge is pushed and becomes the
// ticket's run, with one comment, and the ticket keeps the first build's pull
// request and branch: the round opens none of its own.
func TestRedChallengeStartsOneRoundWhichWhenGreenIsPushed(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.roundChallengeRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	first := f.buildArgs[0]
	if !requestdrivertest.HasFlag(first, "-challenge") || requestdrivertest.ArgValue(first, "-challenge-spec") == "" || requestdrivertest.ArgValue(first, "-challenge-criteria") == "" {
		t.Errorf("the first build = %v, want -challenge with its spec and counts", first)
	}
	if *calls != 1 {
		t.Fatalf("round builds = %d, want exactly 1", *calls)
	}
	args := *lastArgs
	if !requestdrivertest.HasFlag(args, "-challenge-round") || requestdrivertest.ArgValue(args, "-challenge-held") == "" || requestdrivertest.HasFlag(args, "-challenge") {
		t.Errorf("round args = %v, want -challenge-round with the held manifest and no new job", args)
	}
	record := earlierAttemptOf(t, args)
	for _, want := range []string{"internal/sum/sum_edge_test.go", "AC-2", "TestEmptySum"} {
		if !strings.Contains(record, want) {
			t.Errorf("the record lacks %q:\n%s", want, record)
		}
	}
	for _, input := range []string{record, string(mustReadFile(t, requestdrivertest.ArgValue(args, "-challenge-held")))} {
		if strings.Contains(input, "CONTENT-NEVER-TOLD") {
			t.Errorf("a held file's content reached the round's input:\n%s", input)
		}
	}
	f.assertRoundsPushed(t, 1)
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "internal/sum/sum_edge_test.go") || !strings.Contains(f.comments[0], "committed") {
		t.Errorf("comments = %v, want one naming the committed file", f.comments)
	}
}

// assertRoundsPushed checks the request reached pr_review with n pushed
// rounds of the one loop (advice kind), the last the ticket's run, on the
// first build's pull request and branch.
func (f *adviceFixture) assertRoundsPushed(t *testing.T, n int) {
	t.Helper()
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State != request.StatePRReview {
		t.Fatalf("State = %q, want %q (%s)", loaded.State, request.StatePRReview, loaded.Error)
	}
	tk := loaded.Tickets[0]
	last := fmt.Sprintf("%s-advice%d", requestdrivertest.TicketRunID(f.id, 1), n)
	if len(tk.Rounds) != n || tk.RunID != last || !tk.Challenged || tk.ChallengeRounded || tk.Advice != nil {
		t.Fatalf("ticket = %+v, want %d rounds, %s the ticket's run", tk, n, last)
	}
	for _, rnd := range tk.Rounds {
		if rnd.Kind != request.AdviceRoundKind || !rnd.Pushed {
			t.Fatalf("round %+v, want a pushed round of the one loop", rnd)
		}
	}
	if tk.PRURL != f.prURL || tk.Branch != f.branch() {
		t.Errorf("ticket PR %q branch %q, want the first build's %q %q", tk.PRURL, tk.Branch, f.prURL, f.branch())
	}
}

// A round whose held test is still red is judged as any round: pushed, with
// a comment that names the file and its criterion (never its content) as
// still failing. With the budget spent no second round follows.
func TestChallengeRoundThatStaysRedIsAdviceAndTheRunStands(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.roundChallengeRun(t, id, run.ChallengeRed)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("round builds = %d, want 1: a red test gets one round at review_advice_rounds 0", *calls)
	}
	f.assertRoundsPushed(t, 1)
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "internal/sum/sum_edge_test.go") || !strings.Contains(f.comments[0], "AC-2") || !strings.Contains(f.comments[0], "still fail") || strings.Contains(f.comments[0], "CONTENT-NEVER-TOLD") {
		t.Errorf("comments = %v, want one naming the file and criterion as still failing, never its content", f.comments)
	}
}

// No round for a green challenge without findings, and a later build of the
// ticket runs the held files again (-challenge-held) and never starts another
// job.
func TestGreenChallengeStartsNoRound(t *testing.T) {
	f := challengeFixture(t, run.ChallengeGreen)
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.roundChallengeRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 0 {
		t.Errorf("round builds = %d, want none for a green challenge", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if tk := loaded.Tickets[0]; !tk.Challenged || tk.ChallengeRunID != requestdrivertest.TicketRunID(f.id, 1) {
		t.Errorf("ticket = %+v, want it recorded as challenged by its first run", tk)
	}
	later, err := requestdriver.BuildRequestBuildArgs(f.dataDir, loaded, loaded.Tickets[0], f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if requestdrivertest.HasFlag(later, "-challenge") || requestdrivertest.ArgValue(later, "-challenge-held") == "" || requestdrivertest.HasFlag(later, "-challenge-round") {
		t.Errorf("a later build = %v, want the held files to run again and no new job or round", later)
	}
}

// Red tests and a high finding on the head are one round with one record,
// the tests first and then the finding.
func TestRedTestsAndAHighFindingAreOneRoundWithOneBrief(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed, adviceFindings...)
	calls, lastArgs := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.roundChallengeRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("round builds = %d, want 1", *calls)
	}
	record := earlierAttemptOf(t, *lastArgs)
	tests, finding := strings.Index(record, "## Tests written against the accepted change"), strings.Index(record, "## A reviewer's concerns")
	if tests < 0 || finding < tests || !strings.Contains(record, "frozen arrays lose their length") || !strings.Contains(record, "TestEmptySum") {
		t.Errorf("the record lacks both sections, tests first:\n%s", record)
	}
	if strings.Count(record, "That attempt passed every check and was accepted.") != 1 {
		t.Errorf("the record repeats the attempt's intro:\n%s", record)
	}
	f.assertRoundsPushed(t, 1)
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "sum_edge_test.go") || !strings.Contains(f.comments[0], "frozen arrays") {
		t.Errorf("comments = %v, want one naming the test and the finding", f.comments)
	}
}

// review_advice_rounds 0 still gives a red held test its one round, and a
// head with only a finding none.
func TestZeroAdviceRoundsGiveARedTestOneRoundAndAFindingNone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verdict   string
		findings  []run.CodeReviewFinding
		wantCalls int
	}{
		{"a red test", run.ChallengeRed, nil, 1},
		{"a high finding only", run.ChallengeGreen, adviceFindings, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := challengeFixture(t, tc.verdict, tc.findings...)
			calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
				return f.roundChallengeRun(t, id, run.ChallengeRed)
			})
			if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
				t.Fatalf("drive: %v", err)
			}
			if *calls != tc.wantCalls {
				t.Errorf("round builds = %d, want %d", *calls, tc.wantCalls)
			}
		})
	}
}

// A red test that stays red is tried up to the budget, each round given the
// held files to commit; the round that turns them green commits them, and the
// round after it (a high finding remains) is given no -challenge-round.
func TestRedStaysRedRoundsRunToTheBudgetAndCommittedTestsAreNotCommittedAgain(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	f.cfg.Settings.ReviewAdviceRounds = 3
	f.roundFindings[2] = stillHigh
	var all [][]string
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		verdict := run.ChallengeRed
		if roundOf(id) >= 2 {
			verdict = run.ChallengeGreen
		}
		return f.roundChallengeRun(t, id, verdict)
	})
	stub := requestdriver.ReviewCorrectiveRunner
	requestdriver.ReviewCorrectiveRunner = func(ctx context.Context, a []string, onReady func(*run.Run)) error {
		all = append(all, a)
		return stub(ctx, a, onReady)
	}
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 3 {
		t.Fatalf("round builds = %d, want 3", *calls)
	}
	for i, want := range []bool{true, true, false} {
		if got := requestdrivertest.HasFlag(all[i], "-challenge-round"); got != want {
			t.Errorf("round %d: -challenge-round = %t, want %t", i+1, got, want)
		}
	}
	f.assertRoundsPushed(t, 3)
}

// A round that turns a held test that was green on the head red is not pushed:
// the branch is put back, the head stands and no further round follows.
func TestRoundThatRegressesAGreenHeldTestIsNotPushed(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	f.cfg.Settings.ReviewAdviceRounds = 3
	other := run.ChallengeFile{Path: "internal/sum/sum_other_test.go", Criterion: "AC-1"}
	first := f.buildRunner
	f.buildRunner = func(ctx context.Context, args []string, onReady func(*run.Run)) error {
		if err := first(ctx, args, onReady); err != nil {
			return err
		}
		rec, err := run.Load(f.dataDir, requestdrivertest.ArgValue(args, "-ticket"))
		if err != nil {
			return err
		}
		g := other
		g.Verdict = run.ChallengeGreen
		rec.Challenge.Files = append(rec.Challenge.Files, g)
		return rec.Save(f.dataDir)
	}
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		broken := other
		broken.Verdict = run.ChallengeRed
		return f.roundChallengeRun(t, id, run.ChallengeGreen, broken)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 || len(f.pushed) != 0 || len(f.restored) != 1 {
		t.Fatalf("builds %d pushed %v restored %v; want one round, nothing pushed, the branch put back", *calls, f.pushed, f.restored)
	}
	tk := requestdrivertest.LoadRequest(t, f.dataDir, f.id).Tickets[0]
	if len(tk.Rounds) != 1 || tk.Rounds[0].Pushed || tk.RunID != requestdrivertest.TicketRunID(f.id, 1) {
		t.Errorf("ticket = %+v, want the first run to stand with one unpushed round", tk)
	}
	if len(f.comments) != 1 || !strings.Contains(f.comments[0], "sum_edge_test.go") {
		t.Errorf("comments = %v, want one declined comment naming the red file", f.comments)
	}
}

// A round of review advice alone (no red test on its head) that leaves no
// fewer high findings may be pushed, and no round follows it.
func TestAdviceOnlyRoundWithNoFewerHighFindingsIsPushedAndEndsTheRounds(t *testing.T) {
	f := newAdviceFixture(t, adviceFindings)
	f.cfg.Settings.ReviewAdviceRounds = 3
	f.roundFindings[1] = stillHigh
	calls, _ := f.acceptingRounds(t)
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 1 || len(f.pushed) != 1 {
		t.Errorf("builds %d pushed %v, want one round, pushed", *calls, f.pushed)
	}
}

// A push that failed once sent ends a round whose head had a red held test as
// no verdict with a note: a challenge never halts (SC-022).
func TestChallengeRoundPushThatFailsHaltsNothing(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	f.dp.pushExistingBranchFn = func(context.Context, string, string, string) error { return errors.New("remote hung up") }
	requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.roundChallengeRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State == request.StateHalted {
		t.Fatalf("request halted: %q", loaded.Error)
	}
	tk := loaded.Tickets[0]
	if tk.RunID != requestdrivertest.TicketRunID(f.id, 1) || len(tk.Rounds) != 1 || tk.Rounds[0].Pushed || !strings.Contains(tk.Rounds[0].Error, "no verdict") {
		t.Errorf("ticket run %q rounds %+v, want the accepted run standing and one unpushed round noted as no verdict", tk.RunID, tk.Rounds)
	}
}

// A worker that stops mid-round leaves the round in flight; the next pass
// settles it from its run and never builds it again. A record written when
// the challenge round was a round of its own (Advice.Challenge) settles the
// same way and stays recorded as the challenge round it was.
func TestChallengeRoundInterruptedIsSettledNotRelaunched(t *testing.T) {
	for _, tc := range []struct {
		name     string
		started  bool
		wantPush bool
	}{
		{"the round's run was accepted with a green challenge", true, true},
		{"the round's run never started", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := challengeFixture(t, run.ChallengeRed)
			ctx, cancel := context.WithCancel(context.Background())
			calls := 0
			orig := requestdriver.ReviewCorrectiveRunner
			t.Cleanup(func() { requestdriver.ReviewCorrectiveRunner = orig })
			requestdriver.ReviewCorrectiveRunner = func(_ context.Context, args []string, onReady func(*run.Run)) error {
				calls++
				defer cancel()
				if tc.started {
					id := requestdrivertest.ArgValue(args, "-ticket")
					onReady(&run.Run{ID: id})
					if err := f.roundChallengeRun(t, id, run.ChallengeGreen).Save(f.dataDir); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("stopping")
			}
			if err := f.drive(t, ctx, f.buildRunner); err == nil {
				t.Fatal("drive with the worker stopping mid-round returned no error")
			}
			original := requestdrivertest.TicketRunID(f.id, 1)
			// The record as the retired challenge round left it.
			r := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			if r.Tickets[0].Advice == nil || len(r.Tickets[0].Rounds) != 0 || r.Tickets[0].RunID != original {
				t.Fatalf("after the stop: in flight %+v rounds %+v run %q, want a round in flight and the accepted run the ticket's", r.Tickets[0].Advice, r.Tickets[0].Rounds, r.Tickets[0].RunID)
			}
			r.Tickets[0].Advice.Challenge, r.Tickets[0].ChallengeRounded = true, true
			if err := r.Save(f.dataDir); err != nil {
				t.Fatal(err)
			}
			if err := f.drive(t, context.Background(), requestdrivertest.FailingBuildRunner(t)); err != nil {
				t.Fatalf("drive after the restart: %v", err)
			}
			if calls != 1 {
				t.Errorf("round builds = %d, want the one before the stop", calls)
			}
			loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
			tk := loaded.Tickets[0]
			if loaded.State != request.StatePRReview || tk.Advice != nil || len(tk.Rounds) != 1 || tk.Rounds[0].Kind != request.ChallengeRoundKind {
				t.Fatalf("State %q in flight %+v rounds %+v, want pr_review with one challenge round", loaded.State, tk.Advice, tk.Rounds)
			}
			wantRun := original
			if tc.wantPush {
				wantRun = original + "-advice1"
			}
			if tk.RunID != wantRun || (len(f.pushed) == 1) != tc.wantPush {
				t.Errorf("ticket run %q pushed %v, want run %q and a push %t", tk.RunID, f.pushed, wantRun, tc.wantPush)
			}
		})
	}
}

// A round that would push the request past its token budget does not start:
// the accepted run stands, the request moves on, nothing halts.
func TestChallengeRoundRefusedByTheLaunchBudgetStartsNothing(t *testing.T) {
	f := challengeFixture(t, run.ChallengeRed)
	// The accepted run spent 800 tokens: 1000 leaves 200.
	f.cfg.Settings.RequestTokenBudget = 1000
	calls, _ := requestdrivertest.StubReviewCorrectiveRunner(t, f.dataDir, func(dataDir, id string) *run.Run {
		return f.roundChallengeRun(t, id, run.ChallengeGreen)
	})
	if err := f.drive(t, context.Background(), f.buildRunner); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("round builds = %d, want none", *calls)
	}
	loaded := requestdrivertest.LoadRequest(t, f.dataDir, f.id)
	if loaded.State == request.StateHalted || len(loaded.Tickets[0].Rounds) != 0 || loaded.Tickets[0].RunID != requestdrivertest.TicketRunID(f.id, 1) {
		t.Errorf("State %q rounds %+v run %q, want the accepted run standing and no round", loaded.State, loaded.Tickets[0].Rounds, loaded.Tickets[0].RunID)
	}
}
