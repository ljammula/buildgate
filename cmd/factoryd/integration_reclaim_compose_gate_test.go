package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"buildgate/internal/daemonheartbeat"
	"buildgate/internal/modelhost"
	"buildgate/internal/run"
	"buildgate/internal/workflow"
)

// TestIntegrationDaemonReclaimWaitsForComposeServicesSlot: the daemon's
// recovery Worker runs a crashed submitter's remaining phases, which
// launch sidecars, so reclaiming a run whose compose file has services
// needs the host-wide compose slot the submitter held. While another run
// holds it, the reclaim is deferred to a later scan; once released, the
// run is reclaimed.
func TestIntegrationDaemonReclaimWaitsForComposeServicesSlot(t *testing.T) {
	// not parallel-safe: isolatedTemporalAddress calls t.Setenv.
	address := isolatedTemporalAddress(t)
	ws := newFixtureRepo(t)
	if err := os.WriteFile(filepath.Join(ws, "compose.yaml"), []byte("services:\n  db:\n    image: postgres:16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "compose.yaml"}, {"commit", "-q", "-m", "compose"}} {
		if out, err := exec.Command("git", append([]string{"-C", ws}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	baseSHA, err := exec.Command("git", "-C", ws, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	dataDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository := fmt.Sprintf("fixture/reclaim-compose-%d", time.Now().UnixNano())

	env := isolatedSessionConfigEnv(t, "sandbox_docker: "+fakeSandboxDockerBinary(t)+"\nsandbox_image: "+fakeSandboxImage+"\nregistry_proxy: false\n")
	for _, kv := range env {
		if home, ok := strings.CutPrefix(kv, "HOME="); ok {
			t.Setenv("HOME", home) // the slot below and the daemon's share one locks dir
		}
	}
	held, err := modelhost.AcquireNamed(context.Background(), "compose-services", "run-holding-the-slot", 1, nil)
	if err != nil {
		t.Fatalf("hold the compose slot: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = held.Release()
		}
	}()

	daemonCmd := factorydCommand(t, "daemon", "-temporal-address", address, "-repository", repository, "-data-dir", dataDir)
	daemonCmd.Env = append(append(os.Environ(), "FAKE_BUILD_APP_MODE=commit", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), env...)
	var out synchronizedBuffer
	daemonCmd.Stdout, daemonCmd.Stderr = &out, &out
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("start factoryd daemon: %v", err)
	}
	t.Cleanup(func() {
		_ = daemonCmd.Process.Signal(syscall.SIGTERM)
		_ = daemonCmd.Wait()
		t.Logf("daemon output:\n%s", out.String())
	})
	heartbeatPath := daemonheartbeat.Path(dataDir, workflow.RepositoryOwnerWorkflowID(repository))
	waitFor(t, 15*time.Second, "daemon heartbeat", func() bool { _, err := daemonheartbeat.Read(heartbeatPath); return err == nil })

	runID := fmt.Sprintf("fixture-crashed-compose-run-%d", time.Now().UnixNano())
	seeded := &run.Run{ID: runID, Ticket: "fixture-ticket", WorkspacePath: ws, ProjectPath: ws, Repository: repository,
		State: run.StateSliceRunning, BaseSHA: strings.TrimSpace(string(baseSHA)), CreatedAt: time.Now().Format(time.RFC3339Nano)}
	if err := seeded.Save(dataDir); err != nil {
		t.Fatalf("seed crashed-submitter run.json: %v", err)
	}

	waitFor(t, 90*time.Second, "reclaim deferred on the compose slot", func() bool {
		return strings.Contains(out.String(), "recovery for run "+runID+" deferred until the compose sidecars slot is free")
	})
	if strings.Contains(out.String(), "reclaiming nonterminal run "+runID) {
		t.Fatalf("daemon reclaimed %s while another run held the compose slot:\n%s", runID, out.String())
	}

	if err := held.Release(); err != nil {
		t.Fatalf("release the compose slot: %v", err)
	}
	released = true
	waitFor(t, 90*time.Second, "reclaim after the slot freed", func() bool {
		return strings.Contains(out.String(), "reclaiming nonterminal run "+runID)
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
