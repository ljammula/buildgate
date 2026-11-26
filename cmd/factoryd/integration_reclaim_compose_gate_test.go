package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"buildgate/internal/modelhost"
	"buildgate/internal/run"
)

// TestIntegrationWorkerReclaimWaitsForComposeServicesSlot: the worker's
// recovery Worker runs a crashed submitter's remaining phases, which
// launch sidecars, so reclaiming a run whose compose file has services
// needs the host-wide compose slot the submitter held. While another run
// holds it, the reclaim is deferred to a later scan; once released, the
// run is reclaimed.
func TestIntegrationWorkerReclaimWaitsForComposeServicesSlot(t *testing.T) {
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

	// The slot below and the worker's reclaim share one locks dir.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("FAKE_BUILD_APP_MODE", "commit")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
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

	scriptPath, err := filepath.Abs("testdata/fake_build_app.sh")
	if err != nil {
		t.Fatal(err)
	}
	out := startWorkerReclaim(t, address, dataDir, scriptPath)

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
		t.Fatalf("worker reclaimed %s while another run held the compose slot:\n%s", runID, out.String())
	}

	if err := held.Release(); err != nil {
		t.Fatalf("release the compose slot: %v", err)
	}
	released = true
	waitFor(t, 90*time.Second, "reclaim after the slot freed", func() bool {
		return strings.Contains(out.String(), "reclaiming nonterminal run "+runID)
	})
}
