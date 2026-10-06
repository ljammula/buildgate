package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"buildgate/internal/requestsubmit"
	wsisolation "buildgate/internal/workspace"

	"buildgate/internal/sandbox"
)

// validateDefaultSandboxIdentity rejects the one host identity that cannot
// safely become Docker's default -- root. Sandboxing is unconditional (no
// host-execution opt-out exists), so every invocation inherits the caller's
// numeric UID:GID so bind-mounted workspaces stay writable; a root-owned
// service must choose a real non-root identity explicitly rather than
// reaching LaunchSpec.Validate only after its run record already exists.
//
// Root is UID 0 specifically -- GID 0 (the root *group*) confers no special
// privilege by itself, so it must never factor into this check. The
// original condition rejected unless uid != 0 AND gid != 0, so a real
// non-root UID paired with GID 0 (a legitimate, unprivileged identity) was
// incorrectly refused as if it were root (found via a 2026-09-05 swarm
// review, confirmed still present 2026-09-08, fixed 2026-09-09).
func validateDefaultSandboxIdentity(configured string, uid, gid int) error {
	if configured != "" || uid != 0 {
		return nil
	}
	return fmt.Errorf("sandboxing cannot default -sandbox-user from the root factoryd process (%d:%d): pass -sandbox-user <non-root UID:GID> for an identity that can write the workspace", uid, gid)
}

// dockerByteSizePattern matches Docker's own --memory/--tmpfs-size value
// syntax: a non-negative integer with an optional b/k/m/g unit suffix,
// case-insensitive (internal/sandbox/docker.go passes these strings
// straight through to Docker, uninterpreted).
// dockerBytes parses a Docker --memory/--tmpfs-size value into a byte
// count, so validateSandboxResourceLimitFlags can reject not just an empty
// string but any value Docker itself would treat as "no limit" — found by
// a second GitHub Codex App review round on PR #52 (P2): the first pass
// only rejected an empty -sandbox-memory/-sandbox-tmpfs-size, so an
// explicit -sandbox-memory=0 (Docker's own documented "unconstrained"
// value) still parsed as "valid" and reached Docker, launching a worker
// with no memory ceiling at all despite an operator's apparent
// configuration.
func dockerBytes(value string) (int64, error) {
	n, err := sandbox.ParseByteSize(value)
	if err != nil {
		return 0, fmt.Errorf("must be a positive integer with an optional b/k/m/g suffix (Docker --memory syntax), got %q", value)
	}
	return n, nil
}

// dockerCPUs parses a Docker --cpus value: a decimal number of CPUs (e.g.
// "0.5", "2"), same syntax internal/sandbox/docker.go forwards unparsed.
// strconv.ParseFloat itself accepts "NaN"/"Inf"/"-Inf" as valid float64
// values (found by a sixth GitHub Codex App review round on this PR, P2):
// NaN in particular passes the caller's own `<= 0` check (NaN compares
// false against everything), so an operator typo like -sandbox-cpus=NaN
// would otherwise reach Docker as a literal, invalid --cpus value instead
// of being rejected at startup.
func dockerCPUs(value string) (float64, error) {
	f, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, fmt.Errorf("must be a finite decimal number (Docker --cpus syntax), got %q", value)
	}
	return f, nil
}

