package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// TestRunOptionsActivitiesCarriesEveryWorkerSetting gives every option the
// worker's Activities value reads its own distinct value and compares the
// whole struct, so an option wired to the wrong field (two limits swapped)
// fails here: the golden RunWorkflowInput does not carry the sandbox limits.
func TestRunOptionsActivitiesCarriesEveryWorkerSetting(t *testing.T) {
	opts := runOptions{
		DataDir:              "/data",
		ID:                   "run-1",
		BuildAppInterpreter:  "interpreter",
		BuildAppScript:       "script",
		BuildAppMaxAttempts:  3,
		MaxRounds:            7,
		TimeoutMinutes:       11,
		VerifyCommand:        "verify",
		VerifyMaxAttempts:    4,
		FastCheckCommand:     "fast",
		SandboxImage:         "image",
		SandboxDocker:        "docker",
		SandboxUser:          "1:2",
		SandboxWorkerUID:     1234,
		SandboxMemory:        "2g",
		SandboxCPUs:          "1.5",
		SandboxTmpfsSize:     "3g",
		ModelHostConcurrency: 2,
		ApprovalDataDir:      "/operator-data",
		InstallCacheImages:   5,
		Relay:                modelRouteOptions{CABundlePath: "/ca.pem"},
		ComposeServices:      composeServicesOptions{WorkerEnvironment: map[string]string{"K": "V"}},
	}
	want := &workflow.Activities{
		BuildAppInterpreter:      "interpreter",
		BuildAppScript:           "script",
		BuildMaxAttempts:         3,
		MaxRounds:                7,
		TimeoutMinutes:           11,
		VerifyCommand:            "verify",
		VerifyMaxAttempts:        4,
		FastCheckCommand:         "fast",
		SandboxImage:             "image",
		SandboxDocker:            "docker",
		SandboxUser:              "1:2",
		SandboxWorkerUID:         1234,
		SandboxMemory:            "2g",
		SandboxCPUs:              "1.5",
		SandboxTmpfsSize:         "3g",
		ComposeServicesWorkerEnv: map[string]string{"K": "V"},
		ModelHostConcurrency:     2,
		EgressCABundlePath:       "/ca.pem",
		DataDir:                  "/data",
		LogDir:                   filepath.Join("/data", "runs", "run-1"),
		CheckpointDir:            filepath.Join("/data", "temporal-checkpoints", "run-1"),
		SandboxApprovalsDataDir:  "/operator-data",
		InstallCacheImages:       5,
		InstallLockDir:           installLockDir(),
	}
	if got := opts.activities(); !reflect.DeepEqual(got, want) {
		t.Errorf("activities() =\n%+v\nwant\n%+v", got, want)
	}
}

// The hard-termination rollback is handed a checkpoint dir only and must
// find the same run's directory from it.
func TestRunDirOfCheckpointDirIsTheRunsOwnDirectory(t *testing.T) {
	opts := runOptions{DataDir: filepath.Join("some", "data"), ID: "ticket-20261008-1"}
	if got, want := runDirOfCheckpointDir(opts.checkpointDir()), run.Dir(opts.DataDir, opts.ID); got != want {
		t.Errorf("runDirOfCheckpointDir = %q, want %q", got, want)
	}
}

// TestRunThatMayChallengeGetsTheChallengePhasesTime: a run's deadline is
// -timeout-minutes plus five minutes, and a run that may challenge (a first
// build with -challenge, or a challenge round given held files) has the
// challenge phase's own time on top, so a build that used its whole budget is
// not halted by the phase that follows review. An explicit -timeout stands.
func TestRunThatMayChallengeGetsTheChallengePhasesTime(t *testing.T) {
	base := 50 * time.Minute
	for _, tc := range []struct {
		name string
		opts runOptions
		want time.Duration
	}{
		{"no challenge", runOptions{TimeoutMinutes: 45}, base},
		{"first build with -challenge", runOptions{TimeoutMinutes: 45, Challenge: true}, base + workflow.ChallengePhaseTime(45)},
		{"challenge round", runOptions{TimeoutMinutes: 45, ChallengeHeld: []run.ChallengeFile{{Path: "a_test.go"}}}, base + workflow.ChallengePhaseTime(45)},
		{"explicit -timeout", runOptions{TimeoutMinutes: 45, Challenge: true, OverallTimeout: 20 * time.Minute}, 20 * time.Minute},
	} {
		if got := tc.opts.overallTimeout(); got != tc.want {
			t.Errorf("%s: overallTimeout() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
