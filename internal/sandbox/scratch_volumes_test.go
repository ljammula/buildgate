package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"buildgate/internal/run"
)

// volumeDocker writes a fake docker that lists the given volumes for
// `volume ls` (whatever the name filter, as Docker's is a substring match),
// answers `volume inspect` with `<name>|<CreatedAt>` and records every
// `volume rm` argument in removedPath. An entry is "name", "name@CreatedAt"
// or either with an "inuse:" prefix: an in-use volume is left out of a
// listing that asks for dangling=true. A plain name was created in 2020.
func volumeDocker(t *testing.T, volumes ...string) (binary, removedPath string) {
	t.Helper()
	dir := t.TempDir()
	removedPath = filepath.Join(dir, "removed")
	var dangling, all, inspect string
	for _, v := range volumes {
		inUse := strings.HasPrefix(v, "inuse:")
		v = strings.TrimPrefix(v, "inuse:")
		name, created, ok := strings.Cut(v, "@")
		if !ok {
			created = "2020-01-01T00:00:00Z"
		}
		all += "  echo '" + name + "'\n"
		if !inUse {
			dangling += "    echo '" + name + "'\n"
		}
		inspect += "  echo '" + name + "|" + created + "'\n"
	}
	script := "#!/bin/sh\ncase \"$1 $2\" in\n\"volume ls\")\n  case \"$*\" in *dangling=true*)\n" + dangling + "    exit 0 ;;\n  esac\n" + all +
		"  exit 0 ;;\n\"volume inspect\")\n" + inspect + "  exit 0 ;;\n\"volume rm\")\n  echo \"$3\" >> \"" + removedPath + "\"\n  exit 0 ;;\nesac\nexit 0\n"
	binary = filepath.Join(dir, "docker-fake")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, removedPath
}

// recordRun writes a run record for runID, so a volume of its sandboxes is
// judged by inFlight and not by its age.
func recordRun(t *testing.T, dataDir, runID string) {
	t.Helper()
	dir := run.Dir(dataDir, runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.json"), []byte(`{"id":"`+runID+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileScratchVolumesRemovesOnlyTerminalRunsOfThisDataDir(t *testing.T) {
	dataDir := t.TempDir()
	recordNames(t, dataDir, "run-done", "bg-aaaa")
	recordNames(t, dataDir, "run-live", "bg-bbbb")
	recordRun(t, dataDir, "run-done")
	recordRun(t, dataDir, "run-live")
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
	recordRun(t, dataDir, "run-1")
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

func TestStaleScratchVolumesSelectsWithoutRemoving(t *testing.T) {
	dataDir := t.TempDir()
	recordNames(t, dataDir, "run-done", "bg-aaaa")
	recordNames(t, dataDir, "run-live", "bg-bbbb")
	recordRun(t, dataDir, "run-done")
	recordRun(t, dataDir, "run-live")
	docker, removedPath := volumeDocker(t, "bg-scratch-bg-aaaa", "bg-scratch-bg-bbbb", "bg-scratch-bg-other")
	stale, err := StaleScratchVolumes(context.Background(), docker, dataDir, func(runID string) bool { return runID == "run-live" })
	if err != nil {
		t.Fatalf("StaleScratchVolumes: %v", err)
	}
	if want := []string{"bg-scratch-bg-aaaa"}; !reflect.DeepEqual(stale, want) {
		t.Errorf("stale = %v, want %v", stale, want)
	}
	if _, err := os.Stat(removedPath); err == nil {
		t.Error("StaleScratchVolumes ran `docker volume rm`")
	}
}

func TestStaleScratchVolumesKeepsAVolumeAContainerReferences(t *testing.T) {
	dataDir := t.TempDir()
	recordNames(t, dataDir, "run-done", "bg-aaaa", "bg-bbbb")
	recordRun(t, dataDir, "run-done")
	docker, _ := volumeDocker(t, "bg-scratch-bg-aaaa", "inuse:bg-scratch-bg-bbbb")
	stale, err := StaleScratchVolumes(context.Background(), docker, dataDir, func(string) bool { return false })
	if err != nil {
		t.Fatalf("StaleScratchVolumes: %v", err)
	}
	if want := []string{"bg-scratch-bg-aaaa"}; !reflect.DeepEqual(stale, want) {
		t.Errorf("stale = %v, want %v (the in-use volume kept)", stale, want)
	}
}

func TestStaleScratchVolumesWithNoRunRecordGoesByAge(t *testing.T) {
	dataDir := t.TempDir()
	recordNames(t, dataDir, "req-1", "bg-old", "bg-young", "bg-weird", "bg-offset")
	young := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	offset := time.Now().Add(-3 * time.Hour).In(time.FixedZone("x", 5*3600)).Format(time.RFC3339)
	docker, _ := volumeDocker(t,
		"bg-scratch-bg-old@2020-01-01T00:00:00Z",
		"bg-scratch-bg-young@"+young,
		"bg-scratch-bg-weird@not-a-time",
		"bg-scratch-bg-offset@"+offset,
	)
	stale, err := StaleScratchVolumes(context.Background(), docker, dataDir, func(string) bool { return false })
	if err != nil {
		t.Fatalf("StaleScratchVolumes: %v", err)
	}
	if want := []string{"bg-scratch-bg-old", "bg-scratch-bg-offset"}; !reflect.DeepEqual(stale, want) {
		t.Errorf("stale = %v, want %v (only the volumes older than an hour)", stale, want)
	}
}
