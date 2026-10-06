package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"buildgate/internal/run"
)

// RuntimeSandboxNameLabel is the label the sandbox runtime's Docker driver
// puts on the worker and supervisor containers of a sandbox. A container
// the runtime created carries none of this package's own labels, so a run's
// sandbox containers are found through the names the run recorded.
const RuntimeSandboxNameLabel = "openshell.ai/sandbox-name"

// dockerLines runs one short docker listing and returns its non-empty lines.
func dockerLines(ctx context.Context, dockerBinary string, args ...string) ([]string, error) {
	listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(listCtx, dockerBinary, args...)
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("docker %s: %w", args[0], err)
	}
	return dedupeLines(nil, map[string]bool{}, string(out)), nil
}

// recordedSandboxContainerIDs lists the containers, running or not, of every
// sandbox the run recorded.
func recordedSandboxContainerIDs(ctx context.Context, dockerBinary, dataDir, runID string) ([]string, error) {
	records, err := RecordedSandboxes(dataDir, runID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, rec := range records {
		found, err := dockerLines(ctx, dockerBinary, "ps", "-a", "-q", "--filter", "label="+RuntimeSandboxNameLabel+"="+rec.Name)
		if err != nil {
			return nil, fmt.Errorf("list containers of sandbox %s (run %q): %w", rec.Name, runID, err)
		}
		ids = append(ids, found...)
	}
	return ids, nil
}

// runtimeSandboxContainers returns one "container name<TAB>run id" line for
// every runtime-created container whose sandbox a run under dataDir
// recorded, the same line shape ReconcileOrphans reads for the containers
// this package launched itself. With no such container on the machine it
// reads no run record at all.
func runtimeSandboxContainers(ctx context.Context, dockerBinary, dataDir string) ([]string, error) {
	listed, err := dockerLines(ctx, dockerBinary, "ps", "-a", "--filter", "label="+RuntimeSandboxNameLabel,
		"--format", `{{.Names}}`+"\t"+`{{.Label "`+RuntimeSandboxNameLabel+`"}}`)
	if err != nil || len(listed) == 0 {
		return nil, err
	}
	runOf, err := recordedSandboxRuns(dataDir)
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range listed {
		container, sandboxName, found := strings.Cut(line, "\t")
		if runID, recorded := runOf[sandboxName]; found && recorded {
			lines = append(lines, container+"\t"+runID)
		}
	}
	return lines, nil
}

// recordedSandboxRuns maps every sandbox name recorded under dataDir to its
// run. A run whose record cannot be read is an error: its sandbox would
// otherwise never be reconciled.
func recordedSandboxRuns(dataDir string) (map[string]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	runOf := map[string]string{}
	for _, entry := range entries {
		if !entry.IsDir() || !run.ValidID(entry.Name()) {
			continue
		}
		records, err := RecordedSandboxes(dataDir, entry.Name())
		if err != nil {
			return nil, err
		}
		for _, rec := range records {
			runOf[rec.Name] = entry.Name()
		}
	}
	return runOf, nil
}

// ContainerAddress asks Docker for a container's address on one network: how
// a worker that joins no network is told where a sidecar is.
func ContainerAddress(ctx context.Context, dockerBinary, container, network string) (string, error) {
	lines, err := dockerLines(ctx, dockerBinary, "inspect", "--type", "container", "--format",
		`{{with index .NetworkSettings.Networks "`+network+`"}}{{.IPAddress}}{{end}}`, container)
	if err != nil {
		return "", err
	}
	if len(lines) != 1 {
		return "", fmt.Errorf("container %q has no address on network %q", container, network)
	}
	return lines[0], nil
}
