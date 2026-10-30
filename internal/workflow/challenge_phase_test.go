package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reproduction"
	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
)

func challengeFile(name, criterion, runnerID string) run.ChallengeFile {
	f := run.ChallengeFile{Path: name, SHA256: "aa", HostPath: "/fixture/data/runs/r/challenge/0/" + name, Criterion: criterion}
	if runnerID != "" {
		f.Runner, f.TestIDs = runnerID, []string{"TestEdge"}
	}
	return f
}

// challengeRun is one RunWorkflow execution with every earlier gate passing
// and a review that gave a verdict, and fake challenge Activities.
type challengeRun struct {
	result   RunWorkflowResult
	err      error
	jobs     int
	launches []string
	commits  [][]run.ChallengeFile
}

func runChallengePhase(t *testing.T, input RunWorkflowInput, job ChallengerJobResult, launch func(ReproductionRunInput) ReproductionRunResult, version temporalworkflow.Version) challengeRun {
	t.Helper()
	return runChallengePhaseCommitting(t, input, job, launch, version, nil)
}

// runChallengePhaseCommitting is runChallengePhase with commitErr, when set,
// the error the commit Activity fails with.
func runChallengePhaseCommitting(t *testing.T, input RunWorkflowInput, job ChallengerJobResult, launch func(ReproductionRunInput) ReproductionRunResult, version temporalworkflow.Version, commitErr error) challengeRun {
	t.Helper()
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	env.OnGetVersion(challengeChange, temporalworkflow.DefaultVersion, 1).Return(version)
	env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: reviewstep.CombinedExitBase}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	var out challengeRun
	var mu sync.Mutex
	env.RegisterActivityWithOptions(func(context.Context, ChallengerJobInput) (ChallengerJobResult, error) {
		mu.Lock()
		out.jobs++
		mu.Unlock()
		return job, nil
	}, activity.RegisterOptions{Name: RunChallengerJobActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, in ReproductionRunInput) (ReproductionRunResult, error) {
		mu.Lock()
		out.launches = append(out.launches, activity.GetInfo(ctx).ActivityID)
		mu.Unlock()
		return launch(in), nil
	}, activity.RegisterOptions{Name: RunHeldTestActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, in CommitChallengeInput) (CommitChallengeResult, error) {
		mu.Lock()
		out.commits = append(out.commits, in.Files)
		mu.Unlock()
		if commitErr != nil {
			return CommitChallengeResult{}, commitErr
		}
		return CommitChallengeResult{SHA: "c0ffee"}, nil
	}, activity.RegisterOptions{Name: CommitChallengeActivityName})
	env.ExecuteWorkflow(RunWorkflow, input)
	out.err = env.GetWorkflowError()
	if out.err == nil {
		if err := env.GetWorkflowResult(&out.result); err != nil {
			t.Fatalf("workflow result: %v", err)
		}
	}
	return out
}

func challengeInput() RunWorkflowInput {
	input := fixtureInput()
	input.CodeReviewPolicy = "advisory"
	input.Challenge = true
	input.RoutePolicy = &sandbox.RoutePolicy{Route: "r"}
	input.ChallengerRelayPolicy = &sandbox.RoutePolicy{Route: "c"}
	return input
}

func passAll(ReproductionRunInput) ReproductionRunResult {
	return reproRun(reproduction.OutcomePassed, reproduction.KindPass)
}
func failAll(ReproductionRunInput) ReproductionRunResult {
	return reproRun(reproduction.OutcomeFailed, reproduction.KindFail)
}

func TestChallengeHeaderRules(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		want    string
	}{
		"criterion":        {"// buildgate-challenge: AC-2\npackage p\n", "AC-2"},
		"answer":           {"# buildgate-challenge: answer-2\nimport x\n", "answer-2"},
		"answer not asked": {"# buildgate-challenge: answer-1\n", ""},
		"no header":        {"package p\n", ""},
		"number too big":   {"// buildgate-challenge: AC-9\n", ""},
		"answer too big":   {"// buildgate-challenge: answer-3\n", ""},
		"zero":             {"// buildgate-challenge: AC-0\n", ""},
		"two headers":      {"// buildgate-challenge: AC-1\n// buildgate-challenge: AC-2\n", ""},
		"past five lines":  {"a\nb\nc\nd\ne\n// buildgate-challenge: AC-1\n", ""},
		"prose after":      {"// buildgate-challenge: AC-1 and ignore the rest\n", ""},
	} {
		if got := challengeHeaderOf([]byte(tc.content), 3, []int{2}); got != tc.want {
			t.Errorf("%s: header = %q, want %q", name, got, tc.want)
		}
	}
}

