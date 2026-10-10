package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/hostcontrol"
	"buildgate/internal/release"
	"buildgate/internal/run"
	"buildgate/internal/sessionconfig"
)

// apiSandboxTestMu serializes every test that points tier2SettingsOverride
// at a fake sandbox Docker binary -- tier2SettingsOverride is process-wide
// mutable state (the same production mechanism serveMain uses to configure
// every API-started run), so two tests mutating it concurrently would race
// under -race regardless of what either one actually asserts.
var apiSandboxTestMu sync.Mutex

// withFakeSandboxSettings points tier2SettingsOverride's SandboxDocker/
// SandboxImage at testdata/fake_docker.sh for the duration of one test, so
// apiStartStarter's spawned runMainWithReady call sandboxes through it
// instead of a real Docker daemon -- sandboxing is unconditional, with no
// -allow-unsandboxed opt-out anywhere in factoryd anymore. A request's own
// explicit SandboxImage (an allowlisted one, say) still wins over this
// default the same way an explicit -sandbox-image flag always would.
func withFakeSandboxSettings(t *testing.T) {
	t.Helper()
	apiSandboxTestMu.Lock()
	settings := sessionconfig.DefaultSettings()
	settings.SandboxDocker = fakeSandboxDockerBinary(t)
	settings.SandboxImage = fakeSandboxImage
	tier2SettingsOverride = &settings
	t.Cleanup(func() {
		tier2SettingsOverride = nil
		apiSandboxTestMu.Unlock()
	})
}

func TestRefuseAPITokensInEnvironment(t *testing.T) {
	for _, name := range []string{overrideTokenEnvironmentVariable, startTokenEnvironmentVariable, readTokenEnvironmentVariable} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(overrideTokenEnvironmentVariable, "")
			t.Setenv(startTokenEnvironmentVariable, "")
			t.Setenv(readTokenEnvironmentVariable, "")
			t.Setenv(name, "secret")
			if err := refuseAPITokensInEnvironment(); err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("refuseAPITokensInEnvironment() error = %v, want one naming %s", err, name)
			}
		})
	}
}

func TestRepositoryAPIStarterRequiresSerializedTemporalRoute(t *testing.T) {
	t.Parallel()
	called := false
	next := func(context.Context, api.StartRequest) (*run.Run, error) {
		called = true
		return &run.Run{ID: "run-1"}, nil
	}
	starter := repositoryAPIStarter(next)

	for _, req := range []api.StartRequest{
		{TemporalAddress: "localhost:7233"},
		{Repository: "owner/repo"},
		{},
	} {
		if _, err := starter(context.Background(), req); !errors.Is(err, api.ErrInvalidStartRequest) {
			t.Fatalf("repositoryAPIStarter(%+v) error = %v, want ErrInvalidStartRequest", req, err)
		}
	}
	if called {
		t.Fatal("underlying starter called without both repository and temporal_address")
	}
}

func writeNativeTicketForAPI(t *testing.T, workspace, ticket string) {
	t.Helper()
	ticketsDir := filepath.Join(filepath.Dir(workspace), "spec", "tickets")
	if err := os.MkdirAll(ticketsDir, 0o750); err != nil {
		t.Fatalf("mkdir native ticket directory: %v", err)
	}
	content := "This is a brand-new, empty, already-git-init-ed repo.\n\n" +
		"## Goal\nexercise the API adapter\n\n" +
		"## Required changes\nUpdate `ARCHITECTURE.md` and append a `PROGRESS.md` entry before finishing.\n\n" +
		"## Verification\n`make verify` must pass.\n\n" +
		"## Commit\nticket(001): fixture\n"
	path := filepath.Join(ticketsDir, "001-"+ticket+".md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write native ticket: %v", err)
	}
}

