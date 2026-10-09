package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/conformity"
	"buildgate/internal/reviewstep"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
)

func testRelayPolicy() *sandbox.RoutePolicy {
	return &sandbox.RoutePolicy{
		Upstream:                   "https://models.example/v1",
		Route:                      "test-route",
		MaxRequestBytes:            1 << 20,
		RequestsPerMinute:          60,
		TokenBudget:                10000,
		TokenBudgetWindow:          time.Minute,
		CostBudgetMicroUSD:         100000,
		CostBudgetWindow:           time.Minute,
		InputMicroUSDPerMTok:       3000000,
		CachedInputMicroUSDPerMTok: 300000,
		CacheWriteMicroUSDPerMTok:  3750000,
		OutputMicroUSDPerMTok:      15000000,
		TokenCeiling:               50000,
		CostCeilingMicroUSD:        500000,
	}
}

// relayFixtureInput is a sandboxed, relay-contained execution whose worker
// launches through the fake Docker binary the caller supplies.
func relayFixtureInput(t *testing.T, dockerBinary, dataDir string) RunWorkflowInput {
	t.Helper()
	input := fixtureInput()
	input.WorkspacePath = testfixture.NewGitRepo(t)
	input.ProjectConfigCommitSHA = trustedCommit(t, input.WorkspacePath)
	input.SpecPath = ""
	input.RunID = "relay-run"
	input.DataDir = dataDir
	input.LogDir = filepath.Join(dataDir, "logs")
	input.CheckpointDir = filepath.Join(dataDir, "checkpoints")
	input.SandboxImage = "worker@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	input.SandboxDocker = dockerBinary
	input.SandboxUser = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	input.BuildAppScript = "-c"
	input.BuildAppInterpreter = "sh"
	input.RoutePolicy = testRelayPolicy()
	return input
}

// fakeDockerBinary writes a stand-in `docker` whose body is the given shell
// script. No real Docker daemon is involved.
func fakeDockerBinary(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	return path
}

// TestConformityReviewRoutesWorkerRefusesRoutelessReviewPolicy proves the
// same fail-closed mode rule relaySpecFor already applies to the
// build's own RoutePolicy also applies to a routed review: on a routes:
// mode Worker (a.CheckRoute != nil), a ReviewRelayPolicy with NO route
// name (Route == "") is refused outright, never silently accepted
// through the legacy six-worker-model-field branch -- that branch is
// for a legacy (a.CheckRoute == nil) Worker only.
func TestConformityReviewRoutesWorkerRefusesRoutelessReviewPolicy(t *testing.T) {
	dataDir := t.TempDir()
	docker := fakeDockerBinary(t, "exit 125\n")
	activities := &Activities{
		CheckRoute: func(role string, p sandbox.RoutePolicy, thinking string) error { return nil },
		// A routeless ReviewRelayPolicy means routedReview is false (see
		// RunSpecConformityReviewActivity's own doc comment), so
		// baseRelaySpec still resolves the BUILD route's own credential
		// normally through relaySpecFor -- only the review-routing check
		// afterward refuses this run, once it reaches the review policy
		// itself. ResolveRouteCredentials here is legitimately called
		// once, for the build's own route.
		ResolveRouteCredentials: func(route string) (RouteCredentials, error) {
			if route != "build-route" {
				t.Fatalf("ResolveRouteCredentials called with unexpected route %q", route)
			}
			return RouteCredentials{APIKey: sandbox.NewRouteSecret("build-route-secret")}, nil
		},
	}
	input := relayFixtureInput(t, docker, dataDir)
	input.RunID = "spec-conformity-relay-routeless-review-run"
	input.RoutePolicy.Route = "build-route"
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# Spec\n"), 0o600); err != nil {
		t.Fatalf("write fixture spec: %v", err)
	}
	criteriaPath := filepath.Join(t.TempDir(), "criteria.md")
	if err := os.WriteFile(criteriaPath, []byte("1. Fixture criterion\n"), 0o600); err != nil {
		t.Fatalf("write fixture criteria: %v", err)
	}
	input.SpecPath = specPath
	input.SpecAcceptanceCriteria = criteriaPath
	scriptDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scriptDir, "build_app.py"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatalf("write fixture build_app.py: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptDir, conformity.ScriptName), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatalf("write fixture conformity_review.py: %v", err)
	}
	input.BuildAppScript = filepath.Join(scriptDir, "build_app.py")
	input.BuildAppInterpreter = "python3"
	input.LogDir = filepath.Join(dataDir, "logs")
	input.CheckpointDir = filepath.Join(dataDir, "checkpoints")
	// Route == "" -- a legacy-shaped ReviewRelayPolicy submitted to a
	// routes: mode Worker.
	input.ReviewRelayPolicy = &sandbox.RoutePolicy{
		WorkerModelID:  "review-model",
		WorkerModelAPI: "openai-responses",
	}
	sciInput := ReviewStepInput{RunWorkflowInput: input, Step: reviewstep.SpecConformity}

	wrapper := func(ctx context.Context, in ReviewStepInput) (VerifyActivityResult, error) {
		return activities.RunReviewStepActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, sciInput)
	if err == nil {
		t.Fatal("RunSpecConformityReviewActivity: err = nil, want a refusal for a routeless review policy on a routes: mode Worker")
	}
	if !strings.Contains(err.Error(), "routeless") {
		t.Errorf("err = %v, want it to reflect the routeless refusal", err)
	}
}