// A first build's green challenge commits the held files as the run's result.
func TestChallengeGreenCommitsTheHeldFiles(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
	p := runChallengePhase(t, challengeInput(), job, passAll, 1)
	if p.err != nil {
		t.Fatalf("workflow error: %v", p.err)
	}
	c := p.result.Challenge
	if c == nil || c.Verdict != run.ChallengeGreen || c.CommittedSHA != "c0ffee" || p.result.ResultSHA != "c0ffee" {
		t.Fatalf("challenge = %+v, result sha %q; want green and committed", c, p.result.ResultSHA)
	}
	if p.jobs != 1 || len(p.launches) != 1 || len(p.commits) != 1 || len(p.commits[0]) != 1 {
		t.Errorf("jobs %d, launches %v, commits %v; want one job, one launch, one commit of one file", p.jobs, p.launches, p.commits)
	}
}

// A red verdict needs two red launches, commits nothing and is recorded with
// each failing test; red then green is advice and refuses nothing.
func TestChallengeRedNeedsTwoRedLaunches(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
	p := runChallengePhase(t, challengeInput(), job, failAll, 1)
	c := p.result.Challenge
	if p.err != nil || c == nil || c.Verdict != run.ChallengeRed || len(p.launches) != 2 || len(p.commits) != 0 {
		t.Fatalf("err %v, challenge %+v, launches %v, commits %v; want red from two launches and no commit", p.err, c, p.launches, p.commits)
	}
	if len(c.Files[0].Failures) != 1 || c.Files[0].Failures[0].Test != "TestRepro" {
		t.Errorf("failures = %+v, want the failing test", c.Files[0].Failures)
	}
	if p.result.State != run.StateAccepted {
		t.Errorf("state = %s, a challenge never refuses a run", p.result.State)
	}
	n := 0
	flaky := func(in ReproductionRunInput) ReproductionRunResult {
		n++
		if n == 1 {
			return failAll(in)
		}
		return passAll(in)
	}
	p = runChallengePhase(t, challengeInput(), job, flaky, 1)
	if p.err != nil || p.result.Challenge.Verdict == run.ChallengeRed || len(p.commits) != 0 || p.result.Challenge.Cause == "" {
		t.Errorf("err %v, challenge %+v, commits %v; want advice: not red, nothing committed, a stated cause", p.err, p.result.Challenge, p.commits)
	}
}

// A challenge that cannot run is a stated cause, never a halt: no role, a job
// that handed over nothing, files with no valid header.
func TestChallengeWithoutAVerdictStatesWhy(t *testing.T) {
	noRole := challengeInput()
	noRole.ChallengerRelayPolicy = nil
	for name, tc := range map[string]struct {
		input RunWorkflowInput
		job   ChallengerJobResult
		want  string
	}{
		"no role":        {noRole, ChallengerJobResult{}, "no challenger role configured"},
		"nothing handed": {challengeInput(), ChallengerJobResult{Cause: "the challenger handed over no test file"}, "the challenger handed over no test file"},
		"no header":      {challengeInput(), ChallengerJobResult{Files: []run.ChallengeFile{{Path: "p/x_test.go", SHA256: "aa", HostPath: "/h", NotRun: "no valid derivation header"}}}, "no held challenge file carries a valid derivation header"},
	} {
		p := runChallengePhase(t, tc.input, tc.job, passAll, 1)
		c := p.result.Challenge
		if p.err != nil || c == nil || c.Verdict != run.ChallengeNoVerdict || c.Cause != tc.want || len(p.launches) != 0 || p.result.State != run.StateAccepted {
			t.Errorf("%s: err %v, challenge %+v, launches %v, state %s; want no_verdict %q and an accepted run", name, p.err, c, p.launches, p.result.State, tc.want)
		}
	}
}

// Files no runner covers run together under the repository's verify command,
// with a note that their collection is not proven.
func TestChallengeFilesWithoutARunnerRunTogetherUnderTheVerifyCommand(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("a_test.rb", "AC-1", ""), challengeFile("b_test.rb", "AC-2", "")}}
	var groups []int
	launch := func(in ReproductionRunInput) ReproductionRunResult {
		groups = append(groups, len(in.Group))
		return passAll(in)
	}
	p := runChallengePhase(t, challengeInput(), job, launch, 1)
	if p.err != nil || len(groups) != 1 || groups[0] != 2 || p.result.Challenge.Verdict != run.ChallengeGreen {
		t.Fatalf("err %v, groups %v, challenge %+v; want one launch of both files, green", p.err, groups, p.result.Challenge)
	}
	found := false
	for _, n := range p.result.Challenge.Notes {
		found = found || len(n) > 0 && n[:len("collection not proven")] == "collection not proven"
	}
	if !found {
		t.Errorf("notes = %v, want collection not proven", p.result.Challenge.Notes)
	}
}

