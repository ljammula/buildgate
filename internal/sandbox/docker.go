// Package sandbox launches untrusted worker commands inside a restricted
// container. The factory remains responsible for policy and evidence; this
// package only owns the container boundary and observed process result.
package sandbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"buildgate/internal/progress"
	"buildgate/internal/run"
)

const launchSpecVersion = 1

// maxLogBytes bounds how much combined stdout/stderr one worker attempt may
// write to its log file. Docker's own resource limits (memory, PIDs, tmpfs)
// say nothing about a worker that simply prints without end, and an
// unbounded log directory is durable-disk growth the operator never
// budgeted for. Chosen generously above any legitimate build/verify
// transcript observed in this repo's own live runs, so it only trips on a
// runaway or hostile worker. A package-level var (not a const) only so
// tests can shrink it instead of writing tens of megabytes per run.
var maxLogBytes int64 = 64 * 1024 * 1024

// maxRelayedAgentLines and maxRelayedRoundLines bound how many
// FACTORY_PROGRESS lines one worker attempt may relay into the run's
// progress.jsonl (see LaunchSpec.ProgressPath), per worker stage. The raw
// log is already bounded by maxLogBytes; this keeps a hostile or runaway
// worker from also inflating the small, display-only progress feed that the
// console and `factoryd watch` read in full. The two stages are budgeted
// separately (found via review): the chatty "agent" notes must not be able
// to exhaust the budget the sparse "round" start/end lines need, or the
// round counter every surface shows would freeze at a stale round while the
// build kept going. Lines past a cap still reach the log unchanged, they
// just stop being relayed.
const (
	maxRelayedAgentLines = 5000
	maxRelayedRoundLines = 500
)

// errOutputLimitExceeded is wrapped into the error Run returns when a
// worker's combined output crosses maxLogBytes, so callers can distinguish
// this from an ordinary worker failure if they choose to.
var errOutputLimitExceeded = errors.New("sandbox output exceeded the log size limit")

// ErrCleanupUnconfirmed indicates that a canceled worker may still exist.
// Callers must fail closed when this is returned.
var ErrCleanupUnconfirmed = errors.New("sandbox container cleanup unconfirmed")

func dockerClientEnv() []string {
	// This environment is used only by the trusted host-side Docker CLI;
	// worker/container variables remain explicitly allow-listed below.
	return os.Environ()
}

// LaunchSpec is the allow-listed contract for one disposable worker.
// WorkDir is the only writable host path exposed to the worker. InputDir is an
// optional read-only mount owned by the caller; LogPath stays outside Docker.
type LaunchSpec struct {
	Image    string
	WorkDir  string
	InputDir string
	Inputs   []InputMount
	// ReferenceOracleDir, when non-empty, is a host directory bind-mounted
	// read-only at ReferenceOracleMountPath (a path relative to
	// workerContainerWorkDir, e.g. "verify") -- a nested mount inside the
	// otherwise-writable workspace, the same technique already used a few
	// lines below to keep .git unwritable by the worker. Exists because
	// "keep the reference-oracle script outside the ticket's
	// Allowed-Files" is a convention diff_scope enforces only on the
	// FINAL committed diff, not a structural guarantee during the build
	// itself -- the worker can read, tamper with, and then revert the
	// oracle before ever committing, defeating that convention entirely.
	// Found by GitHub Codex App review, PR #151: this closes the actual
	// gap safety-contract.md SC-004/SC-012 require ("the job cannot
	// modify its canonical acceptance oracle"). Both fields empty means
	// no overlay is mounted -- the caller falls back to the
	// Allowed-Files-only convention, weaker but still the same behavior
	// this repo had before this field existed.
	ReferenceOracleDir       string
	ReferenceOracleMountPath string
	// WorkspaceMasks are read-only overlays bind-mounted over paths below
	// /workspace, after the workspace and reference-oracle binds (see
	// SnapshotReviewInstructions). Empty mounts nothing.
	WorkspaceMasks []WorkspaceMask
	LogPath        string
	Name           string
	User           string
	Command        []string
	Environment    []string
	// UnrecordedEnvironment holds KEY=VALUE entries the container gets by
	// name only (`--env KEY`), with each value supplied through the Docker
	// CLI process's own environment. The value never appears in the docker
	// argv, which Result.Command records in run.json attempts and Temporal
	// activity results. It carries operator values that may hold a
	// password (compose_services_worker_env). A key may not also appear in
	// Environment, and these flags precede every other --env flag, so no
	// entry can replace a value the factory sets.
	UnrecordedEnvironment []string
	Memory                string
	CPUs                  string
	TmpfsSize             string
	Timeout               time.Duration
	Network               string
	// Sidecars are the factory's own containers a worker launched through a
	// Runtime may reach by address (see SidecarEndpoint). Run ignores them:
	// a `docker run` worker joins their network instead.
	Sidecars []SidecarEndpoint
	// ComposeNetwork, when set, is this run's own dedicated compose-services
	// network (composeServicesNetworkName's "bg-compose-<runID>" -- see
	// ComposeServicesLifecycle.NetworkName) that the worker can reach at the
	// service aliases this run launched. When Network is "none", Run uses
	// ComposeNetwork as the container's primary network at creation time:
	// Docker's private "none" network cannot coexist with a second network.
	// When Network is a relay or registry-proxy network, ComposeNetwork is
	// attached after creation so the worker can use both networks. Empty (the
	// common case) preserves today's single `docker run` launch unchanged.
	ComposeNetwork string
	// WorkerUmask, when non-empty, wraps Command in a small shell prelude
	// that calls `umask` before exec'ing the real command (see
	// DockerCommand). Docker has no "--umask" flag -- a container's own
	// umask is inherited from whatever process execs its entrypoint, which
	// for a plain `docker run <image> <argv...>` invocation (no shell) is
	// effectively fixed at whatever default the image bakes in, typically
	// 0022. That default denies group-write on every file/directory the
	// worker itself creates, which matters once the worker runs under a
	// dedicated UID that only shares a *group* with the host (see
	// cmd/factoryd's -sandbox-worker-uid): a subdirectory the worker
	// creates would otherwise come back 0755 (owner rwx, group
	// r-x only, no write), and factoryd's own later cleanup -- a
	// different UID than the worker, relying on group membership rather
	// than ownership -- can neither unlink entries inside a directory it
	// can't write to, nor chmod its way out (chmod requires being the
	// owner or root; factoryd is neither for a file the worker created).
	// A value here (e.g. "0002") is validated by Validate to be a small
	// octal string so it can never inject anything into the shell -c
	// argument it becomes.
	//
	// The umask alone only governs the mode a worker-created path starts
	// with; a worker that later chmods one of its own paths restrictive
	// (0700, say) would otherwise strand it exactly as an image-default
	// umask would have. DockerCommand's wrapper closes that by reclaiming
	// group-writability under WorkDir, as the worker's own UID, as the last
	// thing the container does (found via GitHub Codex App review of PR
	// #62, P1) -- see DockerCommand's own comment for why that identity can
	// always chmod it back regardless of the path's current mode.
	//
	// Known limitation (found via GitHub Codex App review of PR #62, P2):
	// this mechanism requires the IMAGE to provide /bin/sh (DockerCommand
	// wraps Command in `/bin/sh -c "umask ... && exec \"$@\"" -- ...`). A
	// digest-pinned custom image with no shell at all (a true distroless
	// image containing only the requested executable, as opposed to the
	// canonical worker image's own distroless-but-shell-having base) fails
	// before its build/verify command ever starts. No general fix exists
	// short of either giving up the umask (and therefore worker-UID
	// separation's write access for anything the worker creates) or
	// requiring a shell as part of this scheme's own image contract, which
	// this codebase does not currently validate or enforce. An operator
	// with a genuinely shell-less image should pass an explicit
	// -sandbox-user of their own (e.g. matching the image's baked-in
	// default), which bypasses ResolveDefaultWorkerIdentity's separated-
	// identity default entirely and never sets WorkerUmask at all.
	WorkerUmask string
	// ProgressPath, when non-empty, is the run's progress.jsonl sidecar
	// (progress.Path/PathInDir) that the stdout-scanning loop below
	// appends to whenever a line matches the FACTORY_PROGRESS worker
	// protocol (see progress.ParseWorkerLine) -- in addition to, never
	// instead of, writing the raw line to LogPath/stdout exactly as
	// before. Empty (the default) disables the relay entirely, matching
	// today's behavior. A write failure here is logged and ignored: the
	// progress feed is informational-only and must never fail or halt a
	// run (see progress-contract.md).
	ProgressPath string
	// RunID and DataDir are recorded as container labels (never passed to
	// the worker itself) solely so ReconcileOrphans can later identify,
	// scoped to this exact data directory, which run a leftover container
	// belongs to. Both are required — see Validate.
	RunID   string
	DataDir string
	// ScratchDir, when set, is a writable, disk-backed host directory
	// bind-mounted rw at WorkerScratchMount for build caches too large for
	// the memory-backed /home/worker tmpfs -- today the Go module and
	// build caches a RegistryProxyLifecycle points GOMODCACHE/GOCACHE at.
	// It belongs to this one container and Run removes it on exit (see
	// RunScratchDir). Found live 2026-09-10: the registry proxy set
	// GOPROXY but left GOMODCACHE on the image's read-only baked cache, so
	// any module the canonical image lacked failed with "read-only file
	// system" and the proxy could never actually replace a per-project
	// image; a real target repo's module graph (~1 GB extracted) does not
	// fit the tmpfs on a default 4 GiB Docker VM either. Must be absolute
	// and must not lie inside WorkDir (it would show up in the worker's
	// diff). Created and removed by the host side, never by the worker.
	ScratchDir string
	// GitCommonDir is populated by Run for linked Git worktrees whose .git
	// entry is a file. It is mounted read-only at the same absolute path in
	// the container so Git can follow the worktree's gitdir/commondir links
	// without exposing repository metadata for writes.
	gitCommonDir       string
	gitCommonDirTarget string
}

// InputMount describes a read-only host directory exposed under /inputs.
type InputMount struct {
	Source string
	Target string
}

// Result records only observed container execution facts.
type Result struct {
	Command    []string
	Container  string
	ExitCode   int
	StartedAt  time.Time
	FinishedAt time.Time
	LogPath    string
	// ImageDigest is the sha256 digest portion of LaunchSpec.Image (Validate
	// requires every image to be digest-pinned), carried onto Result so
	// callers can persist which immutable worker image identity a run
	// actually used without re-parsing the launch spec.
	ImageDigest string
	// RelayFacts contains audit-safe metadata about the model route the
	// worker was launched with. It is empty for a worker with none.
	RelayFacts RouteLaunchFacts
}

// ReferenceOracleSourceContained reports whether dir is equal to or
// beneath workDir -- the exact containment rule Validate enforces for
// ReferenceOracleDir, exported so a caller that snapshots
// ReferenceOracleDir before ever constructing a LaunchSpec (both
// cmd/factoryd's runGate and RunNamedGateActivity do, to close the
// TOCTOU gap PR #152 fixed) can apply it to the operator's ORIGINALLY
// CONFIGURED source first. Validate alone cannot catch this once
// snapshotting is in the picture: by the time a LaunchSpec exists,
// ReferenceOracleDir already names the snapshot destination, which sits
// under a run's own data directory and is therefore never inside
// WorkDir regardless of where the real source lived -- found via review,
// PR #153, after PR #152's own snapshot fix had (unintentionally)
// already defeated the round-2 containment check this same review round
// added.
//
// Both paths are canonicalized (made absolute, then symlink-resolved)
// before the comparison -- a purely lexical filepath.Rel on the raw
// inputs either errors (returning "not contained", the unsafe default)
// when workDir is absolute and a relative -reference-oracle-dir is
// compared against it, or misses a dir whose resolved location is inside
// workDir only via a symlinked ancestor. Found via review, PR #153 round
// 3 (Codex), after round 2's fix had closed the snapshot-substitution
// TOCTOU above but left this purely lexical comparison itself bypassable
// on the operator's ORIGINAL configured source. Any canonicalization
// failure (the path doesn't exist, or Abs/EvalSymlinks otherwise errors)
// fails closed as contained/unsafe -- this function guards a security
// boundary, so an unresolvable path must never read as "proven safe".
func ReferenceOracleSourceContained(workDir, dir string) bool {
	if workDir == "" || dir == "" {
		return false
	}
	// Two independent checks, EITHER sufficient to reject as contained:
	//
	//  1. Lexical: Abs'd but NOT symlink-resolved. Catches a dir that is
	//     literally beneath workDir even when an intermediate path
	//     component is itself a symlink that later resolves elsewhere --
	//     e.g. an operator-configured "-reference-oracle-dir
	//     $WORKDIR/link/oracle" where "link" is a symlink to some
	//     external directory. That symlink's own target string is a
	//     small file living INSIDE the writable workspace, so the worker
	//     can rewrite it during build (before this gate ever runs,
	//     post-build) to point wherever it likes -- letting the worker
	//     choose its own "external" oracle location despite the
	//     operator's original configuration looking safely external
	//     lexically. Checking the raw, unresolved path rejects this
	//     shape outright, regardless of where the symlink happens to
	//     currently point (found via review, GitHub Codex App, PR #154
	//     round 5).
	//  2. Canonical: Abs + EvalSymlinks. Catches the opposite shape: a
	//     dir reached only through a symlinked ANCESTOR (outside
	//     workDir) whose own resolved target sits inside workDir despite
	//     looking like a workDir sibling lexically (found via review,
	//     GitHub Codex App, PR #153 round 3).
	//
	// Either check alone is insufficient (each is the exact case the
	// other was added to close); only rejecting on either indicates
	// "contained" is safe. Any resolution failure fails closed as
	// contained/unsafe.
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return true
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	if pathBeneathOrEqual(absWorkDir, absDir) {
		return true
	}
	canonicalWorkDir, err := canonicalPathForContainmentCheck(workDir)
	if err != nil {
		return true
	}
	canonicalDir, err := canonicalPathForContainmentCheck(dir)
	if err != nil {
		return true
	}
	return pathBeneathOrEqual(canonicalWorkDir, canonicalDir)
}