func TestAPIStartedRunHaltsWhenInitializationFailsAfterReady(t *testing.T) {
	dp := newTestDeps(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	// The failure under test is one found after the ready record exists.
	// Every check of the workspace itself (a git repository, a commit at
	// HEAD, its AGENTS.md, the project-bootstrap preflight) runs before
	// "ready" and would return synchronously, so the workspace is a passing
	// fixture and the failure is the Temporal dial, which an explicit
	// address reaches only after "ready".
	workspace := newFixtureRepo(t)
	writeNativeTicketForAPI(t, workspace, "api-ticket")
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{allowedImages: []string{fakeSandboxImage}})(context.Background(), api.StartRequest{
		TemporalAddress: "127.0.0.1:1", // nothing listens there
		ID:              "api-start-unreachable-temporal",
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		SandboxImage:    fakeSandboxImage,
		// An explicit, offline build script: sandboxing is unconditional,
		// so without this the run would fail the separate default-model-
		// backed-build-requires-a-relay check synchronously, before ever
		// reaching "ready" -- this test needs the post-ready failure
		// instead.
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State == run.StateHalted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run remained non-terminal after post-ready initialization failure: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterUsesBoundedSupervisor proves the serve adapter creates a
// durable ready record and invokes the existing supervisor in-process. The
// fixture fails immediately, keeping this a deterministic wiring test rather
// than requiring a Temporal server or a real build.
func TestAPIStartStarterUsesBoundedSupervisor(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	workspace := newFixtureRepo(t)
	writeNativeTicketForAPI(t, workspace, "api-ticket")
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "fail")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-start-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		// Sandboxing is unconditional and reserves a fixed teardown margin
		// (sandboxAttemptTeardownMargin, 10s) off this run's own deadline
		// before it will even start an attempt.
		Timeout:       "30s",
		VerifyCommand: "true",
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}
	if started.ID != "api-start-1" || started.State != run.StateReady {
		t.Fatalf("initial run = %+v, want api-start-1 in ready state", started)
	}
	// 30s, not 2s: this polls for the end of a whole fake run, which took
	// 3.5-4.9s under make verify's sharded -race load and failed there;
	// the test asserts the terminal state, not how fast it arrives.
	deadline := time.Now().Add(30 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State == run.StateQuarantined {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach terminal quarantine after fixture failure: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterPlumbsIsolateWorkspaceFlag proves an API-started run
// ends up isolated (a distinct worktree/branch, not the shared checkout).
func TestAPIStartStarterPlumbsIsolateWorkspaceFlag(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	workspace := newFixtureRepo(t)
	writeNativeTicketForAPI(t, workspace, "api-ticket")
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-isolated-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		Timeout:             "30s",
		VerifyCommand:       "true",
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && (loaded.State == run.StateAccepted || loaded.State == run.StateHalted || loaded.State == run.StateQuarantined) {
			if loaded.Branch == "" || loaded.WorkspacePath == workspace {
				t.Fatalf("run did not execute isolated: branch=%q workspacePath=%q workspace=%q", loaded.Branch, loaded.WorkspacePath, workspace)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach a terminal state: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterPlumbsFullSuiteCadence proves POST /runs forwards both
// the declared full-suite command and its cadence. A deliberately-failing
// command on the first slice with cadence 2 must be skipped and leave an
// auditable cadence decision; if cadence were dropped, this run would
// quarantine on full_suite_verify instead.
func TestAPIStartStarterPlumbsFullSuiteCadence(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	workspace := newFixtureRepo(t)
	writeNativeTicketForAPI(t, workspace, "api-ticket")
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // see TestAPIStartStarterPlumbsIsolateWorkspaceFlag's matching comment.
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-full-suite-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		Timeout:             "30s",
		VerifyCommand:       "true",
		FullSuiteCommand:    "false",
		FullSuiteCadence:    2,
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && (loaded.State == run.StateAccepted || loaded.State == run.StateHalted || loaded.State == run.StateQuarantined) {
			if loaded.State != run.StateAccepted {
				t.Fatalf("state = %q, want accepted because first slice is not due at cadence 2", loaded.State)
			}
			if !loaded.FullSuiteConfigured || loaded.FullSuiteCadence != 2 || loaded.FullSuiteSlice != 1 || loaded.FullSuiteScheduled {
				t.Fatalf("full-suite cadence evidence = configured:%v cadence:%d slice:%d scheduled:%v, want configured cadence=2 slice=1 not scheduled", loaded.FullSuiteConfigured, loaded.FullSuiteCadence, loaded.FullSuiteSlice, loaded.FullSuiteScheduled)
			}
			for _, g := range loaded.GateResults {
				if g.Check == "full_suite_verify" {
					t.Fatalf("GateResults = %+v, want cadence to skip full_suite_verify", loaded.GateResults)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach a terminal state: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterAllowsIsolateWorkspaceWithTemporalAddress pins the
// behavior once apiStartStarter's own rejection of this combination was
// removed: isolation is now wired into the Temporal path too
// (PrepareIsolatedWorkspaceActivity/RollbackIsolatedWorkspaceActivity), so
// this request must not be rejected synchronously — it starts normally,
// and the background run halts on its own once the CLI invocation finds
// the given Temporal address unreachable, exactly like a direct CLI
// invocation of the same flags
// would (see TestIntegrationIsolateWorkspaceRequiresReachableTemporalServer).
func TestAPIStartStarterAllowsIsolateWorkspaceWithTemporalAddress(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	writeNativeTicketForAPI(t, workspace, "api-ticket")
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		Ticket:    "api-ticket",
		Workspace: workspace,
		Spec:      spec,
		// An explicit, offline build script: this test means to exercise
		// isolation-with-Temporal-address plumbing, not the
		// separate default-model-backed-build-requires-a-relay check --
		// sandboxing is unconditional, so without this the run would fail
		// that check instead of ever reaching the Temporal-unreachable
		// halt this test actually asserts on.
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		TemporalAddress:     "localhost:0",
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v, want it accepted synchronously (isolation is no longer rejected outright with a Temporal address)", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State == run.StateHalted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not halt: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterPlumbsSkipProjectCheckFlag is the regression test for a
// real finding from a GitHub Codex App review round, 2026-08-29:
// api.StartRequest had no way to reach -skip-project-check, the CLI's only
// escape hatch for a project that hasn't adopted the bootstrap convention
// yet, so every API-started run against such a project failed synchronously
// with no path to the bypass the CLI itself advertises. Proves
// SkipProjectCheck: true actually reaches the run (which would otherwise
// fail this mandatory preflight, since the fixture workspace deliberately
// carries no bootstrap scaffold).
func TestAPIStartStarterPlumbsSkipProjectCheckFlag(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	workspace := newFixtureRepoWithoutBootstrapScaffold(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-skip-project-check-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		Timeout:             "30s",
		VerifyCommand:       "true",
		SkipProjectCheck:    true,
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && loaded.State == run.StateAccepted {
			// run.Run.SkipProjectCheck (2026-09-03 Opus factory-pipeline
			// review, item C3): an accepted run's own durable record must
			// itself show whether the mandatory preflight was bypassed --
			// ProductSpecSHA256/ContractSHA256 being empty is not enough,
			// since those are also empty for a project that simply has no
			// spec/spec.md yet, not only for a skipped preflight.
			if !loaded.SkipProjectCheck {
				t.Fatalf("run.json SkipProjectCheck = false, want true: the durable record must show the preflight was bypassed, not just that it happened to pass")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not reach accepted despite SkipProjectCheck, want the missing bootstrap scaffold to have been bypassed: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterReportsProjectBootstrapFailureAsInvalidRequest is the
// regression test for a real console live-validation run, 2026-09-08: a
// project-bootstrap preflight failure (a misconfigured -workspace/-spec
// pairing, or a project that hasn't adopted the convention) used to reach
// api.Server's generic `err != nil` branch, discarding the actual,
// actionable diagnostic (which artifact, which path, why) behind a bare
// 500 "start run" -- indistinguishable, from the console, from a genuine
// internal fault. Proves the failure now wraps api.ErrInvalidStartRequest
// (so startRunWithID reports 400 with the real message, see
// errProjectBootstrapCheckFailed's own doc comment) with that message
// still naming the preflight failure.
func TestAPIStartStarterReportsProjectBootstrapFailureAsInvalidRequest(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// not parallel-safe: newFixtureRepoWithoutBootstrapScaffold calls t.Setenv internally
	workspace := newFixtureRepoWithoutBootstrapScaffold(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	_, err = apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		ID:              "api-project-bootstrap-failure-1",
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		Timeout:         "30s",
		VerifyCommand:   "true",
		// An explicit, offline build script: sandboxing is unconditional,
		// so without this the run would fail the separate default-model-
		// backed-build-requires-a-relay check before ever reaching the
		// project-bootstrap preflight this test actually means to exercise.
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		// SkipProjectCheck deliberately left false: this is the exact
		// omission a real console-started run against a not-yet-onboarded
		// project makes.
	})
	if err == nil {
		t.Fatal("start against a workspace missing the bootstrap scaffold succeeded, want a project-bootstrap preflight failure")
	}
	if !errors.Is(err, api.ErrInvalidStartRequest) {
		t.Errorf("err = %v, want it to wrap api.ErrInvalidStartRequest so the API caller sees 400 with the real message, not a generic 500", err)
	}
	if !strings.Contains(err.Error(), "project-bootstrap preflight failed") {
		t.Errorf("err = %v, want the real preflight diagnostic preserved, not discarded", err)
	}
}

// TestAPIStartStarterNeverSetsRequestTicketFlag proves an authenticated
// POST /runs caller cannot get the project-bootstrap preflight's
// -request-ticket treatment (validate -spec itself with
// policy.TicketStructureBrownfield, never resolve/require a repo-native
// ticket) applied to their own run: api.StartRequest carries no
// RequestTicket-shaped field for apiStartStarter's own args-building code
// to forward (see api_start.go's `args := []string{...}` construction and
// every appendStringFlag/appendIntFlag/bare-append call after it -- none
// names -request-ticket), so a caller cannot request it, encode it as
// unrecognized JSON, or otherwise reach it. Proof: even with a real,
// well-formed request-pipeline ticketspec as -spec (the exact shape a
// -request-ticket run accepts), starting through the API against a
// strict-profile repo with no repo-native spec/tickets/<ticket>.md still
// hits the pre-fix failure mode (resolvePiTicketPath's own native-ticket
// resolution, unaffected by -spec's actual content) rather than passing --
// confirming this run never took the -request-ticket path.
func TestAPIStartStarterNeverSetsRequestTicketFlag(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// not parallel-safe: newFixtureRepo (via testfixture.NewGitRepo) calls
	// t.Setenv, which panics if called after t.Parallel().
	workspace := newFixtureRepo(t)
	// Deliberately no spec/tickets/ directory: newFixtureRepo's own strict
	// scaffold (spec.md/contract.md/ARCHITECTURE.md) is real, so only
	// ticket_structure can fail here.
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte(wellFormedRequestTicketspec), 0o644); err != nil {
		t.Fatalf("write request ticketspec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	_, err = apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-request-ticket-marker-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		Timeout:             "30s",
		VerifyCommand:       "true",
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		// SkipProjectCheck deliberately left false, and no field on this
		// struct can request -request-ticket treatment -- the exact thing
		// this test proves.
	})
	if err == nil {
		t.Fatal("start against a repo with no repo-native ticket succeeded despite a well-formed -spec, want the native-ticket resolution to still fail closed (POST /runs cannot reach -request-ticket)")
	}
	if !errors.Is(err, api.ErrInvalidStartRequest) {
		t.Errorf("err = %v, want it to wrap api.ErrInvalidStartRequest", err)
	}
	if !strings.Contains(err.Error(), "ticket_structure") {
		t.Errorf("err = %v, want the ticket_structure failure named -- this run must still be resolving a repo-native ticket, not validating -spec directly", err)
	}
}

// TestAPIStartStarterPlumbsSandboxImageFlag is the regression test for the
// 2026-09-03 Opus factory-pipeline review's item C2: api.StartRequest had no
// way to reach -sandbox-image at all -- only a bare CLI invocation could
// ever route a run through Docker containment, so every API-started run
// executed unsandboxed on the host regardless of operator intent. Proves
// SandboxImage actually reaches the spawned run's argv without needing a
// real Docker daemon: LaunchSpec.Validate rejects a non-digest-pinned image
// before ever invoking the docker binary, so a deliberately mistagged image
// here produces a deterministic, Docker-independent failure signature that
// only fires if the flag was actually plumbed through.
func TestAPIStartStarterPlumbsSandboxImageFlag(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{allowedImages: []string{"not-digest-pinned:latest"}})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-sandbox-image-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		// A realistic budget, not a short one: runSandboxWithRetries
		// reserves sandboxAttemptTeardownMargin (10s) off the run's own
		// deadline for container teardown, so a -timeout close to that
		// margin hits a *different*, unrelated failure (insufficient time
		// remaining) before ever reaching LaunchSpec.Validate.
		Timeout:          "60s",
		VerifyCommand:    "true",
		SkipProjectCheck: true,
		SandboxImage:     "not-digest-pinned:latest",
	})
	if err != nil {
		t.Fatalf("start through API adapter: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		loaded, loadErr := run.Load(dataDir, started.ID)
		if loadErr == nil && (loaded.State == run.StateHalted || loaded.State == run.StateQuarantined || loaded.State == run.StateAccepted) {
			if loaded.State == run.StateAccepted {
				t.Fatalf("run reached accepted with a non-digest-pinned SandboxImage; want it rejected by LaunchSpec.Validate, which only fires on the sandboxed path -- SandboxImage never reached the run")
			}
			// StateHalted (not quarantined) is exactly LaunchSpec.Validate's
			// own failure mode: an infrastructure failure the build never
			// ran through, not a policy gate that evaluated a real result.
			if loaded.State != run.StateHalted {
				t.Fatalf("run state = %q, want %q (LaunchSpec.Validate rejecting the non-digest-pinned image as an infrastructure failure)", loaded.State, run.StateHalted)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never reached a terminal state: loaded=%+v err=%v", loaded, loadErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAPIStartStarterForwardsServerConfiguredSandboxResourceLimits is the
// regression test for a real GitHub Codex App review finding on this same
// PR (P1): an API-started run never received serveMain's trusted,
// operator-configured resource ceiling at all -- only the
// separately-supervised `factoryd daemon` did -- so it silently fell back
// to runMainWithReady's own flag defaults (4g/2/512/256m) regardless of
// what an operator configured on `serve`, e.g. an operator configuring a 1g
// ceiling would still see ordinary API runs consume up to 4g.
// flags-consolidate (2026-09-10) then removed -sandbox-memory/cpus/pids/
// tmpfs-size as CLI flags entirely, so serveMain now folds them into a
// sessionconfig.Settings (apiStartTier2Settings) and installs it as
// tier2SettingsOverride before ever accepting a connection, rather than
// forwarding them into the spawned run's own argv -- see serveMain's own
// comment at that assignment. Proven the same way as before, adapted to
// this mechanism: deliberately constructing an invalid sandboxResourceLimits
// (empty Memory), folding it into tier2SettingsOverride the way serveMain
// would, and confirming the started run still fails on it -- if
// apiStartStarter/runMainWithReady silently ignored tier2SettingsOverride,
// sessionconfig.DefaultSettings' own "4g" would apply instead and the run
// would start normally. Fails synchronously with
// validateSandboxResourceLimitFlags' own error (runMainWithReady's startup
// validation, closed alongside a related P2 on this same review round --
// see TestValidateSandboxResourceLimitFlags), not via a later
// LaunchSpec.Validate halt, since the invalid value never gets as far as
// launching a sandbox at all.
func TestAPIStartStarterForwardsServerConfiguredSandboxResourceLimits(t *testing.T) {
	dp := newTestDeps(t)
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	// Mirrors serveMain's own tier2SettingsOverride assignment -- see its
	// doc comment for why it is safe to leave set for a real daemon's whole
	// lifetime; reset here so this test does not leak it into another test
	// sharing this package's test binary.
	settings := apiStartTier2Settings(sessionconfig.DefaultSettings(), release.MergePolicy{}, sandboxResourceLimits{memory: "", cpus: "2", tmpfsSize: "256m"})
	tier2SettingsOverride = &settings
	t.Cleanup(func() { tier2SettingsOverride = nil })

	_, err = apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{allowedImages: []string{"factory-worker:test@sha256:deadbeef"}})(context.Background(), api.StartRequest{
		TemporalAddress:     sharedTemporalAddress(t),
		ID:                  "api-sandbox-resource-limits-1",
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		Timeout:             "60s",
		VerifyCommand:       "true",
		SkipProjectCheck:    true,
		SandboxImage:        "factory-worker:test@sha256:deadbeef",
	})
	if err == nil || !strings.Contains(err.Error(), "-sandbox-memory must not be empty") {
		t.Fatalf("start through API adapter with an empty configured sandbox memory limit: error = %v, want it to name the empty -sandbox-memory flag -- this only happens if the value genuinely reached resolveSettings via tier2SettingsOverride instead of being silently dropped", err)
	}
}

// TestAPIStartTier2SettingsPreservesBaseSettingsOutsideReleaseAndSandbox is
// the regression test for an adversarial-review finding on this same
// branch: apiStartTier2Settings originally started from
// sessionconfig.DefaultSettings() (hardcoded defaults) rather than the
// caller-supplied base, silently discarding every session-config field
// outside release/sandbox-resource -- relay token/cost budgets and
// ceilings, registry-proxy upstreams and sizing, sandbox_docker/
// sandbox_user -- for every API-started run, for as long as `serve` ran,
// with no error. Proves a base field this function doesn't itself overlay
// (MeterTokenCeiling, RegistryProxyNPMUpstream) survives into the result
// unchanged, while a field release/sandbox-resource DOES own (SandboxCPUs)
// still comes from the supplied sandboxLimits, not the base -- confirming
// this is a genuine overlay onto base, not base being ignored in either
// direction.
func TestAPIStartTier2SettingsPreservesBaseSettingsOutsideReleaseAndSandbox(t *testing.T) {
	base := sessionconfig.DefaultSettings()
	base.MeterTokenCeiling = 42
	base.RegistryProxyNPMUpstream = "https://internal-mirror.example.com/npm"
	base.SandboxCPUs = "8" // must be overridden by sandboxLimits below, not preserved

	settings := apiStartTier2Settings(base, release.MergePolicy{}, sandboxResourceLimits{memory: "1g", cpus: "2", tmpfsSize: "256m"})

	if settings.MeterTokenCeiling != 42 {
		t.Errorf("MeterTokenCeiling = %d, want 42 (base session-config value silently dropped)", settings.MeterTokenCeiling)
	}
	if settings.RegistryProxyNPMUpstream != "https://internal-mirror.example.com/npm" {
		t.Errorf("RegistryProxyNPMUpstream = %q, want the base session-config value (silently dropped)", settings.RegistryProxyNPMUpstream)
	}
	if settings.SandboxCPUs != "2" {
		t.Errorf("SandboxCPUs = %q, want sandboxLimits' own value \"2\" to win over base's \"8\"", settings.SandboxCPUs)
	}
}

// TestAPIStartStarterRejectsIsolatedRunIDInvalidAsGitRef is the regression
// test for a real GitHub Codex App review finding, 2026-08-29: the earlier
// id check only guards against an unsafe *path* component ("/", a whole
// ".." segment) -- not against an illegal git ref, which is what
// wsisolation.Prepare actually derives the id into ("factoryd/<id>") when
// isolation is on. An id like "foo..bar" passes the path check but two
// consecutive dots make it an invalid ref; POST /runs would otherwise
// already return 202 (onReady fires before Prepare ever runs) only for the
// background run to halt moments later. apiStartStarter must catch this
// itself, synchronously, when isolation is requested.
func TestAPIStartStarterRejectsIsolatedRunIDInvalidAsGitRef(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		ID:              "foo..bar",
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
	})
	if !errors.Is(err, api.ErrInvalidStartRequest) {
		t.Fatalf("err = %v, want api.ErrInvalidStartRequest", err)
	}
}

// TestAPIStartStarterRejectsInvalidGitRefIDWithoutExplicitIsolateWorkspace:
// every build is isolated, so an id that is not a valid git ref is rejected
// synchronously, before the 202.
func TestAPIStartStarterRejectsInvalidGitRefIDWithoutExplicitIsolateWorkspace(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait() // found via CI (-race, no local Temporal server): a background apiStartStarter goroutine can still be writing under dataDir (a t.TempDir()) after run.Load first observes a terminal state, racing t.TempDir()'s own cleanup ("directory not empty"). inFlight.Wait() blocks until that goroutine's own inFlight.Done() actually fires.
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		ID:              "foo..bar",
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		// IsolateWorkspace deliberately omitted (zero value, false) -- the
		// point of this test.
	})
	if !errors.Is(err, api.ErrInvalidStartRequest) {
		t.Fatalf("err = %v, want api.ErrInvalidStartRequest even without an explicit IsolateWorkspace: true", err)
	}
}

// TestAPIStartStarterRejectsSandboxDockerAndSandboxUser covers two
// findings at once. The original (a real Opus review finding, 2026-09-04):
// SandboxDocker becomes the literal executable factoryd runs via
// exec.Command inside internal/sandbox.Run, and nothing validated it, so an
// authenticated API token holder could name any host executable and have
// this trusted process run it. The second (found reviewing the
// flags-consolidate integration): once both fields became session-config
// only, runMainWithReady stopped defining -sandbox-docker/-sandbox-user at
// all, but apiStartStarter kept forwarding them -- so even the previously
// *accepted* values ("docker", any sandbox_user) made the spawned run die
// at flag.Parse with "flag provided but not defined" instead of running.
// Both fields are now refused up front, which is also what keeps this test
// honest: each case asserts api.ErrInvalidStartRequest, a value this
// starter only ever produces before it builds an argv.
func TestAPIStartStarterRejectsSandboxDockerAndSandboxUser(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	for _, test := range []struct {
		name string
		req  api.StartRequest
	}{
		{name: "attacker-controlled sandbox_docker", req: api.StartRequest{SandboxDocker: "/tmp/attacker-controlled-binary"}},
		{name: "the formerly-accepted default sandbox_docker", req: api.StartRequest{SandboxDocker: "docker"}},
		{name: "sandbox_user", req: api.StartRequest{SandboxUser: "0:0"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataDir := t.TempDir()
			var inFlight sync.WaitGroup
			defer inFlight.Wait()
			req := test.req
			req.Ticket = "api-ticket"
			req.Workspace = workspace
			req.Spec = spec
			_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), req)
			if !errors.Is(err, api.ErrInvalidStartRequest) {
				t.Fatalf("err = %v, want api.ErrInvalidStartRequest", err)
			}
		})
	}
}

// TestAPIStartStarterRejectsNonAllowlistedSandboxImage is the regression
// test for a 2026-09-07 containment review finding: before this check, any
// digest-pinned image from any registry was accepted from POST /runs --
// only downstream digest-pinning was enforced, never which image. With no
// -api-allowed-sandbox-images and no daemon-configured sandbox image
// (the default, exercised here via a zero-value apiSandboxPolicy), NO
// image is accepted; anything, even a validly digest-pinned one, is
// rejected synchronously.
func TestAPIStartStarterRejectsNonAllowlistedSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		SandboxImage:    "attacker.example/evil@sha256:" + strings.Repeat("a", 64),
	})
	if !errors.Is(err, api.ErrInvalidStartRequest) {
		t.Fatalf("err = %v, want api.ErrInvalidStartRequest for a non-allowlisted sandbox_image", err)
	}
}

// TestAPIStartStarterRefusesAStartWithNoTemporalAddress: every build runs on
// Temporal, so with autostart off a start without
// one is refused: runMainWithReady resolves the address itself.
func TestAPIStartStarterRefusesAStartWithNoTemporalAddress(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "0")
	workspace := newFixtureRepo(t)
	writeNativeTicketForAPI(t, workspace, "api-ticket")
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	_, err = apiStartStarter(dp, t.TempDir(), &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
		Ticket:              "api-ticket",
		Workspace:           workspace,
		Spec:                spec,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      script,
		VerifyCommand:       "true",
	})
	want := "no Temporal address: pass -temporal-address (FACTORYD_AUTOSTART=0 starts nothing)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("API start with no Temporal address: error = %v, want one containing %q", err, want)
	}
}

// TestAPIStartStarterResolvesAnEmptyTemporalAddress: with autostart on, a
// start that names no address has the run resolve one (temporal.ensure), and a start that names its own is passed through without
// asking. Both runs reach Temporal: the record carries a workflow id.
func TestAPIStartStarterResolvesAnEmptyTemporalAddress(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	address := sharedTemporalAddress(t)
	t.Setenv(hostcontrol.AutostartEnvVar, "1")
	old := fakeTemporalOf(dp).ensureFn
	t.Cleanup(func() { fakeTemporalOf(dp).ensureFn = old })
	var calls int
	fakeTemporalOf(dp).ensureFn = func(context.Context, io.Writer) string { calls++; return address }

	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")

	start := func(id, named string, wantCalls int) {
		t.Helper()
		workspace := newFixtureRepo(t)
		writeNativeTicketForAPI(t, workspace, "api-ticket")
		spec := filepath.Join(t.TempDir(), "spec.md")
		if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
			t.Fatalf("write spec: %v", err)
		}
		dataDir := t.TempDir()
		var inFlight sync.WaitGroup
		defer inFlight.Wait()
		calls = 0
		started, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{})(context.Background(), api.StartRequest{
			ID: id, TemporalAddress: named, Ticket: "api-ticket", Workspace: workspace, Spec: spec,
			BuildAppInterpreter: "/bin/sh", BuildAppScript: script, Timeout: "30s", VerifyCommand: "true",
		})
		if err != nil {
			t.Fatalf("start %s: %v", id, err)
		}
		if calls != wantCalls {
			t.Errorf("%s: ensureTemporal called %d times, want %d", id, calls, wantCalls)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			loaded, loadErr := run.Load(dataDir, started.ID)
			if loadErr == nil && (loaded.State == run.StateAccepted || loaded.State == run.StateHalted || loaded.State == run.StateQuarantined) {
				if loaded.TemporalWorkflowID == "" {
					t.Errorf("%s: state %q with no temporal workflow id; the run did not go through Temporal at %s", id, loaded.State, address)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: no terminal state: %+v err=%v", id, loaded, loadErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	start("api-resolved-1", "", 1)
	start("api-named-1", address, 0)
}

// piforkExecutionRoles is a roles: block whose execution role runs the
// pifork harness -- the shape that replaced the per-request engine.
func piforkExecutionRoles() *sessionconfig.Roles {
	return &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "luna", Harness: "pifork"}}
}

func TestAPIStartStarterRejectsPiforkRoleWithoutWorkerImage(t *testing.T) {
	dp := newTestDeps(t)
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	var inFlight sync.WaitGroup
	defer inFlight.Wait()

	_, err := apiStartStarter(dp, t.TempDir(), &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{roles: piforkExecutionRoles()})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
	})
	if !errors.Is(err, api.ErrInvalidStartRequest) || !strings.Contains(err.Error(), "roles.execution: harness pifork requires an allowlisted digest-pinned sandbox_image") {
		t.Fatalf("pifork API start without worker image: error = %v, want an invalid-request image diagnostic naming the role", err)
	}
}

// TestAPIStartStarterRunsWithExecutionRoleAlias is the success-path
// counterpart to TestAPIStartStarterRejects*WithExecutionRole above:
// with roles.execution set, POST /runs must actually START the run on
// the role's own alias -- not merely stop refusing it. A request can no
// longer select a model of its own, so this covers the only remaining
// shape: an unselected request resolving to roles.execution the same way
// an unselected request everywhere else does. Proven the same way
// TestAPIStartStarterRunsWithExecutionRoleAlias always has: apiStartStarter
// must return no error at all here (unlike an earlier version of this
// test, which used a deliberately invalid alias id specifically so
// RouteSpec.Validate would reject the request synchronously and never
// really "started" anything) -- proving a VALID alias id resolves cleanly
// all the way through the child run's own RoutePolicy construction and
// RouteSpec.Validate. Both fields on the fully-resolved RoutePolicy are
// distinguishing enough (the alias's own model id and API) that a wrong
// or empty resolution would fail one of these earlier field-level
// checks the same way the invalid-id version's test did.
//
// The run then genuinely attempts the sandboxed build through the
// package's shared fake-docker fixture, which stands in for `docker run`
// but has no equivalent for `docker network` (relay launch's first real
// step) -- so it fails at that infrastructure step, not at any
// field-level validation, once past the same checks the invalid-id
// version's test stopped at. Asserting the eventual failure names that
// infrastructure step, not a field-validation message, is this test's
// own proof the run got further -- see the recorded gap below (a full
// live relay launch needs a fixture extension this change doesn't make)
// for what a stronger end-to-end assertion would still need.
func TestAPIStartStarterRunsWithExecutionRoleAlias(t *testing.T) {
	dp := newTestDeps(t)
	dockerBinary := fakeSandboxDockerBinary(t)
	path := isolateSessionConfig(t)
	writeSessionConfig(t, path, "sandbox_docker: "+dockerBinary+"\n"+
		"registry_proxy: false\n"+
		"routes:\n  local:\n    allow_no_credential: true\n    upstream: https://model-a.example.invalid\n"+
		"models:\n  exec-alias:\n    id: gpt-9000\n    api: openai-completions\n    routes: [local]\n"+
		"roles:\n  execution:\n    model: exec-alias\n")
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
		roles:         &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "exec-alias"}},
	})(context.Background(), api.StartRequest{
		TemporalAddress:  sharedTemporalAddress(t),
		Ticket:           "api-ticket",
		Workspace:        workspace,
		Spec:             spec,
		SkipProjectCheck: true,
		// A command that passes on the fixture repo: the default one does
		// not, and the baseline verify would halt the run before the
		// relay launch this test is about.
		VerifyCommand: "true",
	})
	// This is the actual proof: an earlier version of this test used a
	// deliberately invalid alias id specifically so RouteSpec.Validate
	// would reject the request synchronously, right here -- err would be
	// non-nil and started would be nil. A valid id must not be rejected:
	// the run starts.
	if err != nil {
		t.Fatalf("start through API adapter with roles.execution set: %v", err)
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
	// The eventual failure must be the shared fake-docker fixture's own
	// known gap (no `docker network` support, so relay launch's first
	// real step fails) -- an infrastructure failure, not
	// roles.execution/field-validation rejecting the alias a second time
	// further down the same path.
	if loaded.HaltError != "" && !strings.Contains(loaded.HaltError, "relay") {
		t.Errorf("run halted for a reason unrelated to relay launch: %q, want it to name the relay/network infrastructure gap", loaded.HaltError)
	}
	if strings.Contains(loaded.HaltError, "must not contain a slash") || strings.Contains(loaded.HaltError, "roles.execution") {
		t.Errorf("run failed on field validation, not infrastructure: %q -- the alias's id was not accepted", loaded.HaltError)
	}
}

