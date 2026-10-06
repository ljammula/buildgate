package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"buildgate/internal/workflow"
)

// TestRunOptionsActivitiesCarriesEveryWorkerSetting gives every option the
// worker's Activities value reads its own distinct value and compares the
// whole struct, so an option wired to the wrong field (two limits swapped)
// fails here: the golden RunWorkflowInput does not carry the sandbox limits.
func TestRunOptionsActivitiesCarriesEveryWorkerSetting(t *testing.T) {
	opts := runOptions{
		DataDir:                "/data",
		ID:                     "run-1",
		BuildAppInterpreter:    "interpreter",
		BuildAppScript:         "script",
		BuildAppMaxAttempts:    3,
		ConformityPolicy:       "advisory",
		MaxRounds:              7,
		TimeoutMinutes:         11,
		VerifyCommand:          "verify",
		VerifyMaxAttempts:      4,
		FastCheckCommand:       "fast",
		SpecAcceptanceCriteria: "criteria",
		SandboxImage:           "image",
		SandboxDocker:          "docker",
		SandboxUser:            "1:2",
		SandboxWorkerUID:       1234,
		SandboxMemory:          "2g",
		SandboxCPUs:            "1.5",
		SandboxTmpfsSize:       "3g",
		ModelHostConcurrency:   2,
		Relay:                  modelRouteOptions{CABundlePath: "/ca.pem"},
		ComposeServices:        composeServicesOptions{WorkerEnvironment: map[string]string{"K": "V"}},
	}
	want := &workflow.Activities{
		BuildAppInterpreter:      "interpreter",
		BuildAppScript:           "script",
		BuildMaxAttempts:         3,
		ConformityPolicy:         "advisory",
		MaxRounds:                7,
		TimeoutMinutes:           11,
		VerifyCommand:            "verify",
		VerifyMaxAttempts:        4,
		FastCheckCommand:         "fast",
		SpecAcceptanceCriteria:   "criteria",
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
	}
	if got := opts.activities(); !reflect.DeepEqual(got, want) {
		t.Errorf("activities() =\n%+v\nwant\n%+v", got, want)
	}
}