// pathBeneathOrEqual reports whether candidate is root itself or lies
// beneath it, via a plain filepath.Rel comparison -- both callers in
// ReferenceOracleSourceContained already resolved root/candidate to the
// form (lexical-absolute, or canonical) they want compared before
// calling this.
func pathBeneathOrEqual(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalPathForContainmentCheck resolves p to an absolute,
// symlink-free path -- the form ReferenceOracleSourceContained's second
// (canonical) check needs to be immune to both a relative input and a
// symlinked ancestor. p must already exist (both real call sites pass a
// path that must exist to be usable as an oracle source or a workspace
// root at all).
func canonicalPathForContainmentCheck(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

// Validate rejects ambiguous or unsafe launch specifications before Docker is
// invoked. In particular, callers cannot opt into host networking, privileged
// mode, or an empty image/command.
func (s LaunchSpec) Validate() error {
	if s.Image == "" || strings.HasPrefix(s.Image, "-") {
		return errors.New("sandbox image is required")
	}
	// A mutable tag (":latest", ":v1", or even no tag at all) can point at
	// different content from one run to the next without the operator
	// changing any configuration factoryd records — the durable evidence
	// for a run would then describe a worker image that no longer exists.
	// Digest-pinning (name@sha256:...) makes the image identity part of the
	// content-addressed, immutable input the run's evidence already relies
	// on for everything else (spec/log SHA-256, base_sha).
	if !strings.Contains(s.Image, "@sha256:") {
		return errors.New("sandbox image must be pinned by digest (name@sha256:...)")
	}
	if len(s.Command) == 0 || s.Command[0] == "" {
		return errors.New("sandbox command is required")
	}
	if s.WorkDir == "" {
		return errors.New("sandbox work directory is required")
	}
	if !filepath.IsAbs(s.WorkDir) {
		return errors.New("sandbox work directory must be absolute")
	}
	if s.gitCommonDir != "" && !filepath.IsAbs(s.gitCommonDir) {
		return errors.New("sandbox Git common directory must be absolute")
	}
	if s.gitCommonDirTarget != "" && !filepath.IsAbs(s.gitCommonDirTarget) {
		return errors.New("sandbox Git common directory target must be absolute")
	}
	if (s.gitCommonDir == "") != (s.gitCommonDirTarget == "") {
		return errors.New("sandbox Git common directory source and target must be set together")
	}
	if s.InputDir != "" && !filepath.IsAbs(s.InputDir) {
		return errors.New("sandbox input directory must be absolute")
	}
	if s.ReferenceOracleDir != "" && !filepath.IsAbs(s.ReferenceOracleDir) {
		return errors.New("sandbox reference-oracle directory must be absolute")
	}
	if (s.ReferenceOracleDir == "") != (s.ReferenceOracleMountPath == "") {
		return errors.New("sandbox reference-oracle directory and mount path must be set together")
	}
	if s.ReferenceOracleMountPath != "" && filepath.IsAbs(s.ReferenceOracleMountPath) {
		return errors.New("sandbox reference-oracle mount path must be relative to the workspace, not absolute")
	}
	// A source directory equal to or beneath WorkDir defeats the entire
	// point of this mount (GitHub Codex App review, PR #151, round 2):
	// the build worker has full read-write access to WorkDir, so an
	// oracle source living inside it is exactly as tamperable as the
	// plain Allowed-Files convention this field exists to structurally
	// improve on -- the later read-only mount would just expose the
	// worker's own already-tampered bytes back to the gate.
	//
	// This check alone is NOT sufficient once a caller snapshots
	// ReferenceOracleDir before calling Validate (both cmd/factoryd's
	// runGate and RunNamedGateActivity do, to close the separate TOCTOU
	// gap PR #152 fixed): by then s.ReferenceOracleDir is the snapshot
	// destination, which is always outside WorkDir by construction, so
	// THIS check trivially passes regardless of whether the operator's
	// ORIGINAL configured source was inside the workspace. See
	// ReferenceOracleSourceContained's own doc comment -- a caller that
	// snapshots must call it themselves against the original source
	// first (found via review, PR #153).
	if s.ReferenceOracleDir != "" && ReferenceOracleSourceContained(s.WorkDir, s.ReferenceOracleDir) {
		return errors.New("sandbox reference-oracle directory must not be inside the workspace")
	}
	if s.ScratchDir != "" {
		if !filepath.IsAbs(s.ScratchDir) {
			return errors.New("sandbox scratch directory must be absolute")
		}
		if s.ScratchDir == s.WorkDir || strings.HasPrefix(s.ScratchDir, s.WorkDir+string(filepath.Separator)) {
			return errors.New("sandbox scratch directory must not be inside the workspace")
		}
	}
	for _, mount := range s.Inputs {
		if !filepath.IsAbs(mount.Source) || mount.Target == "" || filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) != mount.Target || strings.Contains(mount.Target, "..") {
			return fmt.Errorf("sandbox input mount %q is invalid", mount.Target)
		}
	}
	if s.LogPath == "" || !filepath.IsAbs(s.LogPath) {
		return errors.New("sandbox log path must be absolute")
	}
	if s.Name == "" {
		return errors.New("sandbox container name is required")
	}
	// RunID only ever becomes a Docker --label value (never a container
	// name or a path component itself, so it need not satisfy either of
	// those narrower character sets) and a field ReconcileOrphans parses
	// back out of tab-separated `docker ps --format` output. Found via
	// review: rejecting "/ \:" here disallowed run IDs the rest of
	// factoryd already accepts and reports as started (ticket names, and
	// -run-id from the API starter, routinely contain spaces or colons),
	// so a sandboxed request with one of those halted immediately after
	// its durable "ready" record already promised it had begun. Only a
	// newline or tab is actually unsafe: either would corrupt
	// ReconcileOrphans' own line/field parsing of that format output.
	if s.RunID == "" || strings.ContainsAny(s.RunID, "\n\t") {
		return errors.New("sandbox run id is required and must not contain a newline or tab")
	}
	if s.DataDir == "" || !filepath.IsAbs(s.DataDir) {
		return errors.New("sandbox data directory is required and must be absolute")
	}
	if err := validateUser(s.User); err != nil {
		return err
	}
	if err := validateWorkerUmask(s.WorkerUmask); err != nil {
		return err
	}
	if strings.ContainsAny(s.Name, "/ \\:") {
		return errors.New("sandbox container name contains unsafe characters")
	}
	if s.Timeout <= 0 {
		return errors.New("sandbox timeout must be positive")
	}
	if s.Memory == "" || s.CPUs == "" || s.TmpfsSize == "" {
		return errors.New("sandbox resource limits are required")
	}
	if s.Network == "" {
		return errors.New("sandbox network policy is required")
	}
	if s.Network == "host" {
		return errors.New("sandbox host networking is forbidden")
	}
	// s.Network == relayEgressNetworkName is rejected explicitly, not just
	// implied by the prefix check below: relayEgressNetworkName itself
	// satisfies "factoryd-relay-"-prefixed but is the one relay network
	// created *without* --internal (see ensureRelayEgressNetwork) — it has
	// a real route to the internet. A worker LaunchSpec pointed at it would
	// bypass the relay's per-run isolation entirely (found via review; see
	// the matching check in prepareWorkerSpec).
	if s.Network == relayEgressNetworkName {
		return fmt.Errorf("sandbox network %q is the relay's own egress network, not an allow-listed per-run network", s.Network)
	}
	// The registry proxy's own egress network (registryProxyEgressNetworkName,
	// internal/sandbox/registryproxy.go) is rejected the same way and for the
	// same reason as relayEgressNetworkName just above: it has a real route
	// to the internet, and "factoryd-registryproxy-"-prefixed alone would
	// otherwise admit it.
	if s.Network == registryProxyEgressNetworkName {
		return fmt.Errorf("sandbox network %q is the registry proxy's own egress network, not an allow-listed per-run network", s.Network)
	}
	if s.Network != "none" && !strings.HasPrefix(s.Network, "factoryd-relay-") && !strings.HasPrefix(s.Network, "factoryd-registryproxy-") {
		return fmt.Errorf("sandbox network %q is not allow-listed", s.Network)
	}
	// composeServicesNetworkName's own fixed "bg-compose-" prefix -- see
	// ComposeNetwork's own doc comment. Rejected the same way as an
	// unlisted Network above: an unchecked ComposeNetwork string reaching
	// `docker network connect` would let a caller attach the worker to any
	// network on the host, including one with a real route out.
	if s.ComposeNetwork != "" && !strings.HasPrefix(s.ComposeNetwork, "bg-compose-") {
		return fmt.Errorf("sandbox compose network %q is not allow-listed", s.ComposeNetwork)
	}
	environmentKeys := make(map[string]bool, len(s.Environment))
	for _, value := range s.Environment {
		environmentKeys[strings.SplitN(value, "=", 2)[0]] = true
	}
	for _, value := range s.UnrecordedEnvironment {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" {
			return fmt.Errorf("sandbox unrecorded environment entry for %q is not KEY=VALUE", key)
		}
		if IsForbiddenCredentialEnvKey(key) || key == "ANTHROPIC_API_KEY" || key == "PATH" || strings.HasPrefix(key, "GIT_CONFIG_") {
			return fmt.Errorf("sandbox unrecorded environment contains reserved key %q", key)
		}
		if environmentKeys[key] {
			return fmt.Errorf("sandbox unrecorded environment key %q is already set by the factory", key)
		}
	}
	for _, value := range s.Environment {
		if !strings.Contains(value, "=") {
			return fmt.Errorf("sandbox environment entry %q is not KEY=VALUE", value)
		}
		key := strings.SplitN(value, "=", 2)[0]
		if key == "" || IsForbiddenCredentialEnvKey(key) {
			return fmt.Errorf("sandbox environment contains forbidden credential %q", key)
		}
		// GIT_CONFIG_GLOBAL/SYSTEM are deliberately absent from
		// IsForbiddenCredentialEnvKey's own shared list (see its doc
		// comment) so internal/runner's fixed baseline can admit them for
		// this repo's own test-isolation fixtures on the unsandboxed
		// request-driver jobs (spec-drafting/plan-drafting) that still run
		// on the host. That carve-out belongs there only: a sandboxed
		// worker's own Environment has no legitimate reason to redirect
		// which file git reads as its global/system config at all (found
		// via code review: the shared list's narrowing for Phase 1.1 also
		// silently widened what LaunchSpec.Validate itself would accept
		// here, an unintended side effect of unifying the two enforcement
		// points). Rejected here specifically, on top of the shared list,
		// rather than by broadening that list back out and reopening the
		// gap it was narrowed to close.
		if key == "GIT_CONFIG_GLOBAL" || key == "GIT_CONFIG_SYSTEM" {
			return fmt.Errorf("sandbox environment contains forbidden credential %q", key)
		}
		if key == "ANTHROPIC_API_KEY" && value != "ANTHROPIC_API_KEY="+AnthropicAPIKeyPlaceholder {
			return fmt.Errorf("sandbox environment contains forbidden credential %q", key)
		}
	}
	return nil
}

// standInPIDLimit is the process limit of a worker launched with `docker
// run`, the tests' stand-in. A gateway-launched worker gets the gateway's
// own limit (sandbox_pids_limit in the gateway configuration).
const standInPIDLimit = "512"

// DockerCommand builds the complete allow-listed Docker invocation. It is
// exported for deterministic argument tests; Run is the only method that
// executes it.
func (s LaunchSpec) DockerCommand(dockerBinary string) []string {
	return s.dockerCommand(dockerBinary, "run")
}

// dockerCreateCommand is DockerCommand's own "docker create" counterpart --
// identical flag surface (docker accepts the same flags for both verbs),
// used by Run's create/connect/start sequence when a relay or registry
// network is primary and ComposeNetwork is additional.
func (s LaunchSpec) dockerCreateCommand(dockerBinary string) []string {
	return s.dockerCommand(dockerBinary, "create")
}

