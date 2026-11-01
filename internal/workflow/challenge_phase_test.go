package workflow

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
	// verifies counts the verify command's runs; undos are the result commits
	// the branch was moved back to.
	verifies int
	undos    []string
}

func runChallengePhase(t *testing.T, input RunWorkflowInput, job ChallengerJobResult, launch func(ReproductionRunInput) ReproductionRunResult, version temporalworkflow.Version) challengeRun {
	t.Helper()
	return runChallengePhaseCommitting(t, input, job, launch, version, nil)
}

// runChallengePhaseCommitting is runChallengePhase with commitErr, when set,
// the error the commit Activity fails with.
func runChallengePhaseCommitting(t *testing.T, input RunWorkflowInput, job ChallengerJobResult, launch func(ReproductionRunInput) ReproductionRunResult, version temporalworkflow.Version, commitErr error) challengeRun {
	t.Helper()
	return runChallengePhaseWith(t, input, job, launch, version, commitErr, &challengeFixture{})
}

// challengeFixture varies runChallengePhaseWith: the review's exit code (zero
// is a verdict), a serial history (the parallel marker at DefaultVersion), a
// failing gate, a measured size, and hooks entered by the review and the job.
type challengeFixture struct {
	noVerdict bool
	serial    bool
	gateFails bool
	size      *run.ChangeSize
	onReview  func()
	onJob     func()
	// inTurn runs the input as the host sets it where Docker's memory does not
	// hold both (ChallengeWithReview false); legacy replays a history recorded
	// with parallelChallengeChange and without challengeWithReviewByMemoryChange.
	inTurn, legacy bool
	order          *[]string
	orderMu        sync.Mutex
	// redOnCommit makes the verify command fail once the held files are
	// committed; undoErr is the error the undo Activity fails with; noVerifyChange
	// replays a history recorded before verifyChallengeCommitChange.
	redOnCommit    bool
	verifyErr      error // the verify Activity's error once the files are committed
	undoErr        error
	noVerifyChange bool
	// serialLaunches replays a history recorded before
	// parallelReproductionLaunchesChange; launchErr fails a launch.
	serialLaunches bool
	// retriesV1 replays a history at activity-retries version 1.
	retriesV1 bool
	// noSidecars makes the verify result say positively that launches start no
	// compose sidecars; unset, it is a result without the fact (serial).
	noSidecars bool
	launchErr  func(ReproductionRunInput) error
}

func (f *challengeFixture) record(name string) {
	if f.order == nil {
		return
	}
	f.orderMu.Lock()
	*f.order = append(*f.order, name)
	f.orderMu.Unlock()
}

