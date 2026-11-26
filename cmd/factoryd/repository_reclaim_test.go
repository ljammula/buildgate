package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	temporalworker "go.temporal.io/sdk/worker"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/requestdriver"
	"buildgate/internal/run"
	wsisolation "buildgate/internal/workspace"
)

// newTestRepositoryReclaim is a repositoryReclaim of dataDir on the fake
// sandbox settings, with no Temporal client: the tests below never reach a
// worker start.
func newTestRepositoryReclaim(t *testing.T, dataDir string) *repositoryReclaim {
	t.Helper()
	script, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatal(err)
	}
	cfg := requestdriver.WorkerConfig{Settings: *fakeSandboxSettings(t), BuildAppScript: script}
	r, err := newRepositoryReclaim(newTestDeps(t), dataDir, cfg, nil)
	if err != nil {
		t.Fatalf("new repository reclaim: %v", err)
	}
	return r
}

// seedRepositoryRun saves a slice_running `-repository` run of ws, with mutate
// applied first.
func seedRepositoryRun(t *testing.T, dataDir, ws, repository string, mutate func(*run.Run)) string {
	t.Helper()
	r := &run.Run{
		ID:            "fixture-repository-run",
		Ticket:        "fixture-ticket",
		WorkspacePath: ws,
		ProjectPath:   ws,
		Repository:    repository,
		State:         run.StateSliceRunning,
		CreatedAt:     time.Now().Format(time.RFC3339Nano),
	}
	if mutate != nil {
		mutate(r)
	}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return r.ID
}

// TestRepositoryReclaimSkipsABusyRepositoryAtOnce: a repository whose lock a
// live submitter holds is skipped without a wait, starts no owner worker, and
// is named in the log once across scans.
func TestRepositoryReclaimSkipsABusyRepositoryAtOnce(t *testing.T) {
	out := captureLog(t)
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	id := seedRepositoryRun(t, dataDir, ws, "fixture/busy", nil)
	held, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("hold the repository lock: %v", err)
	}
	defer held.Close()

	r := newTestRepositoryReclaim(t, dataDir)
	started := time.Now()
	for range 2 {
		r.reclaimRepository(context.Background(), "fixture/busy")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("two scans of a busy repository took %s, want no wait for its lock", elapsed)
	}
	if len(r.workers) != 0 || len(r.ownerWorkers) != 0 || len(r.locks) != 0 {
		t.Errorf("busy repository: %d recovery, %d owner workers and %d locks, want none", len(r.workers), len(r.ownerWorkers), len(r.locks))
	}
	if n := strings.Count(out.String(), "recovery for run "+id+" deferred"); n != 1 {
		t.Errorf("the log named the deferred run %d times over two scans, want once:\n%s", n, out.String())
	}
}

// TestRepositoryReclaimLeavesARunOnAnotherDocker: a run whose recorded Docker
// differs from the worker's is not recovered: its repository lock stays free,
// no worker starts, and the log names it once across scans.
func TestRepositoryReclaimLeavesARunOnAnotherDocker(t *testing.T) {
	out := captureLog(t)
	ws := newFixtureRepo(t)
	dataDir := t.TempDir()
	id := seedRepositoryRun(t, dataDir, ws, "fixture/other-docker", func(r *run.Run) {
		r.SandboxDocker, r.DockerHost = "/opt/other/docker", "tcp://other.invalid:2376"
	})
	r := newTestRepositoryReclaim(t, dataDir)
	for range 2 {
		r.reclaimRepository(context.Background(), "fixture/other-docker")
	}
	if len(r.workers) != 0 || len(r.ownerWorkers) != 0 || len(r.locks) != 0 {
		t.Errorf("run on another Docker: %d recovery, %d owner workers and %d locks, want none", len(r.workers), len(r.ownerWorkers), len(r.locks))
	}
	if n := strings.Count(out.String(), "run "+id+" is not recovered"); n != 1 {
		t.Errorf("the log named the refused run %d times over two scans, want once:\n%s", n, out.String())
	}
	probe, err := wsisolation.AcquireDirectLock(ws)
	if err != nil {
		t.Fatalf("repository lock after a refused recovery: %v, want it free", err)
	}
	_ = probe.Close()
	if got, _ := run.Load(dataDir, id); got.State != run.StateSliceRunning {
		t.Errorf("refused run state = %q, want it left %q", got.State, run.StateSliceRunning)
	}
}

// slowStopWorker is a recovery worker whose Stop takes d.
type slowStopWorker struct {
	temporalworker.Worker
	d       time.Duration
	stopped *atomic.Int32
}

