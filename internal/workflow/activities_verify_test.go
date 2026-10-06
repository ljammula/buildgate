package workflow

import (
	"buildgate/internal/evidence"
	"buildgate/internal/runner"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
)

// TestRunVerifyActivityHaltsOnAmbiguousPriorIntent is
// TestRunBuildActivityHaltsOnAmbiguousPriorIntent's counterpart for the
// verify Activity's own instance of the same crash window.
func TestRunVerifyActivityHaltsOnAmbiguousPriorIntent(t *testing.T) {
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
		_, err := activities.RunVerifyActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err == nil {
		t.Fatal("execute verify Activity with pre-existing intent: want error, got nil")
	}
	if !strings.Contains(err.Error(), "prior attempt") {
		t.Fatalf("error = %q, want it to mention the prior attempt", err.Error())
	}
	if calls != 0 {
		t.Fatalf("subprocess calls with a pre-existing intent record = %d, want 0", calls)
	}
}

// TestRunNamedGateActivityComputesReferenceOracleHash is the Temporal-path
// parity test for cmd/factoryd's own reference-oracle snapshot+hash+mount
// (PR #151/#152's review-driven fixes): when the "reference_oracle" check has
// ReferenceOracleDir configured, RunNamedGateActivity must snapshot the
// source, hash the snapshot (not the live source), and record that same
// hash on both the returned VerifyActivityResult and every recorded
// Attempt -- and the snapshot itself must not survive the Activity.
func TestRunNamedGateActivityComputesReferenceOracleHash(t *testing.T) {
	oracleDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(oracleDir, "oracle_test.go"), []byte("package verify\n"), 0o600); err != nil {
		t.Fatalf("write oracle fixture: %v", err)
	}
	wantHash, err := evidence.SHA256Tree(oracleDir)
	if err != nil {
		t.Fatalf("SHA256Tree(oracleDir): %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket:                   "fixture-ticket",
			WorkspacePath:            t.TempDir(),
			SandboxImage:             "factory-worker:test@sha256:deadbeef",
			SandboxDocker:            docker,
			RunID:                    "run-id",
			DataDir:                  activities.DataDir,
			ReferenceOracleDir:       oracleDir,
			ReferenceOracleMountPath: "verify",
		},
		Check:   "reference_oracle",
		Command: "true",
	}

	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return activities.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute reference_oracle gate Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode reference_oracle gate Activity result: %v", err)
	}
	if result.ReferenceOracleSHA256 != wantHash {
		t.Errorf("ReferenceOracleSHA256 = %q, want %q (independently computed over the same source)", result.ReferenceOracleSHA256, wantHash)
	}
	if len(result.Attempts) == 0 {
		t.Fatal("no attempts recorded")
	}
	for _, a := range result.Attempts {
		if a.ReferenceOracleSHA256 != wantHash {
			t.Errorf("Attempt.ReferenceOracleSHA256 = %q, want %q", a.ReferenceOracleSHA256, wantHash)
		}
	}
	// Found live: a snapshot left behind under a durable log directory is
	// exactly the kind of untracked-but-real .go file that broke this
	// repo's own go vet/gofmt whole-tree scan during this same change's
	// development.
	if _, statErr := os.Stat(filepath.Join(activities.LogDir, "reference-oracle-snapshot")); !os.IsNotExist(statErr) {
		t.Errorf("reference-oracle snapshot directory still exists after the Activity completed: stat err = %v", statErr)
	}
}

// TestRunNamedGateActivitySkipsReferenceOracleHashWhenDirUnset confirms
// the converse: a "reference_oracle" check with no ReferenceOracleDir
// configured (the pre-existing, weaker behavior) computes no hash and
// mounts nothing -- matching cmd/factoryd's own opt-in convention.
func TestRunNamedGateActivitySkipsReferenceOracleHashWhenDirUnset(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket:        "fixture-ticket",
			WorkspacePath: t.TempDir(),
			SandboxImage:  "factory-worker:test@sha256:deadbeef",
			SandboxDocker: docker,
			RunID:         "run-id",
			DataDir:       activities.DataDir,
		},
		Check:   "reference_oracle",
		Command: "true",
	}

	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return activities.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute reference_oracle gate Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode reference_oracle gate Activity result: %v", err)
	}
	if result.ReferenceOracleSHA256 != "" {
		t.Errorf("ReferenceOracleSHA256 = %q, want empty when -reference-oracle-dir is unset", result.ReferenceOracleSHA256)
	}
}

