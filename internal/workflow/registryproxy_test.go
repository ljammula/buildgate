package workflow

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"buildgate/internal/sandbox"
)

const testRegistryProxyImage = "registry.example/registry-proxy@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func testRegistryProxyPolicy() *sandbox.RegistryProxyPolicy {
	return &sandbox.RegistryProxyPolicy{
		Image:                 testRegistryProxyImage,
		Routes:                sandbox.DefaultRegistryProxyRoutes(),
		CacheBytes:            1 << 30,
		MaxObjectBytes:        256 << 20,
		MaxConcurrentUpstream: 16,
		UpstreamTimeout:       time.Minute,
	}
}

// registryProxyFixtureInput is relayFixtureInput plus a registry proxy
// policy. The worker identity names this process's own gid so
// sandbox.EnsureScratchDir's chown succeeds for a non-root test runner.
func registryProxyFixtureInput(t *testing.T, dockerBinary, dataDir string) RunWorkflowInput {
	t.Helper()
	input := relayFixtureInput(t, dockerBinary, dataDir)
	input.RunID = "registryproxy-run"
	input.SandboxUser = fmt.Sprintf("65532:%d", os.Getgid())
	input.RegistryProxyPolicy = testRegistryProxyPolicy()
	input.VerifyCommand = "true"
	input.FullSuiteCommand = "true"
	return input
}

// TestRegistryProxyAppliesToEveryPhaseAndIsCleanedUp is the Temporal-path
// counterpart of cmd/factoryd's `sandboxed` closure: a run that declares a
// registry proxy gets one for the build AND for verification (the gate
// compiles the same dependency graph the build did -- found live
// 2026-09-10), each Activity starting exactly one
// proxy and tearing it down once.
func TestRegistryProxyAppliesToEveryPhaseAndIsCleanedUp(t *testing.T) {
	dataDir := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "docker-argv")
	docker := fakeDockerBinary(t, "printf '%s\\n' \"$@\" >> "+argvLog+"\nprintf -- '--- end\\n' >> "+argvLog+"\nexit 0\n")
	var events []string
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.registryProxyHooks = sandbox.RegistryProxyHooks{
		StartRegistryProxy: func(_ context.Context, _ string, spec sandbox.RegistryProxySpec) (*sandbox.RegistryProxyHandle, error) {
			events = append(events, "proxy-start:"+spec.ExistingInternalNetwork)
			if spec.Image != testRegistryProxyImage {
				t.Errorf("registry proxy image = %q, want the request-scoped policy's image", spec.Image)
			}
			network := "factoryd-registryproxy-test"
			if spec.ExistingInternalNetwork != "" {
				network = spec.ExistingInternalNetwork
			}
			return &sandbox.RegistryProxyHandle{
				WorkerBaseURL: "http://registry-proxy:8092",
				AuditFacts:    sandbox.RegistryProxyLaunchFacts{NetworkName: network, WorkerBaseURL: "http://registry-proxy:8092"},
			}, nil
		},
		CleanupRegistryProxy: func(context.Context, *sandbox.RegistryProxyHandle) error {
			events = append(events, "proxy-cleanup")
			return nil
		},
	}
	input := registryProxyFixtureInput(t, docker, dataDir)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	env.RegisterActivity(activities.RunVerifyActivity)
	if _, err := env.ExecuteActivity(activities.RunBuildActivity, input); err != nil {
		t.Fatalf("RunBuildActivity: %v", err)
	}
	if _, err := env.ExecuteActivity(activities.RunVerifyActivity, input); err != nil {
		t.Fatalf("RunVerifyActivity: %v", err)
	}

	want := []string{
		// Build, then verify: each on the proxy's own network.
		"proxy-start:", "proxy-cleanup",
		"proxy-start:", "proxy-cleanup",
	}
	if strings.Join(events, " ") != strings.Join(want, " ") {
		t.Fatalf("lifecycle events = %v, want %v", events, want)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read docker argv log: %v", err)
	}
	// Only the worker launches: sandbox.Run also probes the workspace mount
	// with a throwaway container of its own.
	var launches []string
	for _, launch := range strings.Split(string(argv), "--- end\n") {
		if strings.Contains(launch, "factoryd-temporal-worker-") {
			launches = append(launches, launch)
		}
	}
	if len(launches) != 2 {
		t.Fatalf("worker launches = %d, want 2 (build, verify); argv log:\n%s", len(launches), argv)
	}
	// Each launch mounts its own scratch directory, named for its
	// container, so verify never sees the build's Go caches, and each is
	// removed once its container exits (sandbox.RunScratchDir).
	var mounts []string
	for i, network := range []string{"factoryd-registryproxy-test", "factoryd-registryproxy-test"} {
		name := launchArg(launches[i], "--name")
		scratch := filepath.Join(sandbox.RunScratchDir(dataDir, input.RunID), name)
		scratchMount := "--volume\n" + scratch + ":" + sandbox.WorkerScratchMount + ":rw\n"
		for _, needle := range []string{"--network\n" + network + "\n", "npm_config_registry=http://registry-proxy:8092/npm/\n", "GOPROXY=http://registry-proxy:8092/gomodproxy\n", scratchMount} {
			if !strings.Contains(launches[i], needle) {
				t.Errorf("launch %d missing %q; argv:\n%s", i, needle, launches[i])
			}
		}
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Errorf("launch %d's scratch directory %s outlived its container: %v", i, scratch, err)
		}
		mounts = append(mounts, scratch)
	}
	if mounts[0] == mounts[1] {
		t.Errorf("build and verify shared one scratch directory %s; verify must never reuse the build's caches", mounts[0])
	}
}

