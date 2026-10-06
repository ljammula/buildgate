package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"buildgate/internal/run"
)

// TestReconcileRelayOrphansSkipsNetworkCleanupForSharedRegistryProxy covers
// crash recovery for the shared-network shape: a leaked registry-proxy
// container carrying the buildgate.registryproxy-shared-network
// label must have its container reclaimed without any attempt to remove a
// network it never owned (which could otherwise race or fail against
// whatever the relay's own reconciliation does with that same network).
func TestReconcileRelayOrphansSkipsNetworkCleanupForSharedRegistryProxy(t *testing.T) {
	dataDir := t.TempDir()
	r := run.Run{ID: "run-shared-terminal", State: run.StateHalted, HaltConfirmed: true}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run: %v", err)
	}

	docker := filepath.Join(t.TempDir(), "docker-fake")
	script := `#!/bin/sh
case "$*" in
  *"ps -a --filter label=buildgate.registryproxy=true --filter label=buildgate.data-dir="*)
    printf 'factoryd-registryproxy-container-shared\trun-shared-terminal\ttrue\n'
    ;;
  *"rm -f factoryd-registryproxy-container-shared"*)
    ;;
  *"network rm"*)
    echo "must not attempt to remove a network this container never owned" >&2
    exit 1
    ;;
  *"ps -a --filter name=^/"*)
    ;;
  *"network ls --filter label=buildgate.registryproxy=true --filter label=buildgate.data-dir="*)
    ;;
  *"network ls --filter name=^"*)
    ;;
esac
`
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileRelayOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "factoryd-registryproxy-container-shared" {
		t.Fatalf("removed = %v, want exactly [factoryd-registryproxy-container-shared] (no synthetic network name)", removed)
	}
}
