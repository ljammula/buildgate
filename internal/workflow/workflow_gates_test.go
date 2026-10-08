package workflow

import (
	"buildgate/internal/policy"
	"buildgate/internal/projectconfig"
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
// ported to the Temporal path: both configured named gates (lint,
// security_audit) each call RunNamedGateActivityName exactly once, and
// a failing one
// quarantines the run with its own gate name as the cause -- the same
// contract TestIntegrationSecurityCommandQuarantinesWithGateName pins.
func TestRunWorkflowRunsNamedGates(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, "lint", "golangci-lint run ./...")
	setGateCommand(&input, "security_audit", "govulncheck ./...")
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
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		calls = append(calls, in.Check)
		exitCode := 0
		if in.Check == "security_audit" {
			exitCode = 1
		}
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: exitCode}}, nil
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
	if !slices.Equal(calls, []string{"lint", "security_audit"}) {
		t.Fatalf("named gate Activity calls = %v, want [lint security_audit] in that order", calls)
	}
	seen := map[string]run.GateResult{}
	for _, g := range result.GateResults {
		seen[g.Check] = g
	}
	if g, ok := seen["lint"]; !ok || !g.Passed {
		t.Errorf("expected a passing lint gate, got %+v (present=%v)", g, ok)
	}
	if g, ok := seen["security_audit"]; !ok || g.Passed {
		t.Errorf("expected a failing security_audit gate, got %+v (present=%v)", g, ok)
	}
	if _, ok := seen["unit_tests"]; ok {
		t.Errorf("expected no unit_tests gate (not configured), got %+v", seen["unit_tests"])
	}
}

// TestRunWorkflowRunsAFakeSixthRegistryGate is M4-K2's own registry-
// plumbing proof for RunWorkflow's gate loop: it temporarily appends a
// gate to policy.CommandGates that names nothing else in this test suite
// (no CLI flag, no projectconfig key), and confirms RunWorkflow still
// calls RunNamedGateActivityName for it -- proving the loop is driven by
// iterating policy.CommandGates itself, not a hardcoded list of today's
// five gate names that a real 6th entry would need a matching code change
// here to reach.
func TestRunWorkflowRunsAFakeSixthRegistryGate(t *testing.T) {
	original := policy.CommandGates
	policy.CommandGates = append(append([]policy.CommandGate{}, original...), policy.CommandGate{
		ID:                     "a_hypothetical_sixth_gate",
		Flag:                   "sixth-gate-command",
		ProjectKey:             "sixth_gate_command",
		Help:                   "test-only fake registry entry",
		RerunAfterOracleCommit: true,
	})
	t.Cleanup(func() { policy.CommandGates = original })

	input := fixtureInput()
	setGateCommand(&input, "a_hypothetical_sixth_gate", "true")
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
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		calls = append(calls, in.Check)
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0}}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if !slices.Equal(calls, []string{"a_hypothetical_sixth_gate"}) {
		t.Fatalf("named gate Activity calls = %v, want [a_hypothetical_sixth_gate]", calls)
	}
}