// launchArg returns the value following flag in one launch's argv log
// (one argument per line).
func launchArg(launch, flag string) string {
	lines := strings.Split(launch, "\n")
	for i, line := range lines {
		if line == flag && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

// TestRegistryProxyMissingImageFailsBeforeAnyLaunch proves the fail-closed
// half: a declared proxy whose image is missing halts each sandboxed
// Activity with an InfrastructureFailure before a proxy is started or a
// container is launched, never silently running unproxied.
func TestRegistryProxyMissingImageFailsBeforeAnyLaunch(t *testing.T) {
	dockerMarker := filepath.Join(t.TempDir(), "docker-invoked")
	docker := fakeDockerBinary(t, "touch "+dockerMarker+"\nexit 0\n")
	activities := testRoutedActivities(sandbox.NewRouteSecret("worker-static-upstream-key"))
	activities.registryProxyHooks = sandbox.RegistryProxyHooks{
		StartRegistryProxy: func(context.Context, string, sandbox.RegistryProxySpec) (*sandbox.RegistryProxyHandle, error) {
			t.Error("a registry proxy was started despite a missing image")
			return nil, nil
		},
	}
	input := registryProxyFixtureInput(t, docker, t.TempDir())
	input.RegistryProxyPolicy.Image = ""

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	env.RegisterActivity(activities.RunBuildActivity)
	env.RegisterActivity(activities.RunVerifyActivity)
	env.RegisterActivity(activities.RunFullSuiteVerifyActivity)
	for name, execute := range map[string]func() error{
		"build":      func() error { _, err := env.ExecuteActivity(activities.RunBuildActivity, input); return err },
		"verify":     func() error { _, err := env.ExecuteActivity(activities.RunVerifyActivity, input); return err },
		"full-suite": func() error { _, err := env.ExecuteActivity(activities.RunFullSuiteVerifyActivity, input); return err },
	} {
		err := execute()
		if err == nil {
			t.Fatalf("%s: Activity accepted a registry proxy with no image", name)
		}
		if !hasApplicationErrorType(err, InfrastructureFailureType) {
			t.Fatalf("%s: error = %v, want application error type %q", name, err, InfrastructureFailureType)
		}
		if !strings.Contains(err.Error(), "registry proxy image must be pinned") {
			t.Fatalf("%s: error = %q, want it to name the missing image", name, err.Error())
		}
	}
	if _, err := os.Stat(dockerMarker); err == nil {
		t.Fatal("docker was invoked despite an unusable registry proxy configuration")
	}
}