// TestRunBuildActivityFailsClosedOnUnusableRelayConfiguration proves an
// operator's containment request is never silently downgraded: a run that
// asked for a relay-contained build and cannot get one halts before
// build_app.py is invoked, rather than running the worker with no relay.
func TestRunBuildActivityFailsClosedOnUnusableRelayConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name       string
		activities func() *Activities
		mutate     func(*RunWorkflowInput)
		wantSubstr string
	}{
		{
			name:       "no routes: config",
			activities: func() *Activities { return &Activities{} },
			wantSubstr: "no routes:/models:/roles: config",
		},
		{
			name: "plaintext upstream",
			activities: func() *Activities {
				return &Activities{
					CheckRoute: func(role string, p sandbox.RoutePolicy, thinking string) error { return nil },
					ResolveRouteCredentials: func(route string) (RouteCredentials, error) {
						return RouteCredentials{APIKey: sandbox.NewRouteSecret("worker-static-upstream-key")}, nil
					},
				}
			},
			mutate:     func(input *RunWorkflowInput) { input.RoutePolicy.Upstream = "http://models.example/v1" },
			wantSubstr: "must not travel in plaintext",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launched := filepath.Join(t.TempDir(), "launched")
			docker := fakeDockerBinary(t, "touch "+launched+"\nexit 0\n")
			activities := tc.activities()
			input := relayFixtureInput(t, docker, t.TempDir())
			if tc.mutate != nil {
				tc.mutate(&input)
			}

			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivity(activities.RunBuildActivity)
			_, err := env.ExecuteActivity(activities.RunBuildActivity, input)
			if err == nil {
				t.Fatal("RunBuildActivity accepted a run it could not contain in the requested relay")
			}
			if !hasApplicationErrorType(err, RelayConfigurationFailureType) {
				t.Fatalf("error = %v, want application error type %q", err, RelayConfigurationFailureType)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Fatalf("error = %q, want it to name %q", err.Error(), tc.wantSubstr)
			}
			if _, statErr := os.Stat(launched); statErr == nil {
				t.Error("a worker was launched despite an unusable route configuration")
			}
		})
	}
}

// testRoutedActivities returns an Activities whose CheckRoute always
// accepts and whose ResolveRouteCredentials always resolves to
// credential, the minimal routes: mode Worker configuration relaySpecFor
// needs to build a spec at all.
func testRoutedActivities(credential sandbox.RouteSecret) *Activities {
	return &Activities{
		snapshotReviewInstructions: noReviewInstructions,
		CheckRoute:                 func(role string, p sandbox.RoutePolicy, thinking string) error { return nil },
		ResolveRouteCredentials: func(route string) (RouteCredentials, error) {
			return RouteCredentials{APIKey: credential}, nil
		},
	}
}