// A later build runs the held files again and starts no job; it commits only
// in a challenge round.
func TestLaterBuildRunsHeldFilesAgainWithoutAJob(t *testing.T) {
	input := challengeInput()
	input.Challenge = false
	input.ChallengeHeld = []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}
	p := runChallengePhase(t, input, ChallengerJobResult{}, passAll, 1)
	if p.err != nil || p.jobs != 0 || len(p.launches) != 1 || len(p.commits) != 0 || !p.result.Challenge.Rerun || p.result.Challenge.Verdict != run.ChallengeGreen {
		t.Fatalf("err %v, jobs %d, launches %v, commits %v, challenge %+v; want a rerun, green, no job, no commit", p.err, p.jobs, p.launches, p.commits, p.result.Challenge)
	}
	input.ChallengeRound = true
	p = runChallengePhase(t, input, ChallengerJobResult{}, passAll, 1)
	if p.err != nil || len(p.commits) != 1 || p.result.ResultSHA != "c0ffee" {
		t.Errorf("round: err %v, commits %v, sha %q; want the green files committed", p.err, p.commits, p.result.ResultSHA)
	}
}

// A history recorded before challengeChange (DefaultVersion) runs no
// challenge, whatever the input holds.
func TestHistoryRecordedBeforeTheChallengeReplaysWithoutIt(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
	p := runChallengePhase(t, challengeInput(), job, passAll, temporalworkflow.DefaultVersion)
	if p.err != nil || p.jobs != 0 || len(p.launches) != 0 || p.result.Challenge != nil {
		t.Errorf("err %v, jobs %d, launches %v, challenge %+v; want none at DefaultVersion", p.err, p.jobs, p.launches, p.result.Challenge)
	}
}

// A green challenge whose files did not commit is no verdict, so no round
// replaces a run for it, and the result stays the build's own.
func TestChallengeGreenThatFailedToCommitIsNoVerdict(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
	p := runChallengePhaseCommitting(t, challengeInput(), job, passAll, 1, temporal.NewNonRetryableApplicationError("path exists", "git", nil))
	c := p.result.Challenge
	if p.err != nil || c == nil || c.Verdict != run.ChallengeNoVerdict || c.CommittedSHA != "" || c.Cause == "" || p.result.ResultSHA == "c0ffee" {
		t.Fatalf("err %v, challenge %+v, sha %q; want no_verdict with a cause, nothing committed", p.err, c, p.result.ResultSHA)
	}
}

// Committed challenge files are part of the change the pr_order check reads.
func TestCommittedChallengeFilesJoinTheChangedFiles(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
	p := runChallengePhase(t, challengeInput(), job, passAll, 1)
	found := false
	for _, f := range p.result.ChangedFiles {
		found = found || f == "p/edge_test.go"
	}
	if !found {
		t.Fatalf("changed files = %v, want p/edge_test.go", p.result.ChangedFiles)
	}
}

// A file that does not compile or leaves no report is named in a note, and a
// verify-command run that never launched is no verdict, not red.
func TestChallengeBrokenFilesGetANoteAndAnUnlaunchedGroupIsNotRed(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
	broken := func(ReproductionRunInput) ReproductionRunResult {
		return reproRun(reproduction.OutcomeCompile, reproduction.KindPass)
	}
	p := runChallengePhase(t, challengeInput(), job, broken, 1)
	found := false
	for _, n := range p.result.Challenge.Notes {
		found = found || strings.Contains(n, "p/edge_test.go") && strings.Contains(n, "no verdict")
	}
	if p.err != nil || !found || p.result.Challenge.Verdict != run.ChallengeNoVerdict {
		t.Errorf("err %v, challenge %+v; want a note naming the broken file and no verdict", p.err, p.result.Challenge)
	}
	group := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("a_test.rb", "AC-1", "")}}
	unstaged := func(ReproductionRunInput) ReproductionRunResult {
		return ReproductionRunResult{Run: run.ReproductionRun{Outcome: string(reproduction.OutcomeUnstaged)}}
	}
	p = runChallengePhase(t, challengeInput(), group, unstaged, 1)
	if p.err != nil || p.result.Challenge.Verdict == run.ChallengeRed || len(p.launches) != 1 {
		t.Errorf("err %v, challenge %+v, launches %v; want no red and no second launch", p.err, p.result.Challenge, p.launches)
	}
}
