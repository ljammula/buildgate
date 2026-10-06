package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"buildgate/internal/run"
)

// A request-level job (spec, oracle or plan drafting) launches its worker and
// relay containers under the request's id in the run label, with the owner
// marker at <data-dir>/runs/<request-id>/sandbox-owner.pid. There is no run
// record behind that id, so ReconcileOrphans and ReconcileRelayOrphans, which
// decide from a run record, never reclaim them.

// runLabelled is one container carrying the data-dir label and a run label.
type runLabelled struct {
	name  string
	runID string
}

// listRunLabelled lists every container (running or not) labelled for dataDir,
// with its run label; runID != "" narrows it to that run label.
func listRunLabelled(ctx context.Context, dockerBinary, dataDir, runID string) ([]runLabelled, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	var lines []string
	seen := map[string]bool{}
	for _, prefix := range labelPrefixes {
		args := []string{"ps", "-a", "--filter", "label=" + prefix + "data-dir=" + dataDirLabel(dataDir)}
		if runID != "" {
			args = append(args, "--filter", "label="+prefix+"run="+runID)
		}
		args = append(args, "--format", `{{.Names}}\t{{.Label "`+prefix+`run"}}`)
		listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(listCtx, dockerBinary, args...)
		cmd.Env = dockerClientEnv()
		out, err := cmd.Output()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("list sandbox containers: %w", err)
		}
		lines = dedupeLines(lines, seen, string(out))
	}
	var found []runLabelled
	for _, line := range lines {
		fields := strings.SplitN(line, "\t", 2)
		c := runLabelled{name: fields[0]}
		if len(fields) == 2 {
			c.runID = fields[1]
		}
		found = append(found, c)
	}
	return found, nil
}

// ReconcileRequestJobOrphans removes the worker and relay containers (and the
// relay's network) of request-level jobs whose owner is gone. A container
// qualifies only when its run label names no run record under dataDir, so a
// build's containers stay with the run reconcilers, and either ownerDead(label)
// says its owner process is dead or its owner marker has gone stale; a live,
// freshly heartbeating or unknown owner is left alone. It
// returns the names it removed. A registry-proxy container is never touched:
// no request-level job starts one.
func ReconcileRequestJobOrphans(ctx context.Context, dockerBinary, dataDir string, ownerDead func(id string) bool) ([]string, error) {
	found, err := listRunLabelled(ctx, dockerBinary, dataDir, "")
	if err != nil {
		return nil, err
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	var workers, relays []runLabelled
	// An owner is gone when its pid is dead, or when its marker went stale
	// (nothing refreshes it any more): a reused pid reads as alive, so the
	// marker's age is the only proof against it. A stale-only owner gets the
	// same debounce the run reconcilers apply, once for the whole batch.
	const (
		keep = iota
		dead
		stale
	)
	verdict := map[string]int{}
	for _, c := range found {
		if !run.ValidID(c.runID) || strings.HasPrefix(c.name, "factoryd-registryproxy-container-") {
			continue
		}
		v, known := verdict[c.runID]
		if !known {
			_, loadErr := run.Load(dataDir, c.runID)
			switch {
			case !errors.Is(loadErr, os.ErrNotExist):
				v = keep
			case ownerDead(c.runID):
				v = dead
			case ownerHeartbeatStale(dataDir, c.runID):
				v = stale
			}
			verdict[c.runID] = v
		}
		if v == keep {
			continue
		}
		if strings.HasPrefix(c.name, "factoryd-relay-container-") {
			relays = append(relays, c)
		} else {
			workers = append(workers, c)
		}
	}
	staleOnly := false
	for _, c := range append(slices.Clone(workers), relays...) {
		staleOnly = staleOnly || verdict[c.runID] == stale
	}
	if staleOnly {
		select {
		case <-ownerDebounceAfter(ownerHeartbeatInterval):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		keepFresh := func(list []runLabelled) []runLabelled {
			return slices.DeleteFunc(list, func(c runLabelled) bool {
				return verdict[c.runID] == stale && !ownerHeartbeatStale(dataDir, c.runID)
			})
		}
		workers, relays = keepFresh(workers), keepFresh(relays)
	}
	// Workers go first: a relay's network cannot be removed while one is
	// still attached to it.
	sort.SliceStable(workers, func(i, j int) bool { return workers[i].name < workers[j].name })
	var removed []string
	var errs []error
	for _, c := range workers {
		reclaimContainerWorkDir(dockerBinary, c.name)
		if err := ensureRemoved(ctx, dockerBinary, c.name); err != nil {
			errs = append(errs, fmt.Errorf("reconcile orphaned container %q (request %q): %w", c.name, c.runID, err))
			continue
		}
		removed = append(removed, c.name)
	}
	for _, c := range relays {
		containerRemoved, network, err := removeRelayContainerAndNetwork(ctx, dockerBinary, c.name, false)
		if containerRemoved {
			removed = append(removed, c.name)
		}
		if network != "" {
			removed = append(removed, network)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("reconcile orphaned relay %q (request %q): %w", c.name, c.runID, err))
		}
	}
	return removed, errors.Join(errs...)
}
