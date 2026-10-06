package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RemoveRunContainers force-removes every container labelled for runID under
// dataDir: the run's worker, relay, registry-proxy and compose-sidecar
// containers (all carry buildgate.run and buildgate.data-dir; the legacy
// label prefix is swept too, see labelPrefixes). It lists, removes, then lists
// again, and any container still present is an error wrapping
// ErrCleanupUnconfirmed: a caller fencing a stale Activity attempt before a
// retry touches the worktree must not proceed on an unconfirmed removal,
// because a surviving container could keep editing the worktree. An empty
// runID or dataDir is an error, never a label-less sweep that would match
// another run's containers.
func RemoveRunContainers(ctx context.Context, dockerBinary, dataDir, runID string) error {
	if runID == "" || dataDir == "" {
		return fmt.Errorf("remove run containers: runID and dataDir are required (got runID=%q dataDir=%q)", runID, dataDir)
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	ids, err := listRunContainers(ctx, dockerBinary, dataDir, runID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	rmCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	cmd := exec.CommandContext(rmCtx, dockerBinary, append([]string{"rm", "-f"}, ids...)...)
	cmd.Env = dockerClientEnv()
	rmErr := cmd.Run()
	cancel()
	// Judge by a second listing, not rm's exit status: rm -f exits non-zero
	// when one id vanished concurrently even though everything is gone.
	left, err := listRunContainers(ctx, dockerBinary, dataDir, runID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCleanupUnconfirmed, err)
	}
	if len(left) > 0 {
		return fmt.Errorf("%w: containers %s of run %q still present after removal (rm: %v)", ErrCleanupUnconfirmed, strings.Join(left, ","), runID, rmErr)
	}
	return nil
}

// RunContainerIDs lists, without removing, every container (running or not)
// labelled for runID under dataDir. A resume precondition: a surviving
// container could still be editing the worktree about to be adopted. An empty
// runID or dataDir is an error, never a label-less listing.
func RunContainerIDs(ctx context.Context, dockerBinary, dataDir, runID string) ([]string, error) {
	if runID == "" || dataDir == "" {
		return nil, fmt.Errorf("list run containers: runID and dataDir are required (got runID=%q dataDir=%q)", runID, dataDir)
	}
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	return listRunContainers(ctx, dockerBinary, dataDir, runID)
}

// listRunContainers returns the ids of all containers (running or not)
// carrying both of the run's labels, one listing per label prefix because
// docker ANDs --filter flags.
func listRunContainers(ctx context.Context, dockerBinary, dataDir, runID string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, prefix := range labelPrefixes {
		listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(listCtx, dockerBinary, "ps", "-a", "-q",
			"--filter", "label="+prefix+"run="+runID,
			"--filter", "label="+prefix+"data-dir="+dataDirLabel(dataDir))
		cmd.Env = dockerClientEnv()
		out, err := cmd.Output()
		cancel()
		if err != nil {
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				return nil, fmt.Errorf("list containers for run %q: %w: %s", runID, err, strings.TrimSpace(string(exitErr.Stderr)))
			}
			return nil, fmt.Errorf("list containers for run %q: %w", runID, err)
		}
		ids = dedupeLines(ids, seen, string(out))
	}
	// Plus the containers of the sandboxes the run launched through a
	// sandbox runtime, which carry the runtime's label, not the two above.
	runtimeIDs, err := recordedSandboxContainerIDs(ctx, dockerBinary, dataDir, runID)
	if err != nil {
		return nil, err
	}
	return dedupeLines(ids, seen, strings.Join(runtimeIDs, "\n")), nil
}