// TestRunWorkflowThreadsReferenceOracleDirAndSHA256 is the workflow-level
// counterpart to TestRunNamedGateActivityComputesReferenceOracleHash --
// it checks RunWorkflow's own wiring (ReferenceOracleDir/MountPath reach
// NamedGateActivityInput, and the Activity's returned
// ReferenceOracleSHA256 lands in the final GateResults), with the
// Activity itself mocked, mirroring TestRunWorkflowRunsNamedGates' own
// style rather than re-testing the Activity's real snapshot/hash logic
// (already covered directly in activities_test.go).
func TestRunWorkflowThreadsReferenceOracleDirAndSHA256(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, policy.ReferenceOracleGateID, "go test ./verify/... -run TestOracle")
	input.ReferenceOracleDir = "/oracle/store"
	input.ReferenceOracleMountPath = "verify"
	var gotDir, gotMountPath string
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
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		gotDir = in.ReferenceOracleDir
		gotMountPath = in.ReferenceOracleMountPath
		return VerifyActivityResult{
			Result:                runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: 0},
			ReferenceOracleSHA256: "deadbeef",
		}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})

	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	var result RunWorkflowResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatalf("workflow result: %v", err)
	}
	if gotDir != "/oracle/store" || gotMountPath != "verify" {
		t.Errorf("NamedGateActivityInput carried ReferenceOracleDir=%q ReferenceOracleMountPath=%q, want /oracle/store and verify", gotDir, gotMountPath)
	}
	var found bool
	for _, g := range result.GateResults {
		if g.Check != "reference_oracle" {
			continue
		}
		found = true
		if g.ReferenceOracleSHA256 != "deadbeef" {
			t.Errorf("GateResults reference_oracle.ReferenceOracleSHA256 = %q, want %q", g.ReferenceOracleSHA256, "deadbeef")
		}
	}
	if !found {
		t.Fatal("no reference_oracle GateResult in the workflow result")
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

// repoGateFixture runs RunWorkflow with one registry gate and two gates the
// repository defines for itself, and returns the checks the gate Activity
// was asked to run, in order.
func repoGateFixture(t *testing.T, pin func(*testsuite.TestWorkflowEnvironment)) []string {
	t.Helper()
	input := fixtureInput()
	setGateCommand(&input, "lint", "true")
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
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
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

// A repository's own gates (.factory.yml `gates:`) run after the registry's,
// by name, through the same gate Activity.
func TestRunWorkflowRunsRepoDefinedGatesAfterTheRegistrys(t *testing.T) {
	calls := repoGateFixture(t, nil)
	if want := []string{"lint", "repo-alpha", "repo-zeta"}; !slices.Equal(calls, want) {
		t.Fatalf("named gate Activity calls = %v, want %v", calls, want)
	}
}

// TestHistoryRecordedBeforeRepoDefinedGatesReplaysWithoutThem pins the change
// to DefaultVersion, what a history recorded before repo-defined-gates replays
// as: it schedules the registry's gates and nothing else, so the activities it
// recorded still line up.
func TestHistoryRecordedBeforeRepoDefinedGatesReplaysWithoutThem(t *testing.T) {
	calls := repoGateFixture(t, func(env *testsuite.TestWorkflowEnvironment) {
		env.OnGetVersion(repoDefinedGatesChange, temporalworkflow.DefaultVersion, 1).Return(temporalworkflow.DefaultVersion)
	})
	if want := []string{"lint"}; !slices.Equal(calls, want) {
		t.Fatalf("named gate Activity calls = %v, want %v", calls, want)
	}
}

// After an oracle commit a repository's own gates run again against the
// committed tree, as lint does.
func TestPostOracleCommitGatesIncludeRepoDefinedGates(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, "lint", "golangci-lint run")
	setGateCommand(&input, policy.ReferenceOracleGateID, "python3 oracle.py")
	setGateCommand(&input, "repo-zeta", "z")
	setGateCommand(&input, "repo-alpha", "a")
	var got []string
	for _, g := range postOracleCommitGates(input) {
		got = append(got, g.Check+"="+g.Command)
	}
	if want := []string{"lint=golangci-lint run", "repo-alpha=a", "repo-zeta=z"}; !slices.Equal(got, want) {
		t.Errorf("post-oracle-commit gates = %v, want %v", got, want)
	}
	if !isPostOracleCommitAttemptKind("repo-alpha"+postOracleCommitGateSuffix) || isPostOracleCommitAttemptKind("reference_oracle"+postOracleCommitGateSuffix) {
		t.Error("a repo gate's post-oracle-commit attempt kind must be recognised, and reference_oracle's must not")
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
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
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

// A history recorded before repo-defined-gates hands the post-oracle-commit
// rerun no repo gate either: one that never ran in the main pass has no
// result for a failed rerun to replace.
func TestGateCommandsLoseRepoGatesWhenTheChangeIsOff(t *testing.T) {
	input := fixtureInput()
	setGateCommand(&input, "lint", "golangci-lint run")
	setGateCommand(&input, "repo-alpha", "a")
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
		if hasRepo != wantRepo || slices.Contains(checks, "repo-alpha") != wantRepo || commands["lint"] == "" {
			t.Errorf("version %d: checks %v, commands %v; want repo gate present = %v and lint kept", version, checks, commands, wantRepo)
		}
		if got := postOracleCommitGates(RunWorkflowInput{GateCommands: commands}); (len(got) == 2) != wantRepo {
			t.Errorf("version %d: post-oracle-commit gates = %v", version, got)
		}
	}
	if projectconfig.RepoGateReservedSuffix != postOracleCommitGateSuffix {
		t.Errorf("projectconfig.RepoGateReservedSuffix = %q, want the rerun kind suffix %q", projectconfig.RepoGateReservedSuffix, postOracleCommitGateSuffix)
	}
}
