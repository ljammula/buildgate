package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/api"
	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/workflow"
)

type managedDaemon struct {
	repository string
	process    superviseProcess
	done       chan error
	startedAt  time.Time
	stopping   bool
	exited     bool
}

type serveDaemonController struct {
	mu       sync.Mutex
	config   superviseConfig
	children map[string]*managedDaemon
	factory  func([]string) (superviseProcess, error)
}

func newServeDaemonController(dp *deps, config superviseConfig) *serveDaemonController {
	return &serveDaemonController{
		config:   config,
		children: make(map[string]*managedDaemon),
		factory:  func(a0 []string) (superviseProcess, error) { return realFactorydProcess(dp, a0) },
	}
}

func validateDaemonRepository(repository string) error {
	if strings.TrimSpace(repository) == "" {
		return fmt.Errorf("%w: repository is required", api.ErrInvalidDaemonRequest)
	}
	return nil
}

func (c *serveDaemonController) Start(ctx context.Context, repository string) (api.DaemonStatus, error) {
	if err := validateDaemonRepository(repository); err != nil {
		return api.DaemonStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return api.DaemonStatus{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.children[repository]; ok {
		if existing.exited {
			delete(c.children, repository)
		} else {
			return api.DaemonStatus{}, api.ErrDaemonConflict
		}
	}
	if heartbeat, err := daemonheartbeat.Read(daemonheartbeat.Path(c.config.dataDir, workflow.RepositoryOwnerWorkflowID(repository))); err == nil &&
		superviseHeartbeatIdentityMatches(heartbeat, repository) &&
		!daemonheartbeat.Stale(heartbeat, time.Now(), daemonheartbeat.SandboxStaleAfter) {
		return api.DaemonStatus{}, api.ErrDaemonConflict
	}
	process, err := c.factory(c.config.supervisorArgs(repository))
	if err != nil {
		return api.DaemonStatus{}, err
	}
	child := &managedDaemon{
		repository: repository,
		process:    process,
		done:       make(chan error, 1),
		startedAt:  time.Now(),
	}
	c.children[repository] = child
	go func() {
		err := process.Wait()
		c.mu.Lock()
		child.exited = true
		c.mu.Unlock()
		child.done <- err
	}()
	return c.statusLocked(child), nil
}

func (c *serveDaemonController) List(_ context.Context) ([]api.DaemonStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	repositories := make([]string, 0, len(c.children))
	for repository := range c.children {
		repositories = append(repositories, repository)
	}
	sort.Strings(repositories)
	statuses := make([]api.DaemonStatus, 0, len(repositories))
	for _, repository := range repositories {
		statuses = append(statuses, c.statusLocked(c.children[repository]))
	}
	return statuses, nil
}

func (c *serveDaemonController) statusLocked(child *managedDaemon) api.DaemonStatus {
	status := api.DaemonStatus{
		Repository: child.repository,
		State:      "running",
		PID:        child.process.PID(),
		StartedAt:  child.startedAt.Format(time.RFC3339Nano),
	}
	if child.exited {
		status.State = "exited"
	} else if child.stopping {
		status.State = "stopping"
	}
	if heartbeat, err := daemonheartbeat.Read(daemonheartbeat.Path(c.config.dataDir, workflow.RepositoryOwnerWorkflowID(child.repository))); err == nil {
		status.HeartbeatUpdatedAt = heartbeat.UpdatedAt
	}
	return status
}

func (c *serveDaemonController) Stop(ctx context.Context, repository string) error {
	if err := validateDaemonRepository(repository); err != nil {
		return err
	}
	c.mu.Lock()
	child, ok := c.children[repository]
	if !ok {
		c.mu.Unlock()
		return api.ErrDaemonNotFound
	}
	if child.exited {
		delete(c.children, repository)
		c.mu.Unlock()
		return api.ErrDaemonNotFound
	}
	if child.stopping {
		c.mu.Unlock()
		return api.ErrDaemonConflict
	}
	child.stopping = true
	c.mu.Unlock()

	err := stopManagedDaemon(ctx, child, c.config.stopTimeout)
	if err != nil {
		c.mu.Lock()
		if c.children[repository] == child && !child.exited {
			child.stopping = false
		}
		c.mu.Unlock()
		return err
	}
	c.mu.Lock()
	if c.children[repository] == child {
		delete(c.children, repository)
	}
	c.mu.Unlock()
	return nil
}

func stopManagedDaemon(ctx context.Context, child *managedDaemon, timeout time.Duration) error {
	if err := child.process.Signal(syscall.SIGTERM); err != nil {
		// The daemon may have already exited on its own (crash, or a race
		// with an operator's stop request) — Signal failing on an already-
		// finished process is expected, not a real error, and a Kill on
		// top of it would fail too. Mirrors stopSuperviseChild's own guard
		// in supervisor.go for the same race.
		select {
		case <-child.done:
			return nil
		default:
		}
		if killErr := child.process.Kill(); killErr != nil {
			return fmt.Errorf("signal daemon: %v; kill daemon: %w", err, killErr)
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-child.done:
		return nil
	case <-ctx.Done():
		if err := child.process.Kill(); err != nil {
			return fmt.Errorf("kill daemon after cancellation: %w", err)
		}
		select {
		case <-child.done:
			return ctx.Err()
		case <-time.After(timeout):
			return fmt.Errorf("daemon did not exit after cancellation: %w", ctx.Err())
		}
	case <-timer.C:
		if err := child.process.Kill(); err != nil {
			return fmt.Errorf("kill daemon after graceful-stop timeout: %w", err)
		}
		select {
		case <-child.done:
			return nil
		case <-time.After(timeout):
			return errors.New("daemon did not exit after kill")
		}
	}
}

func (c *serveDaemonController) StopAll(ctx context.Context) error {
	c.mu.Lock()
	repositories := make([]string, 0, len(c.children))
	children := make(map[string]*managedDaemon, len(c.children))
	for repository, child := range c.children {
		if child.exited {
			delete(c.children, repository)
			continue
		}
		repositories = append(repositories, repository)
		child.stopping = true
		children[repository] = child
	}
	c.mu.Unlock()
	sort.Strings(repositories)
	var errs []error
	for _, repository := range repositories {
		if err := stopManagedDaemon(ctx, children[repository], c.config.stopTimeout); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repository, err))
			continue
		}
		c.mu.Lock()
		if c.children[repository] == children[repository] {
			delete(c.children, repository)
		}
		c.mu.Unlock()
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

var _ api.DaemonController = (*serveDaemonController)(nil)
