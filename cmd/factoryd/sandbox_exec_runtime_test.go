package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

// runtimeJobWorkspace is a Git worktree and data directory for a job
// launched through a fake sandbox runtime.
func runtimeJobWorkspace(t *testing.T) (workspace, dataDir string, logPath func(int) string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace, dataDir = filepath.Join(root, "workspace"), filepath.Join(root, "data")
	for _, dir := range []string{workspace, dataDir} {
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", workspace, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	return workspace, dataDir, func(int) string { return filepath.Join(root, "logs", "job.log") }
}

// A docker binary that fails: a launch through the runtime never reaches it.
const noDocker = "/usr/bin/false"

func TestRunSandboxWithRetriesViaLaunchesThroughTheSandboxRuntime(t *testing.T) {
	workspace, dataDir, logPath := runtimeJobWorkspace(t)
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"drafted"}, ExitCode: 2}
	res, err := runSandboxWithRetriesVia(
		rt, filepath.Join(dataDir, "ledgers"), context.Background(), workspace, "", "", "", logPath, 1,
		"worker@sha256:2222222222222222222222222222222222222222222222222222222222222222", noDocker, sandboxTestUser(), sandbox.DefaultWorkerUID,
		"job-1", dataDir, "4g", "2", "256m", nil,
		nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", []string{"K=V"}, nil, "sh", "-c", "true",
	)
	if err != nil {
		t.Fatalf("runSandboxWithRetriesVia: %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("exit code = %d, want 2", res.ExitCode)
	}
	if logged, err := os.ReadFile(logPath(1)); err != nil || string(logged) != "drafted\n" {
		t.Errorf("log = %q, %v", logged, err)
	}
	requests := rt.Requests()
	if len(requests) != 1 || requests[0].RunID != "job-1" || requests[0].Memory != "4g" || requests[0].Route != nil {
		t.Fatalf("requests = %+v", requests)
	}
	if deleted := rt.Deleted(); len(deleted) != 1 || deleted[0] != requests[0].Name {
		t.Errorf("deleted = %v, want the launched sandbox", deleted)
	}
}

func TestRunSandboxWithRetriesViaDoesNotRetryACommandStartedTwice(t *testing.T) {
	workspace, dataDir, logPath := runtimeJobWorkspace(t)
	rt := &sandboxtest.WorkerRuntime{Lines: []string{"x"}, ExitCode: sandbox.WorkerExitRerun}
	_, err := runSandboxWithRetriesVia(
		rt, filepath.Join(dataDir, "ledgers"), context.Background(), workspace, "", "", "", logPath, 3,
		"worker@sha256:2222222222222222222222222222222222222222222222222222222222222222", noDocker, sandboxTestUser(), sandbox.DefaultWorkerUID,
		"job-1", dataDir, "4g", "2", "256m", nil,
		nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
		"", "", nil, nil, "sh", "-c", "true",
	)
	if !errors.Is(err, sandbox.ErrSandboxRerun) {
		t.Fatalf("err = %v, want ErrSandboxRerun", err)
	}
	if n := len(rt.Requests()); n != 1 {
		t.Errorf("launches = %d, want 1", n)
	}
}
