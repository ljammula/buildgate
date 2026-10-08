// Package daemonheartbeat gives `factoryd daemon` — a Worker process meant
// to run indefinitely, servicing one repository's shared Temporal task
// queue (see internal/workflow's RepositoryOwnerWorkflow) — a durable,
// on-disk liveness signal an external process supervisor can poll without
// depending on Temporal itself being reachable. Closes part of the plan's
// own named Phase 6 gap: "one process per repository, with no supervisor,
// health-check endpoint, or internal/api-driven lifecycle management yet".
// A supervisor (systemd, launchd, a small watchdog script, or a future
// internal/api endpoint) reads the file this package writes and decides
// for itself, via Stale, whether the daemon needs restarting — this
// package makes no restart decision and holds no supervisory state of its
// own.
package daemonheartbeat

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// WorkerID is the fixed daemonheartbeat identity `factoryd worker`
// writes under (the id keeps its historical value "queue-run") -- fixed, not repository-derived like `factoryd daemon`'s
// own RepositoryOwnerWorkflowID-based id, because exactly one worker
// drains a given -data-dir at a time (cmd/factoryd's own
// acquireWorkerLock already enforces that), across every
// repository/request it happens to be servicing. Exported so
// internal/api's GET /queue-run read route (added for the console's
// "worker is not running" strip, which previously relied on GET
// /daemons -- a route that 404s whenever `factoryd serve` isn't itself
// managing the daemon lifecycle, the common setup where an operator runs
// `factoryd worker` by hand) can read the same heartbeat file
// cmd/factoryd writes, without either side hardcoding a string the other
// could drift out of sync with.
const WorkerID = "queue-run"

// WorkerPath is where a data directory's worker writes its liveness
// signal -- see WorkerID's own doc comment for why this exists and who
// reads it.
func WorkerPath(dataDir string) string {
	return Path(dataDir, WorkerID)
}

// WorkerStaleAfter mirrors SandboxStaleAfter's three-missed-refreshes
// staleness rule under its own name (rather than callers referencing
// SandboxStaleAfter directly), since that constant's own doc comment
// scopes it to sandbox-divergence callers specifically -- worker
// liveness is a separate use of the identical cadence.
const WorkerStaleAfter = SandboxStaleAfter

// Path returns where a daemon servicing dataDir writes its heartbeat. id
// must be an already filesystem-safe, per-daemon identity distinct across
// every daemon that could ever share dataDir — cmd/factoryd passes its own
// repository-owner Workflow ID (already a safe `repo-owner-<hex>` string,
// per RepositoryOwnerWorkflowID), not the raw repository identity, which
// can contain `/` or other path-unsafe characters. Found via review: an
// earlier version used one fixed filename per dataDir with no id at all,
// so the supported one-daemon-per-repository deployment — multiple
// `factoryd daemon` processes pointed at the same `-data-dir` — had every
// one of them overwrite the same file. Their ticker loops raced on the
// same path, and a crashed repository's daemon could appear healthy
// forever because a *different*, still-live repository's daemon kept
// refreshing the shared heartbeat.
func Path(dataDir, id string) string {
	return filepath.Join(dataDir, fmt.Sprintf("daemon-heartbeat-%s.json", id))
}

// Heartbeat is one daemon process's liveness record. All timestamps are
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
	// SandboxDocker is the daemon's own -sandbox-docker flag value, always
	// through ResolveSandboxDocker first (round 9) rather than the raw
	// flag string — see that function's own doc comment for why. Found
	// via GitHub Codex review of PR #37, round 6: a submitter and the
	// daemon servicing the same repository already have to share one
	// -data-dir for reclaim to work at all (see cmd/factoryd's own
	// runsNeedingReclaim/reconcileReclaimedRun, both keyed on that shared
	// directory) — this heartbeat file, read from that same shared
	// directory, is therefore proof-by-construction that the daemon
	// reading it back is the one servicing this exact repository, letting
	// a submitter detect a diverging -sandbox-docker before ever
	// launching a container the daemon's own reconciliation could never
	// find. Empty for a heartbeat written before this field existed —
	// callers must treat that as "unknown", not "matches".
	SandboxDocker string `json:"sandbox_docker,omitempty"`
	// DockerHost/DockerContext are the daemon process's own DOCKER_HOST/
	// DOCKER_CONTEXT environment values (found via GitHub Codex review of
	// PR #37, round 7): SandboxDocker alone names an executable, but
	// internal/sandbox's own dockerClientEnv() passes the full host
	// environment through to that executable, so two processes both using
	// the identical -sandbox-docker string can still resolve to two
	// different real Docker endpoints — a matching SandboxDocker string is
	// not proof of a matching endpoint. These name the two standard
	// environment variables that select a non-default endpoint; they are
	// not an exhaustive list of every variable that could in principle
	// affect Docker CLI resolution (e.g. DOCKER_TLS_VERIFY,
	// DOCKER_CERT_PATH), only the ones common enough to be worth this
	// explicit check. Both empty is the ordinary, unremarkable case (no
	// endpoint override in effect on either side).
	DockerHost    string `json:"docker_host,omitempty"`
	DockerContext string `json:"docker_context,omitempty"`
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
	// ActiveRequests are the requests the daemon is running a job for right
	// now (a drafting, planning or build job in flight); empty between jobs.
	// It lets status and the console say what the other requests are queued
	// behind, which the request records alone can't: an older request can
	// re-enter a job state while a newer one's build runs.
	ActiveRequests []string `json:"active_requests,omitempty"`
	// JobSlots is how many jobs the daemon runs at once: max_parallel_jobs
	// for `factoryd worker`. 0 (no heartbeat) reads as 1.
	JobSlots int `json:"job_slots,omitempty"`
	// TemporalAddress is the Temporal address a `factoryd worker` serves; ""
	// for a process that is not a worker. A heartbeat that has one belongs to
	// a worker.
	TemporalAddress string `json:"temporal_address,omitempty"`
}