// TestAPIStartStarterForwardsEgressCABundleFlag proves apiSandboxPolicy.
// egressCABundle actually reaches the spawned run's own -egress-ca-bundle
// flag: an operator-configured but unparseable bundle path fails inside
// the spawned runMainWithReady's own startup validation (before any run
// record exists), surfacing here as a synchronous error naming the path --
// the same downstream-error trick TestAPIStartStarterPlumbsRelayModeFlags
// uses, proving the value was actually forwarded rather than merely
// accepted by apiSandboxPolicy.
func TestAPIStartStarterForwardsEgressCABundleFlag(t *testing.T) {
	dp := newTestDeps(t)
	withFakeSandboxSettings(t)
	// roles.execution must resolve to a real (fake-credentialed) route
	// before runMainWithReady ever reaches its own -egress-ca-bundle
	// validation further down (routes:/models:/roles: is the only
	// session-config schema, and that resolution runs unconditionally,
	// before this check): a config with no roles at all would otherwise
	// fail earlier on "roles.execution.model is not configured", never
	// reaching the check this test means to exercise.
	tier2SettingsOverride.Routes = map[string]sessionconfig.Route{
		"local": {AllowNoCredential: true, Upstream: "https://model-a.example.invalid"},
	}
	tier2SettingsOverride.Models = map[string]sessionconfig.Model{
		"m": {ID: "some-model", Routes: []string{"local"}},
	}
	tier2SettingsOverride.Roles = &sessionconfig.Roles{Execution: &sessionconfig.RoleConfig{Model: "m"}}
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	badBundle := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(badBundle, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{egressCABundle: badBundle})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
	})
	if err == nil || !strings.Contains(err.Error(), badBundle) {
		t.Fatalf("err = %v, want an error naming the bad egress CA bundle path %s", err, badBundle)
	}
}

