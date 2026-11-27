package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"go.temporal.io/sdk/client"
	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/modelhost"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
	wsisolation "buildgate/internal/workspace"
)

// reclaimComposeGateTry bounds a reclaimed run's try for its compose sidecars
// slot: one attempt plus one retry, then the run waits for the next scan
// rather than blocking the worker.
const reclaimComposeGateTry = 3 * time.Second

// reclaimWorkerStopTimeout is how long a recovery or owner worker's Stop
// waits for an Activity in flight before cancelling it. One that finishes in
// time reports its result and the next worker start goes on from there; one
// still running is lost, as any lost step is. Every worker stops at once
// (repositoryReclaim.stop), so the whole stop stays under the 15 s
// `factoryd stop` waits before it kills the worker.
const reclaimWorkerStopTimeout = 10 * time.Second

// reclaimReconcileConcurrency bounds how many reconcileReclaimedRun calls (each
// a Temporal query and possibly a locked run.json save) run at once per scan.
const reclaimReconcileConcurrency = 4

// repositoryReclaim is the recovery `factoryd worker` runs for the runs of a
// `-repository` owner whose submitter died. Such a run's child workflow
// waits on a task queue only its submitter polled, so without this the owner
// waits forever and blocks every later request of the repository. A scan:
//
//   - leaves alone a run whose recorded Docker differs from the worker's
//     (named once in the log): its containers are invisible to this Docker;
//   - takes the repository exclusively without waiting (a repository a live
//     submitter or a build holds waits for the next scan), then starts a
//     recovery worker for each abandoned run's own queue and one on the
//     repository's owner queue, holding the repository lock and the compose
//     sidecars slot until the run is terminal;
//   - applies the owner's real result to a run a give-up path saved halted and
//     unconfirmed (reconcileReclaimedRun), so it reaches accepted or
//     quarantined;
//   - stops the workers of runs that reached a terminal state, and a
//     repository's owner worker once none of its runs is reclaimed;
//   - removes the containers, scratch dirs and run-local install images of
//     finished runs.
type repositoryReclaim struct {
	dp         *deps
	dataDir    string
	docker     string
	endpoint   daemonheartbeat.DockerEndpoint
	composeMax int
	client     client.Client
	activities *workflow.Activities

	mu      sync.Mutex
	workers map[string]temporalworker.Worker
	locks   map[string]*wsisolation.DirectLock
	gates   map[string]*modelhost.Handle
	owners  map[string]string // run id -> repository owner workflow id
	// ownerWorkers poll each reclaimed repository's shared owner queue, so the
	// owner workflow waiting on an abandoned child can move on.
	ownerWorkers map[string]temporalworker.Worker // owner workflow id -> worker
	// named holds the runs whose deferral or refusal the log already named,
	// so a scan every minute does not repeat it. Read and written by the scan
	// goroutine only.
	named map[string]bool
}

// newRepositoryReclaim builds the reclaim of dataDir on the worker's config.
func newRepositoryReclaim(dp *deps, dataDir string, cfg requestdriver.WorkerConfig, c client.Client) (*repositoryReclaim, error) {
	script, err := resolveHarnessScript(cfg.BuildAppScript, "build_app.py")
	if err != nil {
		return nil, err
	}
	s := cfg.Settings
	absDataDir, err := canonicalPath(dataDir)
	if err != nil {
		return nil, err
	}
	// Static fallbacks only: every RunWorkflowInput carries its own per-run
	// values, which the activities prefer.
	acts := &workflow.Activities{
		BuildAppInterpreter:      "python3",
		BuildAppScript:           script,
		BuildMaxAttempts:         max(s.BuildAppMaxAttempts, 1),
		VerifyMaxAttempts:        max(s.VerifyMaxAttempts, 1),
		SandboxDocker:            s.SandboxDocker,
		SandboxWorkerUID:         s.SandboxWorkerUID,
		SandboxMemory:            s.SandboxMemory,
		SandboxCPUs:              s.SandboxCPUs,
		SandboxTmpfsSize:         s.SandboxTmpfsSize,
		ComposeServicesWorkerEnv: s.ComposeServicesWorkerEnv,
		ModelHostConcurrency:     s.ModelHostConcurrency,
		EgressCABundlePath:       cfg.EgressCABundle,
		Sandboxes:                dp.sandbox.runtime(),
		InstallImages:            dp.docker.installImages(),
		SandboxApprovalsDataDir:  absDataDir,
		InstallCacheImages:       s.SandboxInstallCacheImages,
		InstallLockDir:           installLockDir(),
		MeterLedgerRoot:          meterLedgerRoot(),
		DataDir:                  absDataDir,
		LogDir:                   filepath.Join(absDataDir, "logs"),
		CheckpointDir:            filepath.Join(absDataDir, "temporal-checkpoints"),
		CheckRoute:               checkRouteFunc(s),
		ResolveRouteCredentials:  resolveRouteCredentialsFunc(s),
		CheckSkills:              checkSkillsFunc(s),
	}
	return &repositoryReclaim{
		dp: dp, dataDir: absDataDir, docker: s.SandboxDocker, composeMax: s.ComposeServicesConcurrency,
		client: c, activities: acts, endpoint: daemonheartbeat.LocalDockerEndpoint(s.SandboxDocker),
		workers:      map[string]temporalworker.Worker{},
		locks:        map[string]*wsisolation.DirectLock{},
		gates:        map[string]*modelhost.Handle{},
		owners:       map[string]string{},
		ownerWorkers: map[string]temporalworker.Worker{},
		named:        map[string]bool{},
	}, nil
}

