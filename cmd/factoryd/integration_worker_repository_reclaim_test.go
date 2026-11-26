package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	temporalclient "go.temporal.io/sdk/client"

	"buildgate/internal/run"
	"buildgate/internal/sandbox"
	"buildgate/internal/workflow"
)

// seedAbandonedRepositoryRun stands in for a `-repository` submitter that
// died mid-run: a slice_running record and the run's request delivered to
// its repository owner, its child waiting on a queue only the dead submitter
// polled. With launched, the submitter got as far as a sandbox launch, so
// the record's sandbox-owner.pid names a process that is gone. verifyCommand
// is the run's verify step. It returns the run's id.
func seedAbandonedRepositoryRun(t *testing.T, address, dataDir, ws, verifyCommand string, launched bool) string {
	t.Helper()
	specPath := filepath.Join(t.TempDir(), "spec.md")
	if err := os.WriteFile(specPath, []byte("# fixture spec\nTests-Required: no -- integration fixture doesn't exercise tests_red_on_base\n"), 0o644); err != nil {
		t.Fatalf("write spec: %v", err)
	}
	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	repository := fmt.Sprintf("fixture/worker-reclaim-%d", time.Now().UnixNano())
	runID := fmt.Sprintf("fixture-abandoned-run-%d", time.Now().UnixNano())
	baseSHABytes, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v", err)
	}
	baseSHA := strings.TrimSpace(string(baseSHABytes))
	logDir := filepath.Join(dataDir, "logs", runID)
	checkpointDir := filepath.Join(dataDir, "temporal-checkpoints", runID)
	for _, dir := range []string{logDir, checkpointDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("create %s: %v", dir, err)
		}
	}
	seeded := &run.Run{
		ID:                     runID,
		Ticket:                 "fixture-ticket",
		WorkspacePath:          ws,
		ProjectPath:            ws,
		SpecPath:               specPath,
		Repository:             repository,
		State:                  run.StateSliceRunning,
		BaseSHA:                baseSHA,
		ProjectConfigCommitSHA: baseSHA,
		CreatedAt:              time.Now().Format(time.RFC3339Nano),
	}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed abandoned run.json: %v", err)
	}
	if launched {
		// The owner marker names the submitter, which has exited and been
		// reaped, so kill(pid, 0) says ESRCH.
		gone := exec.Command("true")
		if err := gone.Run(); err != nil {
			t.Fatalf("run a process to stand in for the dead submitter: %v", err)
		}
		writeSandboxOwnerPID(t, dataDir, runID, gone.Process.Pid)
		if !runOwnerDead(dataDir, runID) {
			t.Fatalf("seeded owner pid %d does not read as dead", gone.Process.Pid)
		}
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: address})
	if err != nil {
		t.Fatalf("dial Temporal: %v", err)
	}
	defer temporalClient.Close()
	ownerID := workflow.RepositoryOwnerWorkflowID(repository)
	terminateRepositoryOwnerAtCleanup(t, address, ownerID)
	taskQueue := "factoryd-repo-" + ownerID
	runInput := workflow.RunWorkflowInput{
		Ticket:              seeded.Ticket,
		WorkspacePath:       ws,
		IsolateWorkspace:    true,
		IsolatedRepoDir:     ws,
		IsolatedParentDir:   t.TempDir(),
		SpecPath:            specPath,
		BaseSHA:             baseSHA,
		RunID:               runID,
		DataDir:             dataDir,
		LogDir:              logDir,
		CheckpointDir:       checkpointDir,
		SandboxImage:        fakeSandboxImage,
		BuildAppInterpreter: "/bin/sh",
		BuildAppScript:      scriptPath,
		MaxRounds:           3,
		TimeoutMinutes:      45,
		BuildMaxAttempts:    1,
		VerifyCommand:       verifyCommand,
		VerifyMaxAttempts:   1,
		TestsRequiredOptOut: "not exercising tests_red_on_base in this fixture",
		TaskQueue:           taskQueue + "-run-" + runID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := temporalClient.SignalWithStartWorkflow(ctx, ownerID, workflow.SubmitRunSignalName,
		workflow.SubmitRunSignal{RequestID: runID, Input: runInput},
		temporalclient.StartWorkflowOptions{ID: ownerID, TaskQueue: taskQueue},
		workflow.RepositoryOwnerWorkflow, workflow.RepositoryOwnerWorkflowInput{Repository: repository}); err != nil {
		t.Fatalf("deliver the abandoned run to its repository owner: %v", err)
	}
	return runID
}

// writeSandboxOwnerPID writes runID's sandbox-owner.pid naming pid.
func writeSandboxOwnerPID(t *testing.T, dataDir, runID string, pid int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(run.Dir(dataDir, runID), "sandbox-owner.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatalf("write sandbox-owner.pid: %v", err)
	}
}

// workerProcess is one `factoryd worker` the test started.
type workerProcess struct {
	cmd  *exec.Cmd
	out  *synchronizedBuffer
	done chan struct{} // closed once cmd.Wait has returned
}

