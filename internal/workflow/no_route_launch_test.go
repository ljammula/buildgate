package workflow

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

// TestVerifyAndGateActivitiesLaunchWithNoModelRoute runs each Activity that
// launches a verify or gate command, on a run that names a model route this
// Worker would accept and resolve, through the sandbox runtime: every sandbox
// it requests has no route and no meter configuration, no credential is
// pushed, and the route is never checked or resolved.
func TestVerifyAndGateActivitiesLaunchWithNoModelRoute(t *testing.T) {
	gate := func(check string) func(context.Context, *Activities, RunWorkflowInput) error {
		return func(ctx context.Context, a *Activities, input RunWorkflowInput) error {
			_, err := a.RunNamedGateActivity(ctx, NamedGateActivityInput{RunWorkflowInput: input, Check: check, Command: "make " + check})
			return err
		}
	}
	cases := []struct {
		name     string
		exitCode int
		edit     func(*RunWorkflowInput)
		run      func(context.Context, *Activities, RunWorkflowInput) error
		launches int
	}{
		{name: "canonical verify", launches: 1, run: func(ctx context.Context, a *Activities, input RunWorkflowInput) error {
			_, err := a.RunVerifyActivity(ctx, input)
			return err
		}},
		{name: "baseline verify", launches: 1, run: func(ctx context.Context, a *Activities, input RunWorkflowInput) error {
			_, err := a.RunBaselineVerifyActivity(ctx, input)
			return err
		}},
		{name: "full suite", launches: 1, run: func(ctx context.Context, a *Activities, input RunWorkflowInput) error {
			_, err := a.RunFullSuiteVerifyActivity(ctx, input)
			return err
		}},
		{name: "a named gate", launches: 1, run: gate("lint")},
		// A failed gate is run again on the base commit.
		{name: "a failed gate and its rerun on the base commit", exitCode: 1, launches: 2, run: gate("unit_tests")},
		// The oracle gate passes, so its command runs again on the canary.
		{name: "the reference oracle gate and its canary", launches: 2,
			edit: func(input *RunWorkflowInput) {
				input.ReferenceOracleDir = t.TempDir()
				input.ReferenceOracleMountPath = ".oracle"
				if err := os.WriteFile(filepath.Join(input.ReferenceOracleDir, "p_oracle_test.go"), []byte(canaryTestOracleSrc), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			run: func(ctx context.Context, a *Activities, input RunWorkflowInput) error {
				_, err := a.RunNamedGateActivity(ctx, NamedGateActivityInput{RunWorkflowInput: input, Check: "reference_oracle", Command: "go test ./.oracle", OracleCanary: true})
				return err
			}},
		// Canonical verify, the full suite and the lint gate, one after another.
		{name: "verify after the oracle commit", launches: 3,
			edit: func(input *RunWorkflowInput) { setGateCommand(input, "lint", "make lint") },
			run: func(ctx context.Context, a *Activities, input RunWorkflowInput) error {
				_, err := a.RunPostOracleCommitVerifyActivity(ctx, input)
				return err
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := &sandboxtest.WorkerRuntime{Lines: []string{"ran"}, ExitCode: tc.exitCode}
			activities, input, _ := runtimeActivities(t, rt)
			var routeCalls atomic.Int32
			activities.CheckRoute = func(string, sandbox.RoutePolicy, string) error {
				routeCalls.Add(1)
				return nil
			}
			activities.ResolveRouteCredentials = func(string) (RouteCredentials, error) {
				routeCalls.Add(1)
				return RouteCredentials{}, nil
			}
			input.RunID = "run-no-route"
			input.BaseSHA = input.ProjectConfigCommitSHA
			input.VerifyCommand = "make verify"
			input.FullSuiteCommand = "make verify-full"
			input.RoutePolicy = &sandbox.RoutePolicy{
				Route: "local", Upstream: "http://127.0.0.1:8080",
				AllowedPathPrefix: "/v1/chat/completions", UsageFormat: "openai", WorkerModelID: "qwen", WorkerBasePath: "/v1",
				AllowUnauthenticatedUpstream: true, AllowPlaintextUpstream: true,
				MaxRequestBytes: 1 << 20, RequestsPerMinute: 60,
				TokenBudget: 1000, TokenBudgetWindow: time.Minute, CostBudgetMicroUSD: 1000, CostBudgetWindow: time.Minute,
				TokenCeiling: 5000, CostCeilingMicroUSD: 5000,
			}
			if tc.edit != nil {
				tc.edit(&input)
			}
			// The control: this Worker binds the run's route for a step
			// that is given one.
			if spec, err := activities.relaySpecFor(input); err != nil || spec == nil {
				t.Fatalf("relaySpecFor = %v, %v; the fixture's route must be one the Worker would launch", spec, err)
			}
			routeCalls.Store(0)

			wrapper := func(ctx context.Context, in RunWorkflowInput) error { return tc.run(ctx, activities, in) }
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(wrapper)
			if _, err := env.ExecuteActivity(wrapper, input); err != nil {
				t.Fatalf("execute Activity: %v", err)
			}

			requests := rt.Requests()
			if len(requests) != tc.launches {
				t.Fatalf("launches = %d, want %d", len(requests), tc.launches)
			}
			for i, req := range requests {
				if req.Route != nil || req.MeterConfig != nil {
					t.Errorf("launch %d (%q) got a model route %+v and meter configuration %v", i+1, req.Command[len(req.Command)-1], req.Route, req.MeterConfig)
				}
			}
			if pushed := rt.Pushed(); len(pushed) != 0 {
				t.Errorf("credentials pushed = %d, want none", len(pushed))
			}
			if n := routeCalls.Load(); n != 0 {
				t.Errorf("the run's route was checked or resolved %d times, want never", n)
			}
		})
	}
}
