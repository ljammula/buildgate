// Package daemonheartbeat is the on-disk liveness record `factoryd worker`
// writes into its data dir and refreshes every Interval, so `status`,
// `doctor`, `restart`, serve's GET /queue-run and the console can tell a
// live worker from a stale or absent one without asking Temporal. It also
// holds the drain request `restart` and `upgrade` leave for a worker, and
// the Docker endpoint the worker uses, which a `-repository` submitter
// compares with its own (SandboxDockerDiverges). This package makes no
// restart decision of its own.
package daemonheartbeat

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// WorkerID is the fixed identity `factoryd worker` writes its heartbeat
// under (the id keeps its historical value "queue-run"): exactly one worker
// drains a given data dir at a time (cmd/factoryd's acquireWorkerLock
// enforces that), across every repository and request. Exported so
// internal/api's GET /queue-run read route reads the same file cmd/factoryd
// writes.
const WorkerID = "queue-run"

// WorkerPath is where a data directory's worker writes its liveness
// signal -- see WorkerID's own doc comment for why this exists and who
// reads it.
func WorkerPath(dataDir string) string {
	return Path(dataDir, WorkerID)
}

// WorkerStaleAfter is how old a worker heartbeat may be before it is stale:
// three missed refreshes, derived from Interval so the two cannot disagree.
const WorkerStaleAfter = 3 * Interval

// Path is the heartbeat file named id in dataDir. id must be filesystem
// safe.
func Path(dataDir, id string) string {
	return filepath.Join(dataDir, fmt.Sprintf("daemon-heartbeat-%s.json", id))
}

// Heartbeat is one worker process's liveness record. All timestamps are
// RFC3339Nano — real sub-second resolution, the same convention run.Run's
// own CreatedAt now uses and for the same reason (see its own doc
// comment): a supervisor computing "how long since the last heartbeat"
// needs more than whole-second precision to be meaningful at a short
// polling interval.
type Heartbeat struct {
	Repository string `json:"repository"`
	TaskQueue  string `json:"task_queue"`
	PID        int    `json:"pid"`
	StartedAt  string `json:"started_at"`
	UpdatedAt  string `json:"updated_at"`
	// Version is the factoryd version of the process that writes this file.
	// A worker runs every build in its own process, so one started before an
	// install keeps building with the code it started with; `doctor` and
	// `factoryd restart` compare this with the installed binary's. Empty
	// from a process older than the field.
	Version string `json:"version,omitempty"`
	// DockerEndpoint is the Docker executable and endpoint the worker
	// launches and reconciles sandboxes through. Empty in a heartbeat written
	// before the fields existed, which callers read as unknown.
	DockerEndpoint
	// DockerBackend is the Docker backend the worker launches sandboxes on
	// (colima, native-linux), read once at its start; "" when Docker did
	// not answer then.
	DockerBackend string `json:"docker_backend,omitempty"`
	// RouteCredentialMode/RouteWorkerModel are the daemon-wide model route
	// (relay_credential_mode and relay worker model id, after session-config/
	// flag resolution) `factoryd worker` is actually using -- surfaced so
	// `factoryd status` can answer "which subscription gets billed by the
	// run that's live right now" (route-visibility plan, 2026-09-25): the
	// relay route is daemon-wide by design (a role's model alias
	// deliberately cannot change upstream/credential, see
	// internal/sessionconfig's own doc comment), so an operator otherwise has
	// no way to see it without re-reading worker's own launch flags/
	// config. Deliberately just these two non-secret identifiers, never a
	// token, an auth-file path, or an upstream URL -- this file is read by
	// `factoryd status` and, through it, anything the console or a skill
	// prints back to an operator. Empty for a heartbeat written before these
	// fields existed, or when worker has neither resolved (never actually
	// true for RouteCredentialMode, which worker always validates
	// non-empty, but callers must still treat empty as "unknown", not
	// "static", the same convention SandboxDocker's own doc comment uses).
	RouteCredentialMode string `json:"route_credential_mode,omitempty"`
	RouteWorkerModel    string `json:"route_worker_model,omitempty"`
	// GitHubLogin is whether the worker's own session could read a GitHub
	// token when it started: GitHubLoginUsable, GitHubLoginUnusable, or ""
	// when it could not tell (no gh on its PATH, or gh did not answer). A worker pushes branches
	// and opens pull requests as itself, and a session can lack the login
	// another session of the same user has (an ssh session on a Mac cannot
	// read the login keychain), so doctor reads the worker's answer here
	// rather than checking its own session.
	GitHubLogin string `json:"github_login,omitempty"`
	// ActiveRequests are the requests the daemon is running a job for right
	// now (a drafting, planning or build job in flight); empty between jobs.
	// It lets status and the console say what the other requests are queued
	// behind, which the request records alone can't: an older request can
	// re-enter a job state while a newer one's build runs.
	ActiveRequests []string `json:"active_requests,omitempty"`
	// Draining is true while a drain request (RequestDrain) is on disk: the
	// worker finishes the steps in flight and starts no new one.
	Draining bool `json:"draining,omitempty"`
	// SlotRequests are the requests whose step holds one of the JobSlots
	// right now: every entry of ActiveRequests, plus a pr_review pass, which
	// runs on the jobs queue but is not a job (stop and upgrade do not wait
	// for it). Running jobs come first, so the first entry is a job whenever
	// one runs. With every slot held, status and the console say a job-state
	// request that holds none is waiting for one.
	SlotRequests []string `json:"slot_requests,omitempty"`
	// RepositoryWaits maps a request id to the RFC3339 time its build step
	// first found its repository held by another build and gave its slot
	// back; an entry is dropped once the step gets past that check. It lets
	// status say a building request is waiting for the repository rather than
	// running.
	RepositoryWaits map[string]string `json:"repository_waits,omitempty"`
	// JobSlots is how many jobs the daemon runs at once: max_parallel_jobs
	// for `factoryd worker`. 0 (no heartbeat) reads as 1.
	JobSlots int `json:"job_slots,omitempty"`
	// TemporalAddress is the Temporal address a `factoryd worker` serves; ""
	// for a process that is not a worker. A heartbeat that has one belongs to
	// a worker.
	TemporalAddress string `json:"temporal_address,omitempty"`
}

