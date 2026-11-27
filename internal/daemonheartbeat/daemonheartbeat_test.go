package daemonheartbeat

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWriteReadRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "repo-owner-abc123")
	want := Heartbeat{
		Repository: "example-app",
		TaskQueue:  "factoryd-repo-abc123",
		PID:        4242,
		StartedAt:  "2026-08-27T10:00:00.000000000Z",
		UpdatedAt:  "2026-08-27T10:00:30.000000000Z",

		ActiveRequests:  []string{"req-1", "req-2"},
		JobSlots:        3,
		TemporalAddress: "localhost:7233",
	}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read() = %+v, want %+v", got, want)
	}
}

// TestWriteReadRoundTripsRoute is route-visibility's own regression test
// (2026-09-25): RouteCredentialMode/RouteWorkerModel must survive the same
// write/read round trip as every other field -- they're the only channel
// `factoryd status` has for answering "which subscription gets billed by
// the live worker" (see Heartbeat.RouteCredentialMode's own doc
// comment).
func TestWriteReadRoundTripsRoute(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "queue-run")
	want := Heartbeat{
		PID:                 4242,
		StartedAt:           "2026-09-25T10:00:00.000000000Z",
		UpdatedAt:           "2026-09-25T10:00:30.000000000Z",
		RouteCredentialMode: "chatgpt-codex",
		RouteWorkerModel:    "gpt-5.6-luna",
	}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Read() = %+v, want %+v", got, want)
	}
}

