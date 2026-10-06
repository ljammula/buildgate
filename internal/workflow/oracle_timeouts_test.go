package workflow

import (
	"context"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"

	"buildgate/internal/policy"
	"buildgate/internal/run"
	"buildgate/internal/runner"
)

// The reference_oracle gate Activity runs the oracle command and then its
// canary, so its StartToCloseTimeout is double a plain named gate's; the
// post-commit Activity gets one single-command budget per phase.
func TestWorkflowScalesOracleActivityTimeouts(t *testing.T) {
	timeouts, flags := oracleActivityTimeoutsAndFlags(t)
	if !flags["oracle_canary"] {
		t.Errorf("a new execution must enable the oracle canary: %v", flags)
	}
	base := timeouts["lint"]
	if base == 0 {
		t.Fatalf("no lint timeout captured: %v", timeouts)
	}
	if got := timeouts["reference_oracle"]; got != 2*base {
		t.Errorf("reference_oracle StartToClose = %v, want double the lint gate's %v", got, base)
	}
	// verify + full suite + lint + unit_tests = 4 phases; reference_oracle is never re-run.
	if got := timeouts[RunPostOracleCommitVerifyActivityName]; got != 4*base {
		t.Errorf("post-commit StartToClose = %v, want 4x the single-command %v", got, base)
	}
}

// oracleActivityTimeoutsAndFlags runs RunWorkflow with a committed oracle and
// returns each captured Activity's StartToCloseTimeout (named gates by check
// name) plus the input flags the workflow passed: "oracle_canary"
// (reference_oracle gate input).
func oracleActivityTimeoutsAndFlags(t *testing.T) (map[string]time.Duration, map[string]bool) {
	t.Helper()
	input := fixtureInput()
	input.FullSuiteCommand = "make full"
	setGateCommand(&input, "lint", "make lint")
	setGateCommand(&input, "unit_tests", "make unit")
	setGateCommand(&input, policy.ReferenceOracleGateID, "make oracle")
	input.ReferenceOracleDir = "/oracle/store"
	input.AllowedFiles = []string{"pkg/x.go"}
	env := newWorkflowEnvironmentSansCommitOracles(t,
		func(context.Context, RunWorkflowInput) (BuildActivityResult, error) {
			return BuildActivityResult{Result: runner.Result{ExitCode: 0}}, nil
		},
		func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
			return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make verify"}}}, nil
		},
		func(context.Context, EvaluateGateInput) (run.GateResult, error) {
			return run.GateResult{Check: "canonical_verify", Passed: true}, nil
		},
	)
	env.RegisterActivityWithOptions(func(_ context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", in.Command}}, ReferenceOracleSHA256: "treehash"}, nil
	}, activity.RegisterOptions{Name: RunNamedGateActivityName})
	env.RegisterActivityWithOptions(func(context.Context, CollectEvidenceInput) (CollectedEvidence, error) {
		return CollectedEvidence{ResultSHA: "sha", ChangedFiles: []string{"pkg/x.go"}}, nil
	}, activity.RegisterOptions{Name: CollectEvidenceActivityName})
	env.RegisterActivityWithOptions(committedOraclesFixture(), activity.RegisterOptions{Name: CommitOraclesActivityName})
	env.RegisterActivityWithOptions(func(context.Context, RunWorkflowInput) (VerifyActivityResult, error) {
		return VerifyActivityResult{Result: runner.Result{Command: []string{"sh", "-c", "make full"}}}, nil
	}, activity.RegisterOptions{Name: RunFullSuiteVerifyActivityName})
	env.RegisterActivityWithOptions(passingPostOracleCommitVerify, activity.RegisterOptions{Name: RunPostOracleCommitVerifyActivityName})

	timeouts := map[string]time.Duration{}
	flags := map[string]bool{}
	env.SetOnActivityStartedListener(func(info *activity.Info, _ context.Context, args converter.EncodedValues) {
		if info.ActivityType.Name == RunNamedGateActivityName {
			var in NamedGateActivityInput
			if args.Get(&in) == nil {
				timeouts[in.Check] = info.StartToCloseTimeout
				if in.Check == "reference_oracle" {
					flags["oracle_canary"] = in.OracleCanary
				}
			}
			return
		}
		timeouts[info.ActivityType.Name] = info.StartToCloseTimeout
	})
	env.ExecuteWorkflow(RunWorkflow, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow: %v", err)
	}
	return timeouts, flags
}
