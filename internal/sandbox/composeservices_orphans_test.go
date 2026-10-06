package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// ownerHooksFor returns dataDir's own ownership-label hash -- t.TempDir()
// already returns an absolute path, matching what
// ReconcileComposeServicesOrphans itself hashes internally (filepath.Abs
// is a no-op on an already-absolute path), so tests can use this directly
// as the value a real ProjectOwner/NetworkOwner would report for a
// container/network this dataDir actually owns.
func ownerHooksFor(dataDir string, _, _ string) (dataDirHash string) {
	return dataDirLabel(dataDir)
}

// TestReconcileComposeServicesOrphansLeavesLiveProjectAndNetworkAlone: a
// project/network whose run isRunLive reports true for must never be torn
// down -- the exact race ReconcileRelayOrphans' own "in-flight" test
// guards against, here for compose services' two resources instead of the
// relay's.
func TestReconcileComposeServicesOrphansLeavesLiveProjectAndNetworkAlone(t *testing.T) {
	dataDir := t.TempDir()
	hash := ownerHooksFor(dataDir, "live-run", "live-run")
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) {
			return []string{"bg-live-run-a1"}, nil
		},
		ProjectOwner: func(context.Context, string, string) (string, string, error) {
			return hash, "live-run", nil
		},
		DownProject: func(context.Context, string, string) error {
			t.Fatal("DownProject called for a live run's project")
			return nil
		},
		ListNetworks: func(context.Context, string) ([]string, error) {
			return []string{"bg-compose-live-run"}, nil
		},
		NetworkOwner: func(context.Context, string, string) (string, string, error) {
			return hash, "live-run", nil
		},
		RemoveNetwork: func(context.Context, string, string) error {
			t.Fatal("RemoveNetwork called for a live run's network")
			return nil
		},
		NetworkPresent: func(context.Context, string, string) (bool, error) {
			t.Fatal("NetworkPresent called for a live run's network")
			return false, nil
		},
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return true }, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want nothing removed for a live run", removed)
	}
}

// TestReconcileComposeServicesOrphansTearsDownDeadProject: a project whose
// run is not live, and whose ownership label matches this dataDir, gets
// torn down via DownProject and is reported removed.
func TestReconcileComposeServicesOrphansTearsDownDeadProject(t *testing.T) {
	dataDir := t.TempDir()
	hash := ownerHooksFor(dataDir, "dead-run", "")
	var downed []string
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) {
			return []string{"bg-dead-run-a2"}, nil
		},
		ProjectOwner: func(context.Context, string, string) (string, string, error) {
			return hash, "dead-run", nil
		},
		DownProject: func(_ context.Context, _ string, project string) error {
			downed = append(downed, project)
			return nil
		},
		ListNetworks: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return false }, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if len(downed) != 1 || downed[0] != "bg-dead-run-a2" {
		t.Fatalf("downed = %v, want exactly [bg-dead-run-a2]", downed)
	}
	if len(removed) != 1 || removed[0] != "bg-dead-run-a2" {
		t.Fatalf("removed = %v, want exactly [bg-dead-run-a2]", removed)
	}
}

// TestReconcileComposeServicesOrphansLeavesUnownedProjectAlone covers a
// finding from Codex review of PR #131, round 2: a project whose
// ProjectOwner reports no data-dir label at all (a legacy, pre-labeling
// project, or one belonging to a different factoryd installation sharing
// this Docker daemon) must never be torn down, even when isRunLive would
// otherwise report it dead -- ownership cannot be confirmed, so this path
// fails closed rather than assuming it's safe to remove.
func TestReconcileComposeServicesOrphansLeavesUnownedProjectAlone(t *testing.T) {
	dataDir := t.TempDir()
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) {
			return []string{"bg-unlabeled-run-a1"}, nil
		},
		ProjectOwner: func(context.Context, string, string) (string, string, error) {
			return "", "", nil // no container carries the ownership label
		},
		DownProject: func(context.Context, string, string) error {
			t.Fatal("DownProject called for a project whose ownership could not be confirmed")
			return nil
		},
		ListNetworks: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return false }, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want the unowned project left alone", removed)
	}
}

// TestReconcileComposeServicesOrphansLeavesOtherInstallationsProjectAlone
// is the other half of that same finding: a project whose ProjectOwner
// reports a data-dir hash for a DIFFERENT installation must be left
// alone too, even though this installation's own isRunLive obviously
// has no record of whatever run owns it (it belongs to another
// -data-dir entirely).
func TestReconcileComposeServicesOrphansLeavesOtherInstallationsProjectAlone(t *testing.T) {
	dataDir := t.TempDir()
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) {
			return []string{"bg-other-installations-run-a1"}, nil
		},
		ProjectOwner: func(context.Context, string, string) (string, string, error) {
			return "some-other-installations-data-dir-hash", "other-installations-run", nil
		},
		DownProject: func(context.Context, string, string) error {
			t.Fatal("DownProject called for another installation's project")
			return nil
		},
		ListNetworks: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	// isRunLive always false here on purpose: this installation has no
	// idea about the other installation's run at all, and must not treat
	// "unknown to me" as "safe to remove" for a project it doesn't own.
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return false }, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want the other installation's project left alone", removed)
	}
}

