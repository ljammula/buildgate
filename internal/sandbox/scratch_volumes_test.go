package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// volumeDocker writes a fake docker that lists the given volumes for
// `volume ls` (whatever the filter, as Docker's name filter is a substring
// match) and records every `volume rm` argument in removedPath.
func volumeDocker(t *testing.T, volumes ...string) (binary, removedPath string) {
	t.Helper()
	dir := t.TempDir()
	removedPath = filepath.Join(dir, "removed")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n\"volume ls\")\n"
	for _, v := range volumes {
		script += "  echo '" + v + "'\n"
	}
	script += "  exit 0 ;;\n\"volume rm\")\n  echo \"$3\" >> \"" + removedPath + "\"\n  exit 0 ;;\nesac\nexit 0\n"
	binary = filepath.Join(dir, "docker-fake")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, removedPath
}

func TestReconcileScratchVolumesRemovesOnlyTerminalRunsOfThisDataDir(t *testing.T) {
	dataDir := t.TempDir()
	recordNames(t, dataDir, "run-done", "bg-aaaa")
	recordNames(t, dataDir, "run-live", "bg-bbbb")
	docker, removedPath := volumeDocker(t,
		"bg-scratch-bg-aaaa",  // terminal run: removed
		"bg-scratch-bg-bbbb",  // in-flight run: kept
		"bg-scratch-bg-other", // no run under this data dir recorded it: kept
		"xbg-scratch-bg-aaaa", // not the exact prefix: ignored
		"unrelated-volume",    // ignored
	)
	removed, err := ReconcileScratchVolumes(context.Background(), docker, dataDir, func(runID string) bool { return runID == "run-live" })
	if err != nil {
		t.Fatalf("ReconcileScratchVolumes: %v", err)
	}
	if want := []string{"bg-scratch-bg-aaaa"}; !reflect.DeepEqual(removed, want) {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	rm, err := os.ReadFile(removedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(rm)); got != "bg-scratch-bg-aaaa" {
		t.Errorf("docker volume rm calls = %q, want only bg-scratch-bg-aaaa", got)
	}
}

func TestReconcileScratchVolumesReadsNoRunRecordWithoutAScratchVolume(t *testing.T) {
	dataDir := t.TempDir()
	runDir := filepath.Join(dataDir, "runs", "run-1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, sandboxRecordFileName), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	docker, _ := volumeDocker(t, "xbg-scratch-a", "unrelated-volume")
	removed, err := ReconcileScratchVolumes(context.Background(), docker, dataDir, func(string) bool { return false })
	if err != nil || len(removed) != 0 {
		t.Fatalf("ReconcileScratchVolumes = %v, %v; want nothing removed and no error", removed, err)
	}
}

func TestReconcileScratchVolumesReportsAVolumeThatWillNotGo(t *testing.T) {
	dataDir := t.TempDir()
	recordNames(t, dataDir, "run-1", "bg-aaaa")
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker-fake")
	script := "#!/bin/sh\ncase \"$1 $2\" in\n\"volume ls\") echo bg-scratch-bg-aaaa; exit 0 ;;\n\"volume rm\") echo 'volume is in use' >&2; exit 1 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(docker, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := ReconcileScratchVolumes(context.Background(), docker, dataDir, func(string) bool { return false })
	if err == nil || !strings.Contains(err.Error(), "bg-scratch-bg-aaaa") || !strings.Contains(err.Error(), "in use") {
		t.Errorf("error = %v, want one naming the volume and Docker's reason", err)
	}
	if len(removed) != 0 {
		t.Errorf("removed = %v, want none", removed)
	}
}
