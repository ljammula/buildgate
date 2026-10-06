package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"buildgate/internal/run"
)

// runtimeDocker writes a fake docker that knows the containers of sandbox
// runtime sandboxes: containers maps a container name to its sandbox name.
// `ps` answers the runtime-label filters and nothing else; `rm -f` records
// its arguments and forgets those containers.
func runtimeDocker(t *testing.T, containers map[string]string) (binary, removedPath string) {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "containers")
	var lines []string
	for name, sandboxName := range containers {
		lines = append(lines, name+"\t"+sandboxName)
	}
	if err := os.WriteFile(state, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removedPath = filepath.Join(dir, "removed")
	script := `#!/bin/sh
state="` + state + `"
case "$1" in
ps)
  for a in "$@"; do
    case "$a" in
      label=openshell.ai/sandbox-name=*) want="${a#label=openshell.ai/sandbox-name=}"; awk -F'\t' -v w="$want" '$2==w {print $1}' "$state"; exit 0 ;;
      label=openshell.ai/sandbox-name) grep -v '^$' "$state"; exit 0 ;;
    esac
  done
  exit 0 ;;
rm)
  shift; shift
  for name in "$@"; do
    echo "$name" >> "` + removedPath + `"
    awk -F'\t' -v n="$name" '$1!=n' "$state" > "$state.new" && mv "$state.new" "$state"
  done
  exit 0 ;;
esac
exit 0
`
	binary = filepath.Join(dir, "docker-fake")
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, removedPath
}

func recordNames(t *testing.T, dataDir, runID string, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := RecordSandbox(dataDir, runID, SandboxRecord{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunContainersIncludeTheRunsRuntimeSandboxes(t *testing.T) {
	dataDir := t.TempDir()
	docker, removedPath := runtimeDocker(t, map[string]string{
		"openshell-worker-a": "bg-aaaa", "openshell-supervisor-a": "bg-aaaa",
		"openshell-worker-other": "bg-other",
	})
	recordNames(t, dataDir, "run-1", "bg-aaaa", "bg-gone")
	recordNames(t, dataDir, "run-2", "bg-other")
	ctx := context.Background()

	ids, err := RunContainerIDs(ctx, docker, dataDir, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("RunContainerIDs = %v, want the run's worker and supervisor", ids)
	}
	if present, err := WorkerContainerPresentForRun(ctx, docker, dataDir, "run-1"); err != nil || !present {
		t.Errorf("WorkerContainerPresentForRun = %v, %v; want true", present, err)
	}
	if present, err := WorkerContainerPresentForRun(ctx, docker, dataDir, "run-never-launched"); err != nil || present {
		t.Errorf("a run with no record: %v, %v; want false", present, err)
	}

	if err := RemoveRunContainers(ctx, docker, dataDir, "run-1"); err != nil {
		t.Fatal(err)
	}
	removed, _ := os.ReadFile(removedPath)
	got := strings.Fields(string(removed))
	if len(got) != 2 || strings.Contains(string(removed), "other") {
		t.Errorf("removed %v, want only run-1's two containers", got)
	}
	if left, err := RunContainerIDs(ctx, docker, dataDir, "run-1"); err != nil || len(left) != 0 {
		t.Errorf("after removal: %v, %v", left, err)
	}
}

func saveRun(t *testing.T, dataDir, runID string, state run.State) {
	t.Helper()
	r := &run.Run{ID: runID, State: state, HaltConfirmed: state == run.StateHalted}
	if err := r.Save(dataDir); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileOrphansRemovesTheRuntimeSandboxesOfTerminalRunsOnly(t *testing.T) {
	dataDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	docker, removedPath := runtimeDocker(t, map[string]string{
		"worker-done": "bg-done", "supervisor-done": "bg-done",
		"worker-live":    "bg-live",
		"worker-foreign": "bg-foreign",
	})
	saveRun(t, dataDir, "run-done", run.StateAccepted)
	recordNames(t, dataDir, "run-done", "bg-done")
	// A run still in progress whose owner heartbeat is fresh.
	saveRun(t, dataDir, "run-live", run.StateSliceRunning)
	recordNames(t, dataDir, "run-live", "bg-live")
	if err := writeOwnerHeartbeat(dataDir, "run-live"); err != nil {
		t.Fatal(err)
	}

	removed, err := ReconcileOrphans(context.Background(), docker, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"supervisor-done", "worker-done"}
	got := append([]string(nil), removed...)
	sortStrings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("removed = %v, want %v: not a live run's sandbox, not one no run here recorded", got, want)
	}
	onDisk, _ := os.ReadFile(removedPath)
	if strings.Contains(string(onDisk), "live") || strings.Contains(string(onDisk), "foreign") {
		t.Errorf("docker rm was given %q", onDisk)
	}
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func TestRuntimeSandboxContainersReadsNoRecordWhenThereIsNoRuntimeContainer(t *testing.T) {
	dataDir := t.TempDir()
	docker, _ := runtimeDocker(t, nil)
	// An unreadable record would be an error if it were read.
	dir := run.Dir(dataDir, "run-1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sandboxRecordFileName), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if lines, err := runtimeSandboxContainers(context.Background(), docker, dataDir); err != nil || len(lines) != 0 {
		t.Fatalf("lines = %v, %v", lines, err)
	}
}