func (s LaunchSpec) dockerCommand(dockerBinary, verb string) []string {
	args := []string{
		verb, "--rm", "--name", s.Name,
		"--label", labelKey("worker") + "=true",
		"--label", labelKey("launch-spec") + "=" + strconv.Itoa(launchSpecVersion),
		"--label", labelKey("run") + "=" + s.RunID,
		"--label", labelKey("data-dir") + "=" + dataDirLabel(s.DataDir),
		"--read-only", "--user", s.User, "--cap-drop=ALL", "--security-opt=no-new-privileges",
		// --cgroupns=private: without it, a container on a cgroup v1 host
		// (or one with the compat mount enabled) sees the host's own root
		// cgroup hierarchy read-only under /sys/fs/cgroup, which leaks host
		// resource-accounting metadata (every cgroup's name and layout) the
		// worker has no legitimate reason to see. No effect on a cgroup v2
		// host already namespacing this by default -- explicit here so the
		// guarantee doesn't depend on the host's own cgroup configuration
		// (found via review).
		"--cgroupns=private",
		// --ulimit core=0: refuse core dumps. A worker crash producing a
		// core file inside the read-only root would fail anyway, but this
		// makes the refusal explicit rather than relying on that side
		// effect, and stops a core dump from landing in a writable mount
		// (tmpfs /home/worker) where one legitimately could. The nofile/
		// nproc ceilings below are deliberately generous, not tuned tight:
		// this is defense against a runaway/hostile worker exhausting file
		// descriptors or forking a fork-bomb, not a resource budget --
		// --pids-limit above already owns the real per-container process
		// ceiling. Chosen high enough that `go build`/`go test`/`pi` were
		// verified (TestRunLiveDockerWithHardenedResourceLimits,
		// DOCKER_SANDBOX_LIVE=1) to still run to completion under them.
		// Note: RLIMIT_NPROC is accounted per real UID at the kernel level,
		// not per-container -- with s.User left at its default (the
		// invoking host UID, not a dedicated sandbox UID; see the deferred
		// worker-UID-separation item in CLAIMS.md), several concurrent
		// sandboxed runs on the same host share one nproc budget, not one
		// each. 2048 is chosen generously with that sharing in mind;
		// dedicated per-worker UIDs would remove the sharing entirely.
		"--ulimit", "core=0",
		"--ulimit", "nofile=4096:4096",
		"--ulimit", "nproc=2048:2048",
		"--pids-limit", standInPIDLimit, "--memory", s.Memory,
		// --memory-swap explicitly equal to --memory, not left at Docker's
		// own default (found via a real Opus review pass, 2026-09-04):
		// unset, Docker gives a container swap equal to 2x --memory, so
		// the real ceiling was double the one this flag claims (e.g. "4g"
		// really meant up to 8g of memory+swap). Setting both to the same
		// value disables swap for this container entirely -- --memory
		// alone is then the actual, honest ceiling.
		"--memory-swap", s.Memory,
		"--cpus", s.CPUs,
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=" + s.TmpfsSize,
		// /home/worker: deliberately `exec`, unlike /tmp above (Docker's
		// default tmpfs options are noexec unless overridden). The worker
		// image's toolchain needs a writable, executable $HOME under the
		// --read-only root filesystem for state a fixed, allow-listed PATH
		// alone can't provide: `go test` links its compiled test binary
		// under GOTMPDIR (defaulted into $HOME by the image) and execs it
		// directly -- verified empirically that a plain `nosuid` tmpfs here
		// still refused to exec a freshly built binary ("permission
		// denied") until `exec` was added explicitly; `go build` needs a
		// writable GOCACHE; `pi` needs a writable ~/.pi config/state dir
		// (its own package.json declares "piConfig":{"configDir":".pi"},
		// relative to $HOME, not XDG). `mode=1777` (matching /tmp's own
		// default) is required too: without it this mount comes up
		// root-owned 0755, unwritable by the non-root worker UID. This is
		// still capped and distinct from /workspace -- the worker's actual
		// changes still only ever land in the bind-mounted workspace above.
		"--tmpfs", "/home/worker:rw,exec,nosuid,mode=1777,size=" + s.TmpfsSize,
		"--network", s.Network,
		"--workdir", workerContainerWorkDir,
		"--volume", s.WorkDir + ":" + workerContainerWorkDir + ":rw",
	}
	// Keep Git metadata unavailable for writes by the untrusted worker. Host
	// safety-net commits must never execute attacker-controlled filters/hooks.
	gitPath := filepath.Join(s.WorkDir, ".git")
	if info, err := os.Stat(gitPath); err == nil && info.IsDir() {
		args = append(args, "--volume", gitPath+":/workspace/.git:ro")
	}
	if s.gitCommonDir != "" {
		// Linked worktree: WorkDir's own .git is not a directory here, it's
		// a regular "gitdir: <path>" pointer FILE -- left inside the rw
		// /workspace mount by the directory-only branch above, so on its
		// own it stays fully writable by the untrusted worker. A worker
		// that rewrites it to point at a second, worker-authored fake repo
		// (also inside /workspace, so equally writable) gets the HOST's
		// own later git calls against this same workspace (GitRevParseHEAD,
		// GitIsClean, GitCommitAll, ...) to read that fake repo's config
		// instead of the real one -- and git resolves filter.<name>.clean,
		// diff.external, core.fsmonitor, core.sshCommand, etc. straight out
		// of whatever config a repo's own .git/config declares, running
		// them as whatever user invokes git. That's the host factoryd
		// process, with its own full environment (API keys, SSH agent,
		// Docker access). The mount below closes it the same way the
		// directory case already was closed: the worker can read the
		// pointer (needed for git inside the container to function at
		// all) but Docker refuses any write through a read-only bind
		// mount, so the pointer can never actually be rewritten. Found via
		// a real Opus review pass, 2026-09-04 -- reproduced end to
		// end: an unpatched build let this exact rewrite get host-side
		// `git add -A` to execute an attacker-chosen filter command as the
		// factoryd host user, silently, with the run proceeding normally
		// (exit 0) as if nothing had happened.
		args = append(args, "--volume", gitPath+":/workspace/.git:ro")
		args = append(args, "--volume", s.gitCommonDir+":"+s.gitCommonDirTarget+":ro")
	}
	if s.ReferenceOracleDir != "" {
		// Nested read-only mount inside the writable /workspace bind mount
		// above, the same technique as the .git:ro mounts just above --
		// see LaunchSpec.ReferenceOracleDir's own doc comment for why.
		args = append(args, "--volume", s.ReferenceOracleDir+":"+workerContainerWorkDir+"/"+s.ReferenceOracleMountPath+":ro")
	}
	for _, mask := range s.WorkspaceMasks {
		args = append(args, "--volume", mask.Source+":"+workerContainerWorkDir+"/"+mask.Target+":ro")
	}
	if s.InputDir != "" {
		args = append(args, "--volume", s.InputDir+":/inputs:ro")
	}
	if s.ScratchDir != "" {
		args = append(args, "--volume", s.ScratchDir+":"+WorkerScratchMount+":rw")
	}
	for _, mount := range s.Inputs {
		args = append(args, "--volume", mount.Source+":/inputs/"+mount.Target+":ro")
	}
	for _, value := range s.UnrecordedEnvironment {
		key, _, _ := strings.Cut(value, "=")
		args = append(args, "--env", key)
	}
	args = append(args,
		"--env", "PATH=/usr/local/bin:/usr/bin:/bin",
		"--env", "GIT_CONFIG_COUNT=1",
		"--env", "GIT_CONFIG_KEY_0=safe.directory",
		"--env", "GIT_CONFIG_VALUE_0=/workspace",
	)
	for _, value := range s.Environment {
		args = append(args, "--env", value)
	}
	args = append(args, s.Image)
	command := s.Command
	if s.WorkerUmask != "" {
		// "--": the classic `sh -c SCRIPT -- $0 $@` idiom. Everything after
		// SCRIPT becomes positional parameters, with the first one bound
		// to $0 (which the script never references) and the rest to
		// "$@" -- without this placeholder, s.Command's own first element
		// would silently become $0 instead of the first element of "$@",
		// dropping it from the command actually exec'd.
		//
		// The command is run (not exec'd) so this script regains control
		// after it exits: a final `chmod -R g+rwX` reclaims group
		// writability under workerContainerWorkDir before the container exits, then
		// re-raises the command's own exit code. This closes the P1 found
		// via GitHub Codex App review of PR #62: the umask above only sets
		// the mode a worker-created path starts with, and a worker (hostile
		// or just running an ordinary tool that hardens a private cache
		// directory to 0700) can chmod its own files however it likes
		// afterward, at which point factoryd -- a different UID relying on
		// group membership, not ownership -- can no longer write or even
		// traverse them for later git/rollback operations. The worker's own
		// UID, which owns everything it created regardless of that path's
		// current mode, is the only identity that can always chmod it back;
		// running this reclaim as that same worker, as the last thing the
		// container does, needs no new privilege the worker didn't already
		// have. Best-effort: a path the worker made unreadable to itself, or
		// one under a read-only bind mount a symlink escaped to, fails this
		// chmod silently (`|| true`) rather than masking the real command's
		// exit code.
		//
		// Reclaims workerContainerWorkDir, the fixed container-side path
		// s.WorkDir is always bind-mounted to (see "--workdir"/"--volume"
		// above) -- not s.WorkDir itself, a HOST path that does not exist
		// inside this container at all (found via a second GitHub Codex App
		// review round, P1: the first version of this fix passed s.WorkDir
		// verbatim, making the reclaim a silent no-op on every real run,
		// its failure unconditionally swallowed by `|| true`).
		// /scratch reclaimed the same way when mounted: its contents are
		// worker-owned too, and a build tool that tightens a directory
		// there would otherwise leave the host-side RemoveScratchDir with
		// an EPERM and a gigabyte of cache behind (Codex review of PR #98).
		// stderr dropped: the workspace's .git is mounted read-only, so this
		// walk always reports it, and that line landed in every step's
		// captured output, a gate's failing lines included.
		reclaim := "chmod -R g+rwX -- " + shSingleQuote(workerContainerWorkDir) + " 2>/dev/null || true"
		if s.ScratchDir != "" {
			// stderr dropped: go's build cache trims itself concurrently,
			// so this walk routinely reports a file that vanished under
			// it -- harmless, and otherwise a noisy line in every run log.
			reclaim += "; chmod -R g+rwX -- " + shSingleQuote(WorkerScratchMount) + " 2>/dev/null || true"
		}
		command = append([]string{"/bin/sh", "-c",
			"umask " + s.WorkerUmask + `; "$@"; ec=$?; ` + reclaim + `; exit $ec`,
			"--"}, s.Command...)
	}
	args = append(args, command...)
	return append([]string{dockerBinary}, args...)
}

// withResolvedMounts returns s with every mount source resolved through
// symlinks and a linked worktree's Git common directory filled in, after
// checking it: the Git credential preflight, Validate, each mount path, and
// free space under the worktree and the data directory. Every launcher
// builds its mounts from the result, never from s.
func (s LaunchSpec) withResolvedMounts() (LaunchSpec, error) {
	var err error
	s.WorkDir, err = filepath.EvalSymlinks(s.WorkDir)
	if err != nil {
		return LaunchSpec{}, fmt.Errorf("resolve sandbox workspace mount: %w", err)
	}
	if s.InputDir != "" {
		s.InputDir, err = filepath.EvalSymlinks(s.InputDir)
		if err != nil {
			return LaunchSpec{}, fmt.Errorf("resolve sandbox input mount: %w", err)
		}
	}
	if s.ReferenceOracleDir != "" {
		s.ReferenceOracleDir, err = filepath.EvalSymlinks(s.ReferenceOracleDir)
		if err != nil {
			return LaunchSpec{}, fmt.Errorf("resolve sandbox reference-oracle mount: %w", err)
		}
	}
	if s.WorkspaceMasks, err = resolveWorkspaceMasks(s.WorkspaceMasks, s.ReferenceOracleMountPath); err != nil {
		return LaunchSpec{}, err
	}
	for i := range s.Inputs {
		s.Inputs[i].Source, err = filepath.EvalSymlinks(s.Inputs[i].Source)
		if err != nil {
			return LaunchSpec{}, fmt.Errorf("resolve sandbox input %s mount: %w", s.Inputs[i].Target, err)
		}
	}
	gitEntry, gitErr := os.Lstat(filepath.Join(s.WorkDir, ".git"))
	if gitErr == nil {
		switch {
		case gitEntry.IsDir():
		case gitEntry.Mode().IsRegular():
			commonDir, target, err := gitCommonDir(s.WorkDir)
			if err != nil {
				return LaunchSpec{}, fmt.Errorf("resolve sandbox worktree Git metadata: %w", err)
			}
			s.gitCommonDir = commonDir
			s.gitCommonDirTarget = target
		default:
			return LaunchSpec{}, errors.New("sandbox workspace .git entry must be a directory or regular worktree file")
		}
	}
	// Per containment-matrix.md's Credentials row: the Git common
	// directory mounted below is read-only, but still readable -- fail
	// closed here, before any mount is built, rather than let a worker
	// read a credential the mount would otherwise expose.
	if err := PreflightGitCredentials(s.WorkDir); err != nil {
		return LaunchSpec{}, err
	}
	if err := s.Validate(); err != nil {
		return LaunchSpec{}, err
	}
	if err := s.validateMountPaths(); err != nil {
		return LaunchSpec{}, err
	}
	// The worker's /workspace mount is rw with no Docker-level size limit
	// (unlike the tmpfs mounts above, which do have one) -- a worker that
	// simply writes without end can fill the host filesystem, including
	// whatever volume holds -data-dir and every run's durable evidence
	// (found via a real Opus review pass, 2026-09-04). This is a cheap,
	// pre-launch check, not a running quota: it refuses to even start a
	// worker against a mount that's already low on space, rather than
	// trying to cap what one writes while it runs.
	//
	// Checks s.DataDir too, not just s.WorkDir (found in self-review,
	// 2026-09-04, right after this was written): -workspace and -data-dir
	// are never required to share a filesystem -- an operator can point
	// them at separate volumes, and only WorkDir's own free space guarded
	// the worker's writable mount alone, silently defeating the stated
	// "protects the volume holding durable run evidence" purpose whenever
	// they differ. statfs is cheap; checking both costs nothing a single
	// check didn't already pay.
	if err := checkMinFreeBytes(s.WorkDir); err != nil {
		return LaunchSpec{}, err
	}
	if s.DataDir != s.WorkDir {
		if err := checkMinFreeBytes(s.DataDir); err != nil {
			return LaunchSpec{}, fmt.Errorf("data directory: %w", err)
		}
	}
	return s, nil
}

