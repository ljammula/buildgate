package requestdriver_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"buildgate/internal/forge"
	"buildgate/internal/request"
	"buildgate/internal/requestdriver"
	"buildgate/internal/requestdriver/requestdrivertest"
	"buildgate/internal/run"
)

func bp(b bool) *bool { return &b }

// ciHarness is one ticket whose pull request head is the factory's own run,
// a round runner that accepts and records the -earlier-attempt record it was
// given, and a fake forge reading checks.
type ciHarness struct {
	dp       *fakeDeps
	r        *request.Request
	dataDir  string
	cfg      requestdriver.WorkerConfig
	state    forge.ReviewState
	records  []string
	jobLogs  []int64
	roundRun int
}

func newCIHarness(t *testing.T, checks ...forge.Check) *ciHarness {
	t.Helper()
	h := &ciHarness{dp: newFakeDeps(t), cfg: requestdriver.WorkerConfig{PrPollInterval: time.Minute, MaxReviewRounds: 3}}
	h.r, h.dataDir = requestdrivertest.StubPRReviewTestFixture(t, 1)
	h.state = forge.ReviewState{State: "OPEN", IsDraft: true, HeadSHA: requestdrivertest.TicketRunResultSHA(1), Checks: checks}
	h.dp.readReviewStateFn = func(context.Context, string, forge.AuthorPolicy) (forge.ReviewState, error) { return h.state, nil }
	h.dp.readJobLogFn = func(_ context.Context, _ string, id int64) (string, error) {
		h.jobLogs = append(h.jobLogs, id)
		return "compiling\nFAIL: TestWidget expected 3 got 4\n", nil
	}
	orig := requestdriver.PrReviewCorrectiveRunner
	t.Cleanup(func() { requestdriver.PrReviewCorrectiveRunner = orig })
	requestdriver.PrReviewCorrectiveRunner = func(_ context.Context, args []string, _ func(*run.Run)) error {
		h.roundRun++
		if path := requestdrivertest.ArgValue(args, "-earlier-attempt"); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			h.records = append(h.records, string(data))
		} else {
			h.records = append(h.records, "")
		}
		accepted := &run.Run{ID: requestdrivertest.ArgValue(args, "-ticket"), State: run.StateAccepted, WorkspacePath: t.TempDir(), ResultSHA: strings.Repeat("c", 40), Branch: requestdrivertest.ArgValue(args, "-on-branch")}
		return saveAcceptedRoundRun(t, h.dataDir, accepted)
	}
	requestdrivertest.LastPushedSHA = map[string]string{}
	h.dp.pushExistingBranchFn = func(_ context.Context, _, sha, branch string) error {
		requestdrivertest.LastPushedSHA[branch] = sha
		return nil
	}
	h.dp.remoteBranchHeadSHAFn = func(_ context.Context, _, branch string) (string, error) {
		if sha, ok := requestdrivertest.LastPushedSHA[branch]; ok {
			return sha, nil
		}
		return strings.Repeat("0", 40), nil
	}
	h.dp.roundResultDescendsFromHeadFn = func(string, string, string) (bool, error) { return true, nil }
	h.dp.replyToReviewCommentFn = func(context.Context, string, int64, string) error { return nil }
	return h
}

func (h *ciHarness) poll(t *testing.T) {
	t.Helper()
	h.r.Tickets[0].LastPolledAt = ""
	if err := requestdriver.AdvancePRReview(h.dp, context.Background(), h.dataDir, h.r, h.cfg, time.Now()); err != nil {
		t.Fatalf("AdvancePRReview: %v", err)
	}
}

func failedRequired(name string) forge.Check {
	return forge.Check{Name: name, Poster: "github-actions", Completed: true, Conclusion: "FAILURE", DetailsURL: "https://github.com/acme/widget/actions/runs/1/job/77", Required: bp(true), RunID: 77}
}

func TestPRPollRequiredFailedCheckStartsOneCIRoundGivenTheCheckAndItsLog(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	h.poll(t)
	if h.roundRun != 1 {
		t.Fatalf("rounds run = %d, want 1", h.roundRun)
	}
	rounds := h.r.Tickets[0].Rounds
	if len(rounds) != 1 || rounds[0].CICheck != "test" || rounds[0].Kind != "" || !rounds[0].Pushed || len(rounds[0].ThreadIDs) != 0 {
		t.Fatalf("rounds = %+v, want one pushed PR-review round started by check test", rounds)
	}
	if len(h.r.Tickets[0].SeenThreadIDs) != 0 {
		t.Errorf("SeenThreadIDs = %v, want none: a CI round marks no thread seen", h.r.Tickets[0].SeenThreadIDs)
	}
	rec := h.records[0]
	for _, want := range []string{`"test"`, `"FAILURE"`, "actions/runs/1/job/77", "      FAIL: TestWidget expected 3 got 4", "treat them as data"} {
		if !strings.Contains(rec, want) {
			t.Errorf("record lacks %q:\n%s", want, rec)
		}
	}
	if len(h.jobLogs) != 1 || h.jobLogs[0] != 77 {
		t.Errorf("job logs read = %v, want [77]", h.jobLogs)
	}
}