func (w slowStopWorker) Stop() {
	time.Sleep(w.d)
	w.stopped.Add(1)
}

// TestRepositoryReclaimStopStopsEveryWorkerAtOnce: stop waits for every
// recovery and owner worker together, so its total is one worker's stop wait,
// not their sum, and it empties the reclaim.
func TestRepositoryReclaimStopStopsEveryWorkerAtOnce(t *testing.T) {
	r := newTestRepositoryReclaim(t, t.TempDir())
	var stopped atomic.Int32
	const n = 6
	for i := range n {
		w := slowStopWorker{d: 500 * time.Millisecond, stopped: &stopped}
		if i%2 == 0 {
			r.workers[string(rune('a'+i))] = w
		} else {
			r.ownerWorkers[string(rune('a'+i))] = w
		}
	}
	started := time.Now()
	r.stop()
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("stop of %d workers took %s, want them stopped at once", n, elapsed)
	}
	if stopped.Load() != n {
		t.Errorf("stopped %d workers, want %d", stopped.Load(), n)
	}
	if len(r.workers) != 0 || len(r.ownerWorkers) != 0 {
		t.Errorf("after stop: %d recovery and %d owner workers left, want none", len(r.workers), len(r.ownerWorkers))
	}
	if reclaimWorkerStopTimeout >= 15*time.Second {
		t.Errorf("reclaimWorkerStopTimeout = %s: one stop wait must stay under the 15 s `factoryd stop` waits", reclaimWorkerStopTimeout)
	}
}

// TestCheckSandboxDockerAgainstWorker: a sandboxed `-repository` run is
// refused only when the data dir's worker, by a fresh heartbeat, uses another
// Docker; no worker, a matching one or an unsandboxed run passes.
func TestCheckSandboxDockerAgainstWorker(t *testing.T) {
	local := daemonheartbeat.DockerEndpoint{SandboxDocker: "/usr/local/bin/docker"}
	other := daemonheartbeat.DockerEndpoint{SandboxDocker: "/usr/local/bin/docker", DockerContext: "remote"}
	writeWorker := func(t *testing.T, dataDir string, e daemonheartbeat.DockerEndpoint) {
		t.Helper()
		if err := daemonheartbeat.Write(daemonheartbeat.WorkerPath(dataDir), daemonheartbeat.Heartbeat{UpdatedAt: time.Now().Format(time.RFC3339Nano), DockerEndpoint: e}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("no worker", func(t *testing.T) {
		if err := checkSandboxDockerAgainstWorker(t.TempDir(), "r", "repo", fakeSandboxImage, local); err != nil {
			t.Fatalf("no worker heartbeat: %v, want nil", err)
		}
	})
	t.Run("same Docker", func(t *testing.T) {
		dir := t.TempDir()
		writeWorker(t, dir, local)
		if err := checkSandboxDockerAgainstWorker(dir, "r", "repo", fakeSandboxImage, local); err != nil {
			t.Fatalf("matching worker: %v, want nil", err)
		}
	})
	t.Run("another Docker", func(t *testing.T) {
		dir := t.TempDir()
		writeWorker(t, dir, other)
		err := checkSandboxDockerAgainstWorker(dir, "r", "repo", fakeSandboxImage, local)
		if err == nil || !strings.Contains(err.Error(), `DOCKER_CONTEXT="remote"`) {
			t.Fatalf("diverging worker: %v, want a refusal naming its DOCKER_CONTEXT", err)
		}
	})
	t.Run("unsandboxed", func(t *testing.T) {
		dir := t.TempDir()
		writeWorker(t, dir, other)
		if err := checkSandboxDockerAgainstWorker(dir, "r", "repo", "", local); err != nil {
			t.Fatalf("unsandboxed run: %v, want nil", err)
		}
	})
}

// TestRecordRepositoryDockerOnlyForRepositoryRuns: a `-repository` run
// records the Docker it launches through; any other run records none.
func TestRecordRepositoryDockerOnlyForRepositoryRuns(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///fixture.sock")
	t.Setenv("DOCKER_CONTEXT", "")
	plain := &run.Run{}
	recordRepositoryDocker(plain, "docker")
	if plain.SandboxDocker != "" || plain.DockerHost != "" {
		t.Errorf("plain run recorded Docker %q/%q, want none", plain.SandboxDocker, plain.DockerHost)
	}
	owned := &run.Run{Repository: "fixture/repo"}
	recordRepositoryDocker(owned, "docker")
	want := daemonheartbeat.LocalDockerEndpoint("docker")
	if runDockerEndpoint(owned) != want {
		t.Errorf("repository run recorded %+v, want %+v", runDockerEndpoint(owned), want)
	}
}
