package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/runner"
)

// deadlineRecorder wraps an Activities fake runner, recording the time left on
// each command's context when it starts.
func deadlineRecorder(a *Activities) *[]time.Duration {
	var left []time.Duration
	inner := a.runWithRetriesChecked
	a.runWithRetriesChecked = func(ctx context.Context, ws string, logPath func(int) string, n int, before func(int) error, after func(int, runner.Result, error) error, name string, args ...string) (runner.Result, error) {
		if d, ok := ctx.Deadline(); ok {
			left = append(left, time.Until(d))
		}
		return inner(ctx, ws, logPath, n, before, after, name, args...)
	}
	return &left
}

// A reference_oracle Activity scheduled before the canary existed keeps its old
// recorded single-command timeout: the new worker must not halve its budget or
// run a canary unless the workflow's version marker said so (OracleCanary).
func TestLegacyReferenceOracleActivityRunsOnceWithTheFullBudget(t *testing.T) {
	for _, canary := range []bool{false, true} {
		oracleDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(oracleDir, "p_oracle_test.go"), []byte(canaryTestOracleSrc), 0o600); err != nil {
			t.Fatal(err)
		}
		a, in, commands := postVerifyFixture(t, func(string) int { return 0 }, nil)
		left := deadlineRecorder(a)
		input := NamedGateActivityInput{
			RunWorkflowInput: in,
			Check:            "reference_oracle",
			Command:          "make oracle",
			OracleCanary:     canary,
		}
		input.ReferenceOracleDir, input.ReferenceOracleMountPath = oracleDir, ".oracle"
		const budget = 8 * time.Second
		wrapper := func(ctx context.Context, i NamedGateActivityInput) (VerifyActivityResult, error) {
			ctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			return a.RunNamedGateActivity(ctx, i)
		}
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestActivityEnvironment()
		env.RegisterActivity(wrapper)
		raw, err := env.ExecuteActivity(wrapper, input)
		if err != nil && !canary {
			t.Fatalf("legacy Activity failed: %v", err)
		}
		var res VerifyActivityResult
		if err == nil {
			if err := raw.Get(&res); err != nil {
				t.Fatal(err)
			}
		}
		if len(*left) == 0 {
			t.Fatalf("canary=%v: no command ran", canary)
		}
		first := (*left)[0]
		if !canary {
			if len(*commands) != 1 || res.OracleCanary != nil || len(canaryAttempts(res)) != 0 {
				t.Errorf("legacy Activity ran %v canary=%+v: want exactly one plain run", *commands, res.OracleCanary)
			}
			if first < budget*3/4 {
				t.Errorf("legacy real run had %v left of %v: its budget was halved", first, budget)
			}
		} else if first > budget*3/4 {
			t.Errorf("canary-enabled real run had %v left of %v: want about half", first, budget)
		}
	}
}