// TestReadParsesHeartbeatWithoutRouteFields proves a heartbeat file
// written before RouteCredentialMode/RouteWorkerModel existed still parses
// cleanly, with both new fields coming back as their zero value ("") --
// omitempty must not break backward compatibility with an older `factoryd
// worker`'s on-disk heartbeat.
func TestReadParsesHeartbeatWithoutRouteFields(t *testing.T) {
	dir := t.TempDir()
	path := Path(dir, "queue-run")
	old := `{
  "repository": "",
  "task_queue": "",
  "pid": 4242,
  "started_at": "2026-08-27T10:00:00.000000000Z",
  "updated_at": "2026-08-27T10:00:30.000000000Z"
}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write legacy heartbeat: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.RouteCredentialMode != "" || got.RouteWorkerModel != "" {
		t.Errorf("Read() route fields = %q/%q, want both empty for a pre-route heartbeat", got.RouteCredentialMode, got.RouteWorkerModel)
	}
	if got.PID != 4242 {
		t.Errorf("Read() PID = %d, want 4242 (pre-existing fields must still parse)", got.PID)
	}
}

func TestPathJoinsDataDirAndID(t *testing.T) {
	got := Path("/data/repo-a", "repo-owner-abc123")
	want := filepath.Join("/data/repo-a", "daemon-heartbeat-repo-owner-abc123.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// TestPathDistinguishesIDs is the regression test for a real P1 finding
// from codex review: an earlier version used one fixed heartbeat filename
// per dataDir, so the supported one-daemon-per-repository deployment —
// multiple daemons sharing the same -data-dir — had every daemon overwrite
// the same file, letting a crashed repository's daemon appear healthy
// forever because a different, still-live repository's daemon kept
// refreshing it. Two distinct ids under the same dataDir must resolve to
// two distinct paths.
func TestPathDistinguishesIDs(t *testing.T) {
	dir := t.TempDir()
	a := Path(dir, "repo-owner-aaa")
	b := Path(dir, "repo-owner-bbb")
	if a == b {
		t.Fatalf("Path(dir, %q) == Path(dir, %q) == %q, want distinct paths", "repo-owner-aaa", "repo-owner-bbb", a)
	}
}

func TestWriteCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "does", "not", "exist", "daemon-heartbeat-repo-owner-abc123.json")
	if err := Write(path, Heartbeat{Repository: "r"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := Read(path); err != nil {
		t.Fatalf("Read after Write into a nonexistent parent: %v", err)
	}
}

func TestReadMissingFileErrors(t *testing.T) {
	if _, err := Read(Path(t.TempDir(), "repo-owner-abc123")); err == nil {
		t.Fatal("Read of a nonexistent heartbeat file returned no error")
	}
}

func TestStaleReportsFreshWithinMaxAge(t *testing.T) {
	now := time.Date(2026, 8, 27, 10, 0, 30, 0, time.UTC)
	hb := Heartbeat{UpdatedAt: now.Add(-10 * time.Second).Format(time.RFC3339Nano)}
	if Stale(hb, now, 30*time.Second) {
		t.Error("Stale() = true, want false: heartbeat is within maxAge")
	}
}

func TestStaleReportsStaleBeyondMaxAge(t *testing.T) {
	now := time.Date(2026, 8, 27, 10, 5, 0, 0, time.UTC)
	hb := Heartbeat{UpdatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano)}
	if !Stale(hb, now, 30*time.Second) {
		t.Error("Stale() = false, want true: heartbeat is well beyond maxAge")
	}
}

// TestStaleTreatsUnparseableTimestampAsStale proves an unparseable
// UpdatedAt (a corrupt file, a heartbeat from an incompatible future
// version) is treated as "needs attention", not silently reported healthy
// — a supervisor's failure mode should default to suspicious, not fine.
func TestStaleTreatsUnparseableTimestampAsStale(t *testing.T) {
	hb := Heartbeat{UpdatedAt: "not-a-timestamp"}
	if !Stale(hb, time.Now(), time.Hour) {
		t.Error("Stale() = false, want true for an unparseable UpdatedAt")
	}
}

// TestResolveSandboxDockerResolvesRelativeName proves the primary case: a
// bare relative name is resolved to the absolute executable exec.Command
// would actually invoke, via $PATH.
func TestResolveSandboxDockerResolvesRelativeName(t *testing.T) {
	dir := t.TempDir()
	realDocker := filepath.Join(dir, "docker")
	if err := os.WriteFile(realDocker, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got := ResolveSandboxDocker("docker")
	if got != realDocker {
		t.Errorf("ResolveSandboxDocker(%q) = %q, want %q", "docker", got, realDocker)
	}
}

// TestResolveSandboxDockerFallsBackOnFailure proves the best-effort
// contract: a name LookPath can't resolve (nothing on $PATH, or an
// absolute path to a file that doesn't exist) is returned unchanged
// rather than as an error — the caller that can't resolve it here will
// get the same failure naturally from Docker itself once it actually
// tries to run it.
func TestResolveSandboxDockerFallsBackOnFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty directory: nothing resolves

	for _, name := range []string{"docker", "/nonexistent/path/to/docker"} {
		if got := ResolveSandboxDocker(name); got != name {
			t.Errorf("ResolveSandboxDocker(%q) = %q, want unchanged %q on resolution failure", name, got, name)
		}
	}
}

func TestWorkerActiveRequestsIgnoresAStaleHeartbeat(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	write := func(updated time.Time) {
		t.Helper()
		hb := Heartbeat{UpdatedAt: updated.Format(time.RFC3339Nano), ActiveRequests: []string{"req-1", "req-2"}, JobSlots: 3, TemporalAddress: "localhost:7233"}
		if err := Write(WorkerPath(dir), hb); err != nil {
			t.Fatal(err)
		}
	}
	if ids, slots := WorkerActiveRequests(dir, now); len(ids) != 0 || slots != 1 {
		t.Errorf("no heartbeat: got %v, %d, want none, 1", ids, slots)
	}
	write(now)
	if ids, slots := WorkerActiveRequests(dir, now); len(ids) != 2 || ids[0] != "req-1" || ids[1] != "req-2" || slots != 3 {
		t.Errorf("fresh heartbeat: got %v, %d, want [req-1 req-2], 3", ids, slots)
	}
	hb, err := Read(WorkerPath(dir))
	if err != nil || hb.TemporalAddress != "localhost:7233" {
		t.Errorf("round trip: %+v, %v", hb, err)
	}
	write(now.Add(-2 * WorkerStaleAfter))
	if ids, slots := WorkerActiveRequests(dir, now); len(ids) != 0 || slots != 1 {
		t.Errorf("stale heartbeat: got %v, %d, want none, 1", ids, slots)
	}
}

func TestDrainMarkerOfADeadOwnerDoesNotDrain(t *testing.T) {
	dir := t.TempDir()
	if err := RequestDrain(dir); err != nil {
		t.Fatal(err)
	}
	if !Draining(dir) {
		t.Fatal("a marker of this live process must drain")
	}
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := requestDrainAs(dir, cmd.Process.Pid); err != nil { // exited and reaped
		t.Fatal(err)
	}
	if Draining(dir) {
		t.Error("a marker whose owner is gone still drains")
	}
	if _, err := os.Stat(DrainPath(dir)); !os.IsNotExist(err) {
		t.Errorf("the stale marker was not removed: %v", err)
	}
}

func TestWorkerBusyRequestsIncludesPRReviewSlots(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	hb := Heartbeat{PID: 1, UpdatedAt: now.Format(time.RFC3339Nano), ActiveRequests: []string{"a"}, SlotRequests: []string{"a", "pr"}, TemporalAddress: "x"}
	if err := Write(WorkerPath(dir), hb); err != nil {
		t.Fatal(err)
	}
	if got := WorkerBusyRequests(dir, now); len(got) != 2 || got[0] != "a" || got[1] != "pr" {
		t.Errorf("busy = %v, want [a pr]", got)
	}
}

// TestSandboxDockerDivergesReadsTheWorkersHeartbeat covers each answer of the
// comparison a `-repository` submitter makes with its data dir's worker: no
// heartbeat, a stale one and one with no endpoint are unknown; a fresh one is
// compared on the executable, DOCKER_HOST and DOCKER_CONTEXT.
func TestSandboxDockerDivergesReadsTheWorkersHeartbeat(t *testing.T) {
	local := DockerEndpoint{SandboxDocker: "/usr/local/bin/docker", DockerHost: "unix:///a.sock"}
	fresh := time.Now().Format(time.RFC3339Nano)
	stale := time.Now().Add(-2 * WorkerStaleAfter).Format(time.RFC3339Nano)
	for _, tc := range []struct {
		name         string
		hb           *Heartbeat
		ok, diverges bool
	}{
		{name: "no heartbeat"},
		{name: "stale", hb: &Heartbeat{UpdatedAt: stale, DockerEndpoint: DockerEndpoint{SandboxDocker: "/usr/local/bin/podman"}}},
		{name: "no endpoint", hb: &Heartbeat{UpdatedAt: fresh}},
		{name: "executable differs", hb: &Heartbeat{UpdatedAt: fresh, DockerEndpoint: DockerEndpoint{SandboxDocker: "/usr/local/bin/podman", DockerHost: "unix:///a.sock"}}, ok: true, diverges: true},
		{name: "host differs", hb: &Heartbeat{UpdatedAt: fresh, DockerEndpoint: DockerEndpoint{SandboxDocker: "/usr/local/bin/docker", DockerHost: "unix:///b.sock"}}, ok: true, diverges: true},
		{name: "context differs", hb: &Heartbeat{UpdatedAt: fresh, DockerEndpoint: DockerEndpoint{SandboxDocker: "/usr/local/bin/docker", DockerHost: "unix:///a.sock", DockerContext: "colima"}}, ok: true, diverges: true},
		{name: "matches", hb: &Heartbeat{UpdatedAt: fresh, DockerEndpoint: local}, ok: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.hb != nil {
				if err := Write(WorkerPath(dir), *tc.hb); err != nil {
					t.Fatal(err)
				}
			}
			_, ok, diverges := SandboxDockerDiverges(dir, local)
			if ok != tc.ok || diverges != tc.diverges {
				t.Fatalf("SandboxDockerDiverges = ok %v diverges %v, want ok %v diverges %v", ok, diverges, tc.ok, tc.diverges)
			}
		})
	}
}

// TestLocalDockerEndpointReadsTheEnvironment proves the endpoint a process
// records is its resolved executable and its own DOCKER_HOST and
// DOCKER_CONTEXT.
func TestLocalDockerEndpointReadsTheEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://example.invalid:2376")
	t.Setenv("DOCKER_CONTEXT", "remote")
	got := LocalDockerEndpoint("definitely-not-on-path-docker")
	want := DockerEndpoint{SandboxDocker: "definitely-not-on-path-docker", DockerHost: "tcp://example.invalid:2376", DockerContext: "remote"}
	if got != want {
		t.Fatalf("LocalDockerEndpoint = %+v, want %+v", got, want)
	}
}
