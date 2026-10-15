package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"buildgate/internal/sandbox"
	"buildgate/internal/sandbox/sandboxtest"
)

// TestDoctorCheckSandboxGoBuildLaunchesLikeABuild runs the probe through a
// worker-like runtime: it is launched with the scratch mount and Go
// environment a build has, a failing probe reports the refused-access line
// of its log, and nothing it made under the data dir is left behind.
func TestDoctorCheckSandboxGoBuildLaunchesLikeABuild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rt      *sandboxtest.WorkerRuntime
		wantErr string
	}{
		{"the build passes", &sandboxtest.WorkerRuntime{Lines: []string{"200"}}, ""},
		{"the worker is refused its own folder", &sandboxtest.WorkerRuntime{ExitCode: 1, Lines: []string{
			"go: downloading nothing",
			"runtime: open /scratch/go-tmp/go-build1/b001/importcfg: permission denied",
			"probe: go build failed with GOTMPDIR=/scratch/go-tmp",
		}}, "exit 1: runtime: open /scratch/go-tmp/go-build1/b001/importcfg: permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dp := newTestDeps(t)
			fakeSandboxOf(dp).runtimeFn = func() sandbox.Runtime { return tc.rt }
			dataDir := t.TempDir()
			t.Setenv("FAKE_DOCKER_CONTAINER_DIR", t.TempDir())
			in := doctorInputs{
				sandboxImage:  "worker@sha256:2222222222222222222222222222222222222222222222222222222222222222",
				sandboxDocker: fakeSandboxDockerBinary(t),
				dataDir:       dataDir,
				// The session config's defaults.
				sandboxTmpfsSize: "1g",
			}
			in.settings.SandboxUser = sandboxTestUser()
			in.settings.SandboxMemory, in.settings.SandboxCPUs = "4g", "2"
			check := doctorCheckSandboxGoBuild(dp, context.Background(), in)
			if tc.wantErr == "" && check.Err != nil {
				t.Fatalf("Err = %v, want a pass", check.Err)
			}
			if tc.wantErr != "" {
				if check.Err == nil || check.Err.Error() != tc.wantErr {
					t.Fatalf("Err = %v, want %q", check.Err, tc.wantErr)
				}
				if !strings.Contains(check.Fix, ".factory.yml") {
					t.Errorf("Fix = %q, want it to name /scratch", check.Fix)
				}
			}
			requests := tc.rt.Requests()
			if len(requests) != 1 {
				t.Fatalf("requests = %d, want one launch", len(requests))
			}
			if !slices.Contains(requests[0].Environment, "GOTMPDIR="+sandbox.WorkerScratchMount+"/go-tmp") {
				t.Errorf("environment = %v, want GOTMPDIR under the scratch mount", requests[0].Environment)
			}
			if !slices.Contains(requests[0].ReadWritePaths, sandbox.WorkerScratchMount) {
				t.Errorf("read-write paths = %v, want the scratch mount", requests[0].ReadWritePaths)
			}
			for _, sub := range []string{"scratch", "runs"} {
				if entries, _ := os.ReadDir(filepath.Join(dataDir, sub)); len(entries) != 0 {
					t.Errorf("%s holds %v after the check, want it emptied", sub, entries)
				}
			}
			if _, err := os.Stat(filepath.Join(dataDir, "doctor-probe")); err == nil {
				t.Error("doctor-probe is left in the data dir")
			}
		})
	}
}

// TestDoctorCheckSandboxGoBuildNeedsASharedDataDir: a data dir the Docker
// VM does not share fails with that cause and launches no sandbox.
func TestDoctorCheckSandboxGoBuildNeedsASharedDataDir(t *testing.T) {
	dp := newTestDeps(t)
	rt := &sandboxtest.WorkerRuntime{}
	fakeSandboxOf(dp).runtimeFn = func() sandbox.Runtime { return rt }
	check := doctorCheckSandboxGoBuild(dp, context.Background(), doctorInputs{
		sandboxImage:  "worker@sha256:2222222222222222222222222222222222222222222222222222222222222222",
		sandboxDocker: noDocker, dataDir: t.TempDir(),
	})
	if check.Err == nil || !strings.HasPrefix(check.Err.Error(), "not run: ") || !strings.Contains(check.Fix, "-data-dir") {
		t.Fatalf("check = %+v, want the data dir sharing failure", check)
	}
	if len(rt.Requests()) != 0 {
		t.Error("a sandbox was launched for an unshared data dir")
	}
}

// TestDoctorSandboxGoBuildApplies: the probe needs the gateway up, an image
// and an existing data dir, and never creates the data dir.
func TestDoctorSandboxGoBuildApplies(t *testing.T) {
	up := []doctorCheck{{Name: doctorOpenShellGatewayCheckName}}
	down := []doctorCheck{{Name: doctorOpenShellGatewayCheckName, Err: os.ErrClosed}}
	dir := t.TempDir()
	for _, tc := range []struct {
		name      string
		openShell []doctorCheck
		in        doctorInputs
		want      bool
	}{
		{"gateway up", up, doctorInputs{sandboxImage: "img", dataDir: dir}, true},
		{"gateway down", down, doctorInputs{sandboxImage: "img", dataDir: dir}, false},
		{"no gateway row", nil, doctorInputs{sandboxImage: "img", dataDir: dir}, false},
		{"no image", up, doctorInputs{dataDir: dir}, false},
		{"no data dir yet", up, doctorInputs{sandboxImage: "img", dataDir: filepath.Join(dir, "missing")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := doctorSandboxGoBuildApplies(tc.openShell, tc.in); got != tc.want {
				t.Errorf("applies = %v, want %v", got, tc.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "missing")); err == nil {
		t.Error("the missing data dir was created")
	}
}
