package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// legacyLabelDocker writes a fake docker that models real `docker ps`/
// `docker network ls` filter semantics for ONE leaked resource: it prints
// line only when the invocation filters on exactly prefix+"data-dir="+hash
// (and, when kind != "", prefix+kind+"=true"), i.e. only when a sweep
// searches the resource's own label prefix under its own data-dir hash.
// Every other command succeeds silently.
func legacyLabelDocker(t *testing.T, listing, prefix, hash, kind, line string) string {
	t.Helper()
	kindClause := ""
	if kind != "" {
		kindClause = fmt.Sprintf(` && case "$*" in *"label=%s%s=true"*) true ;; *) false ;; esac`, prefix, kind)
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"%s"*"label=%sdata-dir=%s"*)
    if true%s; then printf '%s\n'; fi
    ;;
esac
exit 0
`, listing, prefix, hash, kindClause, line)
	docker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return docker
}

func saveTerminalRun(t *testing.T, dataDir, id string) {
	t.Helper()
	r := run.Run{ID: id, State: run.StateHalted, HaltConfirmed: true}
	if err := r.Save(dataDir); err != nil {
		t.Fatalf("save run %s: %v", id, err)
	}
}

func TestLabelPrefixes(t *testing.T) {
	if labelKey("run") != "buildgate.run" {
		t.Errorf("labelKey(run) = %q, want buildgate.run", labelKey("run"))
	}
	if labelPrefixes[0] != labelPrefix || labelPrefixes[1] != legacyLabelPrefix {
		t.Errorf("labelPrefixes = %v, want current then legacy", labelPrefixes)
	}
}

func TestReconcileOrphansReclaimsLegacyLabelledContainerOfSameDataDir(t *testing.T) {
	dataDir := t.TempDir()
	saveTerminalRun(t, dataDir, "run-legacy")
	docker := legacyLabelDocker(t, "ps -a", legacyLabelPrefix, dataDirLabel(dataDir), "", `legacy-worker\trun-legacy`)
	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 1 || removed[0] != "legacy-worker" {
		t.Fatalf("removed = %v, want [legacy-worker]", removed)
	}
}

func TestReconcileOrphansLeavesLegacyLabelledContainerOfOtherDataDir(t *testing.T) {
	dataDir := t.TempDir()
	saveTerminalRun(t, dataDir, "run-legacy")
	docker := legacyLabelDocker(t, "ps -a", legacyLabelPrefix, dataDirLabel(t.TempDir()), "", `legacy-worker\trun-legacy`)
	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want a different data dir's legacy container left alone", removed)
	}
}

func TestWorkerContainerPresentForRunSeesLegacyLabelledContainer(t *testing.T) {
	dataDir := t.TempDir()
	docker := legacyLabelDocker(t, "ps -a", legacyLabelPrefix, dataDirLabel(dataDir), "", `legacy-worker`)
	got, err := WorkerContainerPresentForRun(context.Background(), docker, dataDir, "run-legacy")
	if err != nil || !got {
		t.Fatalf("WorkerContainerPresentForRun = %v, %v; want true for a same-data-dir legacy worker", got, err)
	}
	other := legacyLabelDocker(t, "ps -a", legacyLabelPrefix, dataDirLabel(t.TempDir()), "", `legacy-worker`)
	got, err = WorkerContainerPresentForRun(context.Background(), other, dataDir, "run-legacy")
	if err != nil || got {
		t.Fatalf("WorkerContainerPresentForRun = %v, %v; want false for another data dir's legacy worker", got, err)
	}
}

// A relay container/network (and a registry proxy) leaked by a pre-rename
// factoryd holds a real credential: the relay sweep must still reclaim it,
// and only for its own data dir.
func TestReconcileRelayOrphansReclaimsLegacyLabelledResourcesOfSameDataDirOnly(t *testing.T) {
	cases := []struct {
		name, listing, kind, line, want string
	}{
		{"relay container", "ps -a", "relay", `factoryd-relay-container-legacy\trun-legacy\t`, "factoryd-relay-container-legacy"},
		{"registry proxy container", "ps -a", "registryproxy", `factoryd-registryproxy-container-legacy\trun-legacy\t`, "factoryd-registryproxy-container-legacy"},
		{"relay network", "network ls", "relay", `factoryd-relay-legacy\trun-legacy`, "factoryd-relay-legacy"},
		{"registry proxy network", "network ls", "registryproxy", `factoryd-registryproxy-legacy\trun-legacy`, "factoryd-registryproxy-legacy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			saveTerminalRun(t, dataDir, "run-legacy")
			docker := legacyLabelDocker(t, tc.listing, legacyLabelPrefix, dataDirLabel(dataDir), tc.kind, tc.line)
			removed, err := ReconcileRelayOrphans(context.Background(), docker, dataDir)
			if err != nil {
				t.Fatalf("ReconcileRelayOrphans: %v", err)
			}
			if !strings.Contains(strings.Join(removed, ","), tc.want) {
				t.Fatalf("removed = %v, want it to include %q", removed, tc.want)
			}

			other := legacyLabelDocker(t, tc.listing, legacyLabelPrefix, dataDirLabel(t.TempDir()), tc.kind, tc.line)
			removed, err = ReconcileRelayOrphans(context.Background(), other, dataDir)
			if err != nil {
				t.Fatalf("ReconcileRelayOrphans (other data dir): %v", err)
			}
			if len(removed) != 0 {
				t.Fatalf("removed = %v, want another data dir's legacy resource left alone", removed)
			}
		})
	}
}

func TestComposeServicesOwnerReadsLegacyLabels(t *testing.T) {
	// `docker ps` prints empty strings for the new keys a legacy container
	// lacks, then the legacy pair.
	psDocker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(psDocker, []byte("#!/bin/sh\nprintf '\\t\\tlegacyhash\\tlegacy-run\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hash, runID, err := composeServicesProjectOwner(context.Background(), psDocker, "bg-legacy")
	if err != nil || hash != "legacyhash" || runID != "legacy-run" {
		t.Fatalf("project owner = %q, %q, %v; want legacyhash, legacy-run", hash, runID, err)
	}
	netDocker := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(netDocker, []byte("#!/bin/sh\nprintf '\\n\\nlegacyhash\\nlegacy-run\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hash, runID, err = composeServicesNetworkOwner(context.Background(), netDocker, "bg-legacy")
	if err != nil || hash != "legacyhash" || runID != "legacy-run" {
		t.Fatalf("network owner = %q, %q, %v; want legacyhash, legacy-run", hash, runID, err)
	}
	// The new key wins when present.
	both := filepath.Join(t.TempDir(), "docker-fake")
	if err := os.WriteFile(both, []byte("#!/bin/sh\nprintf 'newhash\\tnew-run\\toldhash\\told-run\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	hash, runID, _ = composeServicesProjectOwner(context.Background(), both, "bg-x")
	if hash != "newhash" || runID != "new-run" {
		t.Fatalf("project owner = %q, %q; want the current-prefix pair", hash, runID)
	}
}

// The compose ownership stamp is written under the new prefix only.
func TestComposeOwnershipLabelKeysUseCurrentPrefix(t *testing.T) {
	for _, k := range []string{composeServicesDataDirLabelKey, composeServicesRunLabelKey} {
		if !strings.HasPrefix(k, labelPrefix) {
			t.Errorf("compose label key %q lacks prefix %q", k, labelPrefix)
		}
	}
}
