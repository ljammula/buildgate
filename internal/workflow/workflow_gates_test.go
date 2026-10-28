package workflow

import (
	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
	"context"
	"slices"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	temporalworkflow "go.temporal.io/sdk/workflow"
)

// registerFullSuiteWorkflowEnvironment is newWorkflowEnvironment plus a
// RunFullSuiteVerifyActivityName registration, for tests that need to
// declare RunWorkflowInput.FullSuiteCommand (gap 3 of the plan's
// 2026-08-28 readiness review, the regression oracle) — newWorkflowEnvironment
// itself has no such mock since every test using it leaves FullSuiteCommand
// unset and must never invoke this Activity at all.
func registerFullSuiteWorkflowEnvironment(
	t *testing.T,
	build func(context.Context, RunWorkflowInput) (BuildActivityResult, error),
	verify func(context.Context, RunWorkflowInput) (VerifyActivityResult, error),
	fullSuite func(context.Context, RunWorkflowInput) (VerifyActivityResult, error),
) *testsuite.TestWorkflowEnvironment {
	t.Helper()
	env := newWorkflowEnvironment(t, build, verify, func(context.Context, EvaluateGateInput) (run.GateResult, error) {
		return run.GateResult{Check: "canonical_verify", Passed: true}, nil
	})
	env.RegisterActivityWithOptions(fullSuite, activity.RegisterOptions{Name: RunFullSuiteVerifyActivityName})
	return env
}

// TestRunWorkflowFullSuiteVerifyQuarantinesOnRegression is gap 3's actual
// target scenario, ported to the Temporal path: canonical_verify (the
// ticket's own targeted test) passes, but the declared FullSuiteCommand
// fails — this slice appears to have regressed a different, already-
// accepted slice's test. The run must quarantine even though its own
// canonical_verify passed.
func TestRunWorkflowFullSuiteVerifyQuarantinesOnRegression(t *testing.T) {
	input := fixtureInput()
	input.FullSuiteCommand = "make verify-full"
	fullSuiteCalls := 0
	env := registerFullSuiteWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			fullSuiteCalls++
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify-full"}, ExitCode: 1}}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", result.State, run.StateQuarantined)
	}
	if fullSuiteCalls != 1 {
		t.Fatalf("full-suite verify Activity calls = %d, want 1", fullSuiteCalls)
	}
	var fullSuiteGate *run.GateResult
	for i := range result.GateResults {
		if result.GateResults[i].Check == "full_suite_verify" {
			fullSuiteGate = &result.GateResults[i]
		}
	}
	if fullSuiteGate == nil || fullSuiteGate.Passed {
		t.Fatalf("GateResults = %+v, want a failing full_suite_verify gate", result.GateResults)
	}
}

// TestRunWorkflowSkipsFullSuiteVerifyWhenNotDeclared confirms the gate is
// opt-in on the Temporal path too, the same convention cmd/factoryd follows: no FullSuiteCommand means RunWorkflow never calls
// RunFullSuiteVerifyActivityName at all.
func TestRunWorkflowSkipsFullSuiteVerifyWhenNotDeclared(t *testing.T) {
	fullSuiteCalls := 0
	env := registerFullSuiteWorkflowEnvironment(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			fullSuiteCalls++
			return VerifyActivityResult{}, nil
		},
	)

	env.ExecuteWorkflow(RunWorkflow, fixtureInput())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q", result.State, run.StateAccepted)
	}
	if fullSuiteCalls != 0 {
		t.Fatalf("full-suite verify Activity calls = %d, want 0 when FullSuiteCommand is not declared", fullSuiteCalls)
	}
	for _, g := range result.GateResults {
		if g.Check == "full_suite_verify" {
			t.Fatalf("GateResults = %+v, want no full_suite_verify gate", result.GateResults)
		}
	}
}

