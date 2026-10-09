package workflow

import (
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/reviewstep"
	"buildgate/internal/runner"
	"buildgate/internal/sandbox"
	"buildgate/internal/testfixture"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
)

// TestRunBuildActivityHonorsPerExecutionCheckpointDir proves
// RunWorkflowInput.LogDir/CheckpointDir override Activities' own
// Worker-static fields when set — the mechanism a shared-task-queue
// deployment needs so an Activity dispatched to a *different* Worker than
// the one that started the Workflow still writes to the submitting run's
// own directories, not whichever Worker happened to pick up the task.
func TestRunBuildActivityHonorsPerExecutionCheckpointDir(t *testing.T) {
	workerStaticDir := t.TempDir()
	perExecutionDir := t.TempDir()
	activities := &Activities{
		LogDir:        workerStaticDir,
		CheckpointDir: workerStaticDir,
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			return runner.Result{ExitCode: 0}, nil
		},
	}
	input := fixtureInput()
	input.LogDir = perExecutionDir
	input.CheckpointDir = perExecutionDir

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
		t.Fatalf("RunBuildActivity: %v", err)
	}

	perExecutionEntries, err := os.ReadDir(filepath.Join(perExecutionDir, "activity-checkpoints"))
	if err != nil || len(perExecutionEntries) == 0 {
		t.Fatalf("expected a checkpoint under the per-execution CheckpointDir %q, got entries=%v err=%v", perExecutionDir, perExecutionEntries, err)
	}
	if _, err := os.Stat(filepath.Join(workerStaticDir, "activity-checkpoints")); !os.IsNotExist(err) {
		t.Fatalf("expected no checkpoint under the Worker-static LogDir/CheckpointDir %q once the per-execution value is set, stat err = %v", workerStaticDir, err)
	}
}

func TestRunBuildActivityDuplicateInvocationRunsSubprocessOnce(t *testing.T) {
	calls := 0
	var logPaths []string
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, logPath func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			logPaths = append(logPaths, logPath(1))
			return runner.Result{ExitCode: 0}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		if _, err := activities.RunBuildActivity(ctx, input); err != nil {
			return err
		}
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute duplicate build Activity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("subprocess calls after duplicate invocation = %d, want 1", calls)
	}

	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute build Activity with different ID: %v", err)
	}
	if calls != 2 {
		t.Fatalf("subprocess calls after different Activity ID = %d, want 2", calls)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("build log paths for different Activity executions = %q, want distinct paths", logPaths[0])
	}
}

// TestRunBuildActivityPropagatesCleanupUnconfirmedThroughCheckpoint is the
// regression for a "cleanup-error propagation through checkpoints" test
// gap: a subprocess failure wrapping
// sandbox.ErrCleanupUnconfirmed (the sandboxed and non-sandboxed paths both
// funnel into the same errors.Is(runErr, sandbox.ErrCleanupUnconfirmed)
// check below) must tag the durable checkpoint's ErrorType as
// CleanupUnconfirmedFailureType, not the generic InfrastructureFailureType
// — and a checkpoint reload (simulating Activity redispatch after a worker
// restart, without rerunning the subprocess) must still return an error
// that type survives on, exactly like checkpoint.Error/Result already do.
// Injects the failure via the same runWithRetries seam
// TestRunBuildActivityDuplicateInvocationRunsSubprocessOnce uses — the
// error-type tagging this proves is keyed on the returned error value
// itself, not on which of the two subprocess paths produced it (see
// runSandboxWithRetries' own errors.Is(runErr, sandbox.ErrCleanupUnconfirmed)
// call site, tagged identically); sandbox.ErrCleanupUnconfirmed's own
// production trigger (a failed `docker rm` after cancellation/log-limit) is
// covered separately by internal/sandbox's own Run tests.
func TestRunBuildActivityPropagatesCleanupUnconfirmedThroughCheckpoint(t *testing.T) {
	calls := 0
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			return runner.Result{ExitCode: -1}, fmt.Errorf("worker canceled: %w", sandbox.ErrCleanupUnconfirmed)
		},
	}
	var firstErr, secondErr error
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, firstErr = activities.RunBuildActivity(ctx, input)
		_, secondErr = activities.RunBuildActivity(ctx, input)
		return nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute build Activity: %v", err)
	}

	if calls != 1 {
		t.Fatalf("subprocess calls = %d, want 1 (second invocation must read the checkpoint, not rerun the subprocess)", calls)
	}
	if firstErr == nil || !hasApplicationErrorType(firstErr, CleanupUnconfirmedFailureType) {
		t.Fatalf("first RunBuildActivity error = %v, want application error type %q", firstErr, CleanupUnconfirmedFailureType)
	}
	if secondErr == nil || !hasApplicationErrorType(secondErr, CleanupUnconfirmedFailureType) {
		t.Fatalf("checkpoint-reload RunBuildActivity error = %v, want application error type %q to have survived the round trip", secondErr, CleanupUnconfirmedFailureType)
	}
}