// validateMountPaths checks every resolved mount source: the worktree must
// be a writable directory, the others directories.
func (s LaunchSpec) validateMountPaths() error {
	if err := validateMountPath(s.WorkDir, true); err != nil {
		return fmt.Errorf("sandbox workspace: %w", err)
	}
	if s.gitCommonDir != "" {
		if err := validateMountPath(s.gitCommonDir, false); err != nil {
			return fmt.Errorf("sandbox Git common directory: %w", err)
		}
	}
	if s.InputDir != "" {
		if err := validateMountPath(s.InputDir, false); err != nil {
			return fmt.Errorf("sandbox inputs: %w", err)
		}
	}
	if s.ReferenceOracleDir != "" {
		if err := validateMountPath(s.ReferenceOracleDir, false); err != nil {
			return fmt.Errorf("sandbox reference-oracle mount: %w", err)
		}
	}
	for _, mount := range s.Inputs {
		if err := validateMountPath(mount.Source, false); err != nil {
			return fmt.Errorf("sandbox input %s: %w", mount.Target, err)
		}
	}
	if err := validateWorkspaceMasks(s.WorkspaceMasks, s.ReferenceOracleMountPath); err != nil {
		return fmt.Errorf("sandbox workspace mask: %w", err)
	}
	return nil
}

// resolveWorkspaceMasks validates masks (so a symlink source is refused
// before it is followed) and returns a copy whose sources are resolved
// through the symlinks of their parent directories.
func resolveWorkspaceMasks(masks []WorkspaceMask, oraclePath string) ([]WorkspaceMask, error) {
	if len(masks) == 0 {
		return nil, nil
	}
	if err := validateWorkspaceMasks(masks, oraclePath); err != nil {
		return nil, fmt.Errorf("sandbox workspace mask: %w", err)
	}
	out := make([]WorkspaceMask, len(masks))
	for i, m := range masks {
		parent, err := filepath.EvalSymlinks(filepath.Dir(m.Source))
		if err != nil {
			return nil, fmt.Errorf("resolve sandbox workspace mask %s: %w", m.Target, err)
		}
		m.Source = filepath.Join(parent, filepath.Base(m.Source))
		out[i] = m
	}
	return out, nil
}

// Run starts and waits for one disposable worker, streaming combined output
// into LogPath. The process group is killed on cancellation; Docker's rm
// flag and a best-effort explicit rm keep a canceled container from lingering.
func Run(ctx context.Context, dockerBinary string, s LaunchSpec) (Result, error) {
	if s.ScratchDir != "" {
		// This container's own scratch directory (RunScratchDir): no later
		// container may see what this one wrote. Removed after the
		// container exits, by which point its exit wrapper has reclaimed
		// group write on the tree (DockerCommand).
		defer func() { _ = removeScratchTree(s.ScratchDir) }()
	}
	s, err := s.withResolvedMounts()
	if err != nil {
		return Result{}, err
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	// Only WorkDir is ever bind-mounted into the worker container itself
	// (see DockerCommand's "--volume" args) -- DataDir never is; it is
	// recorded only as a container label and written to from the HOST
	// side (writeOwnerHeartbeat below), never read by anything running
	// inside a container. So the colima-style "host path silently isn't
	// shared into the Docker VM" trap this check exists to catch can only
	// ever bite WorkDir, and checking it there is where the probe belongs.
	if err := verifyWorkDirMountVisibility(ctx, dockerBinary, s); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(s.LogPath), 0o750); err != nil {
		return Result{}, fmt.Errorf("create sandbox log directory: %w", err)
	}
	logFile, err := os.OpenFile(s.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("create sandbox log: %w", err)
	}
	defer logFile.Close()

	// Recorded and then periodically refreshed for the lifetime of this
	// call (found via review): ReconcileOrphans' only way to tell a
	// container abandoned by an attempt that's no longer being supervised
	// apart from one still legitimately owned by a live sibling invocation
	// against the same data directory is this heartbeat — without it, a
	// container whose run record hadn't yet reached a terminal state was
	// left alone unconditionally, contradicting the intended contract that
	// an unknown prior container is stopped, recorded, and quarantined
	// rather than retried blindly.
	if err := writeOwnerHeartbeat(s.DataDir, s.RunID); err != nil {
		return Result{}, fmt.Errorf("record sandbox owner: %w", err)
	}
	stopHeartbeat := startOwnerHeartbeat(s.DataDir, s.RunID)
	defer stopHeartbeat()

	// Docker's private "none" network cannot be combined with a later
	// `network connect`; use the factory-owned Compose network as the
	// container's primary network instead. It is still an internal,
	// allow-listed network, so this does not grant host or internet access.
	if s.ComposeNetwork != "" && s.Network == "none" {
		s.Network = s.ComposeNetwork
		s.ComposeNetwork = ""
	}
	command := s.DockerCommand(dockerBinary)
	// ComposeNetwork set: the ordinary single `docker run` launch below
	// cannot attach a second network at container-create time, so this
	// launch instead creates the container unstarted (with the exact same
	// flags, including --network for the primary Network above), connects
	// it to the compose-services network, and only then starts it --
	// `docker start -a` replacing `docker run` as the one foreground,
	// streamed, cancelable command the rest of this function already
	// drives identically either way. A launch with no compose services
	// active (ComposeNetwork == "") is completely untouched: command stays
	// exactly the `docker run ...` built above.
	if s.ComposeNetwork != "" {
		createCmd := s.dockerCreateCommand(dockerBinary)
		createCtx, cancelCreate := context.WithTimeout(ctx, relayDockerCommandTimeout)
		create := exec.CommandContext(createCtx, createCmd[0], createCmd[1:]...)
		create.Env = append(dockerClientEnv(), s.UnrecordedEnvironment...)
		out, createErr := create.CombinedOutput()
		cancelCreate()
		if createErr != nil {
			return Result{}, fmt.Errorf("docker create failed: %w: %s", createErr, out)
		}
		connectCtx, cancelConnect := context.WithTimeout(ctx, relayDockerCommandTimeout)
		connectCmd := exec.CommandContext(connectCtx, dockerBinary, "network", "connect", s.ComposeNetwork, s.Name)
		connectCmd.Env = dockerClientEnv()
		connectOut, connectErr := connectCmd.CombinedOutput()
		cancelConnect()
		if connectErr != nil {
			// context.Background(), not ctx/connectCtx: mirrors the
			// cancellation-cleanup call further down -- ctx may already be
			// near its own deadline, and this best-effort removal of the
			// container this call just created must not be starved by it.
			if cleanupErr := ensureRemoved(context.Background(), dockerBinary, s.Name); cleanupErr != nil {
				return Result{}, fmt.Errorf("attach worker to compose services network: %w: %s; cleanup: %v", connectErr, connectOut, cleanupErr)
			}
			return Result{}, fmt.Errorf("attach worker to compose services network: %w: %s", connectErr, connectOut)
		}
		command = []string{dockerBinary, "start", "-a", s.Name}
	}
	result := Result{Command: command, Container: s.Name, ExitCode: -1, LogPath: s.LogPath, StartedAt: time.Now(), ImageDigest: imageDigest(s.Image)}
	runCtx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	// For `docker run`: the values behind UnrecordedEnvironment's --env KEY
	// flags. `docker start` ignores them; create read them above.
	cmd.Env = append(dockerClientEnv(), s.UnrecordedEnvironment...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return result, fmt.Errorf("sandbox stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start sandbox: %w", err)
	}
	defer keepAwake(cmd.Process.Pid)()
	writer := io.MultiWriter(os.Stdout, logFile)
	if err := relayWorkerOutput(stdout, writer, s); err != nil {
		if cleanupErr := reapContainer(cmd, dockerBinary, s.Name, s.WorkerUmask != ""); cleanupErr != nil {
			return result, fmt.Errorf("%w; cleanup: %w", err, cleanupErr)
		}
		return result, err
	}
	waitErr := cmd.Wait()
	result.FinishedAt = time.Now()
	if waitErr == nil {
		result.ExitCode = 0
		return result, nil
	}
	if runCtx.Err() != nil {
		// Same reasoning as reapContainer's own reclaim call (found via
		// GitHub Codex App review of PR #62, P1): cmd.Cancel already
		// SIGKILLed the docker CLI process above, but the container itself
		// may still be running -- best-effort reclaim before it's gone for
		// good below, since this run's own timeout is exactly the forced
		// teardown case that never reaches DockerCommand's post-command
		// shell epilogue.
		if s.WorkerUmask != "" {
			reclaimContainerWorkDir(dockerBinary, s.Name)
		}
		// context.Background(), not runCtx: runCtx is already Done() here
		// (that's this branch's own condition) — passing it straight
		// through would make ensureRemoved's own context.WithTimeout
		// immediately canceled too, giving `docker rm` no real chance to
		// run at all.
		if cleanupErr := ensureRemoved(context.Background(), dockerBinary, s.Name); cleanupErr != nil {
			return result, fmt.Errorf("sandbox cancellation cleanup: %w", cleanupErr)
		}
		return result, fmt.Errorf("sandbox %s: %w", s.Name, runCtx.Err())
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		if result.ExitCode == 125 {
			return result, errors.New("docker run failed before the worker started (exit status 125)")
		}
		return result, nil
	}
	return result, fmt.Errorf("sandbox wait: %w", waitErr)
}

// relayWorkerOutput copies a worker's output to dst line by line until src
// ends, appending each FACTORY_PROGRESS line to the run's progress file (up
// to the per-kind caps) and failing once more than maxLogBytes were written.
// Both launchers use it: over the pipe of `docker run`, and over the output
// file a worker launched through a Runtime writes.
func relayWorkerOutput(src io.Reader, dst io.Writer, s LaunchSpec) error {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var written int64
	var relayedAgent, relayedRound int
	for scanner.Scan() {
		line := scanner.Text()
		if s.ProgressPath != "" {
			if ev, ok := progress.ParseWorkerLine(line); ok {
				relay := false
				switch ev.Stage {
				case "round":
					relay = relayedRound < maxRelayedRoundLines
					relayedRound++
				default:
					relay = relayedAgent < maxRelayedAgentLines
					relayedAgent++
				}
				if relay {
					if err := progress.Append(s.ProgressPath, ev); err != nil {
						log.Printf("sandbox: append progress event for %s: %v", s.Name, err)
					}
				}
			}
		}
		n, err := fmt.Fprintln(dst, line)
		written += int64(n)
		if err != nil {
			return fmt.Errorf("write sandbox log: %w", err)
		}
		if written > maxLogBytes {
			return fmt.Errorf("%w (%d bytes)", errOutputLimitExceeded, written)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read sandbox output: %w", err)
	}
	return nil
}

func gitCommonDir(workDir string) (source, target string, err error) {
	out, err := exec.Command("git", "-C", workDir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", "", err
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workDir, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return "", "", err
	}
	commonDir = filepath.Clean(commonDir)
	resolved, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", "", err
	}
	return resolved, commonDir, nil
}

// imageDigest extracts the "sha256:..." portion of a digest-pinned image
// reference (name@sha256:...), which Validate already requires. Returns ""
// only if called on a spec that skipped Validate.
func imageDigest(image string) string {
	_, digest, found := strings.Cut(image, "@")
	if !found {
		return ""
	}
	return digest
}