// TestRunNamedGateActivityOtherGatesUnaffectedByReferenceOracleConfig is
// the direct regression test for a real P1 (adversarial /code-review,
// 2026-09-15): an earlier version passed input.ReferenceOracleMountPath
// into runSandboxWithRetries unconditionally, gating only the paired
// referenceOracleDir local on input.Check == "reference_oracle" -- so
// ANY other named gate (lint here) running while
// -reference-oracle-dir/-reference-oracle-mount-path were configured for
// the run got a mismatched ReferenceOracleDir=""/ReferenceOracleMountPath=
// <set> pair, which LaunchSpec.Validate's "must be set together" rule
// rejects outright, halting every such gate whenever the oracle-mount
// feature was opted into at all. This must succeed exactly as it would
// with the oracle-mount fields unset.
func TestRunNamedGateActivityOtherGatesUnaffectedByReferenceOracleConfig(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket:        "fixture-ticket",
			WorkspacePath: t.TempDir(),
			SandboxImage:  "factory-worker:test@sha256:deadbeef",
			SandboxDocker: docker,
			RunID:         "run-id",
			DataDir:       activities.DataDir,
			// Configured for the RUN, but this Activity invocation is for
			// "lint", not "reference_oracle" -- exactly the mismatch the
			// bug produced.
			ReferenceOracleDir:       t.TempDir(),
			ReferenceOracleMountPath: "verify",
		},
		Check:   "lint",
		Command: "true",
	}

	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return activities.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, input)
	if err != nil {
		t.Fatalf("execute lint gate Activity with reference-oracle fields configured for the run: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode lint gate Activity result: %v", err)
	}
	if result.Result.ExitCode != 0 {
		t.Errorf("lint gate ExitCode = %d, want 0", result.Result.ExitCode)
	}
	if result.ReferenceOracleSHA256 != "" {
		t.Errorf("lint gate ReferenceOracleSHA256 = %q, want empty -- the oracle mount/hash only applies to the reference_oracle check", result.ReferenceOracleSHA256)
	}
}

// TestRunNamedGateActivityRejectsMountPathWithoutDir is the direct
// regression test for a real P1 (adversarial /code-review, 2026-09-15,
// round 2): an earlier version skipped the whole
// ReferenceOracleMountPath assignment whenever ReferenceOracleDir was
// empty, so a "reference_oracle" check with -reference-oracle-mount-path
// configured but -reference-oracle-dir left unset passed BOTH fields
// through to LaunchSpec as empty -- which Validate reads as plain "not
// configured" rather than the misconfiguration it actually is. The gate
// then ran with no mount at all instead of failing loudly. This must now
// error.
func TestRunNamedGateActivityRejectsMountPathWithoutDir(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket:        "fixture-ticket",
			WorkspacePath: t.TempDir(),
			SandboxImage:  "factory-worker:test@sha256:deadbeef",
			SandboxDocker: docker,
			RunID:         "run-id",
			DataDir:       activities.DataDir,
			// Mount path set, dir deliberately left empty -- the
			// misconfiguration the bug silently ignored.
			ReferenceOracleMountPath: "verify",
		},
		Check:   "reference_oracle",
		Command: "true",
	}

	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return activities.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute reference_oracle gate Activity with mount-path set but dir unset: want error, got nil")
	}
	if !strings.Contains(err.Error(), "set together") {
		t.Errorf("error = %q, want it to name the set-together validation", err.Error())
	}
}