// validateSandboxResourceLimitFlags rejects an operator-supplied sandbox
// resource ceiling that would either fail internal/sandbox.LaunchSpec.Validate
// outright (empty Memory/CPUs/TmpfsSize) or silently launch an
// unbounded worker despite appearing configured (Memory/TmpfsSize/CPUs
// parsing to zero — Docker's own documented meaning for "no limit" on
// each), before any of these ever reaches a durable config value. Found
// missing by two GitHub Codex App review rounds on PR #52 (both P2):
// round one found the empty-string cases (an empty -sandbox-memory/-sandbox-cpus/
// -sandbox-tmpfs-size parsed successfully as a flag and was then silently
// replaced by workflow.Activities' own zero-value fallback — the
// sandboxMemory()/sandboxCPUs()/sandboxTmpfsSize() helpers,
// which exist so an Activities built without these fields at all, every
// pre-existing caller and test, behaves exactly as before); round two
// found that even a non-empty but zero-valued -sandbox-memory/-sandbox-cpus
// (e.g. "0" or "0g") passed round one's check yet still meant "no limit"
// to Docker itself, so an operator's apparently-valid configuration could
// still launch an unbounded worker; round three found a positive but
// too-small -sandbox-memory (below Docker's own documented 6m container
// minimum) passed both prior checks but then broke every subsequent
// sandboxed run at launch time instead of failing loudly once, up front.
// Those Activities-side fallback helpers
// keep their zero-value behavior unchanged; this validates only the
// CLI-supplied flag values themselves, called once per flag set right
// after parsing, at every site that supplies these three values (the
// direct/Temporal/repository-owner path, factoryd daemon, and serve's own
// -sandbox-* pair for API-started runs -- the last two now from
// sessionconfig.Settings rather than their own flags). prefix names the
// flag family in the returned error ("-sandbox") so the message points at
// the actual flag an operator needs to fix.
func validateSandboxResourceLimitFlags(prefix, memory, cpus, tmpfsSize string) error {
	if memory == "" {
		return fmt.Errorf("%s-memory must not be empty", prefix)
	}
	memoryBytes, err := dockerBytes(memory)
	if err != nil {
		return fmt.Errorf("%s-memory %v", prefix, err)
	}
	if memoryBytes <= 0 {
		return fmt.Errorf("%s-memory must be a positive value (Docker treats 0 as unlimited), got %q", prefix, memory)
	}
	// Docker itself refuses to start a container below this ceiling
	// (documented minimum: 6m) -- found by a fifth GitHub Codex App review
	// round on this PR (P2): without this check, a positive but
	// too-small -sandbox-memory (e.g. "1m") passed startup validation but
	// then broke every subsequent sandboxed run at launch time instead of
	// failing loudly once, up front.
	const dockerMinMemoryBytes = 6 * (1 << 20)
	if memoryBytes < dockerMinMemoryBytes {
		return fmt.Errorf("%s-memory must be at least 6m (Docker's own minimum container memory limit), got %q", prefix, memory)
	}
	if cpus == "" {
		return fmt.Errorf("%s-cpus must not be empty", prefix)
	}
	cpuCount, err := dockerCPUs(cpus)
	if err != nil {
		return fmt.Errorf("%s-cpus %v", prefix, err)
	}
	if cpuCount <= 0 {
		return fmt.Errorf("%s-cpus must be greater than zero (Docker treats 0 as unlimited), got %q", prefix, cpus)
	}
	if tmpfsSize == "" {
		return fmt.Errorf("%s-tmpfs-size must not be empty", prefix)
	}
	tmpfsBytes, err := dockerBytes(tmpfsSize)
	if err != nil {
		return fmt.Errorf("%s-tmpfs-size %v", prefix, err)
	}
	if tmpfsBytes <= 0 {
		return fmt.Errorf("%s-tmpfs-size must be a positive value, got %q", prefix, tmpfsSize)
	}
	return nil
}

func pathWithin(parent, candidate string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// stringSetsEqual reports whether a and b hold the same strings, ignoring
// order — used to compare a ticket's declared Allowed-Files:/
// Required-Changed-Files: (which are logically unordered sets of paths)
// between two files, so that two declarations differing only in list order
// are correctly treated as equal, not flagged as a mismatch. nil and an
// empty (non-nil) slice compare equal, matching ticketspec's own "absent
// declaration" convention (a bare key parses to nil, an empty value list
// would be a spec error caught elsewhere).
func stringSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	sortedA := slices.Clone(a)
	sortedB := slices.Clone(b)
	sort.Strings(sortedA)
	sort.Strings(sortedB)
	return slices.Equal(sortedA, sortedB)
}

// canonicalPath resolves path to an absolute, symlink-free form, including
// a path (or trailing component) that doesn't exist on disk yet. Delegates
// to wsisolation.CanonicalPath -- see that function's own doc comment for
// why this exact resolution (Abs, then walk up to the nearest existing
// ancestor for EvalSymlinks) matters: ValidateIsolationMarker's own
// comparisons must use the identical algorithm this package's callers use
// to produce the values a marker is written with, or every comparison
// against a marker's stored (always-canonicalized-via-this-function) path
// silently fails.
func canonicalPath(path string) (string, error) {
	return wsisolation.CanonicalPath(path)
}

// dataDirInsideWorkspace reports whether dataDir's resolved path falls
// inside workspace's -- the one fail-closed check that must keep durable
// run records outside whatever the worker mounts, since Docker
// containment is unconditional. Shared by run_ticket.go's own real,
// pre-run guard and doctor.go's prediction of it (factoryd doctor code
// review, 2026-09-14: two hand-synced copies of the same
// canonicalPath+pathWithin logic risk silently drifting apart). Now a
// thin wrapper around internal/requestsubmit.DataDirInsideWorkspace: that
// package's Submit needs the identical check for `factoryd submit` and the new
// POST /requests console route, so this is a third caller of the same
// single implementation rather than a fourth hand-synced copy. Callers
// own their own error message wording (run_ticket.go's varies on
// whether -sandbox-image was explicit; doctor's does not), so this only
// returns the resolved paths and the boolean, not a formatted error.
func dataDirInsideWorkspace(workspace, dataDir string) (inside bool, workspaceAbs, dataAbs string, err error) {
	return requestsubmit.DataDirInsideWorkspace(workspace, dataDir)
}