// The values of Heartbeat.GitHubLogin.
const (
	GitHubLoginUsable   = "usable"
	GitHubLoginUnusable = "unusable"
)

// Worker reports whether hb was written by `factoryd worker` (it serves a
// Temporal address) rather than an old process that is not a worker.
func (h Heartbeat) Worker() bool { return h.TemporalAddress != "" }

// DrainPath is the marker file whose presence asks dataDir's worker to drain.
func DrainPath(dataDir string) string {
	return filepath.Join(dataDir, "worker-drain")
}

// RequestDrain asks dataDir's worker to finish the steps in flight and start
// no new one. The worker reads the file before it starts every step; a new
// worker clears it on start. The file names its owner, the asking process: a
// marker whose owner has exited (killed, hung up) no longer drains.
func RequestDrain(dataDir string) error {
	return requestDrainAs(dataDir, os.Getpid())
}

func requestDrainAs(dataDir string, pid int) error {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	body := fmt.Sprintf("%d\n%s\n", pid, time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(DrainPath(dataDir), []byte(body), 0o600); err != nil {
		return fmt.Errorf("write drain marker: %w", err)
	}
	return nil
}

// ClearDrain removes the drain marker; none present is not an error.
func ClearDrain(dataDir string) error {
	if err := os.Remove(DrainPath(dataDir)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove drain marker: %w", err)
	}
	return nil
}

// Draining reports whether a drain request is on disk for dataDir and its
// owner still runs. A marker whose owner is gone is removed and reported
// absent: nothing is left to restart the worker it would hold back.
func Draining(dataDir string) bool {
	b, err := os.ReadFile(DrainPath(dataDir))
	if err != nil {
		return false
	}
	var pid int
	if _, err := fmt.Sscanf(string(b), "%d", &pid); err == nil && pid > 0 {
		if kerr := syscall.Kill(pid, 0); kerr == syscall.ESRCH {
			_ = ClearDrain(dataDir)
			return false
		}
	}
	return true
}

// WorkerBusyRequests returns the requests dataDir's worker holds a job slot
// for: its heartbeat's SlotRequests (every running job and every pr_review
// pass, whose corrective build is no job) with ActiveRequests, or nil when
// there is no fresh heartbeat.
func WorkerBusyRequests(dataDir string, now time.Time) []string {
	hb, err := Read(WorkerPath(dataDir))
	if err != nil || Stale(hb, now, WorkerStaleAfter) {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range append(append([]string{}, hb.ActiveRequests...), hb.SlotRequests...) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// WorkerActiveRequests returns the ActiveRequests and JobSlots of dataDir's
// worker heartbeat, or (nil, 1) when there is no heartbeat or it
// is stale (a dead daemon advances nothing).
func WorkerActiveRequests(dataDir string, now time.Time) (ids []string, slots int) {
	hb, err := Read(WorkerPath(dataDir))
	if err != nil || Stale(hb, now, WorkerStaleAfter) {
		return nil, 1
	}
	return hb.ActiveRequests, max(hb.JobSlots, 1)
}

// Write persists hb to path atomically (write to a temp file, then
// rename), so a supervisor polling concurrently never observes a
// partially-written file — the same crash-safety convention run.Run.Save
// already uses for its own durable record.
func Write(path string, hb Heartbeat) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create heartbeat directory: %w", err)
	}
	b, err := json.MarshalIndent(hb, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal heartbeat: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write heartbeat: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename heartbeat into place: %w", err)
	}
	return nil
}

// Read loads a heartbeat previously written by Write.
func Read(path string) (Heartbeat, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Heartbeat{}, fmt.Errorf("read heartbeat: %w", err)
	}
	var hb Heartbeat
	if err := json.Unmarshal(b, &hb); err != nil {
		return Heartbeat{}, fmt.Errorf("decode heartbeat: %w", err)
	}
	return hb, nil
}