// DefaultWorkerUID is the fixed, dedicated UID a sandboxed worker container
// runs as by default (cmd/factoryd's -sandbox-worker-uid), instead of the
// factoryd host process's own UID -- running the worker as the host's own
// UID was flagged as a containment gap and closed by this dedicated
// identity. 65532 is the "distroless nonroot" convention (gcr.io/distroless's
// own nonroot user, and a common Kubernetes runAsUser example) -- chosen
// deliberately over the more familiar "nobody" UID (65534) because a real
// host commonly already has an actual nobody account that may already own
// unrelated files elsewhere on the same filesystem; 65532 is unassigned by
// any mainstream distribution's default /etc/passwd, so files this
// dedicated identity creates are unambiguously attributable to the
// sandboxed worker and nothing else pre-existing on the host.
// workerContainerWorkDir is the fixed, container-side path LaunchSpec.WorkDir
// (a HOST path) is always bind-mounted to -- see DockerCommand's own
// "--workdir"/"--volume" args. Anything that needs to act on the workspace
// from *inside* the container (the WorkerUmask reclaim script below) must
// use this, not s.WorkDir: a worker process only ever sees this container
// path, never the host path s.WorkDir actually holds (found via GitHub
// Codex App review of PR #62, P1 -- the first version of the reclaim chmod
// passed s.WorkDir verbatim into the wrapped script, which ran it against a
// path that does not exist inside the container at all, so the reclaim was
// a silent no-op on every real run).
const workerContainerWorkDir = "/workspace"

// WorkerScratchMount is where LaunchSpec.ScratchDir appears inside the
// worker container.
const WorkerScratchMount = "/scratch"

// RunScratchDir is the per-run host directory holding each launch's
// LaunchSpec.ScratchDir: <dataDir>/scratch/<runID>/<launch>. One
// directory per worker container, never shared: a cache one container
// wrote is worker-writable state, and the canonical verify, full-suite
// and gate containers must not compile or replay what the build phase
// (or an earlier attempt) left there -- sharing it let verify print Go's
// "(cached)" for the build phase's own test results, so the canonical
// oracle neither re-ran them nor could rule out a planted cache entry
// (found in the 2026-09-29 todo-kafka-service demo). Modules come back
// through the registry proxy, which only serves read-only upstream
// content. Run removes a launch's directory when its container exits;
// RemoveScratchDir removes the run's. Under -data-dir, next to the run's
// own workspaces/ subtree, rather than beside the workspace: for a
// non-isolated run the workspace is the operator's own checkout, and a
// sibling there would litter their source tree.
func RunScratchDir(dataDir, runID string) string {
	return filepath.Join(dataDir, "scratch", runID)
}

// RemoveScratchDir deletes RunScratchDir(dataDir, runID), and so every
// launch's directory under it, when the run ends. Removal relies on the group write the container's exit wrapper
// reclaims under /scratch (see DockerCommand): the worker owns what it
// created there and the host only shares its gid, so a host-side chmod
// on those directories fails with EPERM and must never abort the
// removal (Codex review of PR #98). The chmod walk is a best-effort
// backstop for directories the host itself owns (a build tool that
// tightened one under -modcacherw, an override of GOFLAGS). Removing a
// directory that does not exist is not an error.
func RemoveScratchDir(dataDir, runID string) error {
	return removeScratchTree(RunScratchDir(dataDir, runID))
}

// removeScratchTree is RemoveScratchDir for any scratch directory: see its
// doc comment for why the chmod walk is best-effort.
func removeScratchTree(dir string) error {
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o770)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// ReconcileScratchDirs removes every scratch directory under dataDir whose
// run is no longer in flight -- its record is terminal or missing --
// so a factoryd killed mid-run never leaves a gigabyte of module and
// build cache behind permanently (Codex review of PR #98): the in-run
// defer is the normal path, this is the crash path, run at the same
// startup point isolation markers are reconciled. A directory whose run
// record is non-terminal is left alone: another process may still be
// running it.
func ReconcileScratchDirs(dataDir string, inFlight func(runID string) bool) []string {
	entries, err := os.ReadDir(filepath.Join(dataDir, "scratch"))
	if err != nil {
		return nil
	}
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() || inFlight(entry.Name()) {
			continue
		}
		if err := RemoveScratchDir(dataDir, entry.Name()); err == nil {
			removed = append(removed, entry.Name())
		}
	}
	return removed
}

// EnsureScratchDir creates launch's own scratch directory,
// RunScratchDir(dataDir, runID)/<launch>, writable by the worker identity
// the container will run as (user is LaunchSpec.User, "<uid>:<gid>" or
// "<uid>", possibly empty for the image default): both levels are
// group-writable and, when user names a gid, owned by that group -- the
// same grant wsisolation.EnableWorkerGroupWrite makes on the workspace.
// Chown is what makes this work when factoryd runs as root with an
// explicit non-root -sandbox-user, or with a worker gid other than the
// process's own (Codex review of PR #98); a non-root factoryd can only
// chown to a group it belongs to, which is the same constraint the
// workspace grant already lives under. Chmod'd explicitly after MkdirAll
// because the process umask would otherwise strip the group bit. launch
// is the container's name: one directory per container, see
// RunScratchDir.
func EnsureScratchDir(dataDir, runID, launch, user string) (string, error) {
	if dataDir == "" || !filepath.IsAbs(dataDir) || runID == "" {
		return "", errors.New("sandbox scratch directory needs an absolute data directory and a run id")
	}
	if launch == "" || filepath.Base(launch) != launch || launch == "." || launch == ".." {
		return "", fmt.Errorf("sandbox scratch directory needs a plain launch name, got %q", launch)
	}
	runDir := RunScratchDir(dataDir, runID)
	dir := filepath.Join(runDir, launch)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return "", fmt.Errorf("create sandbox scratch directory: %w", err)
	}
	for _, d := range []string{runDir, dir} {
		if err := grantScratchDir(d, user); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// grantScratchDir makes dir group-writable for the worker identity user;
// see EnsureScratchDir.
func grantScratchDir(dir, user string) error {
	if err := os.Chmod(dir, 0o770); err != nil {
		return fmt.Errorf("make sandbox scratch directory group-writable: %w", err)
	}
	if uid, gid, ok := parseWorkerIdentity(user); ok {
		// Only root may change the owner; everyone else may only move
		// the directory into a group they belong to, which is what the
		// worker's own gid (the host gid, by ResolveDefaultWorkerIdentity)
		// needs anyway.
		if os.Geteuid() != 0 {
			uid = -1
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return fmt.Errorf("give sandbox scratch directory to worker identity %q: %w", user, err)
		}
	}
	return nil
}

// parseWorkerIdentity reads a numeric "<uid>:<gid>" (or "<uid>") container
// user. A named or empty user yields ok=false: nothing to chown to.
func parseWorkerIdentity(user string) (uid, gid int, ok bool) {
	uidText, gidText, hasGID := strings.Cut(user, ":")
	uid, err := strconv.Atoi(uidText)
	if err != nil {
		return 0, 0, false
	}
	gid = -1
	if hasGID {
		if gid, err = strconv.Atoi(gidText); err != nil {
			return 0, 0, false
		}
	}
	return uid, gid, true
}

const DefaultWorkerUID = 65532

// DefaultWorkerUmask is the umask (LaunchSpec.WorkerUmask) applied inside a
// sandboxed worker container whenever it runs under DefaultWorkerUID's
// group-write scheme (see EnableWorkerGroupWrite/DisableWorkerGroupWrite in
// internal/workspace): 0002 clears only the "other-write" bit from the
// container's default file-creation mode, so files/directories the worker
// itself creates come back group-writable -- letting factoryd's own later
// host-side operations (its GID matches the container's) still read and,
// where DisableWorkerGroupWrite's own ownership caveat allows it, manage
// them, without granting write access to any unrelated party on the host.
// Exported once here (found via code review: this repo's own cmd/factoryd
// and internal/workflow packages previously each defined this exact string
// literal independently, with nothing enforcing the two stayed in sync) so
// every caller reads the same value.
const DefaultWorkerUmask = "0002"

// ResolveDefaultWorkerIdentity resolves the container identity/umask pair a
// sandboxed launch uses when the caller has not chosen an explicit
// -sandbox-user: user paired with hostGID (factoryd's own primary group --
// sharing it, not reassigning ownership, is what lets a non-root factoryd
// process grant this UID write access at all, see
// internal/workspace.EnableWorkerGroupWrite's own doc comment) as the
// resolved identity, and DefaultWorkerUmask as the umask. A non-empty user is
// returned unchanged with an empty umask: an operator-chosen identity (or,
// on the API/Temporal path, an authenticated caller's own
// RunWorkflowInput.SandboxUser) is a deliberate override this mechanism
// has no business second-guessing, and forcing a umask on a container that
// will run as some other identity entirely (e.g. the pre-existing
// behavior before -sandbox-worker-uid existed, factoryd's own host
// UID:GID) would be a behavior change with no separation benefit to
// justify it.
//
// Shared by both execution paths (cmd/factoryd's own runSandboxWithRetries
// and internal/workflow.Activities' own copy) so this security-relevant
// identity/umask defaulting logic cannot silently diverge between them --
// found via code review: the two previously duplicated this exact
// resolution verbatim, one comment admitting it "mirrors cmd/factoryd's
// own runSandboxWithRetries exactly" instead of calling one shared helper.
func ResolveDefaultWorkerIdentity(user string, workerUID, hostGID int) (resolvedUser, workerUmask string) {
	if user != "" {
		return user, ""
	}
	return fmt.Sprintf("%d:%d", workerUID, hostGID), DefaultWorkerUmask
}

// ValidateWorkerUID rejects the two worker UIDs that would defeat worker
// UID separation entirely: root (0), which LaunchSpec.Validate's own
// validateUser already refuses as a container identity regardless, and
// hostUID, which is exactly the UID separation this flag exists to
// introduce -- a worker container's --user equal to the factoryd host
// process's own UID means files the worker creates are indistinguishable
// from factoryd's own, the gap this whole mechanism closes.
// maxSandboxUID is the largest UID validateUser (below) accepts for a
// container's --user value: strconv.ParseUint(part, 10, 31) rejects anything
// above 2^31-1. ValidateWorkerUID must reject the same range up front (found
// via GitHub Codex App review of PR #62, P2): on a 64-bit host, an
// out-of-range -sandbox-worker-uid otherwise passed this startup check only
// to have every later sandbox launch fail validateUser, after the daemon had
// already started (or a run had already created its durable record and
// isolated worktree) on configuration this function had declared valid.
const maxSandboxUID = 1<<31 - 1

func ValidateWorkerUID(workerUID, hostUID int) error {
	if workerUID <= 0 || workerUID > maxSandboxUID {
		return fmt.Errorf("sandbox worker uid must be a positive non-root UID no greater than %d, got %d", maxSandboxUID, workerUID)
	}
	if workerUID == hostUID {
		return fmt.Errorf("sandbox worker uid %d must differ from the factoryd host process's own UID %d — using the same UID defeats worker UID separation", workerUID, hostUID)
	}
	return nil
}

// validateUser rejects only a root UID -- GID 0 (the root *group*) confers
// no special privilege by itself and must never disqualify an otherwise
// legitimate non-root identity. Found via a real GitHub Codex App review of
// PR #75 (2026-09-09): that PR fixed this exact same UID/GID conflation in
// cmd/factoryd's own validateDefaultSandboxIdentity preflight, but this
// function -- the one LaunchSpec.Validate actually calls before every real
// sandbox launch -- carried the identical bug independently, so a non-root
// UID paired with GID 0 still passed the earlier CLI preflight only to be
// rejected here, after the run record and isolated worktree already
// existed (see this function's own doc comment above maxSandboxUID for
// exactly this failure shape, previously found for a different input).
func validateUser(value string) error {
	parts := strings.Split(value, ":")
	if len(parts) > 2 || value == "" {
		return errors.New("sandbox user must be a numeric non-root UID[:GID]")
	}
	uid, err := strconv.ParseUint(parts[0], 10, 31)
	if err != nil || uid == 0 {
		return errors.New("sandbox user must be a numeric non-root UID[:GID]")
	}
	if len(parts) == 2 {
		if _, err := strconv.ParseUint(parts[1], 10, 31); err != nil {
			return errors.New("sandbox user must be a numeric non-root UID[:GID]")
		}
	}
	return nil
}

// shSingleQuote wraps value in POSIX shell single quotes so it can be
// embedded literally in a `/bin/sh -c` script argument regardless of its
// content: single quotes suppress every other kind of shell expansion, and
// the only character that cannot appear inside them (a literal single quote)
// is escaped by closing the quote, emitting an escaped one, and reopening it
// (the standard '"'"' idiom). Used for LaunchSpec.WorkDir, which -- unlike
// WorkerUmask below -- is factoryd's own trusted configuration, not
// worker-influenced, but gets the same defense-in-depth treatment rather
// than trusting every call site to never pass a path containing a quote or
// other metacharacter.
func shSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// workerUmaskPattern restricts LaunchSpec.WorkerUmask to a plain 3-4 digit
// octal string (e.g. "0002", "022"). This value is interpolated into a
// shell -c argument in DockerCommand, not passed as a discrete argv
// element -- unlike every other user-influenced value in this package, so
// it gets its own, narrower validation rather than reusing an existing
// helper: a value containing shell metacharacters (";", "$(", a quote)
// would otherwise let whatever sets WorkerUmask (daemon-static
// configuration today, see cmd/factoryd's -sandbox-worker-uid; never
// request-supplied) inject arbitrary shell into the worker's own entrypoint.
var workerUmaskPattern = regexp.MustCompile(`^[0-7]{3,4}$`)

func validateWorkerUmask(value string) error {
	if value == "" {
		return nil
	}
	if !workerUmaskPattern.MatchString(value) {
		return errors.New("sandbox worker umask must be a 3-4 digit octal string")
	}
	return nil
}

func reapContainer(cmd *exec.Cmd, dockerBinary, name string, reclaimWorkDir bool) error {
	// Best-effort, and must run BEFORE the SIGKILL/removal below: once the
	// container is gone there is nothing left to exec into. See
	// reclaimContainerWorkDir's own doc comment for why a forced teardown
	// needs this at all -- DockerCommand's own post-command shell epilogue
	// (found via GitHub Codex App review of PR #62, P1, against this exact
	// gap) only ever runs when the wrapped command exits on its own.
	if reclaimWorkDir {
		reclaimContainerWorkDir(dockerBinary, name)
	}
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	_ = cmd.Wait()
	// context.Background(), preserving Run's own existing behavior exactly
	// (unlike ReconcileOrphans' own two call sites below, which pass their
	// real caller context — see ensureRemoved's own doc comment): Run's
	// cancellation is already handled above via SIGKILL+cmd.Wait() before
	// this is ever reached, so there is nothing further here for an outer
	// context to usefully cancel.
	return ensureRemoved(context.Background(), dockerBinary, name)
}

// reclaimContainerWorkDir best-effort reclaims group-writability under
// workerContainerWorkDir via `docker exec`, while the container named name
// is still alive -- for the same reason DockerCommand's own post-command
// shell epilogue does (see LaunchSpec.WorkerUmask's own doc comment), but
// covering the case that epilogue cannot: a forced teardown (this run's own
// timeout, its output-size limit, a log-write failure, or a factoryd crash
// mid-run) kills the wrapped script before it ever reaches its own trailing
// `chmod`, exactly the gap a GitHub Codex App review of PR #62 (P1) found.
// `docker exec` runs as the container's own configured --user (the
// dedicated worker UID) by default, so this needs no privilege the
// container didn't already have. Best-effort and silent by design: a
// container that has already exited, or whose own exec hangs past this
// bounded timeout, leaves the workspace exactly as unreclaimed as it
// already was -- this can only improve on never trying, never regress it,
// so its own failure is never surfaced as this run's failure.
func reclaimContainerWorkDir(dockerBinary, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBinary, "exec", name, "chmod", "-R", "g+rwX", "--", workerContainerWorkDir)
	cmd.Env = dockerClientEnv()
	_ = cmd.Run()
}

