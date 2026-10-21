package workflow

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/progress"
	"buildgate/internal/sandbox"
)

// TestInstallStepMarkedOnFirstAttemptOnly: the first attempt of a step that
// starts a registry proxy writes the install step's start and end to the run's
// feed, naming what started; a second attempt starts the proxy again without a
// second pair, and a start that fails ends the step as failed.
func TestInstallStepMarkedOnFirstAttemptOnly(t *testing.T) {
	dataDir := t.TempDir()
	started, failNext := 0, false
	hooks := sandbox.RegistryProxyHooks{
		StartRegistryProxy: func(_ context.Context, _ string, spec sandbox.RegistryProxySpec) (*sandbox.RegistryProxyHandle, error) {
			started++
			if failNext {
				return nil, errors.New("proxy image missing")
			}
			return &sandbox.RegistryProxyHandle{WorkerBaseURL: "http://registry-proxy:8092", AuditFacts: sandbox.RegistryProxyLaunchFacts{NetworkName: "n", WorkerBaseURL: "http://registry-proxy:8092"}}, nil
		},
		CleanupRegistryProxy: func(context.Context, *sandbox.RegistryProxyHandle) error { return nil },
	}
	input := registryProxyFixtureInput(t, fakeDockerBinary(t, "exit 0\n"), dataDir)
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	spec, err := activities.registryProxySpecFor(input)
	if err != nil || spec == nil {
		t.Fatalf("registryProxySpecFor = %v, %v", spec, err)
	}
	proxy, err := sandbox.BeginRegistryProxyLifecycle(*spec, input.SandboxDocker, input.RunID, dataDir, hooks)
	if err != nil {
		t.Fatalf("BeginRegistryProxyLifecycle: %v", err)
	}
	defer func() { _ = proxy.Cleanup() }()

	ctx := forBuildLaunch(context.Background())
	if err := activities.ensureSidecars(ctx, input, 1, proxy, nil); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if err := activities.ensureSidecars(ctx, input, 2, proxy, nil); err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	events, err := progress.Read(progress.PathInDir(input.LogDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Stage != "install" || events[0].Event != progress.StepStart || events[1].Event != progress.StepEnd || events[1].Outcome != "pass" || events[1].Detail != "registry proxy" {
		t.Fatalf("feed after two attempts = %+v, want one install pair naming the registry proxy", events)
	}
	steps := progress.StartupStepsFrom(events)
	if steps[2].Name != "install" || steps[2].Status != progress.StepDone {
		t.Errorf("install step = %+v, want done", steps[2])
	}

	// No sidecars at all: nothing to install, nothing marked.
	if err := activities.ensureSidecars(ctx, input, 1, nil, nil); err != nil {
		t.Fatal(err)
	}
	if again, _ := progress.Read(progress.PathInDir(input.LogDir)); len(again) != 2 {
		t.Errorf("a step with no sidecars wrote %d more lines", len(again)-2)
	}

	// A proxy that cannot start fails the step and returns the error.
	failNext = true
	proxy.Abandon()
	failing, err := sandbox.BeginRegistryProxyLifecycle(*spec, input.SandboxDocker, input.RunID, dataDir, hooks)
	if err != nil {
		t.Fatal(err)
	}
	if err := activities.ensureSidecars(ctx, input, 1, failing, nil); err == nil || !strings.Contains(err.Error(), "start registry proxy") {
		t.Fatalf("failing start returned %v", err)
	}
	after, _ := progress.Read(filepath.Join(input.LogDir, progress.FileName))
	last := after[len(after)-1]
	if last.Event != progress.StepEnd || last.Outcome != "fail" || !strings.Contains(last.Detail, "proxy image missing") {
		t.Errorf("last line = %+v, want a failed install end naming the cause", last)
	}
}