// TestReconcileComposeServicesOrphansLeavesNetworkWithContainersAlone: a
// dead run's network that still has containers attached (RemoveNetwork is
// attempted but NetworkPresent still reports it present -- `docker network
// rm` itself refuses while any container remains joined) is left alone,
// not reported removed, and not treated as an error -- mirroring
// ReconcileRelayOrphans' own remove-then-confirm tolerance.
func TestReconcileComposeServicesOrphansLeavesNetworkWithContainersAlone(t *testing.T) {
	dataDir := t.TempDir()
	hash := ownerHooksFor(dataDir, "", "dead-run")
	var removeCalled bool
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) { return nil, nil },
		ListNetworks: func(context.Context, string) ([]string, error) {
			return []string{"bg-compose-dead-run"}, nil
		},
		NetworkOwner: func(context.Context, string, string) (string, string, error) {
			return hash, "dead-run", nil
		},
		RemoveNetwork: func(context.Context, string, string) error {
			removeCalled = true
			return nil
		},
		NetworkPresent: func(context.Context, string, string) (bool, error) {
			return true, nil // a container is still attached
		},
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return false }, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if !removeCalled {
		t.Fatal("RemoveNetwork was never attempted for the dead run's network")
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want the still-attached network left alone", removed)
	}
}

// TestReconcileComposeServicesOrphansRemovesNetworkWithNoContainers: a dead
// run's network with nothing left attached, whose ownership label matches
// this dataDir, is removed and reported.
func TestReconcileComposeServicesOrphansRemovesNetworkWithNoContainers(t *testing.T) {
	dataDir := t.TempDir()
	hash := ownerHooksFor(dataDir, "", "dead-run")
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) { return nil, nil },
		ListNetworks: func(context.Context, string) ([]string, error) {
			return []string{"bg-compose-dead-run"}, nil
		},
		NetworkOwner: func(context.Context, string, string) (string, string, error) {
			return hash, "dead-run", nil
		},
		RemoveNetwork: func(context.Context, string, string) error { return nil },
		NetworkPresent: func(context.Context, string, string) (bool, error) {
			return false, nil
		},
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return false }, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "bg-compose-dead-run" {
		t.Fatalf("removed = %v, want exactly [bg-compose-dead-run]", removed)
	}
}

// TestReconcileComposeServicesOrphansOneFailingProjectDoesNotBlockOthers:
// two dead runs' projects, one of which fails to go down, must not prevent
// the other from being reconciled -- the same per-resource fault tolerance
// ReconcileRelayOrphans' own doc comment requires.
func TestReconcileComposeServicesOrphansOneFailingProjectDoesNotBlockOthers(t *testing.T) {
	dataDir := t.TempDir()
	hash := ownerHooksFor(dataDir, "", "")
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) {
			return []string{"bg-broken-run-a1", "bg-fine-run-a1"}, nil
		},
		ProjectOwner: func(_ context.Context, _ string, project string) (string, string, error) {
			runID := strings.TrimSuffix(strings.TrimPrefix(project, "bg-"), "-a1")
			return hash, runID, nil
		},
		DownProject: func(_ context.Context, _ string, project string) error {
			if project == "bg-broken-run-a1" {
				return errors.New("docker compose down: connection refused")
			}
			return nil
		},
		ListNetworks: func(context.Context, string) ([]string, error) { return nil, nil },
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, func(string) bool { return false }, hooks)
	if err == nil || !strings.Contains(err.Error(), "bg-broken-run-a1") {
		t.Fatalf("err = %v, want an error naming the failing project", err)
	}
	if len(removed) != 1 || removed[0] != "bg-fine-run-a1" {
		t.Fatalf("removed = %v, want exactly [bg-fine-run-a1] despite the other project's failure", removed)
	}
}

