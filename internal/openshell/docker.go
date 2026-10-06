package openshell

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
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
	if d.run != nil {
		return d.run(ctx, args...)
	}
	binary := d.Binary
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
