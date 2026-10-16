package openshell

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"buildgate/internal/sandbox"
)

// Labels the gateway's Docker driver puts on the containers of a sandbox.
const (
	labelSandboxName = sandbox.RuntimeSandboxNameLabel
	labelRole        = "openshell.ai/isolation-role"
	roleSupervisor   = "supervisor"
)

const dockerCommandTimeout = 30 * time.Second

// containerNamePattern is the gateway's sandbox-name alphabet; a name outside
// it never reaches a Docker filter.
var containerNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// DockerContainers is Containers over the Docker CLI.
type DockerContainers struct {
	// Binary defaults to "docker".
	Binary string
	// run executes the CLI and returns its stdout; tests replace it.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

var _ Containers = (*DockerContainers)(nil)

func (d *DockerContainers) docker(ctx context.Context, args ...string) ([]byte, error) {
	return runDocker(ctx, d.run, d.Binary, args...)
}

// runDocker runs the Docker CLI, or run when a test supplies one. A failure's
// error carries the CLI's stderr.
func runDocker(ctx context.Context, run func(ctx context.Context, args ...string) ([]byte, error), binary string, args ...string) ([]byte, error) {
	if run != nil {
		return run(ctx, args...)
	}
	if binary == "" {
		binary = "docker"
	}
	ctx, cancel := context.WithTimeout(ctx, dockerCommandTimeout)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// ids lists every container, running or not, labelled with the sandbox name.
func (d *DockerContainers) ids(ctx context.Context, sandboxName string) ([]string, error) {
	if !containerNamePattern.MatchString(sandboxName) {
		return nil, fmt.Errorf("invalid sandbox name %q", sandboxName)
	}
	out, err := d.docker(ctx, "ps", "-a", "-q", "--no-trunc", "--filter", "label="+labelSandboxName+"="+sandboxName)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

func (d *DockerContainers) Present(ctx context.Context, sandboxName string) (bool, error) {
	ids, err := d.ids(ctx, sandboxName)
	return len(ids) > 0, err
}

// WorkerStartedAt inspects the sandbox's containers and returns the start
// time of the one that is not the supervisor. More than one worker is an
// error: the answer would not identify a single start.
func (d *DockerContainers) WorkerStartedAt(ctx context.Context, sandboxName string) (string, error) {
	ids, err := d.ids(ctx, sandboxName)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	format := `{{index .Config.Labels "` + labelRole + `"}}|{{.State.StartedAt}}`
	out, err := d.docker(ctx, append([]string{"inspect", "--type", "container", "--format", format}, ids...)...)
	if err != nil {
		return "", err
	}
	var started []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		role, startedAt, found := strings.Cut(strings.TrimSpace(line), "|")
		if !found {
			return "", fmt.Errorf("docker inspect of sandbox %s: unreadable line %q", sandboxName, line)
		}
		if role != roleSupervisor {
			started = append(started, startedAt)
		}
	}
	switch len(started) {
	case 0:
		return "", nil
	case 1:
		return started[0], nil
	}
	return "", fmt.Errorf("sandbox %s has %d worker containers", sandboxName, len(started))
}

// Remove force-removes the sandbox's containers. A container that vanished
// between the listing and the removal makes `docker rm` fail; the caller
// judges the result by Present.
func (d *DockerContainers) Remove(ctx context.Context, sandboxName string) error {
	ids, err := d.ids(ctx, sandboxName)
	if err != nil || len(ids) == 0 {
		return err
	}
	_, err = d.docker(ctx, append([]string{"rm", "-f"}, ids...)...)
	return err
}

// scratchVolumePattern is the name of a launch's scratch volume; a name
// outside it never reaches the CLI.
var scratchVolumePattern = regexp.MustCompile(`^bg-scratch-[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// DockerVolumes is Volumes over the Docker CLI.
type DockerVolumes struct {
	// Binary defaults to "docker".
	Binary string
	// run executes the CLI and returns its stdout; tests replace it.
	run func(ctx context.Context, args ...string) ([]byte, error)
}

var _ Volumes = (*DockerVolumes)(nil)

func (d *DockerVolumes) docker(ctx context.Context, args ...string) ([]byte, error) {
	return runDocker(ctx, d.run, d.Binary, args...)
}

func noSuchVolume(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "no such volume")
}

// Create makes the volume; one of that name that already exists is an error,
// because it may hold another launch's files. The existence check is inspect
// then create, which is safe without atomicity because the name derives from
// a per-launch hashed sandbox name no worker can predict or create.
func (d *DockerVolumes) Create(ctx context.Context, name string, labels map[string]string) error {
	if !scratchVolumePattern.MatchString(name) {
		return fmt.Errorf("invalid scratch volume name %q", name)
	}
	_, err := d.docker(ctx, "volume", "inspect", "--format", "{{.Name}}", name)
	switch {
	case err == nil:
		return fmt.Errorf("scratch volume %s already exists", name)
	case !noSuchVolume(err):
		return err
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []string{"volume", "create"}
	for _, k := range keys {
		args = append(args, "--label", k+"="+labels[k])
	}
	_, err = d.docker(ctx, append(args, name)...)
	return err
}

// Remove removes the volume; one that does not exist is success.
func (d *DockerVolumes) Remove(ctx context.Context, name string) error {
	if !scratchVolumePattern.MatchString(name) {
		return fmt.Errorf("invalid scratch volume name %q", name)
	}
	if _, err := d.docker(ctx, "volume", "rm", name); err != nil && !noSuchVolume(err) {
		return err
	}
	return nil
}
