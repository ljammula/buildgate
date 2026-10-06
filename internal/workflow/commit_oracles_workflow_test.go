package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"

	"go.temporal.io/sdk/activity"
)

func passingPostOracleCommitVerify(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
	return PostOracleCommitVerifyResult{
		Verify: VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 0}},
		Clean:  true,
	}, nil
}

func oracleWorkflowEnv(t *testing.T, referenceExit, specExit int, commit func(context.Context, CommitOraclesInput) (CommittedOracles, error)) func() RunWorkflowResult {
	t.Helper()
	execute, _ := oracleWorkflowEnvWith(t, referenceExit, specExit, "", commit, passingPostOracleCommitVerify)
	return execute
}

// oracleWorkflowEnvWith also registers RunPostOracleCommitVerifyActivity (post)
// and, when fullSuiteCommand is set, a passing RunFullSuiteVerifyActivity. The
// first returned func fails the test on a workflow error; the second returns it.
func oracleWorkflowEnvWith(t *testing.T, referenceExit, specExit int, fullSuiteCommand string, commit func(context.Context, CommitOraclesInput) (CommittedOracles, error), post func(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error), mutate ...func(*RunWorkflowInput)) (func() RunWorkflowResult, func() (RunWorkflowResult, error)) {
	t.Helper()
	input := fixtureInput()
	input.FullSuiteCommand = fullSuiteCommand
	setGateCommand(&input, policy.ReferenceOracleGateID, "go test ./.oracle/...")
	input.ReferenceOracleDir = "/oracle/store"
	input.AllowedFiles = []string{"pkg/x.go"}
	for _, m := range mutate {
		m(&input)
	}
	if specExit >= 0 {
		input.SpecAcceptanceCriteria = "criterion"
	}
	env := newWorkflowEnvironmentSansCommitOracles(t,
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
		return VerifyActivityResult{
			Result:                runner.Result{Command: []string{"sh", "-c", in.Command}, ExitCode: referenceExit},
			ReferenceOracleSHA256: "treehash",
		}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	env.RegisterActivityWithOptions(func(_ context.Context, collectIn CollectEvidenceInput) (CollectedEvidence, error) {
		ev := CollectedEvidence{ResultSHA: "collect-sha", ChangedFiles: []string{"pkg/x.go"}}
		if len(collectIn.RequiredContent) > 0 {
			// Pre-commit snapshot: the required marker is not in the tree yet.
			ev.RequiredContentBaseFiles = map[string]string{"pkg/o.go": ""}
			ev.RequiredContentFinalFiles = map[string]string{"pkg/o.go": "before the oracle commit"}
		}
		return ev, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	if specExit >= 0 {
		env.RegisterActivityWithOptions(func(context.Context, ReviewStepInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"conformity"}, ExitCode: specExit}}, nil
		}, activity.RegisterOptions{Name: RunReviewStepActivityName})
	}
	env.RegisterActivityWithOptions(commit, activity.RegisterOptions{Name: CommitOraclesActivityName})
	if fullSuiteCommand != "" {
		env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", fullSuiteCommand}, ExitCode: 0}}, nil
		}, activity.RegisterOptions{Name: RunFullSuiteVerifyActivityName})
	}
	if post != nil {
		env.RegisterActivityWithOptions(post, activity.RegisterOptions{Name: RunPostOracleCommitVerifyActivityName})
	}
	executeErr := func() (RunWorkflowResult, error) {
		env.ExecuteWorkflow(RunWorkflow, input)
		if err := env.GetWorkflowError(); err != nil {
			return RunWorkflowResult{}, err
		}
		var result RunWorkflowResult
		if err := env.GetWorkflowResult(&result); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}
	return func() RunWorkflowResult {
		result, err := executeErr()
		if err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		return result
	}, executeErr
}

func committedOraclesFixture() func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
	return func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
		return CommittedOracles{Active: true, Committed: true, ResultSHA: "oracle-sha", ChangedFiles: []string{"pkg/x.go"}}, nil
	}
}

