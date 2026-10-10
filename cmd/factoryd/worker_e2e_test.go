package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/hostcontrol"
	"buildgate/internal/request"
	"buildgate/internal/run"
	"buildgate/internal/testfixture"
)

// workerEndToEndArgs is the `factoryd worker` argv the end-to-end test starts,
// without -temporal-address: testdata/worker_e2e's stand-ins for the model
// scripts, its relay-aware fake Docker through the session config, and no
// doctor preflight (it needs a real Docker daemon). configPath is the session
// config it wrote, for the CLI commands of the same test.
func workerEndToEndArgs(t *testing.T, dataDir string) (args, env []string, configPath string) {
	t.Helper()
	abs := func(name string) string {
		path, err := filepath.Abs(filepath.Join("testdata", "worker_e2e", name))
		if err != nil {
			t.Fatalf("resolve testdata/worker_e2e/%s: %v", name, err)
		}
		return path
	}
	configPath = filepath.Join(t.TempDir(), "config.yml")
	writeSessionConfig(t, configPath, "sandbox_docker: "+abs("docker.sh")+"\n"+
		"routes:\n  fixture:\n    credential_mode: static\n    upstream: https://model.example.invalid\n    credential_env: WORKER_E2E_TEST_KEY\n"+
		"models:\n  fixture-model:\n    id: fixture-model-id\n    api: openai-completions\n    routes: [fixture]\n"+
		"roles:\n  planning: { model: fixture-model }\n  execution: { model: fixture-model }\n  review: { model: fixture-model, allow_shared_model: true }\n")
	args = []string{
		"worker",
		"-config", configPath,
		"-data-dir", dataDir,
		"-skip-doctor",
		"-open-pull-request=false",
		"-sandbox-image", fakeSandboxImage,
		"-draft-spec-script", abs("draft_spec.py"),
		"-plan-tickets-script", abs("plan_tickets.py"),
		// conformity_review.py is found beside the build script.
		"-build-app-script", abs("build_app.py"),
	}
	env = append(os.Environ(), "WORKER_E2E_TEST_KEY=sk-test", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return args, env, configPath
}

// TestWorkerEndToEndRequestReachesAcceptedRun starts the real `factoryd
// worker` binary against the test Temporal server and the fake Docker, submits
// a request, approves its spec and plan through the CLI, and waits for the
// ticket's run to be accepted. It covers the wiring the unit tests stub out:
// the worker's own Temporal address, the request workflow and its activities,
// the wake a CLI decision sends, and the argv buildTicketRunArgs hands the
// build (a missing -temporal-address, -spec or -verify-command fails it).
func TestWorkerEndToEndRequestReachesAcceptedRun(t *testing.T) {
	address := sharedTemporalAddress(t)
	ws := newFixtureRepo(t)
	testfixture.CommitAgentsFile(t, ws)
	dataDir := t.TempDir()
	workerArgs, env, configPath := workerEndToEndArgs(t, dataDir)

	worker := exec.Command(binPath, append(workerArgs, "-temporal-address", address)...)
	worker.Env = env
	// Its own process group, so a forced stop also takes the job it is running.
	worker.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var workerOut synchronizedBuffer
	worker.Stdout, worker.Stderr = &workerOut, &workerOut
	if err := worker.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- worker.Wait() }()
	t.Cleanup(func() {
		_ = worker.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(30 * time.Second):
			_ = syscall.Kill(-worker.Process.Pid, syscall.SIGKILL)
			<-exited
		}
		if t.Failed() {
			t.Logf("worker output:\n%s", workerOut.String())
		}
	})

	// cli runs one factoryd command and returns its stdout.
	cli := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(binPath, args...)
		cmd.Env = env
		var stderr strings.Builder
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("factoryd %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
		}
		return string(out)
	}
	// failIfWorkerExited fails the test at once when the worker is gone.
	failIfWorkerExited := func(what string) {
		t.Helper()
		select {
		case err := <-exited:
			exited <- err // for the cleanup
			t.Fatalf("worker exited while waiting for %s: %v", what, err)
		default:
		}
	}
	// waitForRequest polls the request until done reports true, failing at
	// once when the worker exits or the request stops in a failure state.
	waitForRequest := func(id, what string, done func(*request.Request) bool) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Minute)
		for {
			failIfWorkerExited(what)
			r, err := request.Load(dataDir, id)
			if err == nil && done(r) {
				return
			}
			if err == nil && (r.State == request.StateHalted || r.State == request.StateQuarantined) {
				raw, _ := os.ReadFile(filepath.Join(request.Dir(dataDir, id), "request.json"))
				t.Fatalf("request is %s while waiting for %s:\n%s", r.State, what, raw)
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (request: %+v, load error: %v)", what, r, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	inState := func(state request.State) func(*request.Request) bool {
		return func(r *request.Request) bool { return r.State == state }
	}

	// The worker's heartbeat is what submit and approve read to wake it.
	waitFor(t, time.Minute, "the worker's heartbeat", func() bool {
		failIfWorkerExited("the worker's heartbeat")
		_, err := os.Stat(workerHeartbeatPath(dataDir))
		return err == nil
	})

	out := cli("submit", "-config", configPath, "-data-dir", dataDir, "-verify-command", "true", ws, "Append a line to content.txt")
	id, _, _ := strings.Cut(out, "\n")
	if _, err := request.Load(dataDir, id); err != nil {
		t.Fatalf("submit's first stdout line %q is not a saved request (%v):\n%s", id, err, out)
	}

	waitForRequest(id, "spec_review", inState(request.StateSpecReview))
	cli("approve", "-config", configPath, "-data-dir", dataDir, id)
	waitForRequest(id, "plan_review", inState(request.StatePlanReview))
	cli("approve", "-config", configPath, "-data-dir", dataDir, id)

	// The ticket's run, once its record is terminal and the request has taken
	// its outcome (a halted or quarantined request fails the wait itself).
	var record *run.Run
	waitForRequest(id, "the ticket's run to finish", func(r *request.Request) bool {
		if len(r.Tickets) != 1 || r.Tickets[0].RunID == "" || r.State == request.StateBuilding {
			return false
		}
		loaded, err := run.Load(dataDir, r.Tickets[0].RunID)
		if err != nil || !loaded.TerminalConfirmed() {
			return false
		}
		record = loaded
		return true
	})
	runID := record.ID
	if record.State != run.StateAccepted {
		t.Fatalf("run %s state = %q, want %q", runID, record.State, run.StateAccepted)
	}
	if record.RequestID != id {
		t.Errorf("run %s request id = %q, want %q", runID, record.RequestID, id)
	}
	// The spec's one criterion reached the review step (-spec-acceptance-criteria).
	if len(record.SpecConformityVerdicts) != 1 || record.SpecConformityVerdicts[0].Verdict != "clean" {
		t.Errorf("run %s spec conformity verdicts = %+v, want one clean verdict", runID, record.SpecConformityVerdicts)
	}
}

// TestWorkerEndToEndRefusesToStartWithoutATemporalAddress starts the real
// binary with no -temporal-address and autostart off (as every test
// subprocess has it): the worker must stop with the address error rather than
// go on with an empty address, which the Temporal client reads as
// localhost:7233.
func TestWorkerEndToEndRefusesToStartWithoutATemporalAddress(t *testing.T) {
	workerArgs, env, _ := workerEndToEndArgs(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, workerArgs...)
	cmd.Env = append(env, hostcontrol.AutostartEnvVar+"=0")
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "no Temporal address: pass -temporal-address") {
		t.Fatalf("worker with no -temporal-address and autostart off: err = %v, want the address error; output:\n%s", err, out)
	}
}