// reclaimRepositories returns the repository of every nonterminal run of
// dataDir that went through a repository owner, sorted.
func reclaimRepositories(dataDir string) []string {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		r, loadErr := run.Load(dataDir, entry.Name())
		if loadErr != nil || r.Repository == "" || runTerminalConfirmed(r) {
			continue
		}
		seen[r.Repository] = true
	}
	repos := make([]string, 0, len(seen))
	for repo := range seen {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	return repos
}

// scan is one pass: recover abandoned runs, reconcile the reclaimed ones, stop
// the finished ones, then the container hygiene.
func (r *repositoryReclaim) scan(ctx context.Context) {
	for _, repo := range reclaimRepositories(r.dataDir) {
		if ctx.Err() != nil {
			return
		}
		r.reclaimRepository(ctx, repo)
	}
	r.reconcileAndRetire(ctx)
	if r.docker != "" && ctx.Err() == nil {
		removeFinishedRunContainers(ctx, r.docker, r.dataDir)
		sweepRunLocalInstallImages(ctx, r.dp, r.docker, r.dataDir)
	}
}

// reclaimRepository starts a recovery worker, under the repository's
// exclusive lock and a compose slot, for each abandoned run of repo.
func (r *repositoryReclaim) reclaimRepository(ctx context.Context, repo string) {
	ownerID := workflow.RepositoryOwnerWorkflowID(repo)
	r.mu.Lock()
	already := make(map[string]bool, len(r.workers))
	for id := range r.workers {
		already[id] = true
	}
	r.mu.Unlock()
	ids, err := runsNeedingReclaim(r.dataDir, repo, already)
	if err != nil {
		log.Printf("worker: warning: could not scan durable runs for recovery: %v", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		r.reclaimRun(ctx, id, ownerID)
	}
}

// reclaimRun starts the recovery of run id, owned by ownerID, unless its
// Docker differs from the worker's, its repository is busy, or the compose
// sidecars slot is taken: each of those leaves it for a later scan.
func (r *repositoryReclaim) reclaimRun(ctx context.Context, id, ownerID string) {
	recovered, loadErr := run.Load(r.dataDir, id)
	if loadErr != nil {
		log.Printf("worker: warning: could not load reclaimed run %s for repository ownership: %v", id, loadErr)
		return
	}
	if recorded := runDockerEndpoint(recovered); recorded.Known() && recorded != r.endpoint {
		r.nameOnce(id, "worker: run %s is not recovered: it was submitted on Docker %q (DOCKER_HOST=%q, DOCKER_CONTEXT=%q), and this worker uses %q (DOCKER_HOST=%q, DOCKER_CONTEXT=%q), where its containers are invisible; a worker on the run's Docker recovers it",
			id, recorded.SandboxDocker, recorded.DockerHost, recorded.DockerContext, r.endpoint.SandboxDocker, r.endpoint.DockerHost, r.endpoint.DockerContext)
		return
	}
	lockPath := recovered.WorkspacePath
	if lockPath == "" {
		lockPath = recovered.ProjectPath
	}
	if lockPath == "" {
		if _, haltErr := haltReclaimedRunConfirmed(r.dp, r.dataDir, id, "", ""); haltErr != nil {
			log.Printf("worker: warning: could not durably halt reclaimed run %s after missing repository path: %v", id, haltErr)
		}
		log.Printf("worker: halted reclaimed run %s: no workspace or project path for repository ownership", id)
		return
	}
	// Without waiting: a held lock belongs to a live submitter or a build of
	// the same repository, and a wait here would block every build of it
	// behind this scan.
	lock, lockErr := wsisolation.AcquireDirectLock(lockPath)
	if lockErr != nil {
		// A busy lock can belong to the original submitter, still
		// finishing: leave the run for the next scan, and never start a
		// recovery worker without ownership.
		if errors.Is(lockErr, wsisolation.ErrBusy) {
			r.nameOnce(id, "worker: recovery for run %s deferred until repository ownership is available: %v", id, lockErr)
			return
		}
		if _, haltErr := haltReclaimedRunConfirmed(r.dp, r.dataDir, id, recovered.WorkspacePath, recovered.Branch); haltErr != nil {
			log.Printf("worker: warning: could not durably halt reclaimed run %s after repository ownership failure: %v", id, haltErr)
		}
		log.Printf("worker: halted reclaimed run %s: could not establish repository ownership: %v", id, lockErr)
		return
	}
	repoDir := recovered.ProjectPath
	if repoDir == "" {
		repoDir = lockPath
	}
	reconcileIsolationMarkers(ctx, r.dataDir, repoDir, r.docker, lock, r.client)

	// A brief try, not a wait: a busy slot defers this run to the next
	// scan, so the worker never blocks behind another run's sidecars.
	gateCtx, cancelGate := context.WithTimeout(ctx, reclaimComposeGateTry)
	gate, gateErr := acquireReclaimComposeServicesSlot(gateCtx, r.dataDir, id, r.composeMax, lockPath, recovered.BaseSHA)
	cancelGate()
	if gateErr != nil {
		_ = lock.Close()
		log.Printf("worker: recovery for run %s deferred until the compose sidecars slot is free: %v", id, gateErr)
		return
	}
	taskQueue := "factoryd-repo-" + ownerID
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.ensureOwnerWorker(ownerID, taskQueue) {
		_ = gate.Release()
		_ = lock.Close()
		return
	}
	queue := taskQueue + "-run-" + id
	w := temporalworker.New(r.client, queue, workflow.BoundedWorkerOptions(reclaimWorkerStopTimeout))
	w.RegisterWorkflow(workflow.RunWorkflow)
	w.RegisterActivity(r.activities)
	if startErr := w.Start(); startErr != nil {
		_ = gate.Release()
		_ = lock.Close()
		r.stopUnusedOwnerWorker(ownerID)
		log.Printf("worker: warning: could not start recovery worker for run %s: %v", id, startErr)
		return
	}
	r.workers[id], r.locks[id], r.gates[id], r.owners[id] = w, lock, gate, ownerID
	delete(r.named, id)
	log.Printf("worker: reclaiming nonterminal run %s on task queue %s with repository ownership", id, queue)
}

// runDockerEndpoint is the Docker run r recorded its sandboxes launch through.
func runDockerEndpoint(r *run.Run) daemonheartbeat.DockerEndpoint {
	return daemonheartbeat.DockerEndpoint{SandboxDocker: r.SandboxDocker, DockerHost: r.DockerHost, DockerContext: r.DockerContext}
}

// nameOnce logs format for run id unless an earlier scan already did.
func (r *repositoryReclaim) nameOnce(id, format string, args ...any) {
	if r.named[id] {
		return
	}
	r.named[id] = true
	log.Printf(format, args...)
}

// ensureOwnerWorker starts the worker that polls a reclaimed repository's
// owner queue unless one runs, and reports whether one does; r.mu is held.
func (r *repositoryReclaim) ensureOwnerWorker(ownerID, taskQueue string) bool {
	if _, ok := r.ownerWorkers[ownerID]; ok {
		return true
	}
	w := temporalworker.New(r.client, taskQueue, workflow.BoundedWorkerOptions(reclaimWorkerStopTimeout))
	w.RegisterWorkflow(workflow.RepositoryOwnerWorkflow)
	if err := w.Start(); err != nil {
		log.Printf("worker: warning: could not start the repository owner worker on %s: %v", taskQueue, err)
		return false
	}
	r.ownerWorkers[ownerID] = w
	return true
}

// stopUnusedOwnerWorker stops ownerID's owner worker once no reclaimed run
// belongs to it; r.mu is held.
func (r *repositoryReclaim) stopUnusedOwnerWorker(ownerID string) {
	for _, other := range r.owners {
		if other == ownerID {
			return
		}
	}
	if w, ok := r.ownerWorkers[ownerID]; ok {
		w.Stop()
		delete(r.ownerWorkers, ownerID)
	}
}

// reconcileAndRetire applies each reclaimed run's owner result to its record,
// then stops the worker and releases the ownership of those now terminal.
func (r *repositoryReclaim) reconcileAndRetire(ctx context.Context) {
	r.mu.Lock()
	owners := make(map[string]string, len(r.workers))
	for id := range r.workers {
		owners[id] = r.owners[id]
	}
	r.mu.Unlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, reclaimReconcileConcurrency)
reconcileLoop:
	for id, ownerID := range owners {
		select {
		case <-ctx.Done():
			break reconcileLoop
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id, ownerID string) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, err := reconcileReclaimedRun(r.dp, ctx, r.client, ownerID, id, r.dataDir); err != nil {
				log.Printf("worker: warning: could not reconcile reclaimed run %s against repository owner %s: %v", id, ownerID, err)
			}
		}(id, ownerID)
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	current := make(map[string]bool, len(r.workers))
	for id := range r.workers {
		current[id] = true
	}
	for _, id := range terminalReclaimedRunIDs(r.dataDir, current) {
		r.release(id, "reached a terminal state; stopped its recovery worker and released repository ownership")
	}
}