func TestRunWorkflowReVerifiesAfterOracleCommitAndProceedsWhenItPasses(t *testing.T) {
	calls := 0
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "make verify-full", committedOraclesFixture(), func(ctx context.Context, in RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		calls++
		res, err := passingPostOracleCommitVerify(ctx, in)
		res.FullSuite = &VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify-full"}, ExitCode: 0}}
		res.Attempts = []run.Attempt{{Kind: "verify_after_oracle_commit"}, {Kind: "full_suite_verify_after_oracle_commit"}}
		return res, err
	})
	result := execute()
	if calls != 1 {
		t.Fatalf("post-commit verify Activity calls = %d, want 1", calls)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, gates = %+v", result.State, result.GateResults)
	}
	n := 0
	for _, a := range result.Attempts {
		if strings.HasSuffix(a.Kind, "_after_oracle_commit") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("post-commit attempts were not appended to result.Attempts: %+v", result.Attempts)
	}
}

func TestRunWorkflowPostOracleCommitVerifyFailureReplacesVerifyAndQuarantines(t *testing.T) {
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "", committedOraclesFixture(), func(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		return PostOracleCommitVerifyResult{
			Verify: VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 2}, LogSHA256: "postlog"},
			Clean:  true,
		}, nil
	})
	result := execute()
	if result.Verify.ExitCode != 2 {
		t.Fatalf("result.Verify = %+v, want the failing re-run", result.Verify)
	}
	// EvaluateRunActivity is the real policy here, so the canonical_verify gate
	// must have quarantined the run on the replaced result.
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined; gates = %+v", result.State, result.GateResults)
	}
}

func TestRunWorkflowPostOracleCommitFullSuiteFailureReplacesFullSuiteAndQuarantines(t *testing.T) {
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "make verify-full", committedOraclesFixture(), func(ctx context.Context, in RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		res, err := passingPostOracleCommitVerify(ctx, in)
		res.FullSuite = &VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify-full"}, ExitCode: 1}}
		return res, err
	})
	result := execute()
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q, want quarantined; gates = %+v", result.State, result.GateResults)
	}
	var gate *run.GateResult
	for i := range result.GateResults {
		if result.GateResults[i].Check == "full_suite_verify" {
			gate = &result.GateResults[i]
		}
	}
	if gate == nil || gate.Passed {
		t.Fatalf("want a failing full_suite_verify gate, got %+v", result.GateResults)
	}
}

func TestRunWorkflowFailsWhenPostOracleCommitVerifyLeavesWorkspaceDirty(t *testing.T) {
	_, executeErr := oracleWorkflowEnvWith(t, 0, -1, "", committedOraclesFixture(), func(ctx context.Context, in RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		res, err := passingPostOracleCommitVerify(ctx, in)
		res.Clean = false
		return res, err
	})
	_, err := executeErr()
	if err == nil || !strings.Contains(err.Error(), "left the workspace dirty") {
		t.Fatalf("err = %v, want the dirty-workspace failure", err)
	}
}

func TestRunWorkflowPostOracleCommitVerifyInfrastructureFailureFailsRun(t *testing.T) {
	_, executeErr := oracleWorkflowEnvWith(t, 0, -1, "", committedOraclesFixture(), func(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		return PostOracleCommitVerifyResult{}, errors.New("docker exploded")
	})
	if _, err := executeErr(); err == nil {
		t.Fatal("an infrastructure failure of the post-commit re-verify was swallowed")
	}
}

func TestRunWorkflowDoesNotReVerifyWhenNothingWasCommitted(t *testing.T) {
	for name, out := range map[string]CommittedOracles{
		"inactive":  {},
		"no oracle": {Active: true, Committed: false, ResultSHA: "collect-sha"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			execute, _ := oracleWorkflowEnvWith(t, 0, -1, "", func(context.Context, CommitOraclesInput) (CommittedOracles, error) { return out, nil },
				func(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
					calls++
					return PostOracleCommitVerifyResult{}, nil
				})
			execute()
			if calls != 0 {
				t.Fatalf("post-commit verify Activity ran %d times although nothing was committed", calls)
			}
		})
	}
}