func runChallengePhaseWith(t *testing.T, input RunWorkflowInput, job ChallengerJobResult, launch func(ReproductionRunInput) ReproductionRunResult, version temporalworkflow.Version, commitErr error, fx *challengeFixture) challengeRun {
	t.Helper()
	var out challengeRun
	var mu sync.Mutex
	committed := false
	env := newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			mu.Lock()
			defer mu.Unlock()
			out.verifies++
			if fx.verifyErr != nil && committed {
				return VerifyActivityResult{}, fx.verifyErr
			}
			exit := 0
			if fx.gateFails || fx.redOnCommit && committed {
				exit = 1
			}
			return VerifyActivityResult{NoComposeSidecars: fx.noSidecars, Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: exit}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	env.OnGetVersion(challengeChange, temporalworkflow.DefaultVersion, 1).Return(version)
	if fx.serial {
		env.OnGetVersion(parallelChallengeChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	if fx.noVerifyChange {
		env.OnGetVersion(verifyChallengeCommitChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	if fx.retriesV1 {
		env.OnGetVersion(activityRetriesChange, temporalworkflow.DefaultVersion, 2).Return(temporalworkflow.Version(activityRetriesOn))
	}
	if fx.serialLaunches {
		env.OnGetVersion(parallelReproductionLaunchesChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	if fx.legacy {
		env.OnGetVersion(challengeWithReviewByMemoryChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	}
	input.ChallengeWithReview = !fx.inTurn && !fx.legacy
	if fx.size != nil {
		env.RegisterActivityWithOptions(func(context.Context, MeasureChangeSizeInput) (*run.ChangeSize, error) { return fx.size, nil },
			activity.RegisterOptions{Name: MeasureChangeSizeActivityName})
	}
	env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
		fx.record("review-start")
		if fx.onReview != nil {
			fx.onReview()
		}
		fx.record("review-end")
		exit := reviewstep.CombinedExitBase
		if fx.noVerdict {
			exit = 1
		}
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: exit}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	env.RegisterActivityWithOptions(func(context.Context, ChallengerJobInput) (ChallengerJobResult, error) {
		fx.record("job-start")
		if fx.onJob != nil {
			fx.onJob()
		}
		mu.Lock()
		out.jobs++
		mu.Unlock()
		fx.record("job-end")
		return job, nil
	}, activity.RegisterOptions{Name: RunChallengerJobActivityName})
	env.RegisterActivityWithOptions(func(ctx context.Context, in ReproductionRunInput) (ReproductionRunResult, error) {
		mu.Lock()
		out.launches = append(out.launches, activity.GetInfo(ctx).ActivityID)
		mu.Unlock()
		if fx.launchErr != nil {
			if err := fx.launchErr(in); err != nil {
				return ReproductionRunResult{}, err
			}
		}
		return launch(in), nil
	}, activity.RegisterOptions{Name: RunHeldTestActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, in CommitChallengeInput) (CommitChallengeResult, error) {
		mu.Lock()
		defer mu.Unlock()
		out.commits = append(out.commits, in.Files)
		if commitErr != nil {
			return CommitChallengeResult{}, commitErr
		}
		committed = true
		return CommitChallengeResult{SHA: "c0ffee"}, nil
	}, activity.RegisterOptions{Name: CommitChallengeActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, in UndoChallengeCommitInput) error {
		mu.Lock()
		defer mu.Unlock()
		out.undos = append(out.undos, in.ResultSHA)
		return fx.undoErr
	}, activity.RegisterOptions{Name: UndoChallengeCommitActivityName})
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

// A header citing a criterion of a later ticket is held and not run; an
// answer, a covered criterion and an empty covered list stay in scope.
func TestChallengeInScope(t *testing.T) {
	for name, tc := range map[string]struct {
		header  string
		covered []int
		want    bool
	}{
		"covered":    {"AC-2", []int{1, 2}, true},
		"later":      {"AC-3", []int{1, 2}, false},
		"answer":     {"answer-9", []int{1, 2}, true},
		"empty list": {"AC-3", nil, true},
	} {
		if got := challengeInScope(tc.header, tc.covered); got != tc.want {
			t.Errorf("%s: in scope = %v, want %v", name, got, tc.want)
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

// A red group is the group's verdict: each of its files is red, so the run's
// challenge is red, and each is marked as having run in the group, never as
// failing on its own; a file a runner judged keeps its own verdict.
func TestARedGroupIsMarkedAsTheGroupsVerdict(t *testing.T) {
	job := ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("a_test.rb", "AC-1", ""), challengeFile("b_test.rb", "AC-2", ""), challengeFile("p/edge_test.go", "AC-3", "go-test-json")}}
	launch := func(in ReproductionRunInput) ReproductionRunResult {
		if len(in.Group) > 0 {
			return failAll(in)
		}
		return passAll(in)
	}
	p := runChallengePhase(t, challengeInput(), job, launch, 1)
	c := p.result.Challenge
	if p.err != nil || c == nil || c.Verdict != run.ChallengeRed {
		t.Fatalf("err %v, challenge %+v; want red", p.err, c)
	}
	for _, f := range c.Files {
		wantGroup := f.Runner == ""
		wantVerdict := map[bool]string{true: run.ChallengeRed, false: run.ChallengeGreen}[wantGroup]
		if f.Group != wantGroup || f.Verdict != wantVerdict {
			t.Errorf("%s: group %v verdict %q; want group %v verdict %q", f.Path, f.Group, f.Verdict, wantGroup, wantVerdict)
		}
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

func greenOneFile() ChallengerJobResult {
	return ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
}

// The verify command runs on the commit that adds the held files, and a pass
// keeps the commit as the run's result.
func TestChallengeCommitIsVerifiedAndKeptWhenGreen(t *testing.T) {
	p := runChallengePhaseWith(t, challengeInput(), greenOneFile(), passAll, 1, nil, &challengeFixture{})
	if p.err != nil || p.result.ResultSHA != "c0ffee" || p.result.Challenge.CommittedSHA != "c0ffee" || len(p.undos) != 0 {
		t.Fatalf("err %v, sha %q, challenge %+v, undos %v; want the commit kept", p.err, p.result.ResultSHA, p.result.Challenge, p.undos)
	}
	if p.verifies != 2 {
		t.Errorf("verify ran %d times, want 2 (the build's, the challenge commit's)", p.verifies)
	}
}

// A verify command that fails on the committed files undoes the commit: the
// result and the changed files stay the build's, and the verdict is no verdict.
func TestChallengeCommitThatFailsVerifyIsUndone(t *testing.T) {
	p := runChallengePhaseWith(t, challengeInput(), greenOneFile(), passAll, 1, nil, &challengeFixture{redOnCommit: true})
	c := p.result.Challenge
	if p.err != nil || c == nil || c.Verdict != run.ChallengeNoVerdict || c.CommittedSHA != "" || p.result.ResultSHA == "c0ffee" {
		t.Fatalf("err %v, challenge %+v, sha %q; want no_verdict, nothing committed", p.err, c, p.result.ResultSHA)
	}
	if !strings.HasPrefix(c.Cause, "the held challenge files fail the verify command once committed: ") || len(c.Notes) == 0 || c.Notes[len(c.Notes)-1] != c.Cause {
		t.Errorf("cause %q, notes %v; want the prefixed cause recorded as a note", c.Cause, c.Notes)
	}
	if len(p.undos) != 1 || p.undos[0] != p.result.ResultSHA {
		t.Errorf("undos %v, result sha %q; want one reset to the unchanged result", p.undos, p.result.ResultSHA)
	}
	for _, f := range p.result.ChangedFiles {
		if f == "p/edge_test.go" {
			t.Errorf("changed files %v hold the undone file", p.result.ChangedFiles)
		}
	}
}

// The same through the parallel order (the review and the challenge overlap,
// the files commit after the join): the red commit is undone, and the verify's
// attempt is on the run.
func TestChallengeCommitThatFailsVerifyIsUndoneInTheParallelOrder(t *testing.T) {
	input := challengeInput()
	p := runChallengePhaseWith(t, input, greenOneFile(), passAll, 1, nil, &challengeFixture{redOnCommit: true})
	c := p.result.Challenge
	if p.err != nil || c == nil || !c.WithReview || c.Verdict != run.ChallengeNoVerdict || len(p.undos) != 1 || p.result.ResultSHA == "c0ffee" {
		t.Fatalf("err %v, challenge %+v, undos %v, sha %q; want the parallel order, no_verdict and one undo", p.err, c, p.undos, p.result.ResultSHA)
	}
	if !strings.Contains(c.Cause, "verify exited 1") {
		t.Errorf("cause %q, want the exit status", c.Cause)
	}
}

// A verify step that errors undoes the commit too, and the cause says the
// verify did not finish, not that the files are bad. Its attempt is recorded.
func TestChallengeCommitWhoseVerifyErrorsIsUndoneWithoutBlamingTheFiles(t *testing.T) {
	fx := &challengeFixture{verifyErr: temporal.NewNonRetryableApplicationError("sandbox lost", "infra", nil, []run.Attempt{{Kind: "verify", LogPath: "challenge-verify.log"}})}
	p := runChallengePhaseWith(t, challengeInput(), greenOneFile(), passAll, 1, nil, fx)
	c := p.result.Challenge
	if p.err != nil || c == nil || c.Verdict != run.ChallengeNoVerdict || len(p.undos) != 1 {
		t.Fatalf("err %v, challenge %+v, undos %v; want no_verdict and one undo", p.err, c, p.undos)
	}
	if !strings.HasPrefix(c.Cause, "the verify of the committed held challenge files did not finish: ") || strings.Contains(c.Cause, "fail the verify command") {
		t.Errorf("cause %q; want the did-not-finish wording", c.Cause)
	}
	n := 0
	for _, a := range p.result.Attempts {
		if a.Kind == "verify" && a.LogPath == "challenge-verify.log" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d challenge-verify attempts on the run, want 1", n)
	}
}

// A branch the undo could not move back is not reported as fine: the run halts.
func TestChallengeCommitUndoFailureHaltsTheRun(t *testing.T) {
	fx := &challengeFixture{redOnCommit: true, undoErr: temporal.NewNonRetryableApplicationError("reset failed", "git", nil)}
	p := runChallengePhaseWith(t, challengeInput(), greenOneFile(), passAll, 1, nil, fx)
	if p.err == nil && p.result.State == run.StateAccepted {
		t.Fatalf("undo failure: run accepted with challenge %+v; want a halt", p.result.Challenge)
	}
	if len(p.undos) != 1 {
		t.Errorf("undos %v, want one attempt", p.undos)
	}
}

// A history recorded before verifyChallengeCommitChange replays unchanged: the
// commit is not verified and a red verify command is never asked.
func TestHistoryRecordedBeforeTheChallengeCommitVerifyReplaysUnchanged(t *testing.T) {
	fx := &challengeFixture{redOnCommit: true, noVerifyChange: true}
	p := runChallengePhaseWith(t, challengeInput(), greenOneFile(), passAll, 1, nil, fx)
	if p.err != nil || p.result.ResultSHA != "c0ffee" || p.verifies != 1 || len(p.undos) != 0 {
		t.Fatalf("err %v, sha %q, verifies %d, undos %v; want the old unverified commit", p.err, p.result.ResultSHA, p.verifies, p.undos)
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

func edgeJob() ChallengerJobResult {
	return ChallengerJobResult{Files: []run.ChallengeFile{challengeFile("p/edge_test.go", "AC-1", "go-test-json")}}
}

// The challenger job runs with the review on the same commit: each activity
// waits for the other to have started, which the serial order could not meet.
func TestChallengerJobOverlapsTheReview(t *testing.T) {
	p := runOverlapping(t, &challengeFixture{})
	if p.result.Challenge == nil || !p.result.Challenge.WithReview {
		t.Errorf("challenge %+v; want WithReview recorded", p.result.Challenge)
	}
}

// runOverlapping runs the challenge phase with fx, where each activity waits
// for the other to have started, and fails unless the run is green and committed.
func runOverlapping(t *testing.T, fx *challengeFixture) challengeRun {
	t.Helper()
	reviewIn, jobIn := make(chan struct{}), make(chan struct{})
	var once1, once2 sync.Once
	wait := func(c chan struct{}) {
		select {
		case <-c:
		case <-time.After(30 * time.Second):
			t.Error("the other activity never started: the two do not overlap")
		}
	}
	fx.onReview = func() { once1.Do(func() { close(reviewIn) }); wait(jobIn) }
	fx.onJob = func() { once2.Do(func() { close(jobIn) }); wait(reviewIn) }
	p := runChallengePhaseWith(t, challengeInput(), edgeJob(), passAll, 1, nil, fx)
	if p.err != nil || p.jobs != 1 || len(p.commits) != 1 || p.result.ResultSHA != "c0ffee" || p.result.Challenge == nil || p.result.Challenge.Verdict != run.ChallengeGreen {
		t.Fatalf("err %v, jobs %d, commits %v, sha %q, challenge %+v; want a green challenge committed after both", p.err, p.jobs, p.commits, p.result.ResultSHA, p.result.Challenge)
	}
	return p
}

// A history recorded on the logic that always overlapped (parallelChallengeChange,
// no challengeWithReviewByMemoryChange marker, no input field) replays
// together, as it ran.
func TestHistoryRecordedBeforeTheMemoryMarkerReplaysTogether(t *testing.T) {
	p := runOverlapping(t, &challengeFixture{legacy: true})
	if p.result.Challenge == nil || !p.result.Challenge.WithReview {
		t.Errorf("challenge %+v; want WithReview recorded", p.result.Challenge)
	}
}

// With ChallengeWithReview false the challenger job follows the review, and
// the record says so.
func TestChallengerJobFollowsTheReviewWhenTheHostSaysSo(t *testing.T) {
	var order []string
	p := runChallengePhaseWith(t, challengeInput(), edgeJob(), passAll, 1, nil, &challengeFixture{inTurn: true, order: &order})
	if p.err != nil || strings.Join(order, ",") != "review-start,review-end,job-start,job-end" || len(p.commits) != 1 || p.result.Challenge == nil || p.result.Challenge.WithReview {
		t.Fatalf("err %v, order %v, commits %v, challenge %+v; want the review, then the job, WithReview false", p.err, order, p.commits, p.result.Challenge)
	}
}

// A history recorded before parallelChallengeChange runs the review, then the
// challenge phase.
func TestHistoryRecordedBeforeTheParallelChallengeReplaysSerially(t *testing.T) {
	var order []string
	p := runChallengePhaseWith(t, challengeInput(), edgeJob(), passAll, 1, nil, &challengeFixture{serial: true, order: &order})
	if p.err != nil || strings.Join(order, ",") != "review-start,review-end,job-start,job-end" || len(p.commits) != 1 {
		t.Fatalf("err %v, order %v, commits %v; want the review, then the job, then a commit", p.err, order, p.commits)
	}
}

// With no review verdict the challenge still runs and is recorded, and
// nothing is committed.
func TestChallengeRunsWithoutAReviewVerdictAndCommitsNothing(t *testing.T) {
	p := runChallengePhaseWith(t, challengeInput(), edgeJob(), passAll, 1, nil, &challengeFixture{noVerdict: true})
	c := p.result.Challenge
	if p.err != nil || p.jobs != 1 || c == nil || c.Verdict != run.ChallengeGreen || len(p.commits) != 0 || c.CommittedSHA != "" || p.result.ResultSHA == "c0ffee" {
		t.Fatalf("err %v, jobs %d, challenge %+v, commits %v; want a green record, no commit", p.err, p.jobs, c, p.commits)
	}
	serial := runChallengePhaseWith(t, challengeInput(), edgeJob(), passAll, 1, nil, &challengeFixture{noVerdict: true, serial: true})
	if serial.jobs != 0 || serial.result.Challenge != nil {
		t.Errorf("serial: jobs %d, challenge %+v; want none without a review verdict", serial.jobs, serial.result.Challenge)
	}
}

// Failed gates or a change over the size cap start no challenger job.
func TestNoChallengerJobWhenGatesFailedOrOverTheSizeCap(t *testing.T) {
	for name, fx := range map[string]*challengeFixture{
		"gates failed":   {gateFails: true},
		"over the cap":   {size: &run.ChangeSize{SourceLines: 1 << 20, SourceFiles: 2}},
		"over the files": {size: &run.ChangeSize{SourceLines: 1, SourceFiles: 1 << 10}},
	} {
		input := challengeInput()
		input.ReleaseMaxChangedLines, input.ReleaseMaxFilesChanged = 1000, 25
		p := runChallengePhaseWith(t, input, edgeJob(), passAll, 1, nil, fx)
		if p.err != nil || p.jobs != 0 || len(p.launches) != 0 || len(p.commits) != 0 || p.result.Challenge != nil {
			t.Errorf("%s: err %v, jobs %d, launches %v, challenge %+v; want no challenge", name, p.err, p.jobs, p.launches, p.result.Challenge)
		}
	}
}
