// Package dockerbackend names the Docker a machine runs sandboxes on, from
// facts the caller reads (the docker context and `docker info`), and says
// which backends buildgate refuses and why. It runs no process and reads no
// file: the command reads the facts once through its docker boundary and
// passes them in.
//
// The OS chooses the host lifecycle (the service manager, how Docker is
// started); the backend chooses the containment. colima, on macOS or Linux,
// is the one backend sandboxes run on. Every other backend is refused before
// any launch, each with its own one-line reason: Docker Engine on a Linux
// host is named (NativeLinux) but refused until a uid scheme lets its worker
// use factoryd's directories, and a backend buildgate cannot name is not one
// whose containment has been reviewed.
package dockerbackend

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Backend is the kind of Docker sandboxes would run on.
type Backend string

const (
	// Colima is colima's VM, on macOS or Linux.
	Colima Backend = "colima"
	// NativeLinux is rootful Docker Engine on the Linux host itself.
	NativeLinux Backend = "native-linux"
	// DockerDesktop is Docker Desktop's VM (macOS, or Linux reporting it).
	DockerDesktop Backend = "docker-desktop"
	// DesktopLinux is Docker Desktop for Linux, named by its context.
	DesktopLinux Backend = "desktop-linux"
	// Rootless is Docker Engine in rootless mode.
	Rootless Backend = "rootless"
	// Userns is Docker Engine with user-namespace remapping.
	Userns Backend = "userns"
	// Snap is Docker Engine from the snap package.
	Snap Backend = "snap"
	// Podman is podman behind the docker CLI (podman-docker).
	Podman Backend = "podman"
	// WSL2 is any Docker under Windows' WSL2 kernel.
	WSL2 Backend = "wsl2"
	// Other is a Docker none of the above describes: a remote daemon,
	// another VM product.
	Other Backend = "other"
)

// Facts are what the backend is told apart by.
type Facts struct {
	// GOOS is the OS factoryd runs on, as runtime.GOOS names it.
	GOOS string
	// Context is the docker CLI's current context name; Endpoint is the
	// daemon address it talks to (DOCKER_HOST when set).
	Context  string
	Endpoint string
	// Info is `docker info --format '{{json .}}'`'s output.
	Info []byte
}

// info is the part of `docker info`'s JSON the classification reads.
type info struct {
	Name            string
	OperatingSystem string
	OSType          string
	KernelVersion   string
	DockerRootDir   string
	ServerVersion   string
	SecurityOptions []string
	// Host is set only by podman's info, which has no ServerVersion.
	Host json.RawMessage `json:"host"`
}

// Classify names the backend the facts describe. An info it cannot parse is
// Other: refused, never guessed.
func Classify(f Facts) Backend {
	var in info
	if err := json.Unmarshal(f.Info, &in); err != nil {
		return Other
	}
	switch {
	case (len(in.Host) > 0 && in.ServerVersion == "") || strings.Contains(f.Endpoint, "podman"):
		return Podman
	// Docker Desktop names its context desktop-linux on macOS too: only on
	// a Linux host is that Docker Desktop for Linux.
	case f.GOOS == "linux" && f.Context == "desktop-linux":
		return DesktopLinux
	case in.OperatingSystem == "Docker Desktop" || f.Context == "desktop-linux":
		return DockerDesktop
	case strings.Contains(strings.ToLower(in.KernelVersion), "microsoft"):
		return WSL2
	case hasSecurityOption(in.SecurityOptions, "rootless"):
		return Rootless
	case hasSecurityOption(in.SecurityOptions, "userns"):
		return Userns
	case strings.HasPrefix(in.DockerRootDir, "/var/snap/docker"):
		return Snap
	case isColima(f, in):
		return Colima
	case f.GOOS == "linux" && in.OSType == "linux" && isLocalSocket(f.Endpoint):
		return NativeLinux
	}
	return Other
}

// hasSecurityOption reports whether `docker info`'s SecurityOptions names
// the option ("name=rootless").
func hasSecurityOption(options []string, name string) bool {
	for _, o := range options {
		for _, field := range strings.Split(o, ",") {
			if field == "name="+name {
				return true
			}
		}
	}
	return false
}

// isColima recognises colima by its context (colima, colima-<profile>), its
// socket under ~/.colima or $XDG_CONFIG_HOME/colima, or its VM's host name
// (colima, colima-<profile>, or lima-colima on older VMs). The host name
// alone does not count for a Linux machine's own local socket: that is the
// Docker Engine of the machine factoryd runs on (factoryd inside colima's
// VM, say), whatever the machine is called.
func isColima(f Facts, in info) bool {
	named := func(s string) bool { return s == "colima" || strings.HasPrefix(s, "colima-") }
	socket := strings.Contains(f.Endpoint, "/.colima/") || strings.Contains(f.Endpoint, "/colima/")
	if named(f.Context) || socket {
		return true
	}
	if f.GOOS == "linux" && isLocalSocket(f.Endpoint) {
		return false
	}
	return named(in.Name) || strings.HasPrefix(in.Name, "lima-colima")
}

// isLocalSocket reports whether endpoint is the host's own Docker Engine
// socket.
func isLocalSocket(endpoint string) bool {
	switch endpoint {
	case "", "unix:///var/run/docker.sock", "unix:///run/docker.sock":
		return true
	}
	return false
}

// Supported reports whether sandboxes may run on b: on colima alone.
func (b Backend) Supported() bool { return b == Colima }

// Refusal is the one-line reason sandboxes do not run on b, "" when they do.
func (b Backend) Refusal() string {
	switch b {
	case Colima:
		return ""
	case NativeLinux:
		// The worker runs as the image's 65532 and factoryd's launch
		// directories are the operator's, mode 0750: with no VM file
		// sharing between them, Landlock skips the worker's mounts and its
		// command never starts. A uid scheme for this host is a
		// containment change, not yet made.
		return "sandboxes do not run on Docker Engine on this host yet: the worker's user (65532) cannot use factoryd's launch directories without a VM between them; use colima on this machine"
	case DockerDesktop:
		return "Docker Desktop's VM has neither AppArmor nor Landlock, which the sandbox needs; use colima"
	case DesktopLinux:
		return "Docker Desktop for Linux (context desktop-linux) is not supported; use colima"
	case Rootless:
		return "rootless Docker is not supported: the worker's user maps to a subordinate UID, so no file ownership between it and factoryd is right; use colima"
	case Userns:
		return "Docker with userns-remap is not supported: the worker's user maps to a subordinate UID, so no file ownership between it and factoryd is right; use colima"
	case Snap:
		return "Docker from the snap package is not supported; use colima"
	case Podman:
		return "podman behind the docker CLI is not supported; use colima"
	case WSL2:
		return "Docker under WSL2 is not supported; use a Linux host or a Mac, with colima"
	}
	return "this Docker is not colima, the only backend sandboxes run on"
}

// Detection is a backend with the facts it was named from.
type Detection struct {
	Backend Backend
	Facts   Facts
	// Err is why the facts could not be read (Docker does not answer);
	// Backend is then "".
	Err error
}

// Refused is the error a launch, the stack start or the worker start
// returns on this detection: nil on a supported backend, and nil when
// Docker could not be asked, which every Docker call after it reports for
// itself.
func (d Detection) Refused() error {
	if d.Err != nil || d.Backend.Supported() {
		return nil
	}
	return fmt.Errorf("refusing to run sandboxes on this Docker (%s; context %q, endpoint %q): %s", d.Backend, d.Facts.Context, d.Facts.Endpoint, d.Backend.Refusal())
}
