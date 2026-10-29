package workflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"

	"buildgate/internal/reviewstep"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

const (
	replayOnto = "1111111111111111111111111111111111111111"
	replayHead = "2222222222222222222222222222222222222222"
)

// replayFixtureInput is a replay run of fixtureInput: on its replay branch,
// with the diff base the commit the replay is onto.
func replayFixtureInput() RunWorkflowInput {
	input := fixtureInput()
	input.OnBranch = "buildgate/req-1/t2-restack1"
	input.DiffBaseSHA = replayOnto
	input.Replay = &ReplayInput{OfRunID: "req-1-002", SourceRunIDs: []string{"req-1-002"}, OntoSHA: replayOnto, HeadSHA: replayHead}
	return input
}

// replayFixture is a workflow environment for a replay run: RunBuildActivity
// fails the test if it is ever called, ApplyReplayActivity answers with one
// carried, sandboxed build attempt, and every Activity started is recorded by
// name, in order.
type replayFixture struct {
	env     *testsuite.TestWorkflowEnvironment
	mu      sync.Mutex
	started []string
}

func newReplayFixture(t *testing.T) *replayFixture {
	t.Helper()
	f := &replayFixture{}
	f.env = newWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			t.Error("RunBuildActivity was called for a replay run")
			return BuildActivityResult{}, errors.New("no build for a replay run")
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	f.env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
		return BuildActivityResult{
			Result:   runner.Result{ExitCode: 0},
			Attempts: []run.Attempt{{Kind: "build", ImageDigest: "sha256:" + strings.Repeat("a", 64), ReplayOf: "req-1-002"}},
		}, nil
	}, activity.RegisterOptions{Name: ApplyReplayActivityName})
	f.env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, _ converter.EncodedValues) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.started = append(f.started, info.ActivityType.Name)
	})
	return f
}

func (f *replayFixture) run(t *testing.T, input RunWorkflowInput) (RunWorkflowResult, error) {
	t.Helper()
	f.env.ExecuteWorkflow(RunWorkflow, input)
	var result RunWorkflowResult
	err := f.env.GetWorkflowError()
	if err == nil {
		if getErr := f.env.GetWorkflowResult(&result); getErr != nil {
			t.Fatalf("workflow result: %v", getErr)
		}
	}
	return result, err
}

func assertReplayRefused(t *testing.T, err error) {
	t.Helper()
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != ReplayFailureType || !appErr.NonRetryable() {
		t.Fatalf("error = %v, want a non-retryable %s", err, ReplayFailureType)
	}
	if HaltReasonCodeFromError(err) != run.HaltReasonReplayRefused {
		t.Errorf("halt reason = %q, want %q", HaltReasonCodeFromError(err), run.HaltReasonReplayRefused)
	}
}

// TestRunWorkflowReplaySchedulesNoBuildActivity: a replay run schedules
// ApplyReplayActivity where an ordinary run builds, and runs every step
// after the build in the ordinary order.
func TestRunWorkflowReplaySchedulesNoBuildActivity(t *testing.T) {
	f := newReplayFixture(t)
	result, err := f.run(t, replayFixtureInput())
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("replay run: err=%v state=%q, want accepted", err, result.State)
	}
	want := []string{CaptureBaseSHAActivityName, PrepareIsolatedWorkspaceActivityName, PreflightActivityName, RunBaselineVerifyActivityName, ApplyReplayActivityName, PostBuildActivityName, RunVerifyActivityName, CollectEvidenceActivityName}
	if got := f.started[:min(len(want), len(f.started))]; !slices.Equal(got, want) {
		t.Fatalf("Activities started = %v, want them to begin %v", f.started, want)
	}
	if slices.Contains(f.started, RunBuildActivityName) {
		t.Fatalf("RunBuildActivity started for a replay run: %v", f.started)
	}
	if f.started[len(f.started)-1] != DisableWorkerGroupWriteActivityName || !slices.Contains(f.started, EvaluateRunActivityName) {
		t.Errorf("Activities started = %v, want the run evaluated and its worktree kept", f.started)
	}
	if len(result.Attempts) == 0 || result.Attempts[0].ReplayOf != "req-1-002" {
		t.Errorf("attempts = %+v, want the carried build attempt first", result.Attempts)
	}
}

// TestRunWorkflowReplayRunsEveryGateAndReview: a replay run runs the full
// suite, the named and repository gates and the review, and its evidence and
// review are measured from the commit the replay is onto.
func TestRunWorkflowReplayRunsEveryGateAndReview(t *testing.T) {
	f := newReplayFixture(t)
	input := replayFixtureInput()
	input.FullSuiteCommand = "make full"
	setGateCommand(&input, "repo-lint", "make lint")
	setGateCommand(&input, "repo-docs", "make docs")
	input.CodeReviewPolicy = "advisory"
	var mu sync.Mutex
	var gates []string
	var evidenceBase, reviewBase string
	f.env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
		mu.Lock()
		gates = append(gates, "full_suite_verify")
		mu.Unlock()
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make full"}, ExitCode: 0}}, nil
	}, activity.RegisterOptions{Name: RunFullSuiteVerifyActivityName})
	f.env.RegisterActivityWithOptions(func(_ context.Context, in RepoGateActivityInput) (VerifyActivityResult, error) {
		mu.Lock()
		gates = append(gates, in.Check)
		mu.Unlock()
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0}}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	f.env.RegisterActivityWithOptions(func(_ context.Context, in CollectEvidenceInput) (CollectedEvidence, error) {
		evidenceBase = in.BaseSHA
		return CollectedEvidence{}, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	f.env.RegisterActivityWithOptions(func(_ context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		reviewBase = in.effectiveDiffBase()
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: reviewstep.CombinedExitBase}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})

	result, err := f.run(t, input)
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("replay run: err=%v state=%q, want accepted", err, result.State)
	}
	for _, check := range []string{"full_suite_verify", "repo-lint", "repo-docs"} {
		if !slices.Contains(gates, check) {
			t.Errorf("gates run = %v, want %s among them", gates, check)
		}
	}
	if !slices.Contains(f.started, RunReviewStepActivityName) {
		t.Errorf("Activities started = %v, want the review", f.started)
	}
	if evidenceBase != replayOnto || reviewBase != replayOnto {
		t.Errorf("evidence base %q, review base %q; want both %q", evidenceBase, reviewBase, replayOnto)
	}
}