// TestRunNamedGateActivityRejectsOracleDirInsideWorkspace is the direct
// regression test for a second real P1 in the same review round: the
// TOCTOU fix (PR #152) snapshots ReferenceOracleDir before ever
// constructing a LaunchSpec, so by the time LaunchSpec.Validate's own
// containment check runs, s.ReferenceOracleDir already names the
// snapshot destination -- always outside the workspace by construction
// -- never the operator's real, originally-configured source. An
// operator-configured source that actually sits inside WorkspacePath
// therefore passed silently. This must now be rejected before ever
// snapshotting.
func TestRunNamedGateActivityRejectsOracleDirInsideWorkspace(t *testing.T) {
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	oracleDirInsideWorkspace := filepath.Join(workspace, "verify")
	if err := os.MkdirAll(oracleDirInsideWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	activities := &Activities{LogDir: t.TempDir(), SandboxDocker: docker, DataDir: t.TempDir()}
	input := NamedGateActivityInput{
		RunWorkflowInput: RunWorkflowInput{
			Ticket:                   "fixture-ticket",
			WorkspacePath:            workspace,
			SandboxImage:             "factory-worker:test@sha256:deadbeef",
			SandboxDocker:            docker,
			RunID:                    "run-id",
			DataDir:                  activities.DataDir,
			ReferenceOracleDir:       oracleDirInsideWorkspace,
			ReferenceOracleMountPath: "verify",
		},
		Check:   "reference_oracle",
		Command: "true",
	}

	wrapper := func(ctx context.Context, in NamedGateActivityInput) (VerifyActivityResult, error) {
		return activities.RunNamedGateActivity(ctx, in)
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	_, err := env.ExecuteActivity(wrapper, input)
	if err == nil {
		t.Fatal("execute reference_oracle gate Activity with ReferenceOracleDir inside WorkspacePath: want error, got nil")
	}
	if !strings.Contains(err.Error(), "must not be inside the workspace") {
		t.Errorf("error = %q, want it to name the containment rejection", err.Error())
	}
	// The rejection must fire BEFORE ever snapshotting -- no leftover
	// snapshot directory should exist.
	if _, statErr := os.Stat(filepath.Join(activities.LogDir, "reference-oracle-snapshot")); !os.IsNotExist(statErr) {
		t.Errorf("a snapshot was created despite the containment rejection: stat err = %v", statErr)
	}
}

func TestRunVerifyActivityDuplicateInvocationRunsSubprocessOnce(t *testing.T) {
	calls := 0
	var logPaths []string
	logPath := filepath.Join(t.TempDir(), "verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write verify log: %v", err)
	}
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, activityLogPath func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			logPaths = append(logPaths, activityLogPath(1))
			started := time.Now()
			return runner.Result{ExitCode: 0, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: logPath}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		if _, err := activities.RunVerifyActivity(ctx, input); err != nil {
			return err
		}
		_, err := activities.RunVerifyActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute duplicate verify Activity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("subprocess calls after duplicate invocation = %d, want 1", calls)
	}

	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute verify Activity with different ID: %v", err)
	}
	if calls != 2 {
		t.Fatalf("subprocess calls after different Activity ID = %d, want 2", calls)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("verify log paths for different Activity executions = %q, want distinct paths", logPaths[0])
	}
}