// TestRunBuildActivityRecordsAttempts proves BuildActivityResult.Attempts
// carries every runWithRetries attempt — mirroring cmd/factoryd's direct
// path, which does the same via its own onAttempt callback — closing a
// known gap where a Temporal-routed run's per-attempt build evidence was
// silently dropped (RunBuildActivity used to pass nil for that callback).
func TestRunBuildActivityRecordsAttempts(t *testing.T) {
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, logPath func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			started := time.Now()
			onAttempt(1, runner.Result{Command: []string{"attempt-1"}, ExitCode: -1, StartedAt: started, FinishedAt: started.Add(time.Second)}, context.DeadlineExceeded)
			finalResult := runner.Result{Command: []string{"attempt-2"}, ExitCode: 0, StartedAt: started, FinishedAt: started.Add(2 * time.Second)}
			onAttempt(2, finalResult, nil)
			return finalResult, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
		return activities.RunBuildActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err != nil {
		t.Fatalf("execute build Activity: %v", err)
	}
	var result BuildActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode build Activity result: %v", err)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("Attempts = %+v, want 2 entries", result.Attempts)
	}
	if result.Attempts[0].Kind != "build" || result.Attempts[0].ExitCode != -1 {
		t.Errorf("Attempts[0] = %+v, want kind=build exit_code=-1", result.Attempts[0])
	}
	if result.Attempts[1].Kind != "build" || result.Attempts[1].ExitCode != 0 {
		t.Errorf("Attempts[1] = %+v, want kind=build exit_code=0", result.Attempts[1])
	}
	if result.Attempts[0].LogPath == result.Attempts[1].LogPath {
		t.Errorf("Attempts[0].LogPath == Attempts[1].LogPath = %q, want distinct per-attempt log paths", result.Attempts[0].LogPath)
	}
}