// TestRunWorkflowRefusesAnIncoherentReplayInput: a replay input whose parts
// do not fit is refused before any Activity starts.
func TestRunWorkflowRefusesAnIncoherentReplayInput(t *testing.T) {
	cases := map[string]func(*RunWorkflowInput){
		"no branch":          func(in *RunWorkflowInput) { in.OnBranch = "" },
		"diff base not onto": func(in *RunWorkflowInput) { in.DiffBaseSHA = replayHead },
		"resume":             func(in *RunWorkflowInput) { in.ResumeFrom = &ResumeFrom{RunID: "lost"} },
		"prior run":          func(in *RunWorkflowInput) { in.PriorRunID = "req-1-001" },
		"earlier attempt":    func(in *RunWorkflowInput) { in.EarlierAttemptPath = "/fixture/earlier.md" },
		"baseline check":     func(in *RunWorkflowInput) { in.BaselineCheck = true },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newReplayFixture(t)
			input := replayFixtureInput()
			mutate(&input)
			_, err := f.run(t, input)
			assertReplayRefused(t, err)
			if len(f.started) != 0 {
				t.Errorf("Activities started before the refusal: %v", f.started)
			}
		})
	}
}

// TestOrdinaryRunAsksNoReplayVersion: a run that is not a replay records no
// replay-run marker, so every history recorded before replay runs replays
// unchanged: the marker is asked for only with a replay input, and
// TestReplayRunAtDefaultVersionRefusesInsteadOfBuilding pins what such an
// input does on a history without it.
func TestOrdinaryRunAsksNoReplayVersion(t *testing.T) {
	f := newReplayFixture(t)
	// Asking for the version at all fails the run.
	f.env.OnGetVersion(replayRunChange, temporalworkflow.DefaultVersion, 1).Panic("an ordinary run asked for the replay-run version")
	f.env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
		return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
	}, activity.RegisterOptions{Name: RunBuildActivityName})
	result, err := f.run(t, fixtureInput())
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("ordinary run: err=%v state=%q, want accepted", err, result.State)
	}
	if slices.Contains(f.started, ApplyReplayActivityName) {
		t.Errorf("an ordinary run started %s: %v", ApplyReplayActivityName, f.started)
	}
}

// TestReplayRunAtDefaultVersionRefusesInsteadOfBuilding: a replay input on a
// history without the replay-run marker is refused; it is never built.
func TestReplayRunAtDefaultVersionRefusesInsteadOfBuilding(t *testing.T) {
	f := newReplayFixture(t)
	f.env.OnGetVersion(replayRunChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	_, err := f.run(t, replayFixtureInput())
	assertReplayRefused(t, err)
	if len(f.started) != 0 {
		t.Errorf("Activities started at DefaultVersion: %v", f.started)
	}
}

// TestReplayRunReviewSchedulesNoReproduction: a replay run (replayRunChange
// version 1) reaches the review at reviewReproductionChange version 1 with no
// ReviewReproduction input, so its review is advice only: a review that held
// a reproduction launches nothing and records no review_reproduction check.
func TestReplayRunReviewSchedulesNoReproduction(t *testing.T) {
	f := newReplayFixture(t)
	input := replayFixtureInput()
	input.CodeReviewPolicy = "advisory"
	f.env.OnGetVersion(replayRunChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.Version(1))
	f.env.OnGetVersion(reviewReproductionChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.Version(1))
	f.env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"python3", "combined_review.py"}, ExitCode: reviewstep.CombinedExitBase}, Reproductions: []run.HeldReproduction{heldFixture()}}, nil
	}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	f.env.RegisterActivityWithOptions(func(context.Context, ReproductionRunInput) (ReproductionRunResult, error) {
		t.Error("a replay run launched a reproduction")
		return ReproductionRunResult{}, nil
	}, activity.RegisterOptions{Name: RunHeldTestActivityName})

	result, err := f.run(t, input)
	if err != nil || result.State != run.StateAccepted {
		t.Fatalf("replay run: err=%v state=%q, want accepted", err, result.State)
	}
	if !slices.Contains(f.started, ApplyReplayActivityName) || !slices.Contains(f.started, RunReviewStepActivityName) {
		t.Errorf("Activities started = %v, want the replay applied and the review run", f.started)
	}
	if slices.Contains(f.started, RunHeldTestActivityName) || result.ReviewReproductions != nil {
		t.Errorf("Activities started = %v, findings %+v; want no reproduction", f.started, result.ReviewReproductions)
	}
}