// TestRunWorkflowRunsNamedGates is the named gates' "Done when" case
// ported to the Temporal path: both configured repository gates (repo-lint,
// repo-security) each call RunNamedGateActivityName exactly once, and
// a failing one
// quarantines the run with its own gate name as the cause -- the same
// contract TestIntegrationSecurityCommandQuarantinesWithGateName pins.
func TestRunWorkflowRunsNamedGates(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, "repo-lint", "golangci-lint run ./...")
	setGateCommand(&input, "repo-security", "govulncheck ./...")
	var calls []string
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
	env.RegisterActivityWithOptions(func(_ context.Context, in RepoGateActivityInput) (VerifyActivityResult, error) {
		calls = append(calls, in.Check)
		exitCode := 0
		if in.Check == "repo-security" {
			exitCode = 1
		}
		result := VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: exitCode}}
		if exitCode != 0 {
			result.BaseCheck = &run.GateBaseCheck{Outcome: run.GateBaseFailsSame, BaseSHA: "base-sha", ExitCode: 1}
		}
		return result, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", result.State, run.StateQuarantined)
	}
	if !slices.Equal(calls, []string{"repo-lint", "repo-security"}) {
		t.Fatalf("repository gate Activity calls = %v, want [repo-lint repo-security] in that order", calls)
	}
	// The gate Activity's rerun on the base commit reaches the run's gate
	// result; a gate that passed has none.
	for _, g := range result.GateResults {
		if want := g.Check == "repo-security"; g.FailsSameOnBase() != want {
			t.Errorf("%s: fails the same way on base = %v, want %v (%+v)", g.Check, g.FailsSameOnBase(), want, g.BaseCheck)
		}
	}
	seen := map[string]run.GateResult{}
	for _, g := range result.GateResults {
		seen[g.Check] = g
	}
	if g, ok := seen["repo-lint"]; !ok || !g.Passed {
		t.Errorf("expected a passing repo-lint gate, got %+v (present=%v)", g, ok)
	}
	if g, ok := seen["repo-security"]; !ok || g.Passed {
		t.Errorf("expected a failing repo-security gate, got %+v (present=%v)", g, ok)
	}
	if _, ok := seen["repo-unit"]; ok {
		t.Errorf("expected no repo-unit gate (not configured), got %+v", seen["repo-unit"])
	}
}

// TestRunWorkflowRunsFullSuiteVerifyBeforeCollectingEvidence pins a real P1
// finding from GitHub's own Codex App review round on PR #33: running
// RunFullSuiteVerifyActivity after CollectEvidenceActivity meant a
// full-suite command that itself mutates the checkout (codegen,
// formatting, snapshot updates, coverage artifacts) and exits 0 could
// leave an accepted worktree containing changes invisible to
// CollectEvidenceActivity's ResultSHA/ChangedFiles/diff/dependency/
// required-content evidence and diff_scope, since that Activity had
// already run and committed by the time the full-suite one executed. The
// fix reordered them; this test pins the order directly so a regression
// fails immediately rather than needing a mutating fixture command to
// notice.
func TestRunWorkflowRunsFullSuiteVerifyBeforeCollectingEvidence(t *testing.T) {
	input := fixtureInput()
	input.FullSuiteCommand = "make verify-full"
	var order []string
	var mu sync.Mutex
	record := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, name)
	}

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
	env.RegisterActivityWithOptions(
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			record(RunFullSuiteVerifyActivityName)
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify-full"}, ExitCode: 0}}, nil
		},
		activity.RegisterOptions{Name: RunFullSuiteVerifyActivityName},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
			record(CollectEvidenceActivityName)
			return CollectedEvidence{}, nil
		},
		activity.RegisterOptions{Name: CollectEvidenceActivityName},
	)

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if !slices.Equal(order, []string{RunFullSuiteVerifyActivityName, CollectEvidenceActivityName}) {
		t.Fatalf("Activity call order = %v, want [%s, %s]", order, RunFullSuiteVerifyActivityName, CollectEvidenceActivityName)
	}
}