// TestRunBuildActivityRejectsLegacyShapedCheckpoint is the regression test
// for a real P1 finding from review: a checkpoint written before
// RunBuildActivity's checkpointed type changed from the flat runner.Result
// to the nested BuildActivityResult has its "result" object shaped
// completely differently (flat fields vs. a nested "result"/"attempts"
// pair). Unmarshaling that old JSON into the new type doesn't error — Go
// silently zero-values every field it can't match — so a previously
// nonzero build exit code would decode back as 0 and could let a run be
// accepted without ever rerunning the build. The Activity must detect the
// schema mismatch and halt while preserving the stale checkpoint instead of
// trusting it or silently deleting evidence.
func TestRunBuildActivityRejectsLegacyShapedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	activities := &Activities{
		LogDir: dir,
		runWithRetries: func(context.Context, string, func(int) string, int, func(int, runner.Result, error), string, ...string) (runner.Result, error) {
			calls++
			return runner.Result{ExitCode: 0}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
		info := activity.GetInfo(ctx)
		path := activityCheckpointPath(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return BuildActivityResult{}, err
		}
		// The pre-schema-version, flat-runner.Result checkpoint shape: no
		// "schema_version" key, and "result" holds runner.Result's own
		// fields directly rather than nesting them under a "result" key
		// alongside "attempts".
		legacy := `{"completed":true,"workflow_id":"` + info.WorkflowExecution.ID + `","run_id":"` + info.WorkflowExecution.RunID + `","activity_id":"` + info.ActivityID + `","result":{"exit_code":7,"command":["stale-legacy-result"]}}`
		if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
			return BuildActivityResult{}, err
		}
		return activities.RunBuildActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err == nil {
		t.Fatal("execute build Activity with legacy checkpoint: want ambiguous-checkpoint error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != AmbiguousPriorAttemptType {
		t.Fatalf("error = %v, want ApplicationError type %q", err, AmbiguousPriorAttemptType)
	}
	if calls != 0 {
		t.Fatalf("subprocess calls = %d, want 0 when checkpoint schema is unknown", calls)
	}
}

// TestRunBuildActivityRejectsLegacyShapedCheckpointWithPairedIntent is the
// regression test for a real P2 finding from review on
// TestRunBuildActivityRejectsLegacyShapedCheckpoint's fix: the two-phase
// intent protocol was already in place before the checkpoint schema
// changed, so a real legacy-shaped checkpoint necessarily has a paired
// intent record (written before the subprocess that produced it) sitting
// alongside it. Discarding the checkpoint as unreadable without also
// discarding that intent would make the very next loadActivityIntent call
// find it and incorrectly halt as AmbiguousPriorAttemptType — even though
// a completed checkpoint demonstrably exists, just in an unreadable shape.
func TestRunBuildActivityRejectsLegacyShapedCheckpointWithPairedIntent(t *testing.T) {
	dir := t.TempDir()
	calls := 0
	activities := &Activities{
		LogDir: dir,
		runWithRetries: func(context.Context, string, func(int) string, int, func(int, runner.Result, error), string, ...string) (runner.Result, error) {
			calls++
			return runner.Result{ExitCode: 0}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) (BuildActivityResult, error) {
		info := activity.GetInfo(ctx)
		checkpointPath := activityCheckpointPath(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID)
		if err := os.MkdirAll(filepath.Dir(checkpointPath), 0o750); err != nil {
			return BuildActivityResult{}, err
		}
		legacy := `{"completed":true,"workflow_id":"` + info.WorkflowExecution.ID + `","run_id":"` + info.WorkflowExecution.RunID + `","activity_id":"` + info.ActivityID + `","result":{"exit_code":7,"command":["stale-legacy-result"]}}`
		if err := os.WriteFile(checkpointPath, []byte(legacy), 0o600); err != nil {
			return BuildActivityResult{}, err
		}
		// The paired intent a real pre-upgrade Worker would have written
		// before the subprocess that produced the legacy checkpoint above.
		if _, err := recordActivityIntentForExecution(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "build", []string{"stale-legacy-result"}, "2024-01-01T00:00:00Z"); err != nil {
			return BuildActivityResult{}, err
		}
		return activities.RunBuildActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err == nil {
		t.Fatal("execute build Activity with legacy checkpoint: want ambiguous-checkpoint error, got nil")
	}
	var appErr *temporal.ApplicationError
	if !errors.As(err, &appErr) || appErr.Type() != AmbiguousPriorAttemptType {
		t.Fatalf("error = %v, want ApplicationError type %q", err, AmbiguousPriorAttemptType)
	}
	if calls != 0 {
		t.Fatalf("subprocess calls = %d, want 0 when checkpoint schema is unknown", calls)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "activity-checkpoints")); statErr != nil {
		t.Fatalf("checkpoint directory was removed: %v", statErr)
	}
}

// TestRunBuildActivityHaltsOnAmbiguousPriorIntent proves the two-phase
// intent protocol closes the crash window between build_app.py finishing
// and RunBuildActivity's completed checkpoint being saved: an intent record
// left behind with no matching completed checkpoint must halt the next
// attempt rather than invoke the subprocess a second time or trust an
// unconfirmed result.
func TestRunBuildActivityHaltsOnAmbiguousPriorIntent(t *testing.T) {
	calls := 0
	dir := t.TempDir()
	activities := &Activities{
		LogDir: dir,
		runWithRetries: func(context.Context, string, func(int) string, int, func(int, runner.Result, error), string, ...string) (runner.Result, error) {
			calls++
			return runner.Result{ExitCode: 0}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		info := activity.GetInfo(ctx)
		if _, err := recordActivityIntentForExecution(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "build", []string{"stale"}, "2024-01-01T00:00:00Z"); err != nil {
			return err
		}
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err == nil {
		t.Fatal("execute build Activity with pre-existing intent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "prior attempt") {
		t.Fatalf("error = %q, want it to mention the prior attempt", err.Error())
	}
	if calls != 0 {
		t.Fatalf("subprocess calls with a pre-existing intent record = %d, want 0", calls)
	}
}

// TestRunBuildActivityRejectsDivergentSandboxDataDir is the regression for
// a real P1 finding from GitHub Codex review of PR #37: a shared-task-queue
// execution's own RunWorkflowInput.DataDir (a legitimate per-execution
// override for checkpoint/log placement, per that field's own doc comment)
// also decides sandbox.dataDirLabel's hash for a sandboxed launch — but
// this Worker's own startup reconciliation (sandbox.ReconcileOrphans) only
// ever scans its own Worker-static a.DataDir. Diverging silently would
// launch a container that Worker's own crash-recovery can never find or
// remove; this proves runSandboxWithRetries fails closed instead, before
// ever staging a script or contacting Docker.
func TestRunBuildActivityRejectsDivergentSandboxDataDir(t *testing.T) {
	activities := &Activities{
		LogDir:  t.TempDir(),
		DataDir: "/worker/static/data",
	}
	input := fixtureInput()
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.DataDir = "/different/execution/data"

	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute build Activity with a diverging sandbox data directory: want error, got nil")
	}
	if !strings.Contains(err.Error(), "diverges from this Worker's own -data-dir") {
		t.Fatalf("error = %q, want it to name the data-dir divergence", err.Error())
	}
}

// TestRunBuildActivityRejectsDivergentSandboxDocker is
// TestRunBuildActivityRejectsDivergentSandboxDataDir's counterpart for
// RunWorkflowInput.SandboxDocker: a diverging Docker executable would make
// a launched container invisible to this Worker's own docker CLI entirely,
// not just mislabeled.
func TestRunBuildActivityRejectsDivergentSandboxDocker(t *testing.T) {
	activities := &Activities{
		LogDir:        t.TempDir(),
		SandboxDocker: "docker",
	}
	input := fixtureInput()
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.SandboxDocker = "/some/other/docker"

	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute build Activity with a diverging sandbox Docker executable: want error, got nil")
	}
	if !strings.Contains(err.Error(), "diverges from this Worker's own -sandbox-docker") {
		t.Fatalf("error = %q, want it to name the Docker-executable divergence", err.Error())
	}
}

// TestRunBuildActivityRejectsDaemonHeartbeatDivergenceAtLaunchTime is the
// regression for a real P1 finding from a further round of GitHub Codex
// review of PR #37 (round 8): cmd/factoryd's own submission-time
// heartbeat check only proves no divergence existed at submission — a
// request that sits queued, or a daemon that restarts with a different
// -sandbox-docker, between submission and actual dispatch could still
// diverge by the time a container is really launched. This is the case
// the submission-time check structurally cannot catch:
// RunWorkflowInput.SandboxDocker/Activities.SandboxDocker are both
// self-consistent (this submitter's own short-lived Worker, as always),
// but the daemon's own heartbeat — read fresh here, at actual launch
// time, via RepositoryOwnerID — has since diverged. Only
// runSandboxWithRetries' own launch-time re-check can catch this; the
// earlier self-consistency checks (proven by
// TestRunBuildActivityRejectsDivergentSandboxDataDir/
// TestRunBuildActivityRejectsDivergentSandboxDocker) do not.
func TestRunBuildActivityRejectsDaemonHeartbeatDivergenceAtLaunchTime(t *testing.T) {
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	hb := daemonheartbeat.Heartbeat{
		SandboxDocker: "/usr/local/bin/podman",
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), hb); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	activities := &Activities{
		LogDir:        t.TempDir(),
		DataDir:       dataDir,
		SandboxDocker: "docker",
	}
	input := fixtureInput()
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	// Deliberately self-consistent with activities' own static config —
	// this is exactly what a real submitter's own short-lived Worker
	// always looks like, and what the earlier self-consistency checks
	// above cannot reject.
	input.SandboxDocker = "docker"
	input.DataDir = dataDir
	input.RepositoryOwnerID = ownerID

	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute build Activity with a self-consistent input but a diverging daemon heartbeat: want error, got nil")
	}
	if !strings.Contains(err.Error(), "diverges from repository") || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("error = %q, want it to name the daemon-heartbeat divergence", err.Error())
	}
}

