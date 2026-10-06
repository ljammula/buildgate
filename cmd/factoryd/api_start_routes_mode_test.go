package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// TestAPIStartStarterAcceptsTemporalAddressInRoutesMode proves 2D removed
// temporal_address's routes:-mode 400 refusal at this request's own argv
// gate: the Worker now binds a submitted route to its own session config
// (modelrole.CheckRouteBinding, Activities.CheckRoute/ResolveRouteCredentials) instead of refusing
// it. temporal_address itself (127.0.0.1:1) is unreachable, but that is
// runMainWithReady's own later, unrelated concern -- this request must not be
// rejected as invalid before ever reaching that point.
func TestAPIStartStarterAcceptsTemporalAddressInRoutesMode(t *testing.T) {
	dp := newTestDeps(t)
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	policy := apiSandboxPolicy{}
	_, err := apiStartStarter(dp, t.TempDir(), &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, policy)(context.Background(), api.StartRequest{
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		TemporalAddress: "127.0.0.1:1",
	})
	if errors.Is(err, api.ErrInvalidStartRequest) {
		t.Fatalf("API start with temporal_address and routes: configured: error = %v, must not be refused as invalid any more (2D)", err)
	}
}

// TestAPIStartStarterRunsInRoutesModeWithNoModelFields proves an
// ordinary routes: mode request (no Model/relay_worker_*/temporal_address
// fields at all) reaches real argv building and actually starts a run --
// not just "not refused by the 400 gate" (this test's own predecessor
// only proved that much, and separately, this daemon's session config
// default relay_credential_header ("x-api-key", from sessionconfig.
// DefaultSettings) used to still be forwarded as an explicit
// -relay-credential-header, which the child's own
// refuseLegacyRouteFlagsInRoutesMode rejected -- turning every routes:
// mode POST /runs into an unclassified 500 even for a perfectly ordinary
// request. Fixed by never forwarding any route-scoped flag at all once
// sandboxPolicy.routesConfigured (found via review round 1).
func TestAPIStartStarterRunsInRoutesModeWithNoModelFields(t *testing.T) {
	dp := newTestDeps(t)
	dockerBinary := fakeSandboxDockerBinary(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_docker: "+dockerBinary+"\n"+
		"registry_proxy: false\n"+
		"routes:\n  litellm:\n    credential_mode: static\n    upstream: https://litellm.example.invalid\n    credential_env: API_START_ROUTES_TEST_KEY\n"+
		"models:\n  luna:\n    id: gpt-5.6-luna\n    routes: [litellm]\n"+
		"roles:\n  execution:\n    model: luna\n")
	t.Setenv("API_START_ROUTES_TEST_KEY", "sk-test")

	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	sandboxImage := "operator-approved.example/worker@sha256:" + strings.Repeat("a", 64)
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{
		allowedImages: []string{sandboxImage},
		defaultImage:  sandboxImage,
		roles:         &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna"}},
	})(context.Background(), api.StartRequest{
		TemporalAddress:  sharedTemporalAddress(t),
		Ticket:           "api-ticket",
		Workspace:        workspace,
		Spec:             spec,
		SkipProjectCheck: true,
	})
	if err != nil {
		t.Fatalf("start through API adapter in routes: mode: %v", err)
	}
	if started == nil {
		t.Fatal("apiStartStarter returned no error but no started run either")
	}

	deadline := time.Now().Add(20 * time.Second)
	var loaded *run.Run
	for {
		var loadErr error
		loaded, loadErr = run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State != run.StateSliceRunning && loaded.State != run.StateReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach a terminal state: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Not asserting State == accepted: this fixture's fake sandbox_docker
	// has no real relay behind it, so the run is expected to halt/fail
	// downstream -- the actual proof is that it got THIS far (a real
	// run record was created and driven), never refused as an
	// unclassified 500 at argv-building time.
}