func TestRunWorkflowCommitsOraclesBeforePolicyAndUsesTheNewResult(t *testing.T) {
	var gotPin string
	oracle := "pkg/x_oracle_test.go"
	execute := oracleWorkflowEnv(t, 0, -1, func(_ context.Context, in CommitOraclesInput) (CommittedOracles, error) {
		gotPin = in.PinnedOracleSHA256
		return CommittedOracles{
			Active: true, Committed: true, ResultSHA: "oracle-sha",
			ChangedFiles: []string{".buildgate/oracles.json", "pkg/x.go", oracle},
			Oracles: &run.OracleEvidence{
				Authored:     []run.OracleFile{{Path: oracle, SHA256: "h"}, {Path: ".buildgate/oracles.json", SHA256: "i"}},
				ResultSHA256: map[string]string{oracle: "h", ".buildgate/oracles.json": "i"},
			},
		}, nil
	})
	result := execute()
	if gotPin != "treehash" {
		t.Fatalf("Activity was not given the reference_oracle gate's tree hash as its pin: %q", gotPin)
	}
	if result.ResultSHA != "oracle-sha" || !result.CommittedByWorker || result.Oracles == nil || len(result.ChangedFiles) != 3 {
		t.Fatalf("result did not adopt the oracle commit's evidence: %+v", result)
	}
	// diff_scope saw the hash-verified oracle paths as exempt (Allowed-Files
	// only lists pkg/x.go), so the run is accepted.
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q, gates = %+v", result.State, result.GateResults)
	}
}

func TestRunWorkflowDoesNotCommitOraclesWhenReferenceOracleFailed(t *testing.T) {
	called := false
	execute := oracleWorkflowEnv(t, 1, -1, func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
		called = true
		return CommittedOracles{}, nil
	})
	result := execute()
	if called {
		t.Fatal("CommitOraclesActivity ran although the reference_oracle gate failed")
	}
	if result.State != run.StateQuarantined || result.ResultSHA != "collect-sha" {
		t.Fatalf("result = %+v", result)
	}
}

// A request that opted out of the host oracle commit still schedules the
// Activity, but validate-only: no post-commit verify runs and nothing changes
// the result commit.
func TestRunWorkflowNoCommitOraclesRunsValidateOnlyAndNoPostVerify(t *testing.T) {
	var validateOnly []bool
	postCalls := 0
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "",
		func(_ context.Context, in CommitOraclesInput) (CommittedOracles, error) {
			validateOnly = append(validateOnly, in.ValidateOnly)
			return CommittedOracles{}, nil
		},
		func(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
			postCalls++
			return PostOracleCommitVerifyResult{}, nil
		},
		func(in *RunWorkflowInput) { in.NoCommitOracles = true })
	result := execute()
	if len(validateOnly) != 1 || !validateOnly[0] || postCalls != 0 {
		t.Fatalf("commit Activity calls (validate-only flags) = %v, post-commit verify %d; want one validate-only call and none", validateOnly, postCalls)
	}
	if result.ResultSHA != "collect-sha" || result.CommittedByWorker {
		t.Fatalf("opt-out changed the result commit: %+v", result)
	}
	passed := false
	for _, g := range result.GateResults {
		if g.Check == "reference_oracle" && g.Passed {
			passed = true
		}
	}
	if !passed {
		t.Fatalf("reference_oracle gate must still run and pass: %+v", result.GateResults)
	}
}