// TestRelaySpecForAllowsNoCredentialWhenPolicyOptsIn guards a real fix
// (Codex, PR #59): RunWorkflowInput's RoutePolicy.AllowUnauthenticatedUpstream
// lets a credential-free relay through when the resolved route's own
// credential is unconfigured.
func TestRelaySpecForAllowsNoCredentialWhenPolicyOptsIn(t *testing.T) {
	input := relayFixtureInput(t, "unused-docker-binary", t.TempDir())
	input.RoutePolicy.AllowUnauthenticatedUpstream = true

	activities := testRoutedActivities(sandbox.RouteSecret{})
	spec, err := activities.relaySpecFor(input)
	if err != nil {
		t.Fatalf("relaySpecFor: %v", err)
	}
	if spec == nil {
		t.Fatal("relaySpecFor returned a nil spec for a declared relay")
	}
	if spec.APIKey.Configured() {
		t.Fatalf("spec credential = %+v, want unconfigured: no route credential was ever resolved", spec.APIKey)
	}
}

// TestRelaySpecForAllowsPlaintextUpstreamWhenPolicyOptsIn guards a real fix
// (Codex, PR #59): RunWorkflowInput's RoutePolicy.AllowPlaintextUpstream now
// lets a private-host plaintext relay upstream through the Temporal path
// too -- before this field existed, relaySpecFor's own hardcoded https-only
// check had no way to learn about it at all, making the documented plaintext-local-model mode unusable even when the operator's
// cmd/factoryd invocation had explicitly opted into exactly that with
// -relay-allow-plaintext-upstream.
func TestRelaySpecForAllowsPlaintextUpstreamWhenPolicyOptsIn(t *testing.T) {
	input := relayFixtureInput(t, "unused-docker-binary", t.TempDir())
	input.RoutePolicy.Upstream = "http://127.0.0.1:8080/v1"
	input.RoutePolicy.AllowPlaintextUpstream = true
	input.RoutePolicy.AllowUnauthenticatedUpstream = true

	activities := testRoutedActivities(sandbox.RouteSecret{})
	spec, err := activities.relaySpecFor(input)
	if err != nil {
		t.Fatalf("relaySpecFor: %v", err)
	}
	if spec == nil {
		t.Fatal("relaySpecFor returned a nil spec for a declared relay")
	}
}

// TestRelaySpecForRefusesRouteMismatch proves relaySpecFor fails closed
// when a.CheckRoute (this Worker's own routes: trust check, see
// modelrole.CheckRouteBinding) refuses the submitted RoutePolicy -- a
// routes:-named policy this Worker's own route disagrees with must
// never fall back to a build with no such containment, or to the
// Worker's own static legacy credential fields.
func TestRelaySpecForRefusesRouteMismatch(t *testing.T) {
	input := relayFixtureInput(t, "unused-docker-binary", t.TempDir())
	input.RoutePolicy.Route = "primary"
	activities := &Activities{
		CheckRoute: func(role string, p sandbox.RoutePolicy, thinking string) error {
			return errors.New("route \"primary\": policy's Upstream does not match this Worker's own route")
		},
	}
	_, err := activities.relaySpecFor(input)
	if err == nil {
		t.Fatal("relaySpecFor: err = nil, want a refusal when CheckRoute refuses the policy")
	}
	if !strings.Contains(err.Error(), "primary") {
		t.Errorf("err = %v, want it to name the route", err)
	}
}

// TestWorkerWithoutRoutesRefusesRelayPolicy proves a policy naming a
// route (RoutePolicy.Route != "") fails closed when this Worker has no
// CheckRoute configured at all (no routes:/models:/roles: config) --
// never falls back to running with no relay containment.
func TestWorkerWithoutRoutesRefusesRelayPolicy(t *testing.T) {
	input := relayFixtureInput(t, "unused-docker-binary", t.TempDir())
	input.RoutePolicy.Route = "primary"
	activities := &Activities{}
	_, err := activities.relaySpecFor(input)
	if err == nil {
		t.Fatal("relaySpecFor: err = nil, want a refusal when this Worker has no routes: config")
	}
}