// TestAPIStartStarterAllowsAllowlistedSandboxImage proves an operator can
// still reach a non-canonical sandbox_image from POST /runs by explicitly
// configuring -api-allowed-sandbox-images -- Phase 5.1 narrows the default,
// it does not remove the capability. Uses the same slash-rejection trick
// as TestAPIStartStarterPlumbsRelayModeFlags (RoutePolicy.Validate fires
// after this starter's own SandboxImage checks) as proof
// the image genuinely passed the allowlist rather than failing closed
// earlier for an unrelated reason.
func TestAPIStartStarterAllowsAllowlistedSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	sandboxImage := "operator-approved.example/worker@sha256:" + strings.Repeat("c", 64)
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{allowedImages: []string{sandboxImage}})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		SandboxImage:    sandboxImage,
	})
	if err != nil && errors.Is(err, api.ErrInvalidStartRequest) && strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("start through API adapter with an allowlisted sandbox_image: error = %v, want the image itself to pass the allowlist", err)
	}
}

// TestAPIStartStarterAppliesDefaultSandboxImage proves a request that omits
// sandbox_image entirely still resolves to the daemon's own configured
// -api-default-sandbox-image: the console has no
// UI to ever set sandbox_image itself (see startRun in
// console/src/api/runs.ts), so without a default an allowlist alone could never let it
// reach a sandboxed run at all. Uses the same
// downstream-error trick as TestAPIStartStarterAllowsAllowlistedSandboxImage:
// reaching RelayWorkerModelID's slash rejection proves the default image
// itself passed the allowlist and was actually plumbed through, not that
// the request failed closed earlier for an unrelated reason.
func TestAPIStartStarterAppliesDefaultSandboxImage(t *testing.T) {
	dp := newTestDeps(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	sandboxImage := "operator-approved.example/worker@sha256:" + strings.Repeat("c", 64)
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{
		allowedImages: []string{sandboxImage},
		defaultImage:  sandboxImage,
	})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
	})
	if err != nil && errors.Is(err, api.ErrInvalidStartRequest) && strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("start through API adapter with sandbox_image omitted and a configured default: error = %v, want the default sandbox_image to be applied and pass the allowlist", err)
	}
}