// Worker reports whether hb was written by `factoryd worker` (it serves a
// Temporal address) rather than an old process that is not a worker.
func (h Heartbeat) Worker() bool { return h.TemporalAddress != "" }

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

// Interval is how often a live `factoryd daemon` refreshes its own
// heartbeat file (cmd/factoryd's own writeHeartbeat ticker uses this
// directly, rather than a locally duplicated value, specifically so it
// can never drift out of sync with SandboxStaleAfter below — found via an
// Opus-assisted review: an earlier version of SandboxStaleAfter was a
// bare hardcoded duration with no code-level tie back to the writer's own
// cadence, so changing one without the other would have silently changed
// what "fresh" means without either side's own doc comment or type system
// catching it).
const Interval = 30 * time.Second

// SandboxStaleAfter is the shared staleness threshold every
// SandboxDockerDiverges caller uses — deliberately smaller than
// internal/sandbox's own container-reap staleness window (which guards a
// destructive action and so tolerates a generous grace period): a false
// negative here only means a caller proceeds without this extra check,
// never that anything gets removed, so a tighter window that stops
// trusting a heartbeat sooner after the daemon actually goes quiet is the
// safer default. Three refresh intervals' worth of grace, derived from
// Interval above rather than a second independent duration, so the two
// can never silently disagree about how many missed refreshes count as
// stale. One shared constant, not a value each caller picks for itself,
// so cmd/factoryd's submission-time check and internal/workflow's
// launch-time re-check can never silently disagree about what "fresh"
// means either.
const SandboxStaleAfter = 3 * Interval

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

// SandboxDockerDiverges reports whether a fresh heartbeat at
// Path(dataDir, ownerID) positively proves the daemon servicing this
// repository's own sandbox Docker configuration diverges from
// sandboxDocker/dockerHost/dockerContext — the shared comparison both
// cmd/factoryd's submission-time check and internal/workflow's
// launch-time re-check use (found via GitHub Codex review of PR #37,
// rounds 6-8), factored out here so the two callers can never drift out
// of sync with each other. sandboxDocker here, and the value written into
// the heartbeat this reads, must already be ResolveSandboxDocker's
// output, not a raw flag value — this function does no resolution of its
// own. A missing, unreadable, or stale heartbeat, or one predating the
// SandboxDocker field, is "unknown", not "matches" — diverges is false in
// every such case (ok is false too, so a caller can distinguish "checked
// and it's fine" from "couldn't check at all" if it wants to log the
// difference), never a false positive from an absent signal. Once ok is
// true, DockerHost/DockerContext are compared even when SandboxDocker
// itself matches, since dockerClientEnv() lets an identical executable
// resolve to a different real Docker endpoint.
func SandboxDockerDiverges(dataDir, ownerID, sandboxDocker, dockerHost, dockerContext string) (daemon Heartbeat, ok, diverges bool) {
	hb, err := Read(Path(dataDir, ownerID))
	if err != nil || hb.SandboxDocker == "" {
		return Heartbeat{}, false, false
	}
	if Stale(hb, time.Now(), SandboxStaleAfter) {
		return Heartbeat{}, false, false
	}
	if hb.SandboxDocker != sandboxDocker || hb.DockerHost != dockerHost || hb.DockerContext != dockerContext {
		return hb, true, true
	}
	return hb, true, false
}
