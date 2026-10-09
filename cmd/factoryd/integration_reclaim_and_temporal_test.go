package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// TestIntegrationDaemonReclaimHoldsRepositoryOwnershipAfterSubmitterCrash
// proves the daemon's recovery Worker cannot execute a crashed repository run
// without the same Git-common-dir ownership a live submission would hold. A
// durable running record and delivered child stand in for a submitter killed
// immediately afterward; the daemon adopts that request, and a concurrent
// direct invocation is rejected until the recovered child finishes and its
// lock is released.
func TestIntegrationDaemonReclaimHoldsRepositoryOwnershipAfterSubmitterCrash(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress calls t.Setenv, and Go's
	// testing package panics if a test using t.Setenv also calls
	// t.Parallel() (confirmed by running this file with t.Parallel() here:
	// "testing: test using t.Setenv ... can not use t.Parallel").
	address := isolatedTemporalAddress(t)
	ws := newFixtureRepo(t)
	commonDir, err := wsisolation.GitCommonDir(ws)
	if err != nil {
		t.Fatalf("resolve Git common directory: %v", err)
	}
	lockPath := filepath.Join(commonDir, "factoryd-direct.lock")
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	repository := fmt.Sprintf("fixture/reclaim-lock-%d", time.Now().UnixNano())
	// Resolved through the same symlink-canonicalizing path the daemon
	// itself applies to -data-dir (wsisolation.CanonicalPath, via
	// canonicalPath) -- on macOS t.TempDir() lives under /var, a symlink
	// to /private/var, so the raw path would otherwise read as a
	// different directory from the Worker's own resolved -data-dir and
	// trip RunBuildActivity's own data-directory-divergence check below.
	rawDataDir := t.TempDir()
	dataDir, err := filepath.EvalSymlinks(rawDataDir)
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", dataDir,
	)
	daemonCmd.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	daemonWaited := false
	t.Cleanup(func() {
		if !daemonWaited {
			_ = daemonCmd.Process.Signal(syscall.SIGTERM)
			_ = daemonCmd.Wait()
		}
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	// Wait for the daemon's own liveness signal before creating the run. This
	// makes the first periodic reclaim scan the only race in the test, rather
	// than daemon startup and recovery competing with the submitter setup.
	heartbeatPath := daemonheartbeat.Path(dataDir, workflow.RepositoryOwnerWorkflowID(repository))
	startupDeadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := daemonheartbeat.Read(heartbeatPath); err == nil {
			break
		}
		if time.Now().After(startupDeadline) {
			t.Fatalf("daemon heartbeat never appeared: %s", daemonOutput.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Seed the durable record at the point where a submitter has written its
	// initial state and then crashed. The signal is sent separately to model a
	// request that reached the owner just before that crash, leaving its child
	// on the request-specific queue for daemon recovery.
	runID := fmt.Sprintf("fixture-crashed-run-%d", time.Now().UnixNano())
	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	logDir := filepath.Join(dataDir, "logs", runID)
	checkpointDir := filepath.Join(dataDir, "temporal-checkpoints", runID)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		t.Fatalf("create checkpoint dir: %v", err)
	}
	seeded := &run.Run{
		ID:            runID,
		Ticket:        "fixture-ticket",
		WorkspacePath: ws,
		ProjectPath:   ws,
		SpecPath:      specPath,
		Repository:    repository,
		State:         run.StateSliceRunning,
		BaseSHA:       baseSHA,
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed crashed-submitter run.json: %v", err)
	}
	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal for crashed-submitter request: %v", err)
	}
	defer temporalClient.Close()
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	runInput := workflow.RunWorkflowInput{
		Ticket:              seeded.Ticket,
		WorkspacePath:       ws,
		IsolateWorkspace:    true,
		IsolatedRepoDir:     ws,
		IsolatedParentDir:   t.TempDir(),
		SpecPath:            specPath,
		BaseSHA:             baseSHA,
		RunID:               runID,
		DataDir:             dataDir,
		LogDir:              logDir,
		CheckpointDir:       checkpointDir,
		SandboxImage:        fakeSandboxImage,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildMaxAttempts:    1,
		VerifyCommand:       "sleep 5; true",
		VerifyMaxAttempts:   1,
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
		RepositoryOwnerID:   ownerID,
		TaskQueue:           taskQueue + "-run-" + runID,
	}
	ctx, cancelSignal := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: runID, Input: runInput},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository}); err != nil {
		cancelSignal()
		t.Fatalf("signal crashed-submitter request: %v", err)
	}
	cancelSignal()

	// The reclaim log is emitted only after the daemon has acquired the lock
	// and started its request-specific Worker. Keep the recovered verify phase
	// long enough for the rejected direct invocation to observe that ownership
	// remains held across execution, not just during worker startup.
	reclaimDeadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(daemonOutput.String(), "reclaiming nonterminal run "+runID) {
		if time.Now().After(reclaimDeadline) {
			t.Fatalf("daemon never reclaimed run %s; output:\n%s", runID, daemonOutput.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("daemon reported reclaim without repository ownership lock %q: %v", lockPath, err)
	}

	// A non-blocking exclusive probe of the repository lock: while the daemon's
	// recovered run owns the repository it must fail busy, not wait or succeed.
	probe, probeErr := wsisolation.AcquireDirectLock(ws)
	if probeErr == nil {
		_ = probe.Close()
		t.Fatalf("repository lock acquired while daemon recovery held ownership; daemon output:\n%s", daemonOutput.String())
	}
	if !errors.Is(probeErr, wsisolation.ErrBusy) {
		t.Fatalf("ownership probe error = %v, want %v during daemon recovery", probeErr, wsisolation.ErrBusy)
	}

	// Reconciliation can spend several bounded Temporal query windows
	// establishing that the owner/child is terminal after the crash. Probe
	// the ownership boundary itself instead of guessing that timing: the
	// daemon releases this handle only after terminalReclaimedRunIDs confirms
	// the recovered run is terminal.
	terminalDeadline := time.Now().Add(60 * time.Second)
	ownershipReleased := false
	for time.Now().Before(terminalDeadline) {
		probeCtx, cancelProbe := context.WithTimeout(context.Background(), time.Second)
		probeLock, probeErr := wsisolation.AcquireDirectLockContext(probeCtx, ws)
		cancelProbe()
		if probeErr == nil {
			ownershipReleased = true
			if closeErr := probeLock.Close(); closeErr != nil {
				t.Fatalf("close post-recovery ownership probe: %v", closeErr)
			}
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ownershipReleased {
		t.Fatalf("daemon never released repository ownership after recovery; output:\n%s", daemonOutput.String())
	}
	recovered, err := run.Load(dataDir, runID)
	if err != nil || recovered.State != run.StateAccepted {
		t.Fatalf("recovered run state = %q (err=%v), want %q; daemon output:\n%s", recovered.State, err, run.StateAccepted, daemonOutput.String())
	}

	// Once the recovered worker reaches a terminal result and releases its
	// handle, a fresh run may own the same repository.
	secondDirectCtx, cancelSecondDirect := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelSecondDirect()
	secondDirect := exec.CommandContext(secondDirectCtx, binPath,
		"-ticket", "fixture-ticket",
		"-sandbox-image", fakeSandboxImage,
		"-workspace", ws,
		"-spec", specPath,
		"-build-app-interpreter", "/bin/sh",
		"-build-app-script", scriptPath,
		"-verify-command", "true",
		"-data-dir", t.TempDir(),
		"-skip-project-check",
		"-temporal-address", address,
	)
	secondDirect.Env = append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if output, err := secondDirect.CombinedOutput(); err != nil {
		t.Fatalf("run after daemon recovery failed: %v (context=%v)\noutput:\n%s", err, secondDirectCtx.Err(), output)
	}
	_ = daemonCmd.Process.Signal(syscall.SIGTERM)
	if err := daemonCmd.Wait(); err != nil {
		t.Fatalf("stop daemon: %v", err)
	}
	daemonWaited = true
}

// TestIntegrationReconcileReclaimedRunPreservesCleanupUnconfirmed is the
// regression for a Temporal reclaim test gap:
// reconcileReclaimedRun's result.Err-carrying branch sets
// fresh.HaltConfirmed = !result.CleanupUnconfirmed — an ambiguous sandbox
// cleanup (a container that may still be running) must never be silently
// recorded as a confirmed halt, even though the request did reach a
// terminal, queryable owner result. Rather than driving a genuine sandbox
// cleanup failure through a real Docker daemon (see
// sandbox.ErrCleanupUnconfirmed's own production trigger, already covered
// by internal/sandbox and internal/workflow's own tests), this runs its own
// in-process Worker — real Temporal, no subprocess `factoryd daemon`, no
// Docker — registering a fake RunBuildActivity by name (the same technique
// internal/workflow's own newWorkflowEnvironment helper uses) that fails
// with workflow.CleanupUnconfirmedFailureType directly, so the owner's real
// RunWorkflowResult.CleanupUnconfirmed is genuinely true by the time
// reconcileReclaimedRun queries it.
func TestIntegrationReconcileReclaimedRunPreservesCleanupUnconfirmed(t *testing.T) {
	reconciled := reconcileAfterFailedBuild(t, "cleanup-unconfirmed", 30*time.Second,
		func(context.Context, workflow.RunWorkflowInput) (workflow.BuildActivityResult, error) {
			return workflow.BuildActivityResult{}, temporal.NewApplicationError("sandbox container cleanup is unconfirmed", workflow.CleanupUnconfirmedFailureType)
		})
	if reconciled.HaltConfirmed {
		t.Fatal("HaltConfirmed = true, want false: a cleanup-unconfirmed child's halt must never be silently confirmed — a container may still be running")
	}
	// The reclaimed halt must carry the child's own failure text, not
	// leave HaltError empty and the halt notification pointing at logs
	// that never recorded it.
	if !strings.Contains(reconciled.HaltError, "sandbox container cleanup is unconfirmed") {
		t.Fatalf("HaltError = %q, want it to carry the child workflow's failure text", reconciled.HaltError)
	}
}

// TestIntegrationReconcileReclaimedRunNamesStoppedWorker drives a real
// Activity heartbeat timeout -- a RunBuildActivity that blocks without ever
// heartbeating, standing in for a factoryd process that died mid-build --
// so the triage classification is checked against Temporal's own error
// text, not a hand-written copy of it.
// Takes RunWorkflow's full heartbeat timeout (~30s).
func TestIntegrationReconcileReclaimedRunNamesStoppedWorker(t *testing.T) {
	// The build Activity is retried once, so its heartbeat timeout fires
	// twice before the run halts. Each fake attempt never heartbeats and
	// gives up after 40 s, past the 30 s heartbeat timeout, the way a real
	// stale attempt is cancelled by its first refused heartbeat: the Worker
	// runs one Activity at a time, so the retry starts only once the stale
	// attempt has let go of the slot.
	reconciled := reconcileAfterFailedBuild(t, "worker-stopped", 180*time.Second,
		func(ctx context.Context, _ workflow.RunWorkflowInput) (workflow.BuildActivityResult, error) {
			select {
			case <-ctx.Done():
				return workflow.BuildActivityResult{}, ctx.Err()
			case <-time.After(40 * time.Second):
				return workflow.BuildActivityResult{}, errors.New("stale attempt gave up")
			}
		})
	if !strings.Contains(reconciled.Triage, "stopped mid-run") {
		t.Fatalf("Triage = %q (HaltError %q), want the stopped-worker sentence", reconciled.Triage, reconciled.HaltError)
	}
}

// reconcileAfterFailedBuild runs a real RepositoryOwnerWorkflow on an
// in-process Worker whose RunBuildActivity is buildActivity, then drives
// reconcileReclaimedRun against a seeded run.json (standing in for a
// crashed submitter) until it reports the request terminal within
// deadline, and returns the reconciled, halted run.
func reconcileAfterFailedBuild(t *testing.T, name string, deadline time.Duration, buildActivity func(context.Context, workflow.RunWorkflowInput) (workflow.BuildActivityResult, error)) *run.Run {
	t.Helper()
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	repository := fmt.Sprintf("fixture/repo-reclaim-%s-%d", name, time.Now().UnixNano())
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID

	// A short stop timeout, not production's workerStopTimeout: a
	// buildActivity that blocks until cancelled (the stopped-worker case)
	// holds t.Cleanup's w.Stop for the whole stop timeout, 30 s of pure
	// waiting per run at the production value.
	w := temporalworker.New(temporalClient, taskQueue, workflow.BoundedWorkerOptions(time.Second))
	w.RegisterWorkflow(workflow.RepositoryOwnerWorkflow)
	w.RegisterWorkflow(workflow.RunWorkflow)
	w.RegisterActivityWithOptions(
		func(context.Context, workflow.CaptureBaseSHAInput) (string, error) { return "fixture-base-sha", nil },
		activity.RegisterOptions{Name: workflow.CaptureBaseSHAActivityName},
	)
	w.RegisterActivityWithOptions(
		func(context.Context, workflow.PreflightInput) error { return nil },
		activity.RegisterOptions{Name: workflow.PreflightActivityName},
	)
	w.RegisterActivityWithOptions(
		func(context.Context, workflow.RunWorkflowInput) (workflow.BaselineVerifyResult, error) {
			return workflow.BaselineVerifyResult{Record: run.BaselineVerify{Passed: true}}, nil
		},
		activity.RegisterOptions{Name: workflow.RunBaselineVerifyActivityName},
	)
	w.RegisterActivityWithOptions(buildActivity, activity.RegisterOptions{Name: workflow.RunBuildActivityName})
	w.RegisterActivityWithOptions(
		func(context.Context, workflow.PrepareIsolatedWorkspaceInput) (workflow.PrepareIsolatedWorkspaceResult, error) {
			return workflow.PrepareIsolatedWorkspaceResult{WorktreePath: "/fixture/workspace", Branch: "factoryd/fixture"}, nil
		},
		activity.RegisterOptions{Name: workflow.PrepareIsolatedWorkspaceActivityName},
	)
	w.RegisterActivityWithOptions(
		func(context.Context, workflow.RollbackIsolatedWorkspaceInput) error { return nil },
		activity.RegisterOptions{Name: workflow.RollbackIsolatedWorkspaceActivityName},
	)
	w.RegisterActivityWithOptions(
		func(context.Context, workflow.DisableWorkerGroupWriteInput) error { return nil },
		activity.RegisterOptions{Name: workflow.DisableWorkerGroupWriteActivityName},
	)
	if err := w.Start(); err != nil {
		t.Fatalf("start in-process Worker: %v", err)
	}
	t.Cleanup(w.Stop)

	requestID := fmt.Sprintf("fixture-%s-%d", name, time.Now().UnixNano())
	runInput := workflow.RunWorkflowInput{
		Ticket:              "fixture-" + name + "-ticket",
		WorkspacePath:       "/fixture/workspace",
		SpecPath:            "/fixture/spec.md",
		BaseSHA:             "fixture-base-sha",
		SandboxImage:        fakeSandboxImage,
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
		IsolateWorkspace:    true,
		IsolatedRepoDir:     "/fixture/repo",
		IsolatedParentDir:   "/fixture/data/workspaces",
	}

	// Same seeded-run.json-then-never-touched-again shape as
	// TestIntegrationReconcileReclaimedRunRecoversCrashedSubmitter, standing
	// in for a submitter that crashed right after starting to wait.
	submitterDataDir := t.TempDir()
	seeded := &run.Run{
		ID:            requestID,
		Ticket:        runInput.Ticket,
		WorkspacePath: runInput.WorkspacePath,
		SpecPath:      runInput.SpecPath,
		Repository:    repository,
		State:         run.StateSliceRunning,
		BaseSHA:       runInput.BaseSHA,
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(submitterDataDir); err != nil {
		t.Fatalf("seed crashed-submitter run.json: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestID, Input: runInput},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel()
		t.Fatalf("signal repository owner workflow: %v", err)
	}
	cancel()

	until := time.Now().Add(deadline)
	var terminal bool
	for time.Now().Before(until) {
		terminal, err = reconcileReclaimedRun(context.Background(), temporalClient, ownerID, requestID, submitterDataDir)
		if err != nil {
			t.Fatalf("reconcileReclaimedRun: %v", err)
		}
		if terminal {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !terminal {
		t.Fatal("reconcileReclaimedRun never reported this request terminal within the deadline")
	}

	reconciled, err := run.Load(submitterDataDir, requestID)
	if err != nil {
		t.Fatalf("load reconciled run.json: %v", err)
	}
	if reconciled.State != run.StateHalted {
		t.Fatalf("state = %q, want %q", reconciled.State, run.StateHalted)
	}
	return reconciled
}

// TestIntegrationDaemonWiresSandboxDockerFlagIntoActivities is the
// regression for a real P1 finding from a second round of GitHub Codex
// review of PR #37: daemonMain's new -sandbox-docker flag was used for its
// own startup orphan reconciliation but never assigned to
// activities.SandboxDocker, so runSandboxWithRetries' divergence check
// (rejecting a sandboxed execution whose own SandboxDocker differs from
// this Worker's static value) was a silent no-op — a.SandboxDocker stayed
// "", the check's own `a.SandboxDocker != ""` guard never fired, and a
// sandboxed request would launch through whatever Docker executable it
// supplied instead of being rejected. This drives a real `factoryd daemon`
// subprocess (not the in-process Worker
// TestIntegrationReconcileReclaimedRunPreservesCleanupUnconfirmed uses) so
// the wiring under test is the actual CLI flag reaching the actual
// Activities struct that subprocess registers — signals a sandboxed
// RunWorkflowInput whose own SandboxDocker deliberately diverges from the
// daemon's -sandbox-docker flag, and asserts the run halts on the
// divergence error rather than ever attempting to launch a container.
func TestIntegrationDaemonWiresSandboxDockerFlagIntoActivities(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress calls t.Setenv, and Go's
	// testing package panics if a test using t.Setenv also calls
	// t.Parallel() (confirmed by running this file with t.Parallel() here:
	// "testing: test using t.Setenv ... can not use t.Parallel").
	address := isolatedTemporalAddress(t)
	// sandbox-docker is config-file only as of flags-consolidate
	// (2026-09-10; see sessionconfig.Settings) -- the daemon subprocess
	// below inherits this test process's HOME/XDG_CONFIG_HOME (both set by
	// isolateSessionConfig via t.Setenv, so os.Environ() below already
	// reflects them) and resolves sandbox_docker from the config file
	// written here, the same as a real operator's session config would.
	writeSessionConfig(t, isolateSessionConfig(t), "sandbox_docker: /fixture/daemon-configured-docker\n")

	daemonDataDir := t.TempDir()
	repository := fmt.Sprintf("fixture/repo-sandbox-docker-wiring-%d", time.Now().UnixNano())

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	requestID := fmt.Sprintf("fixture-sandbox-docker-wiring-%d", time.Now().UnixNano())
	logDir := filepath.Join(t.TempDir(), "logs")
	checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		t.Fatalf("create checkpoint dir: %v", err)
	}
	// CaptureBaseSHAActivity (the real production Activity, unmocked here —
	// this drives the daemon subprocess's own registered Activities, not a
	// test double) runs `git rev-parse HEAD` against WorkspacePath before
	// RunBuildActivity ever starts, so this needs a genuine git repo even
	// though the divergence check itself never touches the filesystem.
	ws := newFixtureRepo(t)
	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	runInput := workflow.RunWorkflowInput{
		Ticket:            "fixture-sandbox-docker-wiring-ticket",
		WorkspacePath:     ws,
		IsolateWorkspace:  true,
		IsolatedRepoDir:   ws,
		IsolatedParentDir: t.TempDir(),
		BaseSHA:           strings.TrimSpace(string(baseSHABytes)),
		LogDir:            logDir,
		CheckpointDir:     checkpointDir,
		// SandboxImage set, so RunBuildActivity takes the sandboxed path
		// (a.sandboxImageFor(input) != "") straight into
		// runSandboxWithRetries — the divergence check fires there before
		// any script staging or Docker invocation, so no real build
		// script or Docker binary is ever needed for this test.
		SandboxImage:        "factory-worker:test@sha256:deadbeef",
		SandboxDocker:       "/fixture/request-supplied-docker",
		TestsRequiredOptOut: "not exercising tests_added in this fixture",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestID, Input: runInput},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel()
		t.Fatalf("signal repository owner workflow: %v", err)
	}
	cancel()

	// queryOwnerForRequestResult, not reconcileReclaimedRun: this test only
	// needs the owner's own RunWorkflowResult (whose Err string carries the
	// divergence message) — reconcileReclaimedRun's own job is recovering a
	// crashed submitter's run.json, a different concern, and its own
	// result.Err != "" branch deliberately never persists that string onto
	// run.Run.
	deadline := time.Now().Add(30 * time.Second)
	var result workflow.RunWorkflowResult
	var done bool
	for time.Now().Before(deadline) {
		queryCtx, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
		result, done, err = queryOwnerForRequestResult(queryCtx, temporalClient, ownerID, requestID)
		cancelQuery()
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				// A busy Temporal server can let the child start immediately
				// after this query's short context expires. The outer deadline
				// is the test's actual bound, so retry this transient query
				// timeout instead of misclassifying it as a failed run.
				time.Sleep(200 * time.Millisecond)
				continue
			}
			t.Fatalf("queryOwnerForRequestResult: %v (daemon output so far:\n%s)", err, daemonOutput.String())
		}
		if done {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !done {
		t.Fatalf("request never reached a terminal owner result within the deadline (daemon output:\n%s)", daemonOutput.String())
	}
	if result.State != run.StateHalted {
		t.Fatalf("state = %q, want %q (daemon output:\n%s)", result.State, run.StateHalted, daemonOutput.String())
	}
	if !strings.Contains(result.Err, "diverges from this Worker's own -sandbox-docker") {
		t.Fatalf("result.Err = %q, want it to name the sandbox-docker divergence — proving daemonMain actually wired -sandbox-docker into activities.SandboxDocker (daemon output:\n%s)", result.Err, daemonOutput.String())
	}
}

// TestSignalDeliveredToOwner proves the core mechanism the plan's second
// previously-documented-open Phase 6 reconciliation gap needed: a direct,
// authoritative check of whether a SubmitRunSignal actually reached a
// repository owner, read from the owner's own real Workflow Execution
// History rather than its in-memory Runs/InProgress state — the only
// source that can distinguish "never delivered" from "durably delivered
// but not yet drained" while the owner is fully blocked awaiting a
// current child's completion (see signalDeliveredToOwner's own doc
// comment for why that distinction can't be made any other way).
func TestSignalDeliveredToOwner(t *testing.T) {
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	repository := fmt.Sprintf("fixture/repo-signal-delivered-%d", time.Now().UnixNano())
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	deliveredID := fmt.Sprintf("fixture-delivered-%d", time.Now().UnixNano())
	neverSentID := fmt.Sprintf("fixture-never-sent-%d", time.Now().UnixNano())

	// No Worker is ever started for this owner in this test — the signal
	// is delivered and durably recorded in history, but never drained or
	// acted on by any workflow code, isolating this test to exactly the
	// history-read mechanism itself, independent of RepositoryOwnerWorkflow's
	// own processing.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: deliveredID, Input: workflow.RunWorkflowInput{Ticket: "fixture"}},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	)
	cancel()
	if err != nil {
		t.Fatalf("signal repository owner workflow: %v", err)
	}
	// Termination isn't necessarily instant to observe as history via a
	// second, independent client with no worker of its own — poll rather
	// than assume the signal event is already visible on the first read.
	deadline := time.Now().Add(10 * time.Second)
	var delivered bool
	for time.Now().Before(deadline) {
		queryCtx, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
		delivered, err = signalDeliveredToOwner(queryCtx, temporalClient, ownerID, deliveredID)
		cancelQuery()
		if err != nil {
			t.Fatalf("signalDeliveredToOwner: %v", err)
		}
		if delivered {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !delivered {
		t.Fatal("signalDeliveredToOwner = false, want true for a signal actually delivered to this owner")
	}

	queryCtx, cancelQuery := context.WithTimeout(context.Background(), 5*time.Second)
	neverDelivered, err := signalDeliveredToOwner(queryCtx, temporalClient, ownerID, neverSentID)
	cancelQuery()
	if err != nil {
		t.Fatalf("signalDeliveredToOwner: %v", err)
	}
	if neverDelivered {
		t.Fatal("signalDeliveredToOwner = true, want false for a request ID that was never signaled to this owner")
	}
}

// TestSignalDeliveredToOwnerReturnsFalseForNeverStartedOwner is the
// regression test for a real finding from codex review: GetWorkflowHistory
// against an owner Workflow ID that has never been started at all (a
// brand-new repository, no signal ever sent) fails with a NotFound error
// on its very first Next() call, not an empty iterator — an earlier
// version of signalDeliveredToOwner propagated that as a generic query
// failure, which reconcileReclaimedRun then treated as merely
// inconclusive ("leave it for a later scan") rather than the definitive
// "never delivered" answer it actually is: no owner execution exists at
// all to have ever received this signal. A genuinely never-submitted
// request against a repository whose owner has never run would then be
// retained forever, exactly the bug this whole fix exists to close.
func TestSignalDeliveredToOwnerReturnsFalseForNeverStartedOwner(t *testing.T) {
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	// Deliberately never signaled or started — this owner Workflow ID
	// has never existed under any run ID.
	repository := fmt.Sprintf("fixture/repo-owner-never-started-%d", time.Now().UnixNano())
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	requestID := fmt.Sprintf("fixture-request-%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	delivered, err := signalDeliveredToOwner(ctx, temporalClient, ownerID, requestID)
	cancel()
	// The distinguishable errOwnerNeverStarted sentinel, not a plain
	// (false, nil) — see its own doc comment for why callers must be
	// able to tell "never started at all" apart from "exists, scanned
	// clean" (only the latter still needs terminateOrCancelOwnRequest's
	// own CancelRunSignal safety net).
	if !errors.Is(err, errOwnerNeverStarted) {
		t.Fatalf("signalDeliveredToOwner error = %v, want errOwnerNeverStarted for an owner that has never run", err)
	}
	if delivered {
		t.Fatal("signalDeliveredToOwner = true, want false: this owner Workflow ID has never been started")
	}
}

// TestIntegrationReconcileReclaimedRunCancelsLateArrivingNeverStartedSignal
// closes the plan's second previously-documented-open Phase 6
// reconciliation gap (a request whose SignalWithStartWorkflow attempt
// failed ambiguously, submitted by a process that then crashed before it
// could resolve that ambiguity itself, used to retain its recovery
// Worker for the daemon's entire remaining lifetime) for the specific
// case where the owner Workflow has never been started at all — and is
// the critical safety property behind that case's own errOwnerNeverStarted
// branch, closing a real P1 GitHub Codex review finding on an earlier
// version of this fix: confirming "the owner has never been started"
// from a single snapshot is not actually race-free — the original
// SignalWithStartWorkflow RPC could still be completing server-side
// (creating the owner) at the exact instant that check runs, moments
// before this process retires its recovery Worker. reconcileReclaimedRun
// must not just resolve this case; the resulting confirmed halt must
// also be SAFE even if the original signal turns out to have been merely
// delayed, not actually lost — proven here by sending that exact
// "delayed" SubmitRunSignal only AFTER reconciliation has already
// confirmed the halt, then proving a real Worker services the owner but
// the request is dropped as canceled, never actually dispatched or
// executed.
func TestIntegrationReconcileReclaimedRunCancelsLateArrivingNeverStartedSignal(t *testing.T) {
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	daemonDataDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-late-never-started-%d", time.Now().UnixNano())

	// A real daemon — with a real Worker — so a late-arriving submit
	// signal, if NOT correctly dropped, would actually run to completion
	// (via the fixture's own "commit" mode, a fast real success) rather
	// than this test only ever proving the server accepted an RPC call.
	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=commit",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	requestID := fmt.Sprintf("fixture-late-signal-%d", time.Now().UnixNano())

	// Deliberately never signaled yet — the owner genuinely does not
	// exist at reconciliation time, exercising the exact
	// errOwnerNeverStarted branch this test is proving safe.
	submitterDataDir := t.TempDir()
	seeded := &run.Run{
		ID:         requestID,
		Ticket:     "fixture-late-signal-ticket",
		Repository: repository,
		State:      run.StateSliceRunning,
		CreatedAt:  time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(submitterDataDir); err != nil {
		t.Fatalf("seed run.json: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	var terminal bool
	for time.Now().Before(deadline) {
		terminal, err = reconcileReclaimedRun(context.Background(), temporalClient, ownerID, requestID, submitterDataDir)
		if err != nil {
			t.Fatalf("reconcileReclaimedRun: %v", err)
		}
		if terminal {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !terminal {
		t.Fatal("reconcileReclaimedRun never confirmed this never-submitted request")
	}
	reconciled, err := run.Load(submitterDataDir, requestID)
	if err != nil {
		t.Fatalf("load reconciled run.json: %v", err)
	}
	if reconciled.State != run.StateHalted || !reconciled.HaltConfirmed {
		t.Fatalf("state=%q haltConfirmed=%v, want state=%q haltConfirmed=true", reconciled.State, reconciled.HaltConfirmed, run.StateHalted)
	}

	// Now simulate the original SignalWithStartWorkflow having merely
	// been delayed, not actually lost — sent only now, after this
	// process already confirmed and durably recorded the halt.
	logDir := filepath.Join(t.TempDir(), "logs")
	checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("create log dir: %v", err)
	}
	if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
		t.Fatalf("create checkpoint dir: %v", err)
	}
	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	taskQueue := "factoryd-repo-" + ownerID
	lateCtx, cancelLate := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = temporalClient.SignalWithStartWorkflow(lateCtx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestID, Input: workflow.RunWorkflowInput{
			Ticket:              "fixture-late-signal-ticket",
			WorkspacePath:       ws,
			IsolateWorkspace:    true,
			IsolatedRepoDir:     ws,
			IsolatedParentDir:   t.TempDir(),
			SpecPath:            specPath,
			BaseSHA:             strings.TrimSpace(string(baseSHABytes)),
			LogDir:              logDir,
			CheckpointDir:       checkpointDir,
			SandboxImage:        fakeSandboxImage,
			BuildAppInterpreter: "/bin/sh",
			BuildAppScript:      scriptPath,
			MaxRounds:           3,
			TimeoutMinutes:      45,
			BuildMaxAttempts:    1,
			VerifyCommand:       "true",
			VerifyMaxAttempts:   1,
			TestsRequiredOptOut: "not exercising tests_added in this fixture",
		}},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	)
	cancelLate()
	if err != nil {
		t.Fatalf("signal repository owner workflow (late-arriving original request): %v", err)
	}

	// The real Worker (started by the daemon above) must actually drain
	// and process this — poll for the owner's own durable Runs entry
	// rather than assume any particular timing.
	resultDeadline := time.Now().Add(20 * time.Second)
	var found bool
	var lateResult workflow.RunWorkflowResult
	for time.Now().Before(resultDeadline) {
		queryResp, err := temporalClient.QueryWorkflow(context.Background(), ownerID, "", workflow.RepositoryOwnerQueryName)
		if err == nil {
			var ownerResult workflow.RepositoryOwnerResult
			if err := queryResp.Get(&ownerResult); err == nil {
				if r, ok := ownerResult.Runs[requestID]; ok {
					lateResult = r
					found = true
					break
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !found {
		t.Fatalf("owner never recorded a result for the late-arriving request %s (daemon output:\n%s)", requestID, daemonOutput.String())
	}
	if lateResult.Err != "repository owner request canceled before execution" {
		t.Fatalf("late-arriving request result = %+v, want a canceled-before-execution halt — it ran instead of being dropped, meaning a request this process already told a caller was durably halted could still execute for real (daemon output:\n%s)", lateResult, daemonOutput.String())
	}

	// And run.json itself must still reflect the original confirmed
	// halt, not have been silently overwritten by the late arrival.
	stillReconciled, err := run.Load(submitterDataDir, requestID)
	if err != nil {
		t.Fatalf("load run.json: %v", err)
	}
	if stillReconciled.State != run.StateHalted || !stillReconciled.HaltConfirmed {
		t.Fatalf("run.json state=%q haltConfirmed=%v after the late arrival, want it to remain state=%q haltConfirmed=true", stillReconciled.State, stillReconciled.HaltConfirmed, run.StateHalted)
	}
}

// TestIntegrationReconcileReclaimedRunDoesNotFalselyConfirmQueuedBehindLongRunningWork
// is the critical safety property TestIntegrationReconcileReclaimedRunConfirmsNeverSubmitted's
// own fix could otherwise get backwards, the same way the plan's own
// history describes an earlier timeout-based give-up attempt being
// reverted for exactly this failure mode: a request that WAS durably
// delivered to the owner, but is sitting genuinely queued behind a
// current long-running child (the owner is fully blocked inside
// childFuture.Get for that child's entire execution, never selecting on
// its signal channel at all during that time — see
// signalDeliveredToOwner's own doc comment), must never be misreported as
// "never submitted" just because reconcileReclaimedRun can't yet find it
// in Runs/InProgress either.
//
// Reproduces exactly that: a real daemon, a first request that hangs
// (occupying the owner's only child slot), and a second request signaled
// directly while the first is still running — proven still queued, not
// dispatched, before reconcileReclaimedRun is ever called on it, matching
// TestIntegrationTerminateOrCancelOwnRequestCancelsQueuedRequest's own
// proven pattern for reaching this state. Polls reconcileReclaimedRun
// repeatedly across a real window while the first request keeps hanging:
// terminal must stay false throughout. Only after that is proven does
// this test unblock the owner (terminating the hung first child) and
// confirm the second request is later reconciled normally — proving this
// isn't just "never resolves at all" but genuinely "correctly waits, then
// resolves once there's a real answer".
func TestIntegrationReconcileReclaimedRunDoesNotFalselyConfirmQueuedBehindLongRunningWork(t *testing.T) {
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	daemonDataDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-queued-not-falsely-confirmed-%d", time.Now().UnixNano())

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(),
		"FAKE_BUILD_APP_MODE=hang",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput bytes.Buffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	runningRequestID := fmt.Sprintf("fixture-hung-run-%d", time.Now().UnixNano())
	queuedRequestID := fmt.Sprintf("fixture-queued-run-%d", time.Now().UnixNano())

	newRunInput := func(ticket string) workflow.RunWorkflowInput {
		logDir := filepath.Join(t.TempDir(), "logs")
		checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
		if err := os.MkdirAll(logDir, 0o750); err != nil {
			t.Fatalf("create log dir: %v", err)
		}
		if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
			t.Fatalf("create checkpoint dir: %v", err)
		}
		return workflow.RunWorkflowInput{
			Ticket:              ticket,
			WorkspacePath:       ws,
			IsolateWorkspace:    true,
			IsolatedRepoDir:     ws,
			IsolatedParentDir:   t.TempDir(),
			SpecPath:            specPath,
			BaseSHA:             baseSHA,
			LogDir:              logDir,
			CheckpointDir:       checkpointDir,
			SandboxImage:        fakeSandboxImage,
			BuildAppInterpreter: "/bin/sh",
			BuildAppScript:      scriptPath,
			MaxRounds:           3,
			TimeoutMinutes:      45,
			BuildMaxAttempts:    1,
			VerifyCommand:       "true",
			VerifyMaxAttempts:   1,
			TestsRequiredOptOut: "not exercising tests_added in this fixture",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: runningRequestID, Input: newRunInput("fixture-hung-ticket")},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel()
		t.Fatalf("signal repository owner workflow (hung run): %v", err)
	}
	cancel()

	// Wait for the owner to actually start the hung request before
	// queuing the second one behind it.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		queryResp, err := temporalClient.QueryWorkflow(context.Background(), ownerID, "", workflow.RepositoryOwnerQueryName)
		if err == nil {
			var ownerResult workflow.RepositoryOwnerResult
			if err := queryResp.Get(&ownerResult); err == nil && ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == runningRequestID {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(ctx2, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: queuedRequestID, Input: newRunInput("fixture-queued-ticket")},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancel2()
		t.Fatalf("signal repository owner workflow (queued run): %v", err)
	}
	cancel2()

	// The crashed-submitter run.json a real submitter would have written
	// right before it started waiting.
	submitterDataDir := t.TempDir()
	seeded := &run.Run{
		ID:         queuedRequestID,
		Ticket:     "fixture-queued-ticket",
		Repository: repository,
		State:      run.StateSliceRunning,
		CreatedAt:  time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(submitterDataDir); err != nil {
		t.Fatalf("seed queued run.json: %v", err)
	}

	// The core safety property: while the owner is still blocked on the
	// hung first request, every reconcileReclaimedRun call for the queued
	// second request must report NOT terminal — signalDeliveredToOwner
	// found its signal durably delivered in the owner's own history, so
	// this must never fall through to a confirmed halt.
	safetyDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(safetyDeadline) {
		terminal, err := reconcileReclaimedRun(context.Background(), temporalClient, ownerID, queuedRequestID, submitterDataDir)
		if err != nil {
			t.Fatalf("reconcileReclaimedRun: %v", err)
		}
		if terminal {
			t.Fatalf("reconcileReclaimedRun reported the queued request terminal while the owner is still blocked on the hung first request — it was durably delivered, not never-submitted, and must not be confirmed halted")
		}
		reconciled, err := run.Load(submitterDataDir, queuedRequestID)
		if err != nil {
			t.Fatalf("load run.json: %v", err)
		}
		if reconciled.State != run.StateSliceRunning {
			t.Fatalf("run.json state = %q, want unchanged %q — the queued request must not have been touched while genuinely still queued", reconciled.State, run.StateSliceRunning)
		}
	}

	// Unblock the owner past the hung first request — once dispatched,
	// the second request hangs too (this daemon's env is FAKE_BUILD_APP_MODE=hang
	// unconditionally, same as the first), so it deliberately does not
	// naturally resolve to a normal terminal state here; that path is
	// already covered by TestIntegrationReconcileReclaimedRunRecoversCrashedSubmitter.
	// The remaining assertion this section proves: once the owner
	// actually starts the second request as its own new InProgress child,
	// reconcileReclaimedRun still correctly reports it non-terminal for
	// the right reason (genuinely running now, confirmed via
	// queryOwnerForRequestResult) rather than ever having concluded
	// "never submitted" — the failure mode this whole test exists to
	// rule out.
	var childID, childRunID string
	iter := temporalClient.GetWorkflowHistory(context.Background(), ownerID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		if err != nil {
			t.Fatalf("read owner workflow history: %v", err)
		}
		if attrs := event.GetChildWorkflowExecutionStartedEventAttributes(); attrs != nil {
			childID = attrs.WorkflowExecution.WorkflowId
			childRunID = attrs.WorkflowExecution.RunId
		}
	}
	if childID == "" {
		t.Fatal("owner workflow history has no ChildWorkflowExecutionStarted event for the hung request")
	}
	termCtx, cancelTerm := context.WithTimeout(context.Background(), 5*time.Second)
	if err := temporalClient.TerminateWorkflow(termCtx, childID, childRunID, "test cleanup: unblock hung child so the owner can start the queued request"); err != nil {
		t.Fatalf("terminate hung child: %v", err)
	}
	cancelTerm()

	dispatchDeadline := time.Now().Add(15 * time.Second)
	var dispatched bool
	for time.Now().Before(dispatchDeadline) {
		queryResp, err := temporalClient.QueryWorkflow(context.Background(), ownerID, "", workflow.RepositoryOwnerQueryName)
		if err == nil {
			var ownerResult workflow.RepositoryOwnerResult
			if err := queryResp.Get(&ownerResult); err == nil && ownerResult.InProgress != nil && ownerResult.InProgress.RequestID == queuedRequestID {
				dispatched = true
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !dispatched {
		t.Fatal("owner never started the previously-queued request after the first was unblocked")
	}
	if terminal, err := reconcileReclaimedRun(context.Background(), temporalClient, ownerID, queuedRequestID, submitterDataDir); err != nil {
		t.Fatalf("reconcileReclaimedRun: %v", err)
	} else if terminal {
		t.Fatal("reconcileReclaimedRun reported the now-InProgress request terminal — it is genuinely running, not never-submitted")
	}

	// Test cleanup: terminate the second child too so the daemon (and its
	// owner Workflow) can shut down without leaving a hung subprocess
	// behind for this test's own -repository namespace.
	var secondChildID, secondChildRunID string
	iter2 := temporalClient.GetWorkflowHistory(context.Background(), ownerID, "", false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter2.HasNext() {
		event, err := iter2.Next()
		if err != nil {
			t.Fatalf("read owner workflow history: %v", err)
		}
		if attrs := event.GetChildWorkflowExecutionStartedEventAttributes(); attrs != nil && attrs.WorkflowExecution.WorkflowId != childID {
			secondChildID = attrs.WorkflowExecution.WorkflowId
			secondChildRunID = attrs.WorkflowExecution.RunId
		}
	}
	if secondChildID != "" {
		termCtx2, cancelTerm2 := context.WithTimeout(context.Background(), 5*time.Second)
		_ = temporalClient.TerminateWorkflow(termCtx2, secondChildID, secondChildRunID, "test cleanup")
		cancelTerm2()
	}
}

// TestIntegrationReconcileReclaimedRunSurvivesOwnerIdleRestart is the
// regression test for a real P2 finding from codex review of PR #17:
// reconcileReclaimedRun's own owner query only ever asks whichever
// RepositoryOwnerWorkflow execution is current right now (QueryWorkflow
// with an empty RunID). RepositoryOwnerResult.Runs is not durable across
// an ordinary idle-completion — only across Continue-As-New (see
// RepositoryOwnerWorkflow's own idle-completion comment) — so once the
// execution that actually ran a crashed submitter's request idle-completes
// and a later, unrelated request starts a brand-new execution under the
// same repository, that fresh execution's own Runs map has never heard of
// the earlier request. Before the fix, reconcileReclaimedRun could
// therefore never recover it: every query answered "no record", forever.
//
// This reproduces exactly that sequence — request A's owner execution is
// given a short IdleTimeout and left to idle-complete on its own, then an
// unrelated request B starts a fresh execution — and proves
// reconcileReclaimedRun still recovers request A, via RunWorkflow's own
// deterministic, repository-namespaced WorkflowID (see
// RepositoryOwnerRunWorkflowID's doc comment), independent of either owner
// execution.
func TestIntegrationReconcileReclaimedRunSurvivesOwnerIdleRestart(t *testing.T) {
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	ws := newFixtureRepo(t)
	daemonDataDir := t.TempDir()
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	repository := fmt.Sprintf("fixture/repo-idle-restart-%d", time.Now().UnixNano())

	daemonCmd := factorydCommand(t, "daemon",
		"-temporal-address", address,
		"-repository", repository,
		"-data-dir", daemonDataDir,
	)
	daemonCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	// -sandbox-docker/-sandbox-image are session-config only on `factoryd
	// daemon` (flags-consolidate, 2026-09-10); sandboxing is unconditional,
	// so this daemon needs the same fake docker every other sandboxed test
	// in this package uses, configured the only way this subcommand
	// accepts it.
	daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
	var daemonOutput synchronizedBuffer
	daemonCmd.Stdout = &daemonOutput
	daemonCmd.Stderr = &daemonOutput
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", daemonOutput.String())
	})

	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID

	newRunInput := func(ticket string) workflow.RunWorkflowInput {
		logDir := filepath.Join(t.TempDir(), "logs")
		checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
		if err := os.MkdirAll(logDir, 0o750); err != nil {
			t.Fatalf("create log dir: %v", err)
		}
		if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
			t.Fatalf("create checkpoint dir: %v", err)
		}
		return workflow.RunWorkflowInput{
			Ticket:              ticket,
			WorkspacePath:       ws,
			IsolateWorkspace:    true,
			IsolatedRepoDir:     ws,
			IsolatedParentDir:   t.TempDir(),
			SpecPath:            specPath,
			BaseSHA:             baseSHA,
			LogDir:              logDir,
			CheckpointDir:       checkpointDir,
			SandboxImage:        fakeSandboxImage,
			BuildAppInterpreter: "/bin/sh",
			BuildAppScript:      scriptPath,
			MaxRounds:           3,
			TimeoutMinutes:      45,
			BuildMaxAttempts:    1,
			VerifyCommand:       "true",
			VerifyMaxAttempts:   1,
			TestsRequiredOptOut: "not exercising tests_added in this fixture",
		}
	}

	requestIDA := fmt.Sprintf("fixture-idle-restart-a-%d", time.Now().UnixNano())
	// The submitter's own dataDir, seeded with the "initial durable record
	// exists" run.json a real submitter writes right before it starts
	// waiting — same crash simulation as
	// TestIntegrationReconcileReclaimedRunRecoversCrashedSubmitter, never
	// touched again by anything this test controls from here on.
	submitterDataDir := t.TempDir()
	seeded := &run.Run{
		ID:            requestIDA,
		Ticket:        "fixture-idle-restart-ticket-a",
		WorkspacePath: ws,
		SpecPath:      specPath,
		Repository:    repository,
		State:         run.StateSliceRunning,
		BaseSHA:       baseSHA,
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(submitterDataDir); err != nil {
		t.Fatalf("seed crashed-submitter run.json: %v", err)
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(startCtx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestIDA, Input: newRunInput("fixture-idle-restart-ticket-a")},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow,
		// A short IdleTimeout, not the multi-hour default, is the whole
		// point: request A's own child must complete and then this
		// execution must genuinely idle-complete (return, not
		// Continue-As-New) — losing result.Runs — well within this test's
		// own patience.
		workflow.RepositoryOwnerWorkflowInput{Repository: repository, IdleTimeout: 2 * time.Second},
	); err != nil {
		cancelStart()
		t.Fatalf("signal repository owner workflow (request A): %v", err)
	}
	cancelStart()

	// Poll until this owner execution itself has closed — not just until
	// request A's child finishes — proving it genuinely idle-completed.
	firstRunID := ""
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		descCtx, cancelDesc := context.WithTimeout(context.Background(), 3*time.Second)
		desc, descErr := temporalClient.DescribeWorkflowExecution(descCtx, ownerID, "")
		cancelDesc()
		if descErr == nil {
			firstRunID = desc.WorkflowExecutionInfo.GetExecution().GetRunId()
			if desc.WorkflowExecutionInfo.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if firstRunID == "" {
		t.Fatalf("never observed the first repository owner execution (daemon output:\n%s)", daemonOutput.String())
	}

	// An unrelated request B, started only after the first execution
	// closed, forces exactly the "fresh execution under the same
	// Workflow ID" scenario the review finding described — its own
	// SignalWithStartWorkflow starts a brand-new execution (a different
	// RunId under the same ownerID) with an empty Runs map.
	requestIDB := fmt.Sprintf("fixture-idle-restart-b-%d", time.Now().UnixNano())
	startCtxB, cancelStartB := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := temporalClient.SignalWithStartWorkflow(startCtxB, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: requestIDB, Input: newRunInput("fixture-idle-restart-ticket-b")},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository},
	); err != nil {
		cancelStartB()
		t.Fatalf("signal repository owner workflow (request B): %v", err)
	}
	cancelStartB()

	deadline = time.Now().Add(30 * time.Second)
	freshExecutionConfirmed := false
	for time.Now().Before(deadline) {
		descCtx, cancelDesc := context.WithTimeout(context.Background(), 3*time.Second)
		desc, descErr := temporalClient.DescribeWorkflowExecution(descCtx, ownerID, "")
		cancelDesc()
		if descErr == nil && desc.WorkflowExecutionInfo.GetExecution().GetRunId() != firstRunID {
			freshExecutionConfirmed = true
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !freshExecutionConfirmed {
		t.Fatalf("request B never started a fresh repository owner execution distinct from the first (daemon output:\n%s)", daemonOutput.String())
	}

	// reconcileReclaimedRun now only ever has the fresh execution (request
	// B's own) to query for request A — it has no record of it — so this
	// only succeeds via the deterministic-child-WorkflowID fallback.
	deadline = time.Now().Add(30 * time.Second)
	var terminal bool
	for time.Now().Before(deadline) {
		terminal, err = reconcileReclaimedRun(context.Background(), temporalClient, ownerID, requestIDA, submitterDataDir)
		if err != nil {
			t.Fatalf("reconcileReclaimedRun: %v (daemon output so far:\n%s)", err, daemonOutput.String())
		}
		if terminal {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !terminal {
		t.Fatalf("reconcileReclaimedRun never reported request A terminal after its owner execution idle-restarted (daemon output:\n%s)", daemonOutput.String())
	}

	reconciled, err := run.Load(submitterDataDir, requestIDA)
	if err != nil {
		t.Fatalf("load reconciled run.json: %v", err)
	}
	if reconciled.State != run.StateAccepted {
		t.Fatalf("state = %q, want %q — reconcileReclaimedRun must recover request A via its own child workflow even after its owner execution idle-restarted, not leave it at %q", reconciled.State, run.StateAccepted, run.StateSliceRunning)
	}
}

// TestIntegrationReconcileReclaimedRunNamespacesChildByRepository is the
// regression test for a real P1 finding from codex review of PR #18: two
// different repository owners that happen to receive the same request ID
// (POST /runs's StartRequest.ID is caller-suppliable and never validated
// for uniqueness across repositories) used to assign their children the
// identical namespace-wide Temporal WorkflowID — an active child under one
// repository could block the other repository's own child from ever
// starting, and reconcileReclaimedRun's own direct-child lookup could
// fetch (and durably persist) the wrong repository's result entirely.
//
// This submits the SAME request ID to two independent repository owners,
// each with its own daemon, workspace, and ticket, and proves both
// children run to completion without colliding and that
// reconcileReclaimedRun, called separately for each repository's own
// dataDir, recovers each repository's own distinct result — not the
// other's.
func TestIntegrationReconcileReclaimedRunNamespacesChildByRepository(t *testing.T) {
	// not parallel-safe: relies on the run-wide test Temporal server
	// (sharedTemporalAddress) rather than a dedicated per-test
	// instance, and exercises timing-sensitive reclaim/reconciliation
	// behavior that a busy shared server can perturb.
	address := sharedTemporalAddress(t)

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("abs script path: %v", err)
	}
	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()

	// The same request ID, deliberately, submitted to both repositories
	// below — exactly the collision scenario the finding describes.
	requestID := fmt.Sprintf("fixture-collision-%d", time.Now().UnixNano())

	type repoFixture struct {
		label         string
		repository    string
		ws            string
		ownerID       string
		daemonDataDir string
		daemonCmd     *exec.Cmd
		daemonOutput  *synchronizedBuffer
		submitterDir  string
	}
	fixtures := make([]*repoFixture, 2)
	for i, label := range []string{"a", "b"} {
		ws := newFixtureRepo(t)
		// A distinguishing commit, unique per fixture: newFixtureRepo's
		// own initial commit is byte-identical (same content, same
		// author/message, second-granularity timestamp) across both
		// fixtures created moments apart in this same test, which would
		// otherwise give them identical git object hashes throughout —
		// including the eventual ResultSHA this test relies on to prove
		// no cross-repository mixup below.
		if err := os.WriteFile(filepath.Join(ws, "fixture-label.txt"), []byte(label+"\n"), 0o644); err != nil {
			t.Fatalf("write fixture-label.txt (%s): %v", label, err)
		}
		if out, err := exec.Command("git", "-C", ws, "add", "-A").CombinedOutput(); err != nil {
			t.Fatalf("git add fixture-label.txt (%s): %v: %s", label, err, out)
		}
		if out, err := exec.Command("git", "-C", ws, "commit", "-q", "-m", "fixture label "+label).CombinedOutput(); err != nil {
			t.Fatalf("git commit fixture-label.txt (%s): %v: %s", label, err, out)
		}
		repository := fmt.Sprintf("fixture/repo-collision-%s-%d", label, time.Now().UnixNano())
		daemonDataDir := t.TempDir()
		daemonCmd := factorydCommand(t, "daemon",
			"-temporal-address", address,
			"-repository", repository,
			"-data-dir", daemonDataDir,
		)
		daemonCmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		// -sandbox-docker/-sandbox-image are session-config only on
		// `factoryd daemon` (flags-consolidate, 2026-09-10); sandboxing is
		// unconditional, so this daemon needs the same fake docker every
		// other sandboxed test in this package uses, configured the only
		// way this subcommand accepts it.
		daemonCmd.Env = append(daemonCmd.Env, isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")...)
		var daemonOutput synchronizedBuffer
		daemonCmd.Stdout = &daemonOutput
		daemonCmd.Stderr = &daemonOutput
		if err := daemonCmd.Start(); err != nil {
			t.Fatalf("start factoryd daemon %s: %v", label, err)
		}
		fixtures[i] = &repoFixture{
			label:         label,
			repository:    repository,
			ws:            ws,
			ownerID:       workflow.RepositoryOwnerWorkflowID(repository),
			daemonDataDir: daemonDataDir,
			daemonCmd:     daemonCmd,
			daemonOutput:  &daemonOutput,
			submitterDir:  t.TempDir(),
		}
	}
	t.Cleanup(func() {
		for _, f := range fixtures {
			_ = f.daemonCmd.Process.Signal(syscall.SIGTERM)
			_ = f.daemonCmd.Wait()
			t.Logf("daemon %s output:\n%s", f.label, f.daemonOutput.String())
		}
	})
	for _, f := range fixtures {
		terminateRepositoryOwnerAtCleanup(t, address, f.ownerID)
	}

	for _, f := range fixtures {
		baseSHABytes, err := exec.Command("git", "-C", f.ws, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatalf("git rev-parse HEAD (%s): %v", f.label, err)
		}
		baseSHA := strings.TrimSpace(string(baseSHABytes))
		specPath := filepath.Join(t.TempDir(), "spec.md")
		if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_added\n"), 0o644); err != nil {
			t.Fatalf("write spec (%s): %v", f.label, err)
		}
		logDir := filepath.Join(t.TempDir(), "logs")
		checkpointDir := filepath.Join(t.TempDir(), "checkpoints")
		if err := os.MkdirAll(logDir, 0o750); err != nil {
			t.Fatalf("create log dir (%s): %v", f.label, err)
		}
		if err := os.MkdirAll(checkpointDir, 0o750); err != nil {
			t.Fatalf("create checkpoint dir (%s): %v", f.label, err)
		}
		runInput := workflow.RunWorkflowInput{
			Ticket:              "fixture-collision-ticket-" + f.label,
			WorkspacePath:       f.ws,
			IsolateWorkspace:    true,
			IsolatedRepoDir:     f.ws,
			IsolatedParentDir:   t.TempDir(),
			SpecPath:            specPath,
			BaseSHA:             baseSHA,
			LogDir:              logDir,
			CheckpointDir:       checkpointDir,
			SandboxImage:        fakeSandboxImage,
			BuildAppInterpreter: "/bin/sh",
			BuildAppScript:      scriptPath,
			MaxRounds:           3,
			TimeoutMinutes:      45,
			BuildMaxAttempts:    1,
			VerifyCommand:       "true",
			VerifyMaxAttempts:   1,
			TestsRequiredOptOut: "not exercising tests_added in this fixture",
		}

		seeded := &run.Run{
			ID:            requestID,
			Ticket:        runInput.Ticket,
			WorkspacePath: f.ws,
			SpecPath:      specPath,
			Repository:    f.repository,
			State:         run.StateSliceRunning,
			BaseSHA:       baseSHA,
			CreatedAt:     time.Now().Format(time.RFC3339Nano),
		}
		if err := seeded.Save(f.submitterDir); err != nil {
			t.Fatalf("seed crashed-submitter run.json (%s): %v", f.label, err)
		}

		taskQueue := "factoryd-repo-" + f.ownerID
		startCtx, cancelStart := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = temporalClient.SignalWithStartWorkflow(startCtx, f.ownerID, workflow.SubmitRunSignalName,
			workflow.SubmitRunSignal{RequestID: requestID, Input: runInput},
			temporalclient.StartWorkflowOptions{ID: f.ownerID, TaskQueue: taskQueue},
			workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: f.repository},
		)
		cancelStart()
		if err != nil {
			t.Fatalf("signal repository owner workflow (%s): %v", f.label, err)
		}
	}

	// Both children — same requestID, different repository-namespaced
	// WorkflowIDs — must reach a terminal Temporal status independently.
	// Before the fix, this loop would hang for the second fixture: an
	// active child under the identical namespace-wide WorkflowID from the
	// first would keep the second from ever starting at all.
	for _, f := range fixtures {
		childWorkflowID := workflow.RepositoryOwnerRunWorkflowID(f.ownerID, requestID)
		deadline := time.Now().Add(30 * time.Second)
		closed := false
		for time.Now().Before(deadline) {
			descCtx, cancelDesc := context.WithTimeout(context.Background(), 3*time.Second)
			desc, descErr := temporalClient.DescribeWorkflowExecution(descCtx, childWorkflowID, "")
			cancelDesc()
			if descErr == nil && desc.WorkflowExecutionInfo.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
				closed = true
				break
			}
			time.Sleep(300 * time.Millisecond)
		}
		if !closed {
			t.Fatalf("child workflow %s (repository %s) never reached a terminal status — collision with the other fixture's child? (daemon output:\n%s)", childWorkflowID, f.repository, f.daemonOutput.String())
		}
	}

	// reconcileReclaimedRun, called separately per repository (its own
	// ownerID and its own submitterDir), must recover each repository's
	// own distinct result — never the other's. ResultSHA is real,
	// per-workspace evidence CollectEvidenceActivity/PostBuildActivity
	// captured from that specific fixture's own git history — unlike
	// WorkspacePath (which reconciliation never overwrites; it would
	// round-trip correctly even if the wrong repository's result had been
	// applied on top of it), a cross-repository mixup would show up here
	// as a ResultSHA that does not exist in this fixture's own workspace.
	resultSHAs := make(map[string]string, len(fixtures))
	for _, f := range fixtures {
		deadline := time.Now().Add(30 * time.Second)
		var terminal bool
		var reconcileErr error
		for time.Now().Before(deadline) {
			terminal, reconcileErr = reconcileReclaimedRun(context.Background(), temporalClient, f.ownerID, requestID, f.submitterDir)
			if reconcileErr != nil {
				t.Fatalf("reconcileReclaimedRun (%s): %v (daemon output so far:\n%s)", f.label, reconcileErr, f.daemonOutput.String())
			}
			if terminal {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if !terminal {
			t.Fatalf("reconcileReclaimedRun (%s) never reported this request terminal within the deadline (daemon output:\n%s)", f.label, f.daemonOutput.String())
		}

		reconciled, err := run.Load(f.submitterDir, requestID)
		if err != nil {
			t.Fatalf("load reconciled run.json (%s): %v", f.label, err)
		}
		if reconciled.State != run.StateAccepted {
			t.Fatalf("state (%s) = %q, want %q", f.label, reconciled.State, run.StateAccepted)
		}
		if reconciled.ResultSHA == "" {
			t.Fatalf("reconciled run (%s) has no result_sha", f.label)
		}
		if err := exec.Command("git", "-C", f.ws, "cat-file", "-e", reconciled.ResultSHA).Run(); err != nil {
			t.Fatalf("reconciled run (%s) result_sha %s does not exist in its own workspace %s — reconcileReclaimedRun fetched the wrong repository's child result: %v", f.label, reconciled.ResultSHA, f.ws, err)
		}
		resultSHAs[f.label] = reconciled.ResultSHA
	}
	if resultSHAs["a"] == resultSHAs["b"] {
		t.Fatalf("both fixtures reconciled to the identical result_sha %s — expected two independent workspaces to produce distinct commits", resultSHAs["a"])
	}
}