// ensureRemoved takes ctx explicitly, not an internal context.Background()
// (found via codex review of PR #37, round 6): ReconcileOrphans' own
// staleness-debounce wait already respects its caller's ctx (see that
// select's own comment), but this — the actual removal step right after —
// used to always build its own 10-second timeout from Background()
// regardless, so a caller cancellation (daemonMain's own signalCtx on
// SIGTERM) still could not interrupt an in-flight removal, and multiple
// unresponsive removals in one scan could each add their own full 10s to
// shutdown. ctx still bounds this to at most 10 seconds even when it
// carries no deadline of its own.
func ensureRemoved(ctx context.Context, dockerBinary, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, dockerBinary, "rm", "-f", name)
	cmd.Env = dockerClientEnv()
	err := cmd.Run()
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("%w: remove container %q timed out: %v", ErrCleanupUnconfirmed, name, ctx.Err())
	}
	return fmt.Errorf("%w: remove container %q: %v", ErrCleanupUnconfirmed, name, err)
}

// dataDirLabel turns an absolute data directory path into a short, safe
// Docker label value (a path can carry characters some label-consuming
// tooling mishandles, and label values have a length limit).
func dataDirLabel(dataDir string) string {
	sum := sha256.Sum256([]byte(dataDir))
	return hex.EncodeToString(sum[:])
}

// WorkerContainerPresentForRun reports whether a non-relay worker container
// still exists for runID under dataDir's own label. Found via review
// (GitHub Codex App, PR #42): ReconcileRelayOrphans calls this before
// quarantining a run whose only orphan it found was its relay, to avoid
// marking that run's durable record HaltConfirmed — meaning "nothing left
// that could still be running" (see quarantineAbandonedRun's own doc
// comment) — while a worker container for the exact same run is still
// present, whether because ReconcileOrphans (which always runs immediately
// before ReconcileRelayOrphans at both of its call sites) hasn't reconciled
// it yet this pass or because its own removal attempt just failed.
func WorkerContainerPresentForRun(ctx context.Context, dockerBinary, dataDir, runID string) (bool, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return false, fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	// One listing per label prefix (docker ANDs --filter flags, so the
	// current and legacy keys cannot share one call); see legacyLabelPrefix.
	for _, prefix := range labelPrefixes {
		listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(listCtx, dockerBinary, "ps", "-a",
			"--filter", "label="+prefix+"data-dir="+dataDirLabel(dataDir),
			"--filter", "label="+prefix+"run="+runID,
			"--format", "{{.Names}}")
		cmd.Env = dockerClientEnv()
		out, err := cmd.Output()
		cancel()
		if err != nil {
			return false, fmt.Errorf("list sandbox containers for run %q: %w", runID, err)
		}
		for _, name := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			// The listing above matches this run's relay container too (it
			// carries the same data-dir/run labels); only a non-relay name
			// counts as a worker container still needing its own reconciliation.
			if name != "" && !strings.HasPrefix(name, "factoryd-relay-container-") {
				return true, nil
			}
		}
	}
	runtimeIDs, err := recordedSandboxContainerIDs(ctx, dockerBinary, dataDir, runID)
	if err != nil {
		return false, err
	}
	return len(runtimeIDs) > 0, nil
}

// orphanCandidates lists, as "container name<TAB>run id" lines, every
// container ReconcileOrphans judges: the ones this package launched for
// dataDir, found by its labels, and the ones a sandbox runtime created for a
// sandbox that a run under dataDir recorded.
func orphanCandidates(ctx context.Context, dockerBinary, dataDir string) ([]string, error) {
	var listed []string
	seen := map[string]bool{}
	for _, prefix := range labelPrefixes {
		listCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cmd := exec.CommandContext(listCtx, dockerBinary, "ps", "-a",
			"--filter", "label="+prefix+"data-dir="+dataDirLabel(dataDir),
			"--format", `{{.Names}}\t{{.Label "`+prefix+`run"}}`)
		cmd.Env = dockerClientEnv()
		o, err := cmd.Output()
		cancel()
		if err != nil {
			return nil, fmt.Errorf("list sandbox containers: %w", err)
		}
		listed = dedupeLines(listed, seen, string(o))
	}
	runtimeLines, err := runtimeSandboxContainers(ctx, dockerBinary, dataDir)
	if err != nil {
		return nil, fmt.Errorf("list sandbox runtime containers: %w", err)
	}
	return dedupeLines(listed, seen, strings.Join(runtimeLines, "\n")), nil
}

// ReconcileOrphans finds containers this package previously launched for
// dataDir (identified by dataDirLabel, so a container labeled for a
// different data directory is never even listed) and removes the ones
// bound to a run whose durable record already reached a terminal state
// (run.StateAccepted/StateHalted/StateQuarantined), plus any non-terminal
// one whose owner heartbeat has gone stale (see ownerHeartbeatStale below)
// — never one that is still non-terminal with a fresh heartbeat, or has no
// record in this data directory at all: the former may belong to a
// still-running process, and the latter is too ambiguous to guess about.
// A terminal run record is treated as sufficient proof the container may
// be removed regardless of what Temporal, or this process's own prior
// invocation, believed happened to it — Temporal reporting a workflow
// terminated is never itself treated as proof the Docker container was
// cleaned up. This is meant to be a conservative inspect/remove protocol,
// not a blanket sweep. Callers run
// this both once at factoryd/daemon startup and periodically thereafter
// (see cmd/factoryd's own reconcileSandboxOrphans), so a crash between a
// prior container's start and its own cleanup — or a container abandoned
// only after this process was already running — doesn't accumulate
// indefinitely either way.
func ReconcileOrphans(ctx context.Context, dockerBinary, dataDir string) ([]string, error) {
	dataDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox data directory: %w", err)
	}
	// One listing per label prefix, each reading the run id from its own
	// prefix's key, merged by whole line; the data-dir hash scoping is
	// identical for both. See legacyLabelPrefix.
	listed, err := orphanCandidates(ctx, dockerBinary, dataDir)
	if err != nil {
		return nil, err
	}
	out := strings.Join(listed, "\n")

	type container struct {
		name  string
		runID string
	}
	var removed []string
	var errs []error
	var staleCandidates []container
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		fields := strings.SplitN(line, "\t", 2)
		name := fields[0]
		// A relay container carries this same buildgate.data-dir
		// label (see RouteSpec.DataDir's own doc comment), so it matches
		// this listing's filter too and must be explicitly excluded here
		// (found via review, GitHub Codex App, PR #42): ensureRemoved below
		// only removes a plain container, never a relay's paired network,
		// and quarantineAbandonedRun below that is unconditional -- it has
		// no equivalent of ReconcileRelayOrphans' own
		// WorkerContainerPresentForRun guard. Left unfiltered, this
		// function could remove a stale relay container on its own and
		// immediately mark its run HaltConfirmed=true, while the run's
		// actual worker container -- reconciled by ReconcileRelayOrphans'
		// own guard for this exact case, but never consulted here -- might
		// still be running.
		if strings.HasPrefix(name, "factoryd-relay-container-") {
			continue
		}
		var runID string
		if len(fields) == 2 {
			runID = fields[1]
		}
		// run.ValidID, not just a non-empty check (found via review): the
		// label is Docker-attacker-influenced in principle (any local
		// process with Docker access could tag its own container with our
		// data-dir label), and run.Load/run.WithLock join it beneath
		// dataDir/"runs" without validating it themselves.
		if runID == "" || !run.ValidID(runID) {
			continue
		}
		record, loadErr := run.Load(dataDir, runID)
		if loadErr != nil {
			continue
		}
		// StateHalted alone is not proof the underlying execution actually
		// stopped (found via review, matching run.Run.HaltConfirmed's own
		// doc comment): only a StateHalted record with HaltConfirmed true
		// is terminal here. A container the daemon still has running for
		// an unconfirmed halt must not be removed out from under it.
		terminal := record.State == run.StateAccepted ||
			record.State == run.StateQuarantined ||
			(record.State == run.StateHalted && record.HaltConfirmed)
		if !terminal {
			// A non-terminal record alone does not distinguish "still
			// legitimately owned by a live sibling invocation against
			// this same data directory" from "abandoned by a process
			// that crashed and will never advance it" (found via
			// review — the intended contract is that an unknown prior
			// container must be stopped, recorded, and quarantined,
			// not silently left alone forever). Only the
			// second case is safe to reclaim, decided by
			// ownerHeartbeatStale, not merely whether the owning OS
			// process still exists (found via review: a long-lived
			// `factoryd serve`/daemon process supervising many runs stays
			// alive long after one specific attempt's own goroutine has
			// finished, including the halted-but-cleanup-unconfirmed case
			// this mechanism exists to protect — process existence alone
			// would then never let that attempt's container be reclaimed).
			if !ownerHeartbeatStale(dataDir, runID) {
				continue
			}
			// Collected, not debounced right here (found via GitHub Codex
			// review of PR #37, round 9): an earlier version of the
			// debounce below waited a full ownerHeartbeatInterval
			// separately for every stale candidate in this scan, so N
			// abandoned runs at startup could each add their own wait,
			// delaying daemonMain's caller from ever accepting real work
			// by N * ownerHeartbeatInterval. Waiting once for the whole
			// batch (below, after this loop) gives every candidate the
			// same one interval of grace instead — no less protective
			// (each one still gets a full interval for its own owner to
			// catch up and refresh), just no longer serialized.
			staleCandidates = append(staleCandidates, container{name: name, runID: runID})
			continue
		}
		// Unconditional, unlike Run's own reclaim calls, which gate on
		// s.WorkerUmask: this reconciliation path only has a bare container
		// name and run id, not the original LaunchSpec, to know whether the
		// separated-worker-UID scheme was even in play for it. Best-effort
		// and silent (reclaimContainerWorkDir's own doc comment) makes this
		// safe to call regardless -- a container that never used that
		// scheme already has factoryd's own UID/GID as owner, so this is an
		// inert no-op for it (found via GitHub Codex App review of PR #62,
		// P1: a crashed factoryd, not just a normal run's own forced
		// teardown, can equally strand a worker-created directory the
		// wrapped script's own post-command epilogue never reached).
		reclaimContainerWorkDir(dockerBinary, name)
		if removeErr := ensureRemoved(ctx, dockerBinary, name); removeErr != nil {
			errs = append(errs, fmt.Errorf("reconcile orphaned container %q (run %q): %w", name, runID, removeErr))
			continue
		}
		removed = append(removed, name)
	}

	if len(staleCandidates) > 0 {
		// See the comment above on why this waits once for the whole
		// batch rather than once per candidate — same debounce contract
		// (a process/host suspend longer than ownerStaleAfter can make a
		// genuinely live attempt's heartbeat marker look stale for the
		// brief window between resume and the owning heartbeat
		// goroutine's next scheduled refresh; requiring staleness to
		// still hold a full ownerHeartbeatInterval later closes that),
		// just applied once instead of per-container.
		select {
		case <-ownerDebounceAfter(ownerHeartbeatInterval):
		case <-ctx.Done():
			for _, c := range staleCandidates {
				errs = append(errs, fmt.Errorf("reconcile abandoned container %q (run %q): staleness debounce wait: %w", c.name, c.runID, ctx.Err()))
			}
			return removed, errors.Join(errs...)
		}
		for _, c := range staleCandidates {
			if !ownerHeartbeatStale(dataDir, c.runID) {
				continue
			}
			// Same reasoning as the terminal-container reclaim call above.
			reclaimContainerWorkDir(dockerBinary, c.name)
			if removeErr := ensureRemoved(ctx, dockerBinary, c.name); removeErr != nil {
				errs = append(errs, fmt.Errorf("reconcile abandoned container %q (run %q): %w", c.name, c.runID, removeErr))
				continue
			}
			if quarantineErr := quarantineAbandonedRun(dataDir, c.runID); quarantineErr != nil {
				errs = append(errs, fmt.Errorf("quarantine abandoned run %q after removing container %q: %w", c.runID, c.name, quarantineErr))
				continue
			}
			removed = append(removed, c.name)
		}
	}
	return removed, errors.Join(errs...)
}

