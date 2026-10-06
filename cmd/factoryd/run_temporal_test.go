package main

import (
	"testing"

	"buildgate/internal/modelrole"
	"buildgate/internal/sandbox"
	"buildgate/internal/sessionconfig"
	"buildgate/internal/workflow"
)

// TestBuildRunWorkflowInputCarriesRoutesModeRelayFields proves
// runOptions.workflowInput -- the one function runViaTemporal/
// runViaRepositoryOwner both call to build a Temporal Workflow's input --
// carries a routes:-mode caller's RoutePolicy.Route and ReviewRelayPolicy
// straight through onto RunWorkflowInput, unchanged: the Temporal Worker
// (relaySpecFor/RunReviewStepActivity) needs both to reach it to bind a
// route via Activities.CheckRoute/ResolveRouteCredentials at all.
func TestBuildRunWorkflowInputCarriesRoutesModeRelayFields(t *testing.T) {
	dataDir := t.TempDir()
	id := "routes-mode-input-fixture"
	buildPolicy := &sandbox.RoutePolicy{
		Route:    "primary",
		Upstream: "https://primary.example.invalid",
	}
	reviewPolicy := &sandbox.RoutePolicy{
		Route:    "review-route",
		Upstream: "https://review-route.example.invalid",
	}
	relayOpts := modelRouteOptions{Policy: buildPolicy}

	input := runOptions{
		ID:                  id,
		Ticket:              "fixture-ticket",
		WorkspacePath:       "/workspace",
		SpecSnapshotPath:    "/spec.md",
		BaseSHA:             "deadbeef",
		DataDir:             dataDir,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      "/build_app.py",
		Harness:             "pifork",
		ReviewHarness:       "pi",
		ConformityPolicy:    "required",
		CodeReviewPolicy:    "off",
		VerifyCommand:       "true",
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildAppMaxAttempts: 2,
		VerifyMaxAttempts:   2,
		SandboxImage:        "worker-image",
		SandboxDocker:       "docker",
		Relay:               relayOpts,
		ReviewRelayPolicy:   reviewPolicy,
	}.workflowInput()

	if input.Harness != "pifork" || input.ReviewHarness != "pi" {
		t.Errorf("Harness = %q, ReviewHarness = %q, want the execution role's pifork and the review role's pi carried per execution", input.Harness, input.ReviewHarness)
	}
	if input.RoutePolicy == nil || input.RoutePolicy.Route != "primary" {
		t.Errorf("RoutePolicy.Route = %+v, want %q", input.RoutePolicy, "primary")
	}
	if input.ReviewRelayPolicy == nil || input.ReviewRelayPolicy.Route != "review-route" {
		t.Errorf("ReviewRelayPolicy = %+v, want a policy naming route %q", input.ReviewRelayPolicy, "review-route")
	}
	if input.ReviewRelayPolicy.Upstream != "https://review-route.example.invalid" {
		t.Errorf("ReviewRelayPolicy.Upstream = %q, want the review route's own upstream", input.ReviewRelayPolicy.Upstream)
	}
}

// TestBuildRunWorkflowInputCarriesCodeReviewPolicy is
// TestBuildRunWorkflowInputCarriesRoutesModeRelayFields' counterpart for
// -code-review-policy (M2-C): runOptions.workflowInput must carry it straight
// through onto RunWorkflowInput.CodeReviewPolicy, unchanged -- RunWorkflow's
// own decision to call RunReviewStepActivityName for the code-review step
// at all can only see this
// raw field (see RunWorkflowInput.CodeReviewPolicy's own doc comment), so a
// caller-side typo or drop here would silently disable the whole phase.
// Also proves ReviewThinking (renamed from ConformityThinking in this same
// PR) still carries through unchanged.
func TestBuildRunWorkflowInputCarriesCodeReviewPolicy(t *testing.T) {
	dataDir := t.TempDir()
	id := "code-review-policy-input-fixture"

	input := runOptions{
		ID:                  id,
		Ticket:              "fixture-ticket",
		WorkspacePath:       "/workspace",
		SpecSnapshotPath:    "/spec.md",
		BaseSHA:             "deadbeef",
		DataDir:             dataDir,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      "/build_app.py",
		Harness:             "pi",
		ReviewHarness:       "pi",
		ConformityPolicy:    "required",
		CodeReviewPolicy:    "advisory",
		VerifyCommand:       "true",
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildAppMaxAttempts: 2,
		VerifyMaxAttempts:   2,
		SandboxImage:        "worker-image",
		SandboxDocker:       "docker",
		ReviewThinking:      "review-thinking-level",
	}.workflowInput()

	if input.CodeReviewPolicy != "advisory" {
		t.Errorf("CodeReviewPolicy = %q, want %q", input.CodeReviewPolicy, "advisory")
	}
	if input.ReviewThinking != "review-thinking-level" {
		t.Errorf("ReviewThinking = %q, want %q", input.ReviewThinking, "review-thinking-level")
	}
}