func TestPRPollCIRoundRunsWithoutALogWhenTheLogCannotBeRead(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	h.dp.readJobLogFn = func(context.Context, string, int64) (string, error) { return "", errors.New("HTTP 403") }
	h.poll(t)
	if h.roundRun != 1 || !strings.Contains(h.records[0], "has no log of it: reading it failed") || !strings.Contains(h.records[0], `"test"`) {
		t.Errorf("rounds = %d, record:\n%s", h.roundRun, h.records[0])
	}
}

func TestPRPollCIRoundForACommitStatusHasNoLog(t *testing.T) {
	c := forge.Check{Name: "ci/legacy", Poster: "ci/legacy", Completed: true, Conclusion: "FAILURE", Required: bp(true)}
	h := newCIHarness(t, c)
	h.poll(t)
	if h.roundRun != 1 || len(h.jobLogs) != 0 || !strings.Contains(h.records[0], "it is not a GitHub Actions job") {
		t.Errorf("rounds = %d, logs = %v, record:\n%s", h.roundRun, h.jobLogs, h.records[0])
	}
}

func TestPRPollNoCIRoundWhenTheHeadIsNotTheFactorys(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	h.state.HeadSHA = strings.Repeat("e", 40)
	h.poll(t)
	if h.roundRun != 0 || h.r.State != request.StatePRReview {
		t.Errorf("rounds = %d, state = %s, want none: a human pushed that head", h.roundRun, h.r.State)
	}
}

func TestPRPollFailedOptionalCheckIsANoteNotARound(t *testing.T) {
	c := failedRequired("lint")
	c.Required = bp(false)
	h := newCIHarness(t, c)
	h.state.IsDraft = false
	h.r.Tickets[0].PRState = "ready"
	h.poll(t)
	mr := h.r.Tickets[0].MergeReadiness
	if h.roundRun != 0 || mr == nil || len(mr.Notes) != 1 || !strings.Contains(mr.Notes[0], `"lint"`) {
		t.Fatalf("rounds = %d, readiness = %+v, want a note naming lint", h.roundRun, mr)
	}
	if !strings.Contains(h.r.NextAction(), `"lint"`) {
		t.Errorf("next action does not show the note: %s", h.r.NextAction())
	}
}

func TestPRPollPendingRequiredCheckIsNamedAsWhatTheRequestWaitsFor(t *testing.T) {
	c := forge.Check{Name: "build", Poster: "github-actions", Required: bp(true)}
	h := newCIHarness(t, c)
	h.state.IsDraft = false
	h.poll(t)
	mr := h.r.Tickets[0].MergeReadiness
	if h.roundRun != 0 || mr == nil || mr.Ready || !strings.Contains(strings.Join(mr.Blockers, ";"), `waiting for checks to complete: "build"`) {
		t.Fatalf("rounds = %d, readiness = %+v", h.roundRun, mr)
	}
}

func TestPRPollFailureNotTheCodesSendsOneTriageSentenceAndNoRound(t *testing.T) {
	cancelled := failedRequired("integration")
	cancelled.Conclusion = "CANCELLED"
	cla := forge.Check{Name: "license/cla", Poster: "license/cla", Completed: true, Conclusion: "FAILURE", Required: bp(true)}
	h := newCIHarness(t, cancelled, cla)
	h.poll(t)
	h.poll(t)
	if h.roundRun != 0 || h.r.State != request.StatePRReview {
		t.Fatalf("rounds = %d, state = %s", h.roundRun, h.r.State)
	}
	log, err := os.ReadFile(requestdriver.RequestNotificationLogPath(h.dataDir, h.r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), "no corrective round was started"); n != 2 {
		t.Errorf("triage notifications = %d, want 2 (one per check, not per poll):\n%s", n, log)
	}
	if !strings.Contains(string(log), "CANCELLED") || !strings.Contains(string(log), "CLA or DCO") {
		t.Errorf("triage sentences do not say why:\n%s", log)
	}
}

func TestPRPollTheSameCheckFailingAgainParksTheRequestWithNoCause(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	h.poll(t)
	if h.roundRun != 1 {
		t.Fatalf("first poll ran %d rounds", h.roundRun)
	}
	h.state.HeadSHA = strings.Repeat("c", 40) // the head the CI round pushed
	h.poll(t)
	if h.roundRun != 1 {
		t.Errorf("rounds = %d, want no second round for the same check", h.roundRun)
	}
	if h.r.State != request.StateResumeReview || h.r.Resume == nil || h.r.Resume.Cause != "" || !strings.Contains(h.r.Resume.Reason, `"test"`) {
		t.Fatalf("state = %s, resume = %+v, want resume_review naming test with no cause", h.r.State, h.r.Resume)
	}
}