// TestReconcileComposeServicesOrphansIgnoresUnrelatedNames covers filtering:
// a project/network with no compose-services prefix is skipped before
// ProjectOwner/NetworkOwner is ever consulted, and never passed to
// isRunLive at all.
func TestReconcileComposeServicesOrphansIgnoresUnrelatedNames(t *testing.T) {
	dataDir := t.TempDir()
	var sawRunID string
	hooks := ComposeServicesOrphanHooks{
		ListProjects: func(context.Context, string) ([]string, error) {
			return []string{"unrelated-project", "bg-fix-bug-a5-a3"}, nil
		},
		ProjectOwner: func(_ context.Context, _ string, project string) (string, string, error) {
			if project != "bg-fix-bug-a5-a3" {
				t.Fatalf("ProjectOwner called for an unrelated project %q", project)
			}
			return dataDirLabel(dataDir), "fix-bug-a5", nil
		},
		DownProject: func(_ context.Context, _ string, project string) error { return nil },
		ListNetworks: func(context.Context, string) ([]string, error) {
			return []string{"some-other-network"}, nil
		},
	}
	isRunLive := func(runID string) bool {
		sawRunID = runID
		return true // live, so nothing here is actually torn down
	}
	removed, err := ReconcileComposeServicesOrphans(context.Background(), "docker", dataDir, isRunLive, hooks)
	if err != nil {
		t.Fatalf("ReconcileComposeServicesOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want nothing removed", removed)
	}
	if sawRunID != "fix-bug-a5" {
		t.Fatalf("isRunLive saw runID %q, want %q (from the project's own \"buildgate.run\" label, not parsed from its name)", sawRunID, "fix-bug-a5")
	}
}

// TestComposeServicesDownProjectTearsDownAfterComposeFileIsGoneLiveDocker
// addresses a finding from Codex's review of PR #131: the claim that
// `docker compose -p <project> down -v --remove-orphans`, run with no `-f`
// from an unrelated working directory (the crash-recovery shape this
// reconciliation exists for -- the synthesized compose file lived under
// the dead run's own, possibly-already-gone data directory), would fail
// with "no configuration file provided" because Compose falls back to
// searching the current directory and its parents for a compose file.
//
// Manually verified against the real Docker Compose CLI (v5.4.0) before
// writing this test: `docker compose -p <project> down -v
// --remove-orphans` with no `-f` succeeds by reconstructing the project's
// containers, volumes, and network purely from their own
// com.docker.compose.project label -- with no compose file anywhere in the
// search path, with the project's containers already stopped, and even
// with an unrelated compose.yaml file sitting in the invoking cwd. This
// matches composeServicesDownProject's own doc comment ("`docker compose
// -p <project> down -v --remove-orphans` needs no -f: down (unlike up)
// only ever acts on what is already labeled with this project name"),
// written before this review round. This test locks that verified
// behavior in as a regression guard rather than changing the teardown
// command -- see this fix's own PR comment reply for the full empirical
// record.
func TestComposeServicesDownProjectTearsDownAfterComposeFileIsGoneLiveDocker(t *testing.T) {
	if os.Getenv("DOCKER_SANDBOX_LIVE") != "1" {
		t.Skip("set DOCKER_SANDBOX_LIVE=1 to run the real Docker crash-recovery orphan teardown test")
	}
	dockerBinary := "docker"
	if _, err := exec.LookPath(dockerBinary); err != nil {
		t.Skipf("docker CLI not available on this host: %v", err)
	}

	project := "bg-orphan-livetest-a1"
	composeDir := t.TempDir()
	composePath := composeDir + "/compose.yml"
	if err := os.WriteFile(composePath, []byte("services:\n  web:\n    image: alpine:3.20\n    command: [\"sleep\", \"3600\"]\n"), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	up := exec.Command(dockerBinary, "compose", "-p", project, "-f", composePath, "--project-directory", composeDir, "up", "-d")
	if out, err := up.CombinedOutput(); err != nil {
		t.Fatalf("docker compose up: %v: %s", err, out)
	}
	// Safety net in case an assertion below fails before this test's own
	// teardown call gets to run.
	t.Cleanup(func() {
		_ = exec.Command(dockerBinary, "compose", "-p", project, "down", "-v", "--remove-orphans").Run()
	})

	present, err := composeProjectContainersPresent(context.Background(), dockerBinary, project)
	if err != nil {
		t.Fatalf("confirm project running: %v", err)
	}
	if !present {
		t.Fatal("expected the project's container to be running before simulating the crash")
	}

	// Simulate the crash: the compose file (and its whole data directory)
	// is gone, exactly as it would be for a run whose data directory was
	// already cleaned up, or never survived the crash at all. This test
	// process's own cwd (this package's source directory) has no compose
	// file of its own either, matching the real "unrelated cwd" shape.
	if err := os.RemoveAll(composeDir); err != nil {
		t.Fatalf("remove compose directory to simulate the crash: %v", err)
	}

	if err := composeServicesDownProject(context.Background(), dockerBinary, project); err != nil {
		t.Fatalf("composeServicesDownProject after the compose file is gone: %v", err)
	}

	present, err = composeProjectContainersPresent(context.Background(), dockerBinary, project)
	if err != nil {
		t.Fatalf("confirm project removal: %v", err)
	}
	if present {
		t.Fatal("expected the orphaned project's container to be removed")
	}
}