// ownerPIDPath is where the owner heartbeat records this attempt's
// liveness, so a later ReconcileOrphans call — quite possibly from a
// different process entirely — can tell an abandoned container apart from
// one still actively supervised.
func ownerPIDPath(dataDir, runID string) string {
	return filepath.Join(run.Dir(dataDir, runID), "sandbox-owner.pid")
}

// OwnerPID returns the PID the run's owner heartbeat marker names -- the
// factoryd process that launched (or is launching) this run's containers,
// which for a Temporal run is the Worker process executing its Activities.
// ok is false when there is no marker, or it does not hold a positive
// integer: a run that never got as far as a sandbox launch cannot prove who
// owned it, the same conservative default ownerHeartbeatStale uses.
func OwnerPID(dataDir, runID string) (pid int, ok bool) {
	b, err := os.ReadFile(ownerPIDPath(dataDir, runID))
	if err != nil {
		return 0, false
	}
	pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// ownerHeartbeatInterval/ownerStaleAfter are vars, not consts, only so
// tests can shrink them instead of waiting tens of seconds — mirrors
// maxLogBytes.
var (
	ownerHeartbeatInterval = 15 * time.Second
	ownerStaleAfter        = 6 * ownerHeartbeatInterval
)

// ownerDebounceAfter constructs the channel the batch-staleness debounce
// above waits on. A var, like ownerHeartbeatInterval, so a test asserting
// the batch is debounced only once (as opposed to once per candidate) can
// replace real-time waiting with a deterministic, immediately-firing
// channel instead of asserting on wall-clock elapsed time — which is
// inherently flaky on a loaded machine (found via intermittent -race
// failures under concurrent `make verify` runs, never standalone). Other
// tests that rely on the wait actually taking real time (e.g. to let a
// concurrent heartbeat refresh land mid-wait) leave this at its default.
var ownerDebounceAfter = func(d time.Duration) <-chan time.Time { return time.After(d) }

// startOwnerHeartbeat periodically rewrites runID's owner marker for as
// long as this attempt is actually being supervised, and returns a stop
// func Run defers immediately after starting it. Found via review: an
// earlier version recorded only the OS process's PID and treated that
// process's continued existence as proof of ownership — correct for the
// direct one-process-per-run CLI path, but not for a long-lived host (a
// `factoryd serve`/daemon process supervising many runs' attempts in
// separate goroutines): that process stays alive long after one specific
// attempt's own goroutine has finished — including the halted-but-
// cleanup-unconfirmed case this mechanism exists to protect — so
// ReconcileOrphans would treat that attempt's container as still owned
// forever. Tying liveness to a periodically refreshed marker instead ties
// it to the attempt actually still running, in either deployment: the
// marker goes stale exactly when nothing is refreshing it anymore, whether
// because the owning process crashed or because the owning goroutine
// simply returned.
func startOwnerHeartbeat(dataDir, runID string) (stop func()) {
	stopCh := make(chan struct{})
	done := make(chan struct{})
	// Read here, not inside the goroutine: tests shorten this package var,
	// and a heartbeat goroutine another test left running must never read
	// it concurrently with that write (race detector, 2026-09-24).
	interval := ownerHeartbeatInterval
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				_ = writeOwnerHeartbeat(dataDir, runID)
			}
		}
	}()
	return func() {
		close(stopCh)
		<-done
	}
}

