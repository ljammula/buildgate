package sandbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"buildgate/internal/progress"
	"buildgate/internal/run"
)

func stepSpec(t *testing.T) LaunchSpec {
	t.Helper()
	spec := runtimeSpec(t)
	spec.LogPath = filepath.Join(run.Dir(spec.DataDir, spec.RunID), "launch-1.log")
	spec.ProgressPath = progress.Path(spec.DataDir, spec.RunID)
	return spec
}

// TestRunThroughRuntimeMarksTheSandboxStep: a launch writes the sandbox step's
// start before it creates the sandbox and its end once the command's output
// file appears, naming the launch log relative to the run directory; a launch
// whose sandbox cannot be created ends the step as failed.
func TestRunThroughRuntimeMarksTheSandboxStep(t *testing.T) {
	spec := stepSpec(t)
	rt := &workerRuntime{lines: []string{"building"}, startedAt: "2026-10-04T10:00:00Z"}
	if _, _, err := RunThroughRuntime(context.Background(), rt, spec, fastLaunch()); err != nil {
		t.Fatal(err)
	}
	events, err := progress.Read(spec.ProgressPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Stage != "sandbox" || events[0].Event != progress.StepStart || events[1].Event != progress.StepEnd || events[1].Outcome != "pass" || events[1].Log != "launch-1.log" || events[1].Source != "factory" {
		t.Fatalf("feed = %+v, want sandbox step_start then step_end with the log", events)
	}

	failing := stepSpec(t)
	failing.RunID += "-b"
	failing.ProgressPath = progress.Path(failing.DataDir, failing.RunID)
	failing.LogPath = filepath.Join(run.Dir(failing.DataDir, failing.RunID), "launch-1.log")
	_, _, err = RunThroughRuntime(context.Background(), &workerRuntime{createErr: errors.New("gateway refused")}, failing, fastLaunch())
	if err == nil {
		t.Fatal("a refused create succeeded")
	}
	events, _ = progress.Read(failing.ProgressPath)
	if len(events) != 2 || events[1].Event != progress.StepEnd || events[1].Outcome != "fail" {
		t.Errorf("feed after a refused create = %+v, want a failed step end", events)
	}

	// A log outside the run directory is not named.
	outside := stepSpec(t)
	outside.LogPath = filepath.Join(t.TempDir(), "elsewhere.log")
	if got := relativeToRunDir(outside); got != "" {
		t.Errorf("relativeToRunDir(outside) = %q, want none", got)
	}
}