// Stale reports whether hb's UpdatedAt is older than maxAge as of now, or
// isn't a parseable timestamp at all — an unparseable value is treated as
// stale (not alive) rather than panicking or silently reporting healthy,
// since a supervisor's failure mode should be "assume it needs attention",
// not "assume it's fine".
func Stale(hb Heartbeat, now time.Time, maxAge time.Duration) bool {
	updatedAt, err := time.Parse(time.RFC3339Nano, hb.UpdatedAt)
	if err != nil {
		return true
	}
	return now.Sub(updatedAt) > maxAge
}

// Interval is how often a live worker refreshes its heartbeat file; the
// writer's ticker and WorkerStaleAfter both derive from it.
const Interval = 30 * time.Second

// ResolveSandboxDocker resolves a -sandbox-docker flag value (often a bare
// relative name like "docker") to the absolute executable exec.Command
// would actually invoke, via exec.LookPath plus filepath.Abs —
// best-effort: on any resolution failure, sandboxDocker is returned
// unchanged rather than erroring, since a caller that can't resolve it
// here will simply get the same failure naturally from Docker itself once
// it actually tries to run it, and this function's only job is to make
// the *comparison* more accurate, not to be Docker's own error-reporting
// path. Every caller that writes or compares Heartbeat.SandboxDocker
// (cmd/factoryd's own writeHeartbeat closure, and both
// SandboxDockerDiverges call sites) must resolve through this first —
// found via GitHub Codex review of PR #37, round 9: two processes both
// configured with the identical relative name "docker" can still resolve
// to two different real executables (and so two different real Docker
// endpoints) when their own $PATH values differ, which a bare string
// comparison of the unresolved flag value can never catch.
//
// filepath.Abs after LookPath, not LookPath alone (found via an
// Opus-assisted review of the round-9 fix): exec.LookPath's own contract
// only verifies executability for a name that already contains a path
// separator — for that shape (e.g. "./docker", "bin/docker") it returns
// the input string verbatim, NOT resolved against the process's current
// working directory, so two processes both configured with the identical
// relative-with-slash value from two different working directories would
// still compare equal here even though exec.CommandContext resolves each
// to a different real file — the same bug class this function exists to
// close, just reachable through a working-directory difference instead of
// a $PATH difference. This does not resolve symlinks (no EvalSymlinks):
// a bind mount or symlinked Docker binary can still read as a spurious
// divergence, but that fails closed (an unnecessary rejection), not open,
// so it is accepted as a known imprecision rather than chased further.
func ResolveSandboxDocker(sandboxDocker string) string {
	resolved, err := exec.LookPath(sandboxDocker)
	if err != nil {
		return sandboxDocker
	}
	if abs, absErr := filepath.Abs(resolved); absErr == nil {
		return abs
	}
	return resolved
}

// DockerEndpoint is the Docker a process launches sandboxes through: the
// resolved -sandbox-docker executable (ResolveSandboxDocker) and the
// DOCKER_HOST and DOCKER_CONTEXT it passes on to it. The same executable can
// reach a different Docker under a different DOCKER_HOST or DOCKER_CONTEXT,
// so all three are compared. Other variables that can steer the Docker CLI
// (DOCKER_CERT_PATH, DOCKER_TLS_VERIFY) are not.
type DockerEndpoint struct {
	SandboxDocker string `json:"sandbox_docker,omitempty"`
	DockerHost    string `json:"docker_host,omitempty"`
	DockerContext string `json:"docker_context,omitempty"`
}

// LocalDockerEndpoint is this process's DockerEndpoint for sandboxDocker.
func LocalDockerEndpoint(sandboxDocker string) DockerEndpoint {
	return DockerEndpoint{
		SandboxDocker: ResolveSandboxDocker(sandboxDocker),
		DockerHost:    os.Getenv("DOCKER_HOST"),
		DockerContext: os.Getenv("DOCKER_CONTEXT"),
	}
}

// Known reports whether e records an endpoint at all: a record written before
// the fields existed has none, which is unknown, never a match or a mismatch.
func (e DockerEndpoint) Known() bool { return e.SandboxDocker != "" }

// SandboxDockerDiverges reports whether dataDir's worker, by a fresh
// heartbeat that records its endpoint, uses a Docker other than local. A
// missing, unreadable or stale heartbeat, or one without an endpoint, is
// unknown: ok and diverges are both false, so an absent worker never refuses
// anything. worker is the heartbeat's endpoint when ok.
func SandboxDockerDiverges(dataDir string, local DockerEndpoint) (worker DockerEndpoint, ok, diverges bool) {
	hb, err := Read(WorkerPath(dataDir))
	if err != nil || !hb.DockerEndpoint.Known() || Stale(hb, time.Now(), WorkerStaleAfter) {
		return DockerEndpoint{}, false, false
	}
	return hb.DockerEndpoint, true, hb.DockerEndpoint != local
}
