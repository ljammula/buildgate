package dockerbackend

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	const engine = `{"Name":"build-01","OperatingSystem":"Ubuntu 24.04.1 LTS","OSType":"linux","KernelVersion":"6.8.0-45-generic","DockerRootDir":"/var/lib/docker","ServerVersion":"28.5.1","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]}`
	cases := []struct {
		name string
		f    Facts
		want Backend
	}{
		{"colima on a Mac", Facts{GOOS: "darwin", Context: "colima", Endpoint: "unix:///Users/op/.colima/default/docker.sock",
			Info: []byte(`{"Name":"colima","OperatingSystem":"Ubuntu 24.04 LTS","OSType":"linux","ServerVersion":"27.4.0","SecurityOptions":["name=apparmor","name=seccomp,profile=builtin"]}`)}, Colima},
		{"a colima profile on Linux", Facts{GOOS: "linux", Context: "colima-work", Endpoint: "unix:///home/op/.colima/work/docker.sock",
			Info: []byte(`{"Name":"colima-work","OSType":"linux","ServerVersion":"27.4.0"}`)}, Colima},
		{"colima through DOCKER_HOST", Facts{GOOS: "darwin", Context: "default", Endpoint: "unix:///Users/op/.colima/default/docker.sock",
			Info: []byte(`{"Name":"colima","OSType":"linux","ServerVersion":"27.4.0"}`)}, Colima},
		{"colima under XDG_CONFIG_HOME", Facts{GOOS: "linux", Context: "default", Endpoint: "unix:///home/op/.config/colima/default/docker.sock",
			Info: []byte(`{"Name":"lima-colima","OSType":"linux","ServerVersion":"24.0.7"}`)}, Colima},
		{"an older colima VM's host name", Facts{GOOS: "darwin", Context: "default", Endpoint: "unix:///var/run/docker.sock",
			Info: []byte(`{"Name":"lima-colima","OSType":"linux","ServerVersion":"24.0.7"}`)}, Colima},
		{"Docker Engine on Ubuntu", Facts{GOOS: "linux", Context: "default", Endpoint: "unix:///var/run/docker.sock", Info: []byte(engine)}, NativeLinux},
		{"Docker Engine, endpoint unset", Facts{GOOS: "linux", Info: []byte(engine)}, NativeLinux},
		{"Docker Engine reached from a Mac", Facts{GOOS: "darwin", Context: "default", Endpoint: "unix:///var/run/docker.sock", Info: []byte(engine)}, Other},
		{"a remote Engine", Facts{GOOS: "linux", Context: "builder", Endpoint: "ssh://op@build-02", Info: []byte(engine)}, Other},
		{"Docker Desktop on a Mac", Facts{GOOS: "darwin", Context: "desktop-linux", Endpoint: "unix:///Users/op/.docker/run/docker.sock",
			Info: []byte(`{"Name":"docker-desktop","OperatingSystem":"Docker Desktop","OSType":"linux","ServerVersion":"28.0.1"}`)}, DockerDesktop},
		{"Docker Desktop for Linux", Facts{GOOS: "linux", Context: "desktop-linux", Endpoint: "unix:///home/op/.docker/desktop/docker.sock",
			Info: []byte(`{"Name":"docker-desktop","OperatingSystem":"Docker Desktop","OSType":"linux","ServerVersion":"28.0.1"}`)}, DesktopLinux},
		{"Docker Desktop, default context", Facts{GOOS: "darwin", Context: "default", Endpoint: "unix:///var/run/docker.sock",
			Info: []byte(`{"Name":"docker-desktop","OperatingSystem":"Docker Desktop","OSType":"linux","ServerVersion":"28.0.1"}`)}, DockerDesktop},
		{"rootless", Facts{GOOS: "linux", Context: "rootless", Endpoint: "unix:///run/user/1000/docker.sock",
			Info: []byte(`{"OSType":"linux","ServerVersion":"28.5.1","SecurityOptions":["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]}`)}, Rootless},
		{"userns-remap", Facts{GOOS: "linux", Endpoint: "unix:///var/run/docker.sock",
			Info: []byte(`{"OSType":"linux","ServerVersion":"28.5.1","SecurityOptions":["name=apparmor","name=userns"]}`)}, Userns},
		{"snap", Facts{GOOS: "linux", Endpoint: "unix:///var/run/docker.sock",
			Info: []byte(`{"OSType":"linux","ServerVersion":"27.2.0","DockerRootDir":"/var/snap/docker/common/var-lib-docker","SecurityOptions":["name=apparmor"]}`)}, Snap},
		{"podman-docker", Facts{GOOS: "linux", Endpoint: "unix:///run/user/1000/podman/podman.sock",
			Info: []byte(`{"host":{"arch":"amd64"},"store":{},"version":{"Version":"4.9.3"}}`)}, Podman},
		{"podman info without its socket name", Facts{GOOS: "linux", Info: []byte(`{"host":{"arch":"amd64"}}`)}, Podman},
		{"WSL2", Facts{GOOS: "linux", Endpoint: "unix:///var/run/docker.sock",
			Info: []byte(`{"OSType":"linux","ServerVersion":"28.5.1","KernelVersion":"5.15.153.1-microsoft-standard-WSL2"}`)}, WSL2},
		{"unparseable info", Facts{GOOS: "linux", Info: []byte("Cannot connect")}, Other},
	}
	for _, c := range cases {
		if got := Classify(c.f); got != c.want {
			t.Errorf("%s: Classify = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEveryRefusedBackendSaysWhy: each refused backend has its own reason,
// and only colima and native-linux have none.
func TestEveryRefusedBackendSaysWhy(t *testing.T) {
	seen := map[string]Backend{}
	for _, b := range []Backend{Colima, NativeLinux, DockerDesktop, DesktopLinux, Rootless, Userns, Snap, Podman, WSL2, Other} {
		reason := b.Refusal()
		if b.Supported() != (reason == "") {
			t.Errorf("%s: Supported = %v but Refusal = %q", b, b.Supported(), reason)
		}
		if reason == "" {
			continue
		}
		if strings.Contains(reason, "\n") {
			t.Errorf("%s: the reason is more than one line", b)
		}
		if other, dup := seen[reason]; dup {
			t.Errorf("%s and %s give the same reason", b, other)
		}
		seen[reason] = b
	}
}

func TestDetectionRefused(t *testing.T) {
	if err := (Detection{Backend: Colima}).Refused(); err != nil {
		t.Errorf("colima refused: %v", err)
	}
	if err := (Detection{Backend: NativeLinux}).Refused(); err != nil {
		t.Errorf("native-linux refused: %v", err)
	}
	err := (Detection{Backend: Rootless, Facts: Facts{Context: "rootless", Endpoint: "unix:///run/user/1000/docker.sock"}}).Refused()
	if err == nil || !strings.Contains(err.Error(), "rootless") || !strings.Contains(err.Error(), "/run/user/1000/docker.sock") {
		t.Errorf("rootless = %v, want a refusal naming the backend and its endpoint", err)
	}
	if err := (Detection{Err: errTest{}}).Refused(); err != nil {
		t.Errorf("an unreachable Docker is refused here (%v); the Docker call after it says why", err)
	}
}

type errTest struct{}

func (errTest) Error() string { return "docker: not answering" }