// repoGateFixture runs RunWorkflow with two gates the
// repository defines for itself, and returns the checks the gate Activity
// was asked to run, in order.
func repoGateFixture(t *testing.T, pin func(*testsuite.TestWorkflowEnvironment)) []string {
	t.Helper()
	input := fixtureInput()
	setGateCommand(&input, "repo-zeta", "true")
	setGateCommand(&input, "repo-alpha", "true")
	var calls []string
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
	env.RegisterActivityWithOptions(func(_ context.Context, in RepoGateActivityInput) (VerifyActivityResult, error) {
		calls = append(calls, in.Check)
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0}}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	if pin != nil {
		pin(env)
	}
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	return calls
}

// A repository's own gates (.factory.yml `gates:`) run by name, through the
// gate Activity.
func TestRunWorkflowRunsRepoDefinedGatesAfterTheRegistrys(t *testing.T) {
	calls := repoGateFixture(t, nil)
	if want := []string{"repo-alpha", "repo-zeta"}; !slices.Equal(calls, want) {
		t.Fatalf("repository gate Activity calls = %v, want %v", calls, want)
	}
}

// TestHistoryRecordedBeforeRepoDefinedGatesReplaysWithoutThem pins the change
// to DefaultVersion, what a history recorded before repo-defined-gates replays
// as: it schedules no repository gate, so the activities it recorded still
// line up.
func TestHistoryRecordedBeforeRepoDefinedGatesReplaysWithoutThem(t *testing.T) {
	calls := repoGateFixture(t, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(repoDefinedGatesChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	})
	if len(calls) != 0 {
		t.Fatalf("repository gate Activity calls = %v, want none", calls)
	}
	if want := []string(nil); !slices.Equal(calls, want) {
		t.Fatalf("repository gate Activity calls = %v, want %v", calls, want)
	}
}

// A repo-defined gate that exits non-zero quarantines the run and is named
// in its gate results, like a registry gate.
func TestRunWorkflowQuarantinesOnAFailingRepoDefinedGate(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, "repo-alpha", "true")
	setGateCommand(&input, "repo-zeta", "false")
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
	env.RegisterActivityWithOptions(func(_ context.Context, in RepoGateActivityInput) (VerifyActivityResult, error) {
		exit := 0
		if in.Check == "repo-zeta" {
			exit = 1
		}
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: exit}}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want %q", result.State, run.StateQuarantined)
	}
	passed := map[string]bool{}
	for _, g := range result.GateResults {
		if policy.IsRepoGate(g.Check) {
			passed[g.Check] = g.Passed
		}
	}
	if len(passed) != 2 || !passed["repo-alpha"] || passed["repo-zeta"] {
		t.Errorf("repo gate results = %v, want repo-alpha passed and repo-zeta failed", passed)
	}
}

// A history recorded before repo-defined-gates is handed no repo gate.
func TestGateCommandsLoseRepoGatesWhenTheChangeIsOff(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, "repo-alpha", "a")
	setGateCommand(&input, "other", "o")
	var s testsuite.WorkflowTestSuite
	for version, wantRepo := range map[temporalworkflow.Version]bool{temporalworkflow.DefaultVersion: false, 1: true} {
		env := s.NewTestWorkflowEnvironment()
		env.OnGetVersion(repoDefinedGatesChange, temporalworkflow.DefaultVersion, 1).Return(version)
		var checks []string
		var commands map[string]string
		env.ExecuteWorkflow(func(ctx temporalworkflow.Context) error {
			checks, commands = gateChecksToRun(ctx, input)
			return nil
		})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		_, hasRepo := commands["repo-alpha"]
		if hasRepo != wantRepo || slices.Contains(checks, "repo-alpha") != wantRepo || commands["other"] == "" {
			t.Errorf("version %d: checks %v, commands %v; want repo gate present = %v and the other command kept", version, checks, commands, wantRepo)
		}
	}
}
