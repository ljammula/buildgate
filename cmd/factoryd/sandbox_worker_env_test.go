package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/harness"
	"buildgate/internal/sandbox"
)

// TestRunSandboxWithRetriesPassesTheWorkerEnvironmentItIsGiven: the sandbox
// launch's environment is exactly the workerEnv the caller resolved from the
// job's own harness -- pifork's for a pifork job, nothing extra for pi or
// for a job with no harness (verify, gates) -- never read back from the argv.
func TestRunSandboxWithRetriesPassesTheWorkerEnvironmentItIsGiven(t *testing.T) {
	pifork, err := harness.Lookup("pifork")
	if err != nil {
		t.Fatal(err)
	}
	pi, err := harness.Lookup("pi")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		env     []string
		wantEnv bool
	}{
		{"pifork job", pifork.WorkerEnv, true},
		{"pi job", pi.WorkerEnv, false},
		{"job without a harness", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argvPath := filepath.Join(t.TempDir(), "argv.txt")
			docker := filepath.Join(t.TempDir(), "docker-fake")
			script := "#!/bin/sh\n{\n  printf '=== %s\\n' \"$1\"\n  for a in \"$@\"; do printf '%s\\n' \"$a\"; done\n} >> \"" + argvPath + "\"\nexit 0\n"
			if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			logDir := t.TempDir()
			logPath := func(int) string { return filepath.Join(logDir, "job.log") }
			_, err := runSandboxWithRetries(
				context.Background(), t.TempDir(), "", "", "", logPath, 1,
				"worker@sha256:2222222222222222222222222222222222222222222222222222222222222222", docker, sandboxTestUser(), sandbox.DefaultWorkerUID,
				"worker-env-run", t.TempDir(), "4g", "2", "256m", nil,
				nil, nil, sandbox.RegistryProxyHooks{}, nil, sandbox.ComposeServicesHooks{},
				"", "", tc.env, nil, "sh", "-c", "true",
			)
			if err != nil {
				t.Fatalf("runSandboxWithRetries: %v", err)
			}
			argv, err := os.ReadFile(argvPath)
			if err != nil {
				t.Fatal(err)
			}
			got := string(argv)
			if has := strings.Contains(got, "PI_CODING_AGENT_DIR=/home/worker/.pi/agent"); has != tc.wantEnv {
				t.Errorf("docker argv has PI_CODING_AGENT_DIR = %v, want %v:\n%s", has, tc.wantEnv, got)
			}
		})
	}
}