// TestRunFullSuiteVerifyActivityDuplicateInvocationRunsSubprocessOnce is
// TestRunVerifyActivityDuplicateInvocationRunsSubprocessOnce's counterpart
// for RunFullSuiteVerifyActivity (gap 3 of the plan's 2026-08-28 readiness
// review, the regression oracle): its checkpoint must key by this
// Activity's own ActivityID, not collide with RunVerifyActivity's.
func TestRunFullSuiteVerifyActivityDuplicateInvocationRunsSubprocessOnce(t *testing.T) {
	calls := 0
	var logPaths []string
	logPath := filepath.Join(t.TempDir(), "full_suite_verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write full-suite verify log: %v", err)
	}
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, activityLogPath func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			calls++
			logPaths = append(logPaths, activityLogPath(1))
			started := time.Now()
			return runner.Result{ExitCode: 0, StartedAt: started, FinishedAt: started.Add(time.Second), LogPath: logPath}, nil
		},
	}
	input := fixtureInput()
	input.FullSuiteCommand = "make verify-full"
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		if _, err := activities.RunFullSuiteVerifyActivity(ctx, input); err != nil {
			return err
		}
		_, err := activities.RunFullSuiteVerifyActivity(ctx, input)
		return err
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatalf("execute duplicate full-suite verify Activity: %v", err)
	}
	if calls != 1 {
		t.Fatalf("subprocess calls after duplicate invocation = %d, want 1", calls)
	}

	if _, err := env.ExecuteActivity(wrapper, input); err != nil {
		t.Fatalf("execute full-suite verify Activity with different ID: %v", err)
	}
	if calls != 2 {
		t.Fatalf("subprocess calls after different Activity ID = %d, want 2", calls)
	}
	if logPaths[0] == logPaths[1] {
		t.Fatalf("full-suite verify log paths for different Activity executions = %q, want distinct paths", logPaths[0])
	}
}

// TestRunVerifyActivityRecordsAttempts is
// TestRunBuildActivityRecordsAttempts' counterpart for the verify
// Activity's own onAttempt callback.
func TestRunVerifyActivityRecordsAttempts(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write verify log: %v", err)
	}
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(_ context.Context, _ string, _ func(int) string, _ int, onAttempt func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			started := time.Now()
			onAttempt(1, runner.Result{Command: []string{"attempt-1"}, ExitCode: -1, StartedAt: started, FinishedAt: started.Add(time.Second)}, context.DeadlineExceeded)
			finalResult := runner.Result{Command: []string{"attempt-2"}, ExitCode: 0, StartedAt: started, FinishedAt: started.Add(2 * time.Second), LogPath: logPath}
			onAttempt(2, finalResult, nil)
			return finalResult, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) (VerifyActivityResult, error) {
		return activities.RunVerifyActivity(ctx, input)
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	raw, err := env.ExecuteActivity(wrapper, fixtureInput())
	if err != nil {
		t.Fatalf("execute verify Activity: %v", err)
	}
	var result VerifyActivityResult
	if err := raw.Get(&result); err != nil {
		t.Fatalf("decode verify Activity result: %v", err)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("Attempts = %+v, want 2 entries", result.Attempts)
	}
	if result.Attempts[0].Kind != "verify" || result.Attempts[0].ExitCode != -1 {
		t.Errorf("Attempts[0] = %+v, want kind=verify exit_code=-1", result.Attempts[0])
	}
	if result.Attempts[1].Kind != "verify" || result.Attempts[1].ExitCode != 0 {
		t.Errorf("Attempts[1] = %+v, want kind=verify exit_code=0", result.Attempts[1])
	}
}

func TestRunVerifyActivityHeartbeatsDuringSubprocess(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "verify.log")
	if err := os.WriteFile(logPath, []byte("verified\n"), 0o644); err != nil {
		t.Fatalf("write verify log: %v", err)
	}
	var heartbeats atomic.Int32
	activities := &Activities{
		LogDir: t.TempDir(),
		runWithRetries: func(ctx context.Context, _ string, _ func(int) string, _ int, _ func(int, runner.Result, error), _ string, _ ...string) (runner.Result, error) {
			for i := 0; i < 5; i++ {
				activity.RecordHeartbeat(ctx)
				heartbeats.Add(1)
				time.Sleep(time.Millisecond)
			}
			started := time.Now()
			return runner.Result{ExitCode: 0, StartedAt: started, FinishedAt: started.Add(time.Millisecond), LogPath: logPath}, nil
		},
	}
	wrapper := func(ctx context.Context, input RunWorkflowInput) error {
		_, err := activities.RunVerifyActivity(ctx, input)
		return err
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(wrapper)
	if _, err := env.ExecuteActivity(wrapper, fixtureInput()); err != nil {
		t.Fatalf("execute verify Activity: %v", err)
	}
	if heartbeats.Load() == 0 {
		t.Fatal("no heartbeats recorded during the verify subprocess")
	}
}