// writeOwnerHeartbeat records this attempt as alive right now. Called once
// synchronously before Run starts the container (so even a container that
// exits before the first periodic tick has a marker at all) and then
// periodically by startOwnerHeartbeat's goroutine for as long as Run keeps
// running.
func writeOwnerHeartbeat(dataDir, runID string) error {
	path := ownerPIDPath(dataDir, runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

// ownerHeartbeatStale reports whether runID's owner marker (see
// startOwnerHeartbeat) has gone stale — nothing has refreshed it in over
// ownerStaleAfter. A missing marker (a run that predates this mechanism,
// or one whose owner never got far enough to write it) cannot prove
// abandonment, so this conservatively returns false — the same
// conservative default ReconcileOrphans already uses whenever it cannot
// positively establish a container is safe to remove.
func ownerHeartbeatStale(dataDir, runID string) bool {
	info, err := os.Stat(ownerPIDPath(dataDir, runID))
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) > ownerStaleAfter
}

// quarantineAbandonedRun marks runID quarantined after ReconcileOrphans has
// already confirmed both its owner heartbeat is stale and its abandoned
// container has been removed. Locked the same way every other run.json
// mutator is (run.WithLock), since a live sibling process could in
// principle be reading or writing this same record concurrently even
// though this specific attempt's own owner has gone stale.
func quarantineAbandonedRun(dataDir, runID string) error {
	return run.WithLock(dataDir, runID, func() error {
		record, err := run.Load(dataDir, runID)
		if err != nil {
			return err
		}
		// Found via review: no reason to reclaim if the run reached a
		// terminal state in the window between ReconcileOrphans' first
		// read and this locked re-read (e.g. a slow but very-much-alive
		// owner finished and saved its own terminal state right as its
		// heartbeat happened to already be stale by the time it returned).
		if record.State == run.StateAccepted || record.State == run.StateQuarantined ||
			(record.State == run.StateHalted && record.HaltConfirmed) {
			return nil
		}
		record.State = run.StateQuarantined
		// HaltConfirmed: true — the container this run's owner launched
		// has just been confirmed removed by the caller before this was
		// invoked, and its owner heartbeat is confirmed stale; there is
		// nothing left that could still be running.
		record.HaltConfirmed = true
		return record.Persist(dataDir)
	})
}

// StageFile copies one allow-listed file into a private directory suitable for
// a read-only input mount, avoiding exposure of the source file's parent tree.
// parent must be a host directory already shared with the Docker runtime.
func StageFile(path, parent string) (dir, staged string, cleanup func(), err error) {
	parent, err = filepath.Abs(parent)
	if err != nil {
		return "", "", nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", "", nil, err
	}
	if !info.Mode().IsRegular() {
		return "", "", nil, errors.New("sandbox input must be a regular file")
	}
	in, err := os.Open(path)
	if err != nil {
		return "", "", nil, err
	}
	if err := os.MkdirAll(parent, 0o750); err != nil {
		_ = in.Close()
		return "", "", nil, err
	}
	dir, err = os.MkdirTemp(parent, "factoryd-sandbox-input-")
	if err != nil {
		_ = in.Close()
		return "", "", nil, err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	staged = filepath.Join(dir, filepath.Base(path))
	out, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		_ = os.RemoveAll(dir)
		_ = in.Close()
		return "", "", nil, err
	}
	_, copyErr := io.Copy(out, in)
	closeOutErr := out.Close()
	closeInErr := in.Close()
	if copyErr == nil {
		copyErr = closeOutErr
	}
	if copyErr == nil {
		copyErr = closeInErr
	}
	err = copyErr
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	return dir, staged, func() { _ = os.RemoveAll(dir) }, nil
}

// HarnessSiblingModules is the fixed set of agent/pi/scripts files
// StageSiblingModules may ever copy beside a staged script -- every module
// the harness scripts themselves import from one another (draft_spec.py
// and plan_tickets.py both `import build_app`, and conformity_review.py
// does too), every prompt template they load, and nothing else. An allowlist, not "every .py beside the
// script" (its original, looser rule): scriptDir is
// resolveHarnessScript's own extracted harness cache directory in
// production, always exactly these files, but StageSiblingModules also
// runs against an explicit -build-app-script/-draft-spec-script override
// pointing at an arbitrary checkout directory, where "every .py file in
// this directory" is a far wider, unintended surface to stage into the
// sandbox than this mechanism was ever meant to cover.
var HarnessSiblingModules = map[string]bool{
	"build_app.py":         true,
	"harness_adapters.py":  true, // imported by build_app.py and every model-bound script
	"draft_spec.py":        true,
	"plan_tickets.py":      true,
	"ticket_runner.py":     true,
	"goal_pilot.py":        true,
	"conformity_review.py": true, // imported by combined_review.py
	"code_review.py":       true, // imported by combined_review.py
	"prompt_templates.py":  true, // loads the *.prompt.md templates below
	"round_feedback.py":    true, // imported by build_app.py

	// Run by harness_adapters.py's CopilotAdapter under node, around the
	// Copilot CLI on a chatgpt-codex route.
	"fill_responses_output.mjs": true,

	// Prompt templates, read at import by the script they are named after.
	"build_corrective.agent.prompt.md":                   true,
	"build_corrective.closing.prompt.md":                 true,
	"build_corrective.history.prompt.md":                 true,
	"build_corrective.log.prompt.md":                     true,
	"build_corrective.stuck.prompt.md":                   true,
	"build_corrective.excerpt.prompt.md":                 true,
	"build_corrective.intro.prompt.md":                   true,
	"build_corrective.oracle.prompt.md":                  true,
	"build_corrective.reviewer.prompt.md":                true,
	"build_corrective.verify.prompt.md":                  true,
	"build_escalation.prompt.md":                         true,
	"build_handoff_notes.prompt.md":                      true,
	"build_round_checklist.prompt.md":                    true,
	"code_review.command_outcome_rule.prompt.md":         true,
	"code_review.diff_inline.prompt.md":                  true,
	"code_review.diff_self.prompt.md":                    true,
	"code_review.json_contract.prompt.md":                true,
	"code_review.prompt.md":                              true,
	"code_review.scope_rule.prompt.md":                   true,
	"code_review.severity_rule.prompt.md":                true,
	"combined_review.diff_inline.prompt.md":              true,
	"combined_review.diff_self.prompt.md":                true,
	"combined_review.prompt.md":                          true,
	"draft_acceptance_oracles.criterion_index.prompt.md": true,
	"draft_acceptance_oracles.feedback.prompt.md":        true,
	"draft_acceptance_oracles.go.prompt.md":              true,
	"draft_acceptance_oracles.prior_imports.prompt.md":   true,
	"draft_acceptance_oracles.python.prompt.md":          true,
	"draft_acceptance_oracles.qualification.prompt.md":   true,
	"draft_feedback.prompt.md":                           true,
	"draft_spec.design_guide.prompt.md":                  true,
	"draft_spec.example_check.prompt.md":                 true,
	"draft_spec.previous_draft.prompt.md":                true,
	"draft_spec.prompt.md":                               true,
	"plan_tickets.design_guide.prompt.md":                true,
	"plan_tickets.previous_draft.prompt.md":              true,
	"plan_tickets.prompt.md":                             true,
	"spec_conformity.command_outcome_rule.prompt.md":     true,
	"spec_conformity.diff_inline.prompt.md":              true,
	"spec_conformity.diff_self.prompt.md":                true,
	"spec_conformity.formatting_rule.prompt.md":          true,
	"spec_conformity.json_contract.prompt.md":            true,
	"spec_conformity.prompt.md":                          true,
}

// StageSiblingModules copies scriptDir's own HarnessSiblingModules
// files (other than scriptName itself) into stagedDir, alongside a
// script StageFile already staged there -- the harness scripts import
// each other as plain sibling modules, so the staged directory mounted
// into the sandbox must carry those siblings too, not the one requested
// file alone. Found live on the direct-execution path: the first
// sandboxed spec draft died on ModuleNotFoundError before the model ran
// (draft_spec.py's own `import build_app`); found again on the Temporal
// path, 2026-09-17, the first time the Temporal path's own conformity-
// review Activity (RunReviewStepActivity since M4-K3, previously
// RunSpecConformityReviewActivity) ran a real conformity_review.py
// against a live sandbox -- that Activity's own script-staging block had
// never called this at all, since no Temporal Activity before it had
// ever staged a harness script with a sibling import. Shared by both
// cmd/factoryd's own stageSandboxScript and
// internal/workflow's runSandboxWithRetries so the two paths cannot drift
// on this again.
func StageSiblingModules(scriptDir, scriptName, stagedDir string) error {
	entries, err := os.ReadDir(scriptDir)
	if err != nil {
		return fmt.Errorf("list harness modules beside %s: %w", scriptName, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == scriptName || !entry.Type().IsRegular() || !HarnessSiblingModules[name] {
			continue
		}
		content, err := os.ReadFile(filepath.Join(scriptDir, name))
		if err != nil {
			return fmt.Errorf("read harness module %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(stagedDir, name), content, 0o644); err != nil {
			return fmt.Errorf("stage harness module %s: %w", name, err)
		}
	}
	return nil
}

// ScriptsSHA256 returns a stable sha256 digest over every regular file
// already staged in dir -- a script StageFile just staged, plus whatever
// siblings StageSiblingModules copied beside it. Sorted by name before
// hashing (name then content, zero-byte separated) so the digest neither
// depends on directory read order nor collides two script sets that
// happen to concatenate to the same bytes. Called once per launch, right
// after staging finishes, by cmd/factoryd/sandbox_exec.go and internal/workflow's
// activities, and set onto the resulting Attempt's
// HarnessScriptsSHA256 -- so a run's durable record shows exactly which
// harness script bytes ran it, the same way ImageDigest already records
// which container image did (M4-K1).
func ScriptsSHA256(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("list staged scripts in %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("read staged script %s: %w", name, err)
		}
		h.Write([]byte(name))
		h.Write([]byte{0})
		h.Write(content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// minFreeBytesForLaunch is the free-space floor checkMinFreeBytes enforces
// on the filesystem backing a worker's writable /workspace mount before
// Docker is invoked at all -- a package-level var (not a const) only so a
// test can lower it instead of needing gigabytes of real free space to
// exercise the halt path. 1 GiB is generous above what a normal build/
// verify transcript needs, chosen to trip only on a filesystem already in
// real trouble, the same reasoning maxLogBytes documents for its own
// generous ceiling.
var minFreeBytesForLaunch uint64 = 1 << 30

// checkMinFreeBytes refuses to launch a worker against a workDir whose
// filesystem already has less than minFreeBytesForLaunch free. This is a
// pre-launch guard, not a running quota -- it does nothing to stop a
// worker that starts with plenty of room and fills the disk during its
// own run (found via a real Opus review pass, 2026-09-04); closing that
// half would need a post-attempt check in every caller that retries a
// launch, not just here.
// statfsFreeBytes reports path's filesystem's free space, as (bytes, ok) --
// ok is false when the lookup itself failed (unsupported filesystem, a
// permissions quirk, or the path not existing yet -- DataDir in particular
// has no guarantee of existing before its first write). A package-level
// var, not a direct syscall.Statfs call inline in checkMinFreeBytes, so a
// test can make one specific path report arbitrary free space without
// needing two real, distinct filesystems to prove WorkDir's and DataDir's
// checks are independent (found via a real Codex review comment on this
// same change's own PR: the original test put both temp dirs on the same
// real filesystem, so WorkDir's check -- always run first -- short-circuited
// before DataDir's was ever reached, and deleting the DataDir branch
// entirely would still have passed that test).
var statfsFreeBytes = func(path string) (free uint64, ok bool) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, false
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), true
}

func checkMinFreeBytes(path string) error {
	free, ok := statfsFreeBytes(path)
	if !ok {
		// Fails open deliberately: an unreadable filesystem says nothing
		// about whether space is actually low, and failing every launch
		// closed on an inability to check would be a worse regression
		// than the gap this guard exists to narrow.
		return nil
	}
	if free < minFreeBytesForLaunch {
		return fmt.Errorf("filesystem holding %s has %d bytes free, below the %d-byte minimum required to launch a worker", path, free, minFreeBytesForLaunch)
	}
	return nil
}

// SkipMountVisibilityCheckEnv opts a launch out of verifyWorkDirMountVisibility
// when set to "1". This is an environment variable rather than a new
// -sandbox-* CLI flag deliberately: a real CLI flag needs a definition at
// every one of this repo's flag-definition sites (direct CLI, daemon,
// API-started runs, `supervise`, and the Temporal path's own zero-value
// fallback -- a review found exactly this kind of change landing at 3 of
// 5 sites and silently missing the other two), for an escape valve
// expected to be rare (an operator
// whose worker images have no shell at all, see the "ambiguous" branch in
// verifyWorkDirMountVisibility, or who has already independently confirmed
// their Docker backend's mount behavior and wants to skip the extra
// container's startup cost on every launch). A process-wide env var read
// once here, with no threading through main.go's flag surface, is the
// narrower and safer surface for that.
const SkipMountVisibilityCheckEnv = "SANDBOX_SKIP_MOUNT_VISIBILITY_CHECK"

// mountVisibilityProbePath is the fixed, container-side path WorkDir is
// bind-mounted to for the probe container only -- deliberately distinct
// from workerContainerWorkDir so a probe run can never be confused with a
// real worker launch in `docker ps`/logs, and so the read-only probe mount
// never collides with the real read-write mount a retried launch performs
// moments later at the same container path.
const mountVisibilityProbePath = "/mnt/sandbox-mount-probe"

// mountVisibilityProbeTimeout bounds how long verifyWorkDirMountVisibility
// waits for its own throwaway container. Generous relative to what a plain
// `test -f` against an already-pulled image needs, but still small enough
// that a genuinely hung Docker daemon fails this fast rather than stalling
// the real launch it's meant to protect.
var mountVisibilityProbeTimeout = 20 * time.Second

// mountVisibilityVerified caches, per resolved host WorkDir, that
// verifyWorkDirMountVisibility has already passed once in this process.
// internal/runner.RunWithRetries and its callers invoke sandbox.Run fresh
// on every retry attempt against the same WorkDir -- without this cache,
// a launch that fails for an unrelated reason (the worker's own command
// exiting non-zero, say) would re-pay the probe container's cost on every
// single retry, even though the Docker backend's mount behavior for that
// path cannot have changed between attempts within one process's
// lifetime. Deliberately process-lifetime, not persisted: a long-running
// daemon that later launches against a *different* WorkDir under the same
// parent still gets checked, and nothing here claims to survive a daemon
// restart or a mid-run change to the Docker backend itself.
var mountVisibilityVerified sync.Map

// verifyWorkDirMountVisibility asks the SAME Docker backend that a real
// worker launch is about to use whether it actually shares s.WorkDir into
// its containers at all, before that real launch starts. This exists
// because of a real, reproduced failure mode: on a machine using colima as
// its Docker backend, colima only shares $HOME into its VM by default, not
// /tmp or /private/tmp. A factoryd invocation given a -data-dir/-workspace
// outside $HOME still resolves and bind-mounts what LOOKS like the right
// host path -- docker run accepts the --volume flag without complaint --
// but the container sees an EMPTY directory tree at that mount point (only
// a bind-mounted .git entry made it through, since Git-related mounts
// often resolve to paths already under $HOME). The worker then fails with
// a plain "No such file or directory" for files that genuinely exist on
// the host, several layers removed from the real cause: an operator
// reading that failure has every reason to suspect a factoryd bug or a
// permissions problem, not "the host path isn't shared into the Docker VM
// at all."
//
// The only reliable way to know whether a host path is visible inside a
// container from the CURRENT Docker backend is to ask Docker directly, so
// this writes a marker file into WorkDir on the host, then launches a
// small, fast, throwaway container that bind-mounts WorkDir read-only (at
// a path distinct from the real worker's read-write mount) and checks
// whether that exact marker is visible from inside it. Probing WorkDir
// itself, not a synthetic path, is deliberate: a synthetic probe path
// could pass while the actual path that matters is the one silently
// unshared (some subtree/mount visible, but not the one in question) --
// exactly the failure mode found live. It reuses s.Image, already
// resolved and digest-pinned for this exact run (see Validate), rather
// than pulling a separate probe image: no extra network round-trip, and
// the same image the real worker is about to run from is the only image
// guaranteed present without a pull.
//
// Runs once per resolved WorkDir per process (mountVisibilityVerified) --
// see its own doc comment -- and can be skipped entirely via
// SkipMountVisibilityCheckEnv.
//
// The probe container itself gets the same --read-only/--cap-drop=ALL/
// --security-opt=no-new-privileges hardening the real worker launch does
// (found via an adversarial pass on this exact mechanism): the command it
// runs is fully fixed (a static `test -f <marker>`, the marker itself
// derived from this process's own PID/timestamp, never worker- or
// attacker-influenced) and WorkDir is mounted read-only, so none of these
// flags close an exploit chain that would otherwise exist here -- but this
// codebase applies that hardening to every docker run as blanket policy,
// not per-command risk analysis, and there's no reason for this one launch
// to be the exception.
func verifyWorkDirMountVisibility(ctx context.Context, dockerBinary string, s LaunchSpec) error {
	if os.Getenv(SkipMountVisibilityCheckEnv) == "1" {
		return nil
	}
	if _, done := mountVisibilityVerified.Load(s.WorkDir); done {
		return nil
	}
	marker := fmt.Sprintf(".sandbox-mount-probe-%d-%d", os.Getpid(), time.Now().UnixNano())
	markerHostPath := filepath.Join(s.WorkDir, marker)
	if err := os.WriteFile(markerHostPath, []byte("sandbox-mount-probe\n"), 0o600); err != nil {
		return fmt.Errorf("write mount-visibility probe marker under %s: %w", s.WorkDir, err)
	}
	defer os.Remove(markerHostPath)

	probeCtx, cancel := context.WithTimeout(ctx, mountVisibilityProbeTimeout)
	defer cancel()
	markerContainerPath := mountVisibilityProbePath + "/" + marker
	name := "factory-mount-probe-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	args := []string{
		"run", "--rm", "--name", name,
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--network", "none",
		"--user", s.User,
		"--volume", s.WorkDir + ":" + mountVisibilityProbePath + ":ro",
		"--entrypoint", "/bin/sh",
		s.Image,
		"-c", "test -f " + shSingleQuote(markerContainerPath),
	}
	cmd := exec.CommandContext(probeCtx, dockerBinary, args...)
	cmd.Env = dockerClientEnv()
	_, runErr := cmd.CombinedOutput()
	if runErr == nil {
		mountVisibilityVerified.Store(s.WorkDir, true)
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 {
		// Exit 1 is `test`'s own "file does not exist" -- the shell ran
		// fine, so the image does have a shell, and it genuinely could not
		// see the marker this same process just wrote to the host side of
		// this exact mount. This is the failure this check exists to
		// catch.
		return fmt.Errorf(
			"the Docker backend running %q does not appear to share %s into its containers: "+
				"a probe container bind-mounted this exact path but could not see a marker file "+
				"this process just wrote to it on the host. If your Docker backend is colima, "+
				"check the mounts in ~/.colima/default/colima.yaml -- colima shares only the "+
				"directories listed there (USAGE.md: ~/buildgate read-write, your repositories "+
				"read-only), so a -data-dir/-workspace outside them is invisible to containers "+
				"even though `docker run` accepts the mount without complaint. Move -data-dir "+
				"under ~/buildgate, add this path to colima's mounts, or set %s=1 to skip this check "+
				"if you have already confirmed your setup another way",
			dockerBinary, s.WorkDir, SkipMountVisibilityCheckEnv)
	}
	// Any other failure (the image has no shell at all, "docker" itself
	// isn't reachable, the probe container was killed some other way, ...)
	// is ambiguous: it says nothing conclusive about whether WorkDir is
	// actually visible. Fails OPEN here deliberately, the same reasoning
	// checkMinFreeBytes documents for its own inconclusive case: the real
	// worker launch immediately below runs the same "docker run" against
	// the same backend and will surface its own, unambiguous error if the
	// underlying problem is real (an unreachable daemon, say), while
	// failing the whole launch closed on an inconclusive signal here would
	// also permanently block any legitimate worker image with no shell at
	// all (see LaunchSpec.WorkerUmask's own documented limitation for the
	// same class of image) -- a worse regression than the gap this check
	// exists to narrow. Not cached as a verified success either
	// (mountVisibilityVerified is only written on a confirmed pass above)
	// -- the next launch against this WorkDir tries the probe again rather
	// than silently trusting an inconclusive result forever.
	return nil
}

func validateMountPath(path string, writable bool) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("mount path is not a directory")
	}
	lstat, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if lstat.Mode()&os.ModeSymlink != 0 {
		return errors.New("symlinked mount path is forbidden")
	}
	if writable && info.Mode().Perm()&0o200 == 0 {
		return errors.New("workspace is not writable")
	}
	return nil
}