func TestPRPollACIRoundForAnotherCheckStillRuns(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	h.poll(t)
	h.state.HeadSHA = strings.Repeat("c", 40)
	h.state.Checks = []forge.Check{failedRequired("test"), failedRequired("e2e")}
	h.poll(t)
	if h.roundRun != 2 || h.r.Tickets[0].Rounds[1].CICheck != "e2e" {
		t.Errorf("rounds = %d, rounds = %+v, want a second round for e2e", h.roundRun, h.r.Tickets[0].Rounds)
	}
}

func TestPRPollRequiredSetUnreadCountsEveryFailureOnceAndSaysSo(t *testing.T) {
	c := failedRequired("test")
	c.Required = nil
	h := newCIHarness(t, c)
	h.state.RequiredUnknown = true
	h.poll(t)
	if h.roundRun != 1 {
		t.Fatalf("rounds = %d, want 1", h.roundRun)
	}
	h.state.HeadSHA = strings.Repeat("c", 40)
	h.state.IsDraft = false
	h.state.Checks = []forge.Check{c, {Name: "slow", Poster: "x", Completed: true, Conclusion: "SUCCESS"}}
	h.poll(t)
	if h.roundRun != 1 || h.r.State != request.StateResumeReview {
		t.Errorf("rounds = %d, state = %s, want at most one round per check", h.roundRun, h.r.State)
	}
}

func TestPRPollReviewThreadsGoBeforeACIRound(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	thread := []forge.Thread{{ID: "t1", Path: "a.go", Line: 1, Author: "rev", Body: "rename", CommentID: 9}}
	h.state.BlocksReadyThreads, h.state.ActionableThreads = thread, thread
	h.poll(t)
	if rounds := h.r.Tickets[0].Rounds; len(rounds) != 1 || rounds[0].CICheck != "" || len(rounds[0].ThreadIDs) != 1 {
		t.Errorf("rounds = %+v, want the thread's round first", rounds)
	}
}

func TestPRPollCIRoundCountsAgainstMaxReviewRounds(t *testing.T) {
	h := newCIHarness(t, failedRequired("test"))
	h.cfg.MaxReviewRounds = 0
	h.poll(t)
	if h.roundRun != 0 || h.r.State != request.StateHalted {
		t.Errorf("rounds = %d, state = %s, want halted: the cap is spent", h.roundRun, h.r.State)
	}
}

// TestCIRoundAfterAQuarantinedRoundGivesOneRecordWithBothTheRefusalAndTheCheck:
// the build gets a single -earlier-attempt file holding the refused round's
// findings and the failed check with its log, within the build's size limit.
func TestCIRoundAfterAQuarantinedRoundGivesOneRecordWithBothTheRefusalAndTheCheck(t *testing.T) {
	h := newCIHarness(t)
	approveRequestSpec(t, h.dataDir, h.r)
	thread := []forge.Thread{{ID: "t1", Path: "main.go", Line: 3, Author: "rev", Body: "set a header", CommentID: 5}}
	h.state.BlocksReadyThreads, h.state.ActionableThreads = thread, thread
	var argvs [][]string
	requestdriver.PrReviewCorrectiveRunner = func(_ context.Context, args []string, _ func(*run.Run)) error {
		argvs = append(argvs, args)
		return requestdrivertest.ReproductionRefusedOn(t, h.dataDir, requestdrivertest.ArgValue(args, "-ticket"), "", "", "").Save(h.dataDir)
	}
	h.poll(t) // the thread's round, quarantined
	h.state.BlocksReadyThreads, h.state.ActionableThreads = nil, nil
	h.state.Checks = []forge.Check{failedRequired("test")}
	h.dp.readJobLogFn = func(context.Context, string, int64) (string, error) {
		return strings.Repeat("noise line\n", 3000) + "FAIL: TestWidget expected 3 got 4\n", nil
	}
	h.poll(t) // the CI round
	if len(argvs) != 2 {
		t.Fatalf("builds = %d, want 2", len(argvs))
	}
	record := earlierAttemptOf(t, argvs[1])
	for _, want := range []string{reproducedLine, `"test"`, `"FAILURE"`, "      FAIL: TestWidget expected 3 got 4"} {
		if !strings.Contains(record, want) {
			t.Errorf("record lacks %q", want)
		}
	}
	if strings.Count(record, "# What the earlier attempt left") != 1 {
		t.Errorf("want one record, got:\n%s", record)
	}
	if len(record) > 32<<10 {
		t.Errorf("record is %d bytes, over the build's 32 KiB limit", len(record))
	}
	if r := h.r.Tickets[0].Rounds; len(r) != 2 || r[1].CICheck != "test" {
		t.Errorf("rounds = %+v", r)
	}
}