// TestBuildRunWorkflowInputCarriesGateCommandsForAnyGateID is M4-K2's own
// registry-plumbing proof: runOptions.workflowInput threads its gateCommands
// parameter onto RunWorkflowInput.GateCommands verbatim, by map identity,
// not by naming policy.CommandGates' five known IDs individually -- so a
// hypothetical 6th command gate (registered nowhere else in this test)
// reaches RunWorkflowInput exactly as the five real ones do, proving this
// call site needs no change to carry a new registry entry.
func TestBuildRunWorkflowInputCarriesGateCommandsForAnyGateID(t *testing.T) {
	dataDir := t.TempDir()
	id := "gate-commands-input-fixture"
	gateCommands := map[string]string{
		"lint":               "golangci-lint run ./...",
		"a-hypothetical-6th": "make sixth-gate",
	}

	input := runOptions{
		ID:                  id,
		Ticket:              "fixture-ticket",
		WorkspacePath:       "/workspace",
		SpecSnapshotPath:    "/spec.md",
		BaseSHA:             "deadbeef",
		DataDir:             dataDir,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      "/build_app.py",
		Harness:             "pi",
		ReviewHarness:       "pi",
		ConformityPolicy:    "required",
		CodeReviewPolicy:    "off",
		VerifyCommand:       "true",
		GateCommands:        gateCommands,
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildAppMaxAttempts: 2,
		VerifyMaxAttempts:   2,
		SandboxImage:        "worker-image",
		SandboxDocker:       "docker",
	}.workflowInput()

	if input.GateCommands["lint"] != "golangci-lint run ./..." {
		t.Errorf("GateCommands[lint] = %q", input.GateCommands["lint"])
	}
	if input.GateCommands["a-hypothetical-6th"] != "make sixth-gate" {
		t.Errorf("GateCommands[a-hypothetical-6th] = %q, want it carried through unfiltered", input.GateCommands["a-hypothetical-6th"])
	}
}

// TestInProcessRoutesWorkerRefusesARoutelessPolicy proves
// applyRelayOptsToActivities -- the one place run_temporal.go's own
// in-process Worker and run_repository_owner.go's own Worker both wire
// relayOpts.CheckRoute/ResolveRouteCredentials onto Activities -- and
// that its own CheckRoute (the real production closure, checkRouteFunc,
// not a fake) still refuses a routeless (Route == "") policy, proving
// the refusal comes from modelrole.CheckRouteBinding itself, not merely
// from this test's own stub.
func TestInProcessRoutesWorkerRefusesARoutelessPolicy(t *testing.T) {
	t.Setenv("IPRW_TEST_KEY", "sk-test")
	s := sessionconfig.DefaultSettings()
	s.Routes = map[string]sessionconfig.Route{
		"primary": {CredentialMode: "static", Upstream: "https://primary.example.invalid", CredentialEnv: "IPRW_TEST_KEY"},
	}
	s.Models = map[string]sessionconfig.Model{
		"m": {ID: "test-model", Routes: []string{"primary"}},
	}
	s.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}
	relayOpts := modelRouteOptions{
		CheckRoute:              checkRouteFunc(s),
		ResolveRouteCredentials: resolveRouteCredentialsFunc(s),
	}
	activities := &workflow.Activities{}
	applyRelayOptsToActivities(activities, relayOpts)

	if activities.CheckRoute == nil || activities.ResolveRouteCredentials == nil {
		t.Fatal("routes-mode Activities has CheckRoute/ResolveRouteCredentials unset")
	}

	// A routeless policy must be refused by this routes:-mode Activities'
	// own CheckRoute -- it must never be silently accepted.
	routelessPolicy := sandbox.RoutePolicy{Upstream: "https://example.invalid"}
	if err := activities.CheckRoute(string(modelrole.RoleExecution), routelessPolicy, ""); err == nil {
		t.Error("CheckRoute: err = nil, want a refusal for a routeless policy on a routes:-mode Activities")
	}
}