// release stops id's recovery worker and frees its lock and slot; r.mu is held.
func (r *repositoryReclaim) release(id, why string) {
	ownerID := r.owners[id]
	r.workers[id].Stop()
	if lock := r.locks[id]; lock != nil {
		if err := lock.Close(); err != nil {
			log.Printf("worker: warning: release repository ownership for reclaimed run %s: %v", id, err)
		}
	}
	_ = r.gates[id].Release()
	delete(r.workers, id)
	delete(r.locks, id)
	delete(r.gates, id)
	delete(r.owners, id)
	r.stopUnusedOwnerWorker(ownerID)
	log.Printf("worker: reclaimed run %s %s", id, why)
}

// stop releases every recovery and owner worker. No scan may be running.
// The workers stop at once, each waiting at most reclaimWorkerStopTimeout
// for an Activity in flight; the
// repository locks and compose slots are freed only after every worker
// stopped, so no Activity runs without them.
func (r *repositoryReclaim) stop() {
	r.mu.Lock()
	workers := make([]temporalworker.Worker, 0, len(r.workers)+len(r.ownerWorkers))
	for _, w := range r.workers {
		workers = append(workers, w)
	}
	for _, w := range r.ownerWorkers {
		workers = append(workers, w)
	}
	locks, gates := r.locks, r.gates
	r.workers, r.locks, r.gates = map[string]temporalworker.Worker{}, map[string]*wsisolation.DirectLock{}, map[string]*modelhost.Handle{}
	r.owners, r.ownerWorkers = map[string]string{}, map[string]temporalworker.Worker{}
	r.mu.Unlock()

	var wg sync.WaitGroup
	for _, w := range workers {
		wg.Add(1)
		go func(w temporalworker.Worker) {
			defer wg.Done()
			w.Stop()
		}(w)
	}
	wg.Wait()
	for id, lock := range locks {
		if err := lock.Close(); err != nil {
			log.Printf("worker: warning: release repository ownership for reclaimed run %s at shutdown: %v", id, err)
		}
		_ = gates[id].Release()
	}
	if len(locks) > 0 {
		log.Printf("worker: released %d reclaimed run(s) at worker shutdown; the next worker start recovers them again", len(locks))
	}
}

// run scans once at start and on every tick until ctx is done.
func (r *repositoryReclaim) run(ctx context.Context, ticks <-chan time.Time) {
	r.scan(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			r.scan(ctx)
		}
	}
}