// startWorkerProcess starts the real `factoryd worker` on dataDir, with the
// fake build script in commit mode. It is stopped at cleanup if the test did
// not stop it.
func startWorkerProcess(t *testing.T, address, dataDir string) *workerProcess {
	t.Helper()
	args, env, _ := workerEndToEndArgs(t, dataDir)
	cmd := exec.Command(binPath, append(args, "-temporal-address", address)...)
	cmd.Env = append(env, "FAKE_BUILD_APP_MODE=commit")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p := &workerProcess{cmd: cmd, out: &synchronizedBuffer{}, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = p.out, p.out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.stop(t)
		if t.Failed() {
			t.Logf("worker %d output:\n%s", cmd.Process.Pid, p.out.String())
		}
	})
	return p
}

// stop sends the worker SIGTERM, as `factoryd stop` and `restart` do, and
// waits for it to exit; a second call does nothing.
func (p *workerProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		<-p.done
		t.Errorf("worker %d did not exit within 30s of SIGTERM", p.cmd.Process.Pid)
	}
}

// waitForRunState waits up to timeout for runID to be saved in want, and
// fails at once on any other terminal state.
func waitForRunState(t *testing.T, dataDir, runID string, want run.State, timeout time.Duration, workers ...*workerProcess) *run.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		r, err := run.Load(dataDir, runID)
		if err == nil && r.State == want {
			return r
		}
		if err == nil && (r.State == run.StateHalted || r.State == run.StateQuarantined || r.State == run.StateAccepted) {
			t.Fatalf("run %s state = %q (triage %q, halt %q), want %q", runID, r.State, r.Triage, r.HaltError, want)
		}
		if time.Now().After(deadline) {
			state := run.State("")
			if r != nil {
				state = r.State
			}
			for _, w := range workers {
				t.Logf("worker %d output:\n%s", w.cmd.Process.Pid, w.out.String())
			}
			t.Fatalf("run %s state = %q after %s (err=%v), want %q", runID, state, timeout, err, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestIntegrationWorkerStartRecoversAbandonedRepositoryRun drives the real
// `factoryd worker` start against a `-repository` run whose submitter died
// after its sandbox launched. The worker's start-up halt of runs whose owner
// is gone runs before the repository reclaim, and must leave this run to it:
// the reclaim then finishes the child on its own queue and the run reaches
// accepted, not a dead-owner halt.
func TestIntegrationWorkerStartRecoversAbandonedRepositoryRun(t *testing.T) {
	address := sharedTemporalAddress(t)
	ws := newFixtureRepo(t)
	dataDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	runID := seedAbandonedRepositoryRun(t, address, dataDir, ws, "true", true)

	w := startWorkerProcess(t, address, dataDir)
	recovered := waitForRunState(t, dataDir, runID, run.StateAccepted, 2*time.Minute, w)
	if !strings.Contains(w.out.String(), "reclaiming nonterminal run "+runID) {
		t.Errorf("worker accepted run %s without the repository reclaim naming it; output:\n%s", runID, w.out.String())
	}
	if recovered.HaltReasonCode != "" {
		t.Errorf("recovered run HaltReasonCode = %q, want none", recovered.HaltReasonCode)
	}
}

// TestIntegrationWorkerRestartReadoptsRepositoryRecovery stops the worker
// with SIGTERM, as `factoryd restart` and an upgrade do, while it recovers an
// abandoned `-repository` run, then starts another. The recovery had stamped
// the first worker's pid into the run's sandbox-owner.pid, which reads as a
// dead owner to the second; it must adopt the recovery instead of halting
// the run, and the run reaches accepted.
func TestIntegrationWorkerRestartReadoptsRepositoryRecovery(t *testing.T) {
	address := sharedTemporalAddress(t)
	ws := newFixtureRepo(t)
	dataDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve data dir: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	// Long enough that the first worker is stopped mid-recovery, short
	// enough that its step ends within the recovery worker's stop wait. No
	// owner marker from the submitter: the only dead owner the second worker
	// sees is the first worker.
	runID := seedAbandonedRepositoryRun(t, address, dataDir, ws, "sleep 3; true", false)

	first := startWorkerProcess(t, address, dataDir)
	deadline := time.Now().Add(time.Minute)
	for {
		if pid, ok := sandbox.OwnerPID(dataDir, runID); ok && pid == first.cmd.Process.Pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the first worker never launched run %s's sandbox; output:\n%s", runID, first.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	first.stop(t)
	if r, err := run.Load(dataDir, runID); err != nil || r.State != run.StateSliceRunning {
		t.Fatalf("run %s after the first worker stopped: state %q (err=%v), want %q; output:\n%s", runID, r.State, err, run.StateSliceRunning, first.out.String())
	}
	if !runOwnerDead(dataDir, runID) {
		pid, _ := sandbox.OwnerPID(dataDir, runID)
		t.Fatalf("run %s owner pid %d does not read as dead after the worker that stamped it stopped", runID, pid)
	}

	second := startWorkerProcess(t, address, dataDir)
	waitForRunState(t, dataDir, runID, run.StateAccepted, 2*time.Minute, first, second)
	if !strings.Contains(second.out.String(), "reclaiming nonterminal run "+runID) {
		t.Errorf("the second worker did not adopt run %s's recovery; output:\n%s", runID, second.out.String())
	}
}