// TestRunBuildActivityRejectsPathResolutionDivergenceAtLaunchTime is the
// regression for a real P1 finding from a ninth round of GitHub Codex
// review of PR #37: two processes both configured with the identical bare
// -sandbox-docker="docker" can still resolve to two different real
// executables when their own $PATH values differ — a plain string
// comparison of the unresolved value can never catch this. Two real
// (fake) "docker" binaries in two different directories, with this
// process's own $PATH pointed at one of them, stand in for a submitter
// and daemon whose PATH differs, even though both are "self-consistently"
// configured with the same bare name.
func TestRunBuildActivityRejectsPathResolutionDivergenceAtLaunchTime(t *testing.T) {
	submitterDir := t.TempDir()
	submitterDocker := filepath.Join(submitterDir, "docker")
	if err := os.WriteFile(submitterDocker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	daemonDir := t.TempDir()
	daemonDocker := filepath.Join(daemonDir, "docker")
	if err := os.WriteFile(daemonDocker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("PATH", submitterDir)

	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	if err := daemonheartbeat.Write(daemonheartbeat.Path(dataDir, ownerID), daemonheartbeat.Heartbeat{
		// What the daemon's own ResolveSandboxDocker would have written
		// for its own $PATH — its own resolved path, not the bare
		// "docker" string.
		SandboxDocker: daemonDocker,
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	activities := &Activities{
		LogDir:        t.TempDir(),
		DataDir:       dataDir,
		SandboxDocker: "docker",
	}
	input := fixtureInput()
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	// Deliberately self-consistent, the same bare name on both sides —
	// exactly what the earlier self-consistency checks cannot reject, and
	// what a naive (unresolved) comparison would also wrongly allow.
	input.SandboxDocker = "docker"
	input.DataDir = dataDir
	input.RepositoryOwnerID = ownerID

	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute build Activity with a $PATH-diverging \"docker\": want error, got nil")
	}
	if !strings.Contains(err.Error(), submitterDocker) || !strings.Contains(err.Error(), daemonDocker) {
		t.Fatalf("error = %q, want it to name both resolved executable paths", err.Error())
	}
}

// TestRunBuildActivityAllowsMissingDaemonHeartbeatAtLaunchTime proves the
// launch-time re-check never blocks a launch when RepositoryOwnerID is
// unset (no repository-owner submission, e.g. runViaTemporal) or when no heartbeat is visible — unknown, not diverging, exactly
// like the submission-time check's own convention.
func TestRunBuildActivityAllowsMissingDaemonHeartbeatAtLaunchTime(t *testing.T) {
	dataDir := t.TempDir()
	activities := &Activities{
		LogDir:        t.TempDir(),
		DataDir:       dataDir,
		SandboxDocker: "docker",
	}
	input := fixtureInput()
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.SandboxDocker = "docker"
	input.DataDir = dataDir
	input.RepositoryOwnerID = "repo-owner-fixture" // no heartbeat file written for it

	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	// This will fail past the divergence check (no real Docker/workspace
	// in this test), but must not fail *with the divergence message* —
	// proving the missing heartbeat itself was never treated as a
	// rejection.
	if err != nil && strings.Contains(err.Error(), "diverges from repository") {
		t.Fatalf("execute build Activity with no daemon heartbeat: got a divergence rejection, want the launch-time re-check to treat this as unknown: %v", err)
	}
}

// TestRunBuildActivityRejectsDaemonHeartbeatDivergenceBetweenRetries is the
// regression for a real finding from an Opus-assisted review of the
// checkDaemonHeartbeatDivergence fix above: an earlier version of that fix
// re-checked the daemon heartbeat only once, before the retry loop — correct
// for the single-attempt case, but a daemon that changes its own
// -sandbox-docker *between* two retries of the same Activity execution
// (a single attempt can run for up to an hour) would still launch an
// unreconcilable container on that later retry. This drives a real,
// fake-docker-backed retry: attempt 1's own fake `docker run` rewrites the
// heartbeat to a diverging SandboxDocker as a side effect of failing (an
// infra-style exit 125, forcing runSandboxWithRetries to retry) before
// exiting, standing in for "the daemon restarted with new config while
// this attempt was in flight" — attempt 2 must then be rejected by the
// re-check at the top of the loop, never actually invoking `docker run`
// a second time.
func TestRunBuildActivityRejectsDaemonHeartbeatDivergenceBetweenRetries(t *testing.T) {
	dataDir := t.TempDir()
	ownerID := "repo-owner-fixture"
	runCallsPath := filepath.Join(t.TempDir(), "run-calls")
	docker := filepath.Join(t.TempDir(), "docker-fake")
	realHeartbeatPath := daemonheartbeat.Path(dataDir, ownerID)

	// Matches the fake docker binary's own path below, not the literal
	// string "docker" — this test's launch genuinely uses that fake path
	// as its SandboxDocker, so the *initial* heartbeat must agree with it
	// for attempt 1's own pre-loop check to pass and actually reach
	// `docker run` at all.
	if err := daemonheartbeat.Write(realHeartbeatPath, daemonheartbeat.Heartbeat{
		SandboxDocker: docker,
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write initial heartbeat: %v", err)
	}
	// Staged separately, then swapped into place by the fake docker
	// script's first "run" invocation — deterministic (no goroutine racing
	// the loop's own re-check), unlike trying to mutate the real heartbeat
	// file from a background goroutine timed against the retry loop.
	divergedHeartbeatPath := filepath.Join(t.TempDir(), "diverged-heartbeat.json")
	if err := daemonheartbeat.Write(divergedHeartbeatPath, daemonheartbeat.Heartbeat{
		SandboxDocker: "/usr/local/bin/podman",
		UpdatedAt:     time.Now().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("write staged diverged heartbeat: %v", err)
	}
	// sandbox.Run's own pre-launch mount-visibility probe
	// (verifyWorkDirMountVisibility) also invokes "docker run" once
	// before each real attempt -- matched here by "sandbox-mount-probe"
	// in its args and answered immediately, without touching
	// runCallsPath, so it isn't mistaken for one of the two counted
	// real attempts this test's own divergence logic depends on.
	script := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in\n*sandbox-mount-probe*) exit 0 ;;\nesac\ncase \"$1\" in\nrun)\n  printf x >> %s\n  n=$(wc -c < %s)\n  if [ \"$n\" -eq 1 ]; then\n    cp %s %s\n    exit 125\n  fi\n  exit 0\n  ;;\nesac\n",
		runCallsPath, runCallsPath, divergedHeartbeatPath, realHeartbeatPath)
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	buildScript := filepath.Join(t.TempDir(), "build_app.py")
	if err := os.WriteFile(buildScript, []byte("print('ok')"), 0o644); err != nil {
		t.Fatal(err)
	}

	activities := &Activities{
		LogDir:           t.TempDir(),
		DataDir:          dataDir,
		SandboxDocker:    docker,
		BuildMaxAttempts: 2,
	}
	input := fixtureInput()
	input.WorkspacePath = t.TempDir()
	input.ProjectConfigCommitSHA = trustedCommit(t, input.WorkspacePath)
	input.SpecPath = ""
	input.BuildAppInterpreter = "/bin/sh"
	input.BuildAppScript = buildScript
	input.SandboxImage = "factory-worker:test@sha256:deadbeef"
	input.SandboxDocker = docker
	input.DataDir = dataDir
	input.RepositoryOwnerID = ownerID

	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute build Activity across a heartbeat change between retries: want error, got nil")
	}
	if !strings.Contains(err.Error(), "diverges from repository") || !strings.Contains(err.Error(), "podman") {
		t.Fatalf("error = %q, want it to name the daemon-heartbeat divergence caught on the retry", err.Error())
	}
	calls, readErr := os.ReadFile(runCallsPath)
	if readErr != nil {
		t.Fatalf("read run-call marker: %v", readErr)
	}
	if len(calls) != 1 {
		t.Fatalf("fake `docker run` invocation count = %d, want exactly 1 — attempt 2 must be rejected by the heartbeat re-check before ever invoking docker again", len(calls))
	}
}

// TestPostBuildActivityDuplicateInvocationCommitsOnce is the regression
// test for a real gap: PostBuildActivity's safety-net commit was
// previously uncheckpointed on the theory that a git commit is
// "naturally idempotent" (redispatch would just find the workspace
// already clean and do nothing) — true of the *workspace*, but not of the
// *evidence*: a second invocation of this exact Activity execution, after
// the first already committed, would see a clean workspace and take the
// early "already clean" return path instead of the commit path, silently
// reporting CommittedByWorker=false even though the factory itself made
// that commit. This proves both halves: the underlying git commit really
// only happens once (HEAD doesn't advance further on the second call),
// and the second call still correctly reports CommittedByWorker=true
// with the same ResultSHA as the first, from the checkpoint.
func TestPostBuildActivityDuplicateInvocationCommitsOnce(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHA, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("simulate uncommitted build output: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	input := PostBuildInput{WorkspacePath: workspacePath, BaseSHA: strings.TrimSpace(string(baseSHA)), BuildExitCode: 0}
	wrapper := func(ctx context.Context, input PostBuildInput) ([2]PostBuildResult, error) {
		first, err := activities.PostBuildActivity(ctx, input)
		if err != nil {
			return [2]PostBuildResult{}, err
		}
		second, err := activities.PostBuildActivity(ctx, input)
		if err != nil {
			return [2]PostBuildResult{}, err
		}
		return [2]PostBuildResult{first, second}, nil
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute duplicate post-build Activity: %v", err)
	}
	var results [2]PostBuildResult
	if err := raw.Get(&results); err != nil {
		t.Fatalf("decode post-build Activity results: %v", err)
	}
	if !results[0].CommittedByWorker || !results[1].CommittedByWorker {
		t.Fatalf("results = %+v, want CommittedByWorker=true on both calls", results)
	}
	if results[0].ResultSHA != results[1].ResultSHA {
		t.Fatalf("ResultSHA changed between duplicate invocations: %q -> %q, want the same (cached) value", results[0].ResultSHA, results[1].ResultSHA)
	}
	headBytes, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD after duplicate invocation: %v", err)
	}
	if head := strings.TrimSpace(string(headBytes)); head != results[0].ResultSHA {
		t.Fatalf("workspace HEAD = %q, want it still at %q — the second call must not have committed again", head, results[0].ResultSHA)
	}
}

// TestPostBuildActivityUsesTicketGoalAsCommitSubject is N2's own
// regression test: the safety-net commit's subject used to be the
// generic "ticket: apply verified change" unconditionally, which a
// squash merge then surfaced verbatim on every such PR
// ("ticket: apply verified change (#302)") -- SpecPath's own "## Goal"
// paragraph must be used instead when it resolves to one.
func TestPostBuildActivityUsesTicketGoalAsCommitSubject(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHA, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("simulate uncommitted build output: %v", err)
	}
	specPath := filepath.Join(t.TempDir(), "001.spec.md")
	if err := os.WriteFile(specPath, []byte("## Goal\n\nAdd idempotency keys to POST /refunds\n\n## Plan\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	activities := &Activities{LogDir: t.TempDir()}
	input := PostBuildInput{WorkspacePath: workspacePath, BaseSHA: strings.TrimSpace(string(baseSHA)), BuildExitCode: 0, SpecPath: specPath}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.PostBuildActivity)
	if _, err := env.ExecuteActivity(activities.PostBuildActivity, input); err != nil {
		t.Fatalf("execute post-build Activity: %v", err)
	}

	subjectBytes, err := exec.Command("git", "-C", workspacePath, "log", "-1", "--format=%s").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	subject := strings.TrimSpace(string(subjectBytes))
	if subject != "Add idempotency keys to POST /refunds" {
		t.Errorf("commit subject = %q, want the ticket's own Goal paragraph", subject)
	}
}

// TestPostBuildActivityHaltsOnAmbiguousPriorIntent is
// TestRunBuildActivityHaltsOnAmbiguousPriorIntent's counterpart for the
// crash window between PostBuildActivity's safety-net commit succeeding and
// its checkpoint persisting — found via a second codex review round on the
// checkpointing added for TestPostBuildActivityDuplicateInvocationCommitsOnce:
// checkpointing alone still let a worker crash right after the commit but
// before the checkpoint save leave a redispatch with no checkpoint and a
// workspace the prior attempt's own commit already made clean, so it would
// take the early "already clean" path and silently report
// CommittedByWorker=false again. An intent record left behind with no
// matching completed checkpoint must halt instead.
func TestPostBuildActivityHaltsOnAmbiguousPriorIntent(t *testing.T) {
	workspacePath := testfixture.NewGitRepo(t)
	baseSHA, err := exec.Command("git", "-C", workspacePath, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspacePath, "content.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("simulate uncommitted build output: %v", err)
	}

	dir := t.TempDir()
	activities := &Activities{LogDir: dir}
	input := PostBuildInput{WorkspacePath: workspacePath, BaseSHA: strings.TrimSpace(string(baseSHA)), BuildExitCode: 0}
	wrapper := func(ctx context.Context, input PostBuildInput) (PostBuildResult, error) {
		info := activity.GetInfo(ctx)
		if _, err := recordActivityIntentForExecution(dir, info.WorkflowExecution.ID, info.WorkflowExecution.RunID, info.ActivityID, 1, "post-build-commit", []string{"stale"}, "2024-01-01T00:00:00Z"); err != nil {
			return PostBuildResult{}, err
		}
		return activities.PostBuildActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err = env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute post-build Activity with pre-existing intent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "prior attempt") {
		t.Fatalf("error = %q, want it to mention the prior attempt", err.Error())
	}
	clean, err := runner.GitIsClean(workspacePath)
	if err != nil {
		t.Fatalf("check workspace cleanliness: %v", err)
	}
	if clean {
		t.Error("workspace is clean after the halt; the stale intent must have prevented a second commit attempt entirely")
	}
}

// TestRunBuildActivityHeartbeatsDuringSubprocess and
// TestRunVerifyActivityHeartbeatsDuringSubprocess prove RunBuildActivity/
// RunVerifyActivity actually wire heartbeatWhileRunning around their real
// subprocess call, not just that the helper itself works in isolation.
func TestRunBuildActivityHeartbeatsDuringSubprocess(t *testing.T) {
	var heartbeats atomic.Int32
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(ctx context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			for i := 0; i < 5; i++ {
				activity.RecordHeartbeat(ctx)
				heartbeats.Add(1)
				time.Sleep(time.Millisecond)
			}
			return runner.Result{ExitCode: 0}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunBuildActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute build Activity: %v", err)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("no heartbeats recorded during the build subprocess")
	}
}

// TestBuildActivityArgsIncludesVerifyCommand guards the --verify-command
// plumbing added 2026-09-09 for the Temporal paths, mirroring
// cmd/factoryd's buildAppArgs test: without this, the
// Activity's own build_app.py invocation silently disagreed with its own
// canonical-verify step about which command actually gates a round, for
// any workspace whose auto-detected root command needs a toolchain the
// sandbox doesn't have.
func TestBuildActivityArgsIncludesVerifyCommand(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "cd backend && go test ./...", "", "", "", "", "pi")

	want := []string{
		"/path/build_app.py",
		"--workspace", "/ws",
		"--spec", "/ws/spec.md",
		"--conformity-policy", "required",
		"--max-rounds", "3",
		"--timeout-minutes", "45",
		"--review-base-sha", "deadbeef",
		"--verify-command", "cd backend && go test ./...",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("buildActivityArgs() =\n  %v\nwant\n  %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("buildActivityArgs()[%d] = %q, want %q (full: got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

// TestBuildActivityArgsOmitsVerifyCommandWhenEmpty guards the other
// direction: an empty verifyCommand (a caller with no resolved command to
// give) must not emit a bare "--verify-command" with nothing after it --
// build_app.py's own argparse would consume the next flag as its value
// instead of erroring.
func TestBuildActivityArgsOmitsVerifyCommandWhenEmpty(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "", "", "", "", "", "pi")
	for i, arg := range got {
		if arg == "--verify-command" {
			t.Fatalf("buildActivityArgs() included --verify-command with an empty value at index %d: %v", i, got)
		}
	}
}

// TestBuildActivityArgsIncludesFastCheckCommand guards the
// --fast-check-command plumbing on the Temporal paths (Codex review of
// PR #90, round 2): without it, a run routed through -temporal-address or
// -repository silently ignored .factory.yml's fast_check_command and
// always paid for the full verify command.
func TestBuildActivityArgsIncludesFastCheckCommand(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "make verify", "make fast-check", "", "", "", "pi")

	want := []string{
		"/path/build_app.py",
		"--workspace", "/ws",
		"--spec", "/ws/spec.md",
		"--conformity-policy", "required",
		"--max-rounds", "3",
		"--timeout-minutes", "45",
		"--review-base-sha", "deadbeef",
		"--verify-command", "make verify",
		"--fast-check-command", "make fast-check",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("buildActivityArgs() =\n  %v\nwant\n  %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("buildActivityArgs()[%d] = %q, want %q (full: got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

// TestBuildActivityArgsOmitsFastCheckCommandWhenEmpty mirrors
// TestBuildActivityArgsOmitsVerifyCommandWhenEmpty: an unset
// fastCheckCommand must not emit a bare "--fast-check-command" with
// nothing after it.
func TestBuildActivityArgsOmitsFastCheckCommandWhenEmpty(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "make verify", "", "", "", "", "pi")
	for i, arg := range got {
		if arg == "--fast-check-command" {
			t.Fatalf("buildActivityArgs() included --fast-check-command with an empty value at index %d: %v", i, got)
		}
	}
}

// TestBuildActivityArgsIncludesThinkingWhenSet is buildActivityArgs' own
// counterpart to cmd/factoryd's TestBuildAppArgsIncludesThinkingWhenSet:
// roles.execution's resolved Pi reasoning-effort level (carried on
// RunWorkflowInput.Thinking, see RunBuildActivity's own call site) must
// reach build_app.py's argv on the Temporal path exactly like it does in cmd/factoryd.
func TestBuildActivityArgsIncludesThinkingWhenSet(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "", "", "", "", "medium", "pi")
	want := []string{
		"/path/build_app.py",
		"--workspace", "/ws",
		"--spec", "/ws/spec.md",
		"--conformity-policy", "required",
		"--max-rounds", "3",
		"--timeout-minutes", "45",
		"--review-base-sha", "deadbeef",
		"--thinking", "medium",
		"--harness", "pi",
	}
	if len(got) != len(want) {
		t.Fatalf("buildActivityArgs() =\n  %v\nwant\n  %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("buildActivityArgs()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildActivityArgsForwardsSpecAcceptanceCriteriaOnlyWhenSet pins the
// --spec-acceptance-criteria argv: carried with its value when set, absent
// (never a bare flag) when empty.
func TestBuildActivityArgsForwardsSpecAcceptanceCriteriaOnlyWhenSet(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "", "", "", "/ws/criteria.md", "", "pi")
	want := []string{
		"/path/build_app.py",
		"--workspace", "/ws",
		"--spec", "/ws/spec.md",
		"--conformity-policy", "required",
		"--max-rounds", "3",
		"--timeout-minutes", "45",
		"--review-base-sha", "deadbeef",
		"--spec-acceptance-criteria", "/ws/criteria.md",
		"--harness", "pi",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("buildActivityArgs() =\n  %v\nwant\n  %v", got, want)
	}
	for _, arg := range buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "", "", "", "", "", "pi") {
		if arg == "--spec-acceptance-criteria" {
			t.Fatalf("--spec-acceptance-criteria emitted with an empty value")
		}
	}
}

func TestBuildActivityArgsOmitsThinkingWhenEmpty(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "", "", "", "", "", "pi")
	for i, arg := range got {
		if arg == "--thinking" {
			t.Fatalf("buildActivityArgs() included --thinking with an empty value at index %d: %v", i, got)
		}
	}
}

// TestBuildAndReviewArgsCarryTheirOwnRolesHarness pins that every Temporal
// launch that runs the coding agent names its own role's harness: the build
// carries the execution role's, every review step the review role's, and the
// two can differ inside one execution.
func TestBuildAndReviewArgsCarryTheirOwnRolesHarness(t *testing.T) {
	got := buildActivityArgs("/path/build_app.py", "/ws", "/ws/spec.md", "required", 3, 45, "deadbeef", "", "", "", "", "", "pifork")
	if n := len(got); n < 2 || got[n-2] != "--harness" || got[n-1] != "pifork" {
		t.Fatalf("buildActivityArgs() = %v, want it to end with --harness pifork", got)
	}
	a := &Activities{}
	in := ReviewStepInput{RunWorkflowInput: RunWorkflowInput{Harness: "pifork", ReviewHarness: "pi", SpecAcceptanceCriteria: "/ws/criteria.md"}}
	for _, step := range append(append([]reviewstep.Step{}, reviewstep.Steps...), reviewstep.CombinedStep) {
		args := reviewStepArgs(a, step, in, "/path/script.py")
		if n := len(args); n < 2 || args[n-2] != "--harness" || args[n-1] != "pi" {
			t.Errorf("reviewStepArgs(%s) = %v, want it to end with --harness pi (the review role's, not the execution role's)", step.Name, args)
		}
	}
}

// TestWorkerEnvComesFromTheJobsOwnHarness pins the per-role worker
// environment: the execution role's pifork harness gives the build worker
// pifork's environment while the review role's pi harness adds none.
func TestWorkerEnvComesFromTheJobsOwnHarness(t *testing.T) {
	in := RunWorkflowInput{Harness: "pifork", ReviewHarness: "pi"}
	if env := executionHarnessEnv(in); len(env) == 0 || env[0] != "PI_CODING_AGENT_DIR=/home/worker/.pi/agent" {
		t.Errorf("executionHarnessEnv = %v, want pifork's worker environment", env)
	}
	if env := reviewHarnessEnv(in); len(env) != 0 {
		t.Errorf("reviewHarnessEnv = %v, want none for pi", env)
	}
}