func TestRunWorkflowCommitsOraclesOnlyAfterSpecConformityPasses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		specExit   int
		wantCommit bool
	}{{"conformity passed", 0, true}, {"conformity failed", 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			execute := oracleWorkflowEnv(t, 0, tc.specExit, func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
				called = true
				return CommittedOracles{}, nil
			})
			execute()
			if called != tc.wantCommit {
				t.Fatalf("CommitOraclesActivity called = %v, want %v", called, tc.wantCommit)
			}
		})
	}
}

func TestRunWorkflowInactiveOracleCommitKeepsCollectedEvidence(t *testing.T) {
	execute := oracleWorkflowEnv(t, 0, -1, func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
		return CommittedOracles{}, nil
	})
	result := execute()
	if result.ResultSHA != "collect-sha" || result.CommittedByWorker || result.Oracles != nil || len(result.ChangedFiles) != 1 {
		t.Fatalf("an inactive oracle commit changed the result: %+v", result)
	}
	if result.State != run.StateAccepted {
		t.Fatalf("state = %q", result.State)
	}
}

func gateByCheck(t *testing.T, gates []run.GateResult, check string) run.GateResult {
	t.Helper()
	for _, g := range gates {
		if g.Check == check {
			return g
		}
	}
	t.Fatalf("no %s gate in %+v", check, gates)
	return run.GateResult{}
}

// Policy must be evaluated on the POST-commit evidence (log hash and duration),
// not on the pre-commit run's: the hashes are the tamper-evidence for what
// actually failed.
func TestRunWorkflowPolicyInputCarriesPostCommitEvidenceWhenBothReRunsFail(t *testing.T) {
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "make verify-full", committedOraclesFixture(), func(context.Context, RunWorkflowInput) (PostOracleCommitVerifyResult, error) {
		return PostOracleCommitVerifyResult{
			Verify:    VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}, ExitCode: 2}, LogSHA256: "post-verify-log", DurationMs: 1234},
			FullSuite: &VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify-full"}, ExitCode: 1}, LogSHA256: "post-full-log", DurationMs: 5678},
			Clean:     true,
		}, nil
	})
	result := execute()
	if result.State != run.StateQuarantined {
		t.Fatalf("state = %q", result.State)
	}
	v := gateByCheck(t, result.GateResults, "canonical_verify")
	if v.Passed || v.LogSHA256 != "post-verify-log" || v.DurationMs != 1234 {
		t.Fatalf("canonical_verify gate = %+v, want the failing post-commit evidence", v)
	}
	f := gateByCheck(t, result.GateResults, "full_suite_verify")
	if f.Passed || f.LogSHA256 != "post-full-log" || f.DurationMs != 5678 {
		t.Fatalf("full_suite_verify gate = %+v, want the failing post-commit evidence", f)
	}
}

// The host commit can write or supersede a path in the ticket's required
// files, so the required-content gate must judge the COMMITTED tree, not the
// pre-commit snapshot CollectEvidenceActivity took (Codex review of #203).
func TestRunWorkflowRequiredContentGateUsesTheContentAfterTheOracleCommit(t *testing.T) {
	withRequired := func(in *RunWorkflowInput) {
		in.RequiredChangedFiles = []string{"pkg/o.go"}
		in.RequiredContent = []string{"MARKER"}
	}
	commit := func(context.Context, CommitOraclesInput) (CommittedOracles, error) {
		return CommittedOracles{
			Active: true, Committed: true, ResultSHA: "oracle-sha", ChangedFiles: []string{"pkg/x.go", "pkg/o.go"},
			RequiredContentFinalFiles: map[string]string{"pkg/o.go": "after the oracle commit: MARKER"},
		}, nil
	}
	execute, _ := oracleWorkflowEnvWith(t, 0, -1, "", commit, passingPostOracleCommitVerify, withRequired)
	result := execute()
	for _, g := range result.GateResults {
		if g.Check == "required_content_present" {
			if !g.Passed {
				t.Fatalf("required_content_present failed although the committed tree carries the marker (stale pre-commit evidence): %+v", g)
			}
			return
		}
	}
	t.Fatalf("no required_content_present gate in %+v", result.GateResults)
}
