package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"buildgate/internal/dockerbackend"
)

// stubBackend makes dp's Docker the given backend.
func stubBackend(dp *deps, b dockerbackend.Backend) {
	fakeDockerOf(dp).backendFn = func(context.Context) dockerbackend.Detection {
		return dockerbackend.Detection{Backend: b, Facts: dockerbackend.Facts{Context: "ctx", Endpoint: "unix:///sock"}}
	}
}

// TestRealDockerBackendReadsContextEndpointAndInfo drives backend against a
// docker on disk that answers as Docker Engine on a Linux host.
func TestRealDockerBackendReadsContextEndpointAndInfo(t *testing.T) {
	dp := newTestDeps(t)
	docker := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
case "$1 $2" in
"info --format") echo '{"Name":"build-01","OSType":"linux","ServerVersion":"28.5.1","SecurityOptions":["name=apparmor"]}' ;;
"context show") echo default ;;
"context inspect") echo unix:///var/run/docker.sock ;;
*) exit 2 ;;
esac
`
	if err := writeExecutable(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeDockerOf(dp).dockerBinaryFn = func() string { return docker }
	t.Setenv("DOCKER_HOST", "")

	d := realDocker{dp: dp}.backend(context.Background())
	if d.Err != nil {
		t.Fatal(d.Err)
	}
	if d.Facts.Context != "default" || d.Facts.Endpoint != "unix:///var/run/docker.sock" || d.Facts.GOOS != runtime.GOOS {
		t.Errorf("facts = %+v", d.Facts)
	}
	want := dockerbackend.Other
	if runtime.GOOS == "linux" {
		want = dockerbackend.NativeLinux
	}
	if d.Backend != want {
		t.Errorf("backend = %q, want %q", d.Backend, want)
	}

	t.Setenv("DOCKER_HOST", "unix:///home/op/.colima/default/docker.sock")
	if d := (realDocker{dp: dp}).backend(context.Background()); d.Facts.Endpoint != "unix:///home/op/.colima/default/docker.sock" || d.Backend != dockerbackend.Colima {
		t.Errorf("with DOCKER_HOST: %+v", d)
	}

	fakeDockerOf(dp).dockerBinaryFn = func() string { return filepath.Join(t.TempDir(), "no-docker") }
	if d := (realDocker{dp: dp}).backend(context.Background()); d.Err == nil || d.Backend != "" || d.Refused() != nil {
		t.Errorf("an unreachable Docker = %+v, want an error and no refusal", d)
	}
}

func TestDoctorCheckDockerBackend(t *testing.T) {
	if _, ok := doctorCheckDockerBackend(dockerbackend.Detection{Err: errors.New("down")}); ok {
		t.Error("a row for a Docker that did not answer")
	}
	c, ok := doctorCheckDockerBackend(dockerbackend.Detection{Backend: dockerbackend.Colima})
	if !ok || c.Err != nil || c.Name != "docker backend: colima" {
		t.Errorf("colima = %+v", c)
	}
	c, _ = doctorCheckDockerBackend(dockerbackend.Detection{Backend: dockerbackend.NativeLinux})
	if c.Err == nil || !c.Advisory {
		t.Errorf("native-linux = %+v, want an advisory warning", c)
	}
	c, _ = doctorCheckDockerBackend(dockerbackend.Detection{Backend: dockerbackend.Snap})
	if c.Err == nil || c.Advisory || !strings.Contains(c.Fix, "apt repository") {
		t.Errorf("snap = %+v, want a failure with its reason", c)
	}
}

// TestGatewayRuntimeRefusesARefusedBackend: every launch connects through
// Ready first, which refuses a backend buildgate does not run sandboxes on
// before asking the meter or the gateway anything.
func TestGatewayRuntimeRefusesARefusedBackend(t *testing.T) {
	dp := newTestDeps(t)
	asked := false
	fakeSandboxOf(dp).meterHealthyFn = func(context.Context) error { asked = true; return errors.New("down") }
	for _, b := range []dockerbackend.Backend{dockerbackend.Rootless, dockerbackend.DockerDesktop, dockerbackend.Other} {
		stubBackend(dp, b)
		err := newGatewayRuntime(dp).Ready(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refusing to run sandboxes") || !strings.Contains(err.Error(), string(b)) {
			t.Errorf("%s: Ready = %v, want the refusal", b, err)
		}
	}
	if asked {
		t.Error("the meter was asked after a refusal")
	}
	fakeDockerOf(dp).backendFn = func(context.Context) dockerbackend.Detection {
		return dockerbackend.Detection{Err: errors.New("docker info: timed out")}
	}
	if err := newGatewayRuntime(dp).Ready(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out") || asked {
		t.Errorf("an unread backend: Ready = %v, meter asked %v; want a refusal before the meter", err, asked)
	}
	stubBackend(dp, dockerbackend.NativeLinux)
	if err := newGatewayRuntime(dp).Ready(context.Background()); err == nil || !asked {
		t.Errorf("native-linux: Ready = %v, meter asked %v; want it to go on to the meter", err, asked)
	}
}

func TestStartStackRefusesARefusedBackend(t *testing.T) {
	dp := newTestDeps(t)
	stubBackend(dp, dockerbackend.Podman)
	err := realSandboxRuntime{dp: dp}.startStack(context.Background(), io.Discard, "meter@sha256:"+strings.Repeat("a", 64))
	if err == nil || !strings.Contains(err.Error(), "podman") {
		t.Errorf("startStack = %v, want the podman refusal", err)
	}
}

func TestMountVisibilityFixNamesTheBackendsCause(t *testing.T) {
	if fix := doctorMountVisibilityFix(dockerbackend.NativeLinux, "-data-dir", "/home/op/buildgate/data"); !strings.Contains(fix, "65532") || strings.Contains(fix, "colima's mounts") {
		t.Errorf("native-linux fix = %q", fix)
	}
	for _, b := range []dockerbackend.Backend{dockerbackend.Colima, ""} {
		if fix := doctorMountVisibilityFix(b, "-data-dir", "/home/op/buildgate/data"); !strings.Contains(fix, "colima's mounts") {
			t.Errorf("%q fix = %q", b, fix)
		}
	}
}

func TestUninstallGatewayStateCommandFollowsTheBackend(t *testing.T) {
	for b, want := range map[dockerbackend.Backend]string{
		dockerbackend.NativeLinux: "`sudo rm -rf /var/lib/openshell`",
		dockerbackend.Colima:      "`colima ssh -- sudo rm -rf /var/lib/openshell`",
	} {
		e := &uninstallEnv{backend: b}
		if got := e.gatewayStateRemoveCommand(); got != want {
			t.Errorf("%s: %q, want %q", b, got, want)
		}
	}
	if got := (&uninstallEnv{}).gatewayStateRemoveCommand(); !strings.Contains(got, "colima ssh") || !strings.Contains(got, "machine Docker runs on") {
		t.Errorf("unknown backend: %q", got)
	}
}