// TestRelaySpecForRoutesWorkerRefusesRoutelessPolicy proves the mode
// switch itself is decided by this Worker's OWN configuration, never by
// the input: a Worker with a.CheckRoute configured (a routes: mode
// Worker) refuses a policy with NO route name (Route == "") rather than
// silently falling through to the legacy per-AuthMode branch, which
// would otherwise trust an AuthMode/Upstream/credential-mode
// combination this Worker never configured through routes: at all.
func TestRelaySpecForRoutesWorkerRefusesRoutelessPolicy(t *testing.T) {
	input := relayFixtureInput(t, "unused-docker-binary", t.TempDir())
	input.RoutePolicy.Route = ""
	activities := &Activities{
		// Mirrors modelrole.CheckRouteBinding's own real refusal for a
		// routeless policy -- this stub only needs to prove relaySpecFor
		// reaches CheckRoute at all (never the legacy per-AuthMode
		// branch) and propagates its refusal; ResolveRouteCredentials
		// must never be reached since CheckRoute always refuses first.
		CheckRoute: func(role string, p sandbox.RoutePolicy, thinking string) error {
			if p.Route == "" {
				return errors.New("modelrole: CheckRouteBinding requires a policy naming a route (got a legacy, routeless policy)")
			}
			return nil
		},
		ResolveRouteCredentials: func(route string) (RouteCredentials, error) {
			t.Fatal("ResolveRouteCredentials must not be called for a routeless policy")
			return RouteCredentials{}, nil
		},
	}
	_, err := activities.relaySpecFor(input)
	if err == nil {
		t.Fatal("relaySpecFor: err = nil, want a refusal for a routeless policy on a routes: mode Worker")
	}
	if !strings.Contains(err.Error(), "routeless") {
		t.Errorf("err = %v, want it to reflect the routeless refusal", err)
	}
}

// TestRelaySpecForRoutedPolicyUsesBoundCredentials proves a routes:-named
// policy's spec carries the credential a.ResolveRouteCredentials
// resolves for it, not this Worker's own static RouteSecret -- the
// two must never be conflated: a routes:-mode Worker holds no legacy
// credential at all (cmd/factoryd's daemon_cmd.go), and even one that
// did must never let an input-chosen route dispatch to a different,
// statically-configured upstream's credential.
func TestRelaySpecForRoutedPolicyUsesBoundCredentials(t *testing.T) {
	input := relayFixtureInput(t, "unused-docker-binary", t.TempDir())
	input.RoutePolicy.Route = "primary"
	bound := sandbox.NewRouteSecret("bound-route-secret")
	activities := &Activities{
		CheckRoute: func(role string, p sandbox.RoutePolicy, thinking string) error { return nil },
		ResolveRouteCredentials: func(route string) (RouteCredentials, error) {
			return RouteCredentials{APIKey: bound}, nil
		},
	}
	spec, err := activities.relaySpecFor(input)
	if err != nil {
		t.Fatalf("relaySpecFor: %v", err)
	}
	if spec == nil {
		t.Fatal("relaySpecFor returned a nil spec for a declared relay")
	}
	if spec.APIKey != bound {
		t.Errorf("spec.APIKey = %+v, want ResolveRouteCredentials' own resolved credential %+v (not the Worker's static RouteSecret)", spec.APIKey, bound)
	}
}

// TestRunWorkflowInputCannotCarryACredential is the structural guard for the
// credential split this path is built on: Temporal persists Workflow input in
// Event History, so anything reachable from RunWorkflowInput is written to
// durable Temporal storage. This walks that whole type graph (via the shared
// testfixture.FindCredentialLeak, also used by internal/request and
// cmd/factoryd's own credential guards) and fails if a credential-bearing
// type (sandbox.RouteSpec, sandbox.RouteSecret) or a credential-named
// field ever becomes reachable from it. The walker's own detector-catches-a-
// violation self-tests live in internal/testfixture/credentialwalk_test.go,
// not here.
func TestRunWorkflowInputCannotCarryACredential(t *testing.T) {
	forbiddenTypes := map[reflect.Type]bool{
		reflect.TypeOf(sandbox.RouteSecret{}): true,
		reflect.TypeOf(sandbox.RouteSpec{}):   true,
		reflect.TypeOf(RouteCredentials{}):    true,
	}
	if err := testfixture.FindCredentialLeak(reflect.TypeOf(RunWorkflowInput{}), forbiddenTypes, nil); err != nil {
		t.Fatalf("%v: Temporal would persist it in Event History; the upstream credential must stay on the Worker's own static configuration (Activities.RouteSecret)", err)
	}
}

// noReviewInstructions is the review-instructions seam for tests whose
// worktree has no real base commit and no instruction files: it masks and
// removes nothing.
func noReviewInstructions(context.Context, string, string, string) (sandbox.ReviewInstructionSnapshot, error) {
	return sandbox.ReviewInstructionSnapshot{}, nil
}
