package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

// fakeRequestJobDocker is a docker stand-in whose every `ps` lists the lines
// of the returned psFile ("name<TAB>run-label"), which answers a removal-confirmation lookup (name=^...) with nothing, which logs each invocation to
// logFile, and which succeeds at everything else.
func fakeRequestJobDocker(t *testing.T, containers string) (docker, logFile string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	psFile := filepath.Join(dir, "ps.txt")
	logFile = filepath.Join(dir, "calls.log")
	if err := os.WriteFile(psFile, []byte(containers), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> " + logFile + "\n" +
		"case \"$*\" in *name=^*) ;; *) if [ \"$1\" = ps ]; then cat " + psFile + "; fi ;; esac\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return docker, logFile
}

func readRequestJobCalls(t *testing.T, logFile string) string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReconcileRequestJobOrphansRemovesOnlyDeadOwnersWithoutRunRecords(t *testing.T) {
	dataDir := t.TempDir()
	// A build's run record: its containers stay with the run reconcilers
	// even though its owner reads as dead.
	if err := (&run.Run{ID: "run-1", State: run.StateSliceRunning}).Save(dataDir); err != nil {
		t.Fatal(err)
	}
	docker, logFile := fakeRequestJobDocker(t,
		"factoryd-sandbox-dead\treq-dead\n"+
			"factoryd-relay-container-dead\treq-dead\n"+
			"factoryd-sandbox-live\treq-live\n"+
			"factoryd-relay-container-live\treq-live\n"+
			"factoryd-sandbox-build\trun-1\n"+
			"factoryd-registryproxy-container-dead\treq-dead\n")
	dead := map[string]bool{"req-dead": true, "run-1": true}

	removed, err := ReconcileRequestJobOrphans(context.Background(), docker, dataDir, func(id string) bool { return dead[id] })
	if err != nil {
		t.Fatalf("ReconcileRequestJobOrphans: %v", err)
	}

	if !slices.Contains(removed, "factoryd-sandbox-dead") || !slices.Contains(removed, "factoryd-relay-container-dead") || !slices.Contains(removed, "factoryd-relay-dead") {
		t.Errorf("removed = %v, want the dead request's worker, relay and relay network", removed)
	}
	calls := readRequestJobCalls(t, logFile)
	for _, name := range []string{"factoryd-sandbox-live", "factoryd-relay-container-live", "factoryd-sandbox-build", "factoryd-registryproxy-container-dead"} {
		if strings.Contains(calls, "rm -f "+name) || slices.Contains(removed, name) {
			t.Errorf("%s was removed; calls:\n%s", name, calls)
		}
	}
	// The worker goes before its relay, whose network cannot go while it is attached.
	if w, r := strings.Index(calls, "rm -f factoryd-sandbox-dead"), strings.Index(calls, "rm -f factoryd-relay-container-dead"); w < 0 || r < 0 || w > r {
		t.Errorf("want the worker removed before the relay; calls:\n%s", calls)
	}
}

func TestReconcileRequestJobOrphansLeavesALiveOwnerAlone(t *testing.T) {
	docker, logFile := fakeRequestJobDocker(t, "factoryd-sandbox-live\treq-live\nfactoryd-relay-container-live\treq-live\n")

	removed, err := ReconcileRequestJobOrphans(context.Background(), docker, t.TempDir(), func(string) bool { return false })
	if err != nil || len(removed) != 0 {
		t.Fatalf("removed = %v, err = %v, want nothing removed", removed, err)
	}
	if calls := readRequestJobCalls(t, logFile); strings.Contains(calls, "rm ") || strings.Contains(calls, "network") {
		t.Errorf("a live owner's job was touched; calls:\n%s", calls)
	}
}

func TestReconcileRequestJobOrphansReclaimsAStaleMarkerWithALivePidButNotAFreshOne(t *testing.T) {
	prevStale, prevDebounce := ownerStaleAfter, ownerDebounceAfter
	ownerDebounceAfter = func(time.Duration) <-chan time.Time {
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	t.Cleanup(func() { ownerStaleAfter, ownerDebounceAfter = prevStale, prevDebounce })

	dataDir := t.TempDir()
	for _, id := range []string{"req-stale", "req-fresh"} {
		if err := writeOwnerHeartbeat(dataDir, id); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(ownerPIDPath(dataDir, "req-stale"), old, old); err != nil {
		t.Fatal(err)
	}
	ownerStaleAfter = time.Minute
	docker, logFile := fakeRequestJobDocker(t, "factoryd-sandbox-stale\treq-stale\nfactoryd-sandbox-fresh\treq-fresh\n")

	// Every pid reads as alive, as a reused pid does.
	removed, err := ReconcileRequestJobOrphans(context.Background(), docker, dataDir, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"factoryd-sandbox-stale"}) {
		t.Errorf("removed = %v, want only the stale-marker container; calls:\n%s", removed, readRequestJobCalls(t, logFile))
	}
}