// resolveDataDirFromSessionConfig sets *dataDir from the session config's
// data_dir key when -data-dir was not given explicitly on the command line,
// and logs the resolved directory and its source ("-data-dir", "session
// config <path>", or "default") -- the same session-config precedence level
// `factoryd serve`/`factoryd worker` already give -data-dir (via
// applySessionConfigDataDir/applySessionConfig respectively). Shared by
// `factoryd serve`, `factoryd status`, `factoryd watch`, `factoryd submit`
// and `factoryd retry` -- the last two previously ignored the session
// config's data_dir key entirely and only ever reported "-data-dir" or
// "default", so they worked only when the operator's cwd happened to match
// a configured data_dir.
//
// A missing config file or one with no data_dir key is not an error here:
// none of these commands have execution flags that depend on the file
// existing at all (unlike worker's own applySessionConfig, which refuses
// to run with neither a config nor explicit sandbox/relay flags), and the
// "data" default already works standalone.
//
// configPath, when non-empty, is loaded directly (loadConfigForPath) in
// place of the default-path search: `serve`'s own -config flag
// previously had no effect here at all, so an API-started run resolved
// -data-dir from whichever config the default search happened to find,
// not the one actually named on the command line. Callers with no -config
// flag of their own (retry/status/watch today) still pass "" and keep the
// prior default-search-only behavior.
func resolveDataDirFromSessionConfig(flags *flag.FlagSet, dataDir *string, configPath string) error {
	explicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "data-dir" {
			explicit = true
		}
	})
	if explicit {
		log.Printf("data dir: %q (source: -data-dir)", *dataDir)
		return nil
	}
	cfg, path, found, err := loadConfigForPath(configPath)
	if err != nil {
		return fmt.Errorf("session config: %w", err)
	}
	if !found || cfg.DataDir == nil {
		log.Printf("data dir: %q (source: default)", *dataDir)
		return nil
	}
	*dataDir = *cfg.DataDir
	log.Printf("data dir: %q (source: session config %s)", *dataDir, path)
	return nil
}

// applyProjectConfigDefaults fills in flags the caller left unset from
// workspace's committed .factory.yml, one field per flag it
// covers. An explicit flag always wins -- explicit names only the flags
// actually named on this invocation's command line (a real caller builds
// this via flags.Visit on its own *flag.FlagSet; submitRequest, which has
// no FlagSet of its own to Visit, builds the same map directly from its
// two explicit-tracking bools), so a value this function itself just wrote
// never looks explicit to a later check elsewhere in runMainWithReady.
// Missing or absent .factory.yml is not an error; this is a no-op in that
// case.
//
// sandbox_image is deliberately not a config field at all: the API path's
// -api-allowed-sandbox-images allowlist check happens in serve, before
// the run process (and this function) ever reads .factory.yml, so a repo
// config could otherwise name an image serve never approved. Image
// digests belong in the operator's own signed policy bundle, not in a
// repo a run's own agent can edit.
//
// meter-token-ceiling/meter-cost-ceiling-micro-usd are the other
// exception to "explicit always wins": config can only TIGHTEN them, so
// a repo can never raise a ceiling above what the operator set,
// explicitly or by default -- see the tightening checks below, which run
// regardless of flags.Visit. meterTokenBudget/meterCostBudget are the
// already-parsed -meter-token-budget/-meter-cost-budget-micro-usd values:
// a ceiling flag left at its literal zero is not actually unbounded --
// run_ticket.go resolves it to 5x the corresponding budget later -- so
// the tightening comparison must be against that resolved effective
// ceiling, not the raw zero (found via a real Codex review of PR #84,
// round 2: a config value between the raw zero and the true 5x-budget
// default previously looked like a tightening and silently raised the
// effective ceiling instead).
//
// Now a thin wrapper around internal/requestsubmit.ApplyProjectConfigDefaults:
// the new POST /requests console route needs this exact precedence too, and moving
// the real body there means `factoryd submit`, `factoryd worker`/
// run_ticket.go, `factoryd doctor`, and POST /requests all read the
// same logic rather than risking a fourth hand-drifted copy of it.
func applyProjectConfigDefaults(explicit map[string]bool, workspace string, verifyCommand, fastCheckCommand *string, gateCommands map[string]*string, preflightProfile, releaseProtectedPaths *string, testPatterns *[]string, meterTokenCeiling *int, meterCostCeilingMicroUSD *int64, meterTokenBudget int, meterCostBudget int64) error {
	return requestsubmit.ApplyProjectConfigDefaults(explicit, workspace, verifyCommand, fastCheckCommand, gateCommands, preflightProfile, releaseProtectedPaths, testPatterns, meterTokenCeiling, meterCostCeilingMicroUSD, meterTokenBudget, meterCostBudget)
}