// TestAPIStartStarterExplicitSandboxImageWinsOverDefault proves an explicit
// request sandbox_image is used instead of a configured
// -api-default-sandbox-image, even when the default itself is not
// allowlisted (and so could never be used were the request's own value not
// preferred) -- the default only ever fills in an empty request field.
func TestAPIStartStarterExplicitSandboxImageWinsOverDefault(t *testing.T) {
	dp := newTestDeps(t)
	// not parallel-safe: newFixtureRepo calls t.Setenv internally
	workspace := newFixtureRepo(t)
	spec := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(spec, []byte("# fixture spec\nTests-Required: no -- API-start fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	dataDir := t.TempDir()
	var inFlight sync.WaitGroup
	defer inFlight.Wait()
	sandboxImage := "operator-approved.example/worker@sha256:" + strings.Repeat("c", 64)
	_, err := apiStartStarter(dp, dataDir, &inFlight, release.MergePolicy{}, sandboxResourceLimits{memory: "4g", cpus: "2", tmpfsSize: "256m"}, apiSandboxPolicy{
		allowedImages: []string{sandboxImage},
		defaultImage:  "unrelated.example/never-used@sha256:" + strings.Repeat("e", 64),
	})(context.Background(), api.StartRequest{
		TemporalAddress: sharedTemporalAddress(t),
		Ticket:          "api-ticket",
		Workspace:       workspace,
		Spec:            spec,
		SandboxImage:    sandboxImage,
	})
	if err != nil && errors.Is(err, api.ErrInvalidStartRequest) && strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("start through API adapter with an explicit sandbox_image and an unrelated configured default: error = %v, want the explicit sandbox_image, not the (non-allowlisted) default, to be used", err)
	}
}

// TestServeMainRejectsDefaultSandboxImageNotAllowlisted proves serveMain
// fails loud at startup, before ever binding a listener, if
// -api-default-sandbox-image is configured but is not itself a member of
// -api-allowed-sandbox-images -- a daemon must never start with a default
// it would then reject on every request that omits the field.
func TestServeMainRejectsDefaultSandboxImageNotAllowlisted(t *testing.T) {
	dp := newTestDeps(t)
	allowed := "operator-approved.example/worker@sha256:" + strings.Repeat("c", 64)
	other := "unrelated.example/other@sha256:" + strings.Repeat("e", 64)
	err := serveMain(dp, []string{
		"-data-dir", t.TempDir(),
		"-api-allowed-sandbox-images", allowed,
		"-api-default-sandbox-image", other,
	})
	if err == nil || !strings.Contains(err.Error(), "-api-default-sandbox-image") || !strings.Contains(err.Error(), "not allowlisted") {
		t.Fatalf("serveMain with a non-allowlisted -api-default-sandbox-image: error = %v, want it to name the misconfigured default", err)
	}
}

// TestServeMainRefusesInvalidRolesBlock proves serve's own startup path
// validates roles: once, explicitly (right after baseSettings resolves,
// before any Docker/image check) -- an unknown model_aliases entry named
// by a role must refuse the whole invocation. Uses an explicit -config
// (never the default search path, isolated or not) so this cannot pick up
// a real developer session config.
func TestServeMainRefusesInvalidRolesBlock(t *testing.T) {
	dp := newTestDeps(t)
	configPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(configPath, []byte("roles:\n  execution:\n    model: does-not-exist\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	err := serveMain(dp, []string{
		"-data-dir", t.TempDir(),
		"-config", configPath,
	})
	if err == nil || !strings.Contains(err.Error(), "roles.execution") {
		t.Fatalf("serveMain with an invalid roles: block: error = %v, want it to name roles.execution", err)
	}
}
