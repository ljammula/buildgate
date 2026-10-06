package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/composeservices"
	"buildgate/internal/modelhost"
	"buildgate/internal/run"
)

// ComposeServicesFileNames lists the four compose project filenames, in
// docker compose's own precedence order (highest first), that
// BeginComposeServicesLifecycle's caller checks for in the target repo's
// base commit. Documented once, here, so cmd/factoryd's own file-selection
// logic and this package's own doc comments never have to restate the
// order independently and risk drifting.
var ComposeServicesFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// composeServicesDataDirLabelKey/composeServicesRunLabelKey are the exact
// "buildgate.data-dir"/"buildgate.run" label keys
// internal/sandbox/docker.go's dataDirLabel and relay.go already stamp on
// every worker/relay container and network -- reused here, not reinvented,
// so composeservices_orphans.go's ownership check filters on the same
// convention every other sandbox resource already uses (see
// BeginComposeServicesLifecycle's own doc comment on why this matters:
// two factoryd installations sharing one Docker daemon must never tear
// down each other's compose projects/networks).
const (
	composeServicesDataDirLabelKey = labelPrefix + "data-dir"
	composeServicesRunLabelKey     = labelPrefix + "run"
)

// sortedKeys returns m's keys in sorted order -- used only to make the
// `docker network create`/synthesized-service `--label`/`labels:` argument
// order deterministic, never for correctness (Docker itself doesn't care
// about label order).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ComposeServicesReadyTimeout bounds how long `docker compose up --wait`
// waits for every service's healthcheck (or, absent one, its own running
// state) before EnsureForAttempt gives up on that attempt. Exported so a
// caller computing a worker's own execution timeout can reserve room for
// it, like RegistryProxyCleanupTimeout.
var ComposeServicesReadyTimeout = 3 * time.Minute

// ComposeServicesCleanupTimeout bounds Cleanup's own teardown call,
// like RegistryProxyCleanupTimeout.
const ComposeServicesCleanupTimeout = 30 * time.Second

// LoadComposeServicesSpecFromGit builds a ComposeServicesSpec by reading
// the target repo's own compose file and .env, if any, from baseSHA via
// `git show` -- never from gitDir's own working tree, so an isolated
// run's already-applied changes (or, on a later attempt, an untrusted
// worker's own edits) can never influence what gets launched, matching
// ComposeServicesSpec.ComposeYAML's own provenance contract. Tries
// ComposeServicesFileNames in precedence order; the first name `git show`
// succeeds on wins. Shared by cmd/factoryd and
// internal/workflow so this file-selection and read logic
// cannot drift between the two the way relay_upstream once did (PR #129).
//
// A baseSHA with none of the four names present is not an error: spec is
// returned with an empty ComposeYAML, which BeginComposeServicesLifecycle
// itself already treats as "no compose file found in the target
// repository" (disabled, not failed).
func LoadComposeServicesSpecFromGit(gitDir, baseSHA string, parseOptions composeservices.Options, synthesizeOptions composeservices.SynthesizeOptions, readyTimeout time.Duration) (ComposeServicesSpec, error) {
	spec := ComposeServicesSpec{
		BaseGitDir:        gitDir,
		BaseSHA:           baseSHA,
		ParseOptions:      parseOptions,
		SynthesizeOptions: synthesizeOptions,
		ReadyTimeout:      readyTimeout,
	}
	for _, name := range ComposeServicesFileNames {
		content, err := gitShowBlobAtCommit(gitDir, baseSHA, name)
		if err != nil {
			continue
		}
		spec.ComposeYAML = content
		break
	}
	if len(spec.ComposeYAML) == 0 {
		return spec, nil
	}
	if envContent, err := gitShowBlobAtCommit(gitDir, baseSHA, ".env"); err == nil {
		spec.EnvFileContent = envContent
	}
	return spec, nil
}

// gitShowBlobAtCommit returns path's content at commit in the repository
// rooted at gitDir, or a non-nil error whenever git itself reports the
// path absent at that commit (an ordinary, expected outcome the caller
// above treats as "try the next name"/"no .env", not a real failure).
func gitShowBlobAtCommit(gitDir, commit, path string) ([]byte, error) {
	out, err := exec.Command("git", "-C", gitDir, "show", commit+":"+path).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ComposeServicesHooks replaces the Docker-backed side effects of one
// compose-services lifecycle. The zero value uses the real implementations;
// only tests set these -- RegistryProxyHooks' own
// pattern, but with one function per underlying `docker`/`docker compose`
// operation rather than a single Start/Cleanup pair: unlike a
// registry proxy (one container, launched by one internal function),
// a compose project's lifecycle genuinely has this many independently
// useful seams (pull once, up per attempt, logs before down, down per
// attempt, teardown-vs-cleanup network ownership), and collapsing them
// into one opaque Start/Cleanup pair here would just move that same
// number of steps inside a single untestable function instead.
type ComposeServicesHooks struct {
	CreateNetwork func(ctx context.Context, dockerBinary, networkName string, labels map[string]string) error
	RemoveNetwork func(ctx context.Context, dockerBinary, networkName string) error
	Pull          func(ctx context.Context, dockerBinary, projectName, composeFile, projectDir string, services []string) error
	ImageDigest   func(ctx context.Context, dockerBinary, image string) (string, error)
	Up            func(ctx context.Context, dockerBinary, projectName, composeFile, projectDir string, waitTimeout time.Duration) error
	Down          func(ctx context.Context, dockerBinary, projectName, composeFile, projectDir string) error
	Logs          func(ctx context.Context, dockerBinary, projectName, composeFile, projectDir, service string) ([]byte, error)
	// ProjectContainersPresent/NetworkPresent back Cleanup's own
	// confirm-before-trusting-exit-status check (list, then
	// check) -- separated from Down/RemoveNetwork above so a test can
	// inject a deterministic answer for "did the removal actually take"
	// without either shelling out to a real Docker CLI or conflating that
	// check with the best-effort removal call itself.
	// ServiceAddresses returns each service's address on the run's compose
	// network, for a worker that reaches them by address; nil asks Docker.
	ServiceAddresses         func(ctx context.Context, dockerBinary, project, network string) (map[string]string, error)
	ProjectContainersPresent func(ctx context.Context, dockerBinary, project string) (bool, error)
	NetworkPresent           func(ctx context.Context, dockerBinary, networkName string) (bool, error)
}

// ComposeServicesSpec configures one run's compose-services lifecycle.
type ComposeServicesSpec struct {
	// BaseGitDir and BaseSHA identify the immutable source used for
	// materializing relative bind inputs. They are host-side lifecycle
	// provenance, never paths or content supplied by the worker.
	BaseGitDir string
	BaseSHA    string
	// ComposeYAML is the raw bytes of the target repo's own compose file at
	// the base commit, or nil/empty if the repo has none of
	// ComposeServicesFileNames. Never a path: mirroring
	// composeservices.ParseFile's own "never touch disk for provenance"
	// contract, this function must never itself decide which host path (if
	// any) is trustworthy to read -- that decision, and the read itself,
	// belongs entirely to the caller (see this package's own doc comment
	// on ComposeServicesFileNames).
	ComposeYAML []byte
	// EnvFileContent is the target repo's own top-level .env content at
	// that same base commit, or nil if it has none -- passed straight
	// through to composeservices.Options.EnvFileContent. Same provenance
	// contract as ComposeYAML.
	EnvFileContent []byte
	// ParseOptions configures composeservices.ParseFile.
	ParseOptions composeservices.Options
	// SynthesizeOptions configures composeservices.Synthesize -- the
	// factoryd-side resource ceilings, never anything from the target
	// repo's own file.
	SynthesizeOptions composeservices.SynthesizeOptions
	// ReadyTimeout overrides ComposeServicesReadyTimeout for this run's own
	// EnsureForAttempt calls; zero means use the package default.
	ReadyTimeout time.Duration
	// WorkerEnvironment is an operator-controlled, host-side mapping for
	// application-specific service endpoints (for example PSQL_URL or
	// REDIS_URL). It is deliberately not part of RunWorkflowInput; callers
	// populate it from session configuration on the worker that launches
	// the sidecars.
	WorkerEnvironment map[string]string
	// Phase namespaces this lifecycle's own evidence (services.json and
	// TeardownAttempt's per-service logs) from any other phase's lifecycle
	// sharing the same dataDir/runID -- e.g. "build", "verify",
	// "full_suite_verify", or a named gate's own check name. internal/
	// workflow's Activities construct a fresh ComposeServicesLifecycle per
	// phase against the same run, so leaving this empty (a caller that only ever creates one compose lifecycle per run)
	// would otherwise let a later phase's report overwrite an earlier
	// phase's -- see composeServicesReportPath's own doc comment.
	Phase string
}

// ComposeServiceRecord is one successfully-launched service's audit-safe
// facts, persisted into services.json and used to compute this run's
// BG_SERVICE_* worker environment (see cmd/factoryd's own worker-launch
// wiring).
type ComposeServiceRecord struct {
	Name string `json:"name"`
	// Alias is the DNS name other services -- and, once attached, the
	// worker -- reach this service by on the shared per-run network:
	// always equal to Name, since this package's synthesized project never
	// sets container_name or a networks.aliases override (both are
	// ParseFile-rejected fields), so compose's own default network alias
	// (the service name itself) is always what's actually reachable.
	// Recorded explicitly anyway, rather than left for a reader to
	// re-derive from Name, so services.json stays a complete, self-
	// contained record of what a worker could rely on even if that
	// default ever changed.
	Alias string `json:"alias"`
	// Port is the container-network port a client should reach this
	// service's Alias on, or 0 when neither the service's own compose file
	// declared a `ports:` mapping nor set the "x-bg-service-port"
	// extension -- see aliasReachablePort for how it's derived, and
	// ApplyToWorkerLaunch for where it becomes BG_SERVICE_<NAME>_PORT.
	// Recorded here (rather than left for a services.json reader to
	// re-derive from the compose file) so evidence reflects the actual
	// port used even when it came from the override, not just the
	// `ports:`-derived default.
	Port   int    `json:"port,omitempty"`
	Image  string `json:"image"`
	Digest string `json:"digest,omitempty"`
}

// ComposeAttemptRecord is one worker attempt's own compose up/down timing,
// persisted into services.json alongside the run-level facts above.
type ComposeAttemptRecord struct {
	UpDuration   string `json:"up_duration,omitempty"`
	UpError      string `json:"up_error,omitempty"`
	DownDuration string `json:"down_duration,omitempty"`
	DownError    string `json:"down_error,omitempty"`
}

// ComposeServicesReport is the full audit record BeginComposeServicesLifecycle
// and every later EnsureForAttempt/TeardownAttempt call persist to
// services.json under this run's own compose directory -- the one place an
// operator (or a later run's own troubleshooting) can see why compose
// services did or didn't come up, without re-deriving it from log scraping.
type ComposeServicesReport struct {
	Enabled        bool                   `json:"enabled"`
	DisabledReason string                 `json:"disabled_reason,omitempty"`
	NetworkName    string                 `json:"network_name,omitempty"`
	Services       []ComposeServiceRecord `json:"services,omitempty"`
	// Skipped/Rejected carry composeservices.ParseFile's own per-service
	// reasons through verbatim, so services.json remains the single place
	// recording every reason a service didn't launch, not only the
	// whole-run-disable ones.
	Skipped  []composeservices.Rejection `json:"skipped,omitempty"`
	Rejected []composeservices.Rejection `json:"rejected,omitempty"`
	// Forwards is BG_COMPOSE_FORWARDS as a list: each "<host
	// port>=<service>:<container port>" the worker's loopback forwards.
	Forwards []string                         `json:"forwards,omitempty"`
	Attempts map[string]*ComposeAttemptRecord `json:"attempts,omitempty"`
}

// ComposeServicesLifecycle owns exactly one factory-owned compose project
// for the lifetime of one *run*, like RegistryProxyLifecycle's own
// one-per-run contract. Unlike it, a compose
// project is torn down and re-launched fresh on EVERY attempt (not once
// and reused): a project's services carry no run-scoped in-memory budget
// the way a relay does, and a target repo's own test suite is far more
// likely to assume a clean-state service (an empty database, an empty
// queue) at the start of every attempt than a relay's own credential
// limiters ever were.
type ComposeServicesLifecycle struct {
	// byAddress is set for a worker launched through a sandbox runtime;
	// addresses holds the current attempt's services (composeservices_address.go).
	byAddress    bool
	addresses    map[string]string
	dockerBinary string
	runID        string
	dataDir      string
	// phase mirrors ComposeServicesSpec.Phase -- see its own doc comment.
	// Carried on the lifecycle itself, not just at construction time,
	// because TeardownAttempt and recordAttempt both need it on every
	// later call, not only at Begin.
	phase string
	hooks ComposeServicesHooks

	disabled       bool
	disabledReason string

	networkName       string
	composeDir        string
	composeFilePath   string
	bindInputDir      string
	readyTimeout      time.Duration
	workerEnvironment []string
	forwards          []string

	// services is the final, launch-ready set: composeservices.ParseFile's
	// own allow-listed services, minus any whose depends_on could not be
	// resolved after build-skips were applied (see
	// BeginComposeServicesLifecycle's own doc comment) -- exactly what was
	// synthesized into composeFilePath and what TeardownAttempt captures
	// logs for.
	services []composeservices.ServiceSpec

	lastAttempt int
}

// composeServicesReportPath is where BeginComposeServicesLifecycle and
// every later call persist this run's ComposeServicesReport. phase (see
// ComposeServicesSpec.Phase's own doc comment) namespaces the filename so
// multiple phases sharing one dataDir/runID each keep their own report
// rather than the last phase to write silently overwriting every earlier
// one; "" (no phase given) keeps the original, unnamespaced "services.json"
// so a single-phase caller's evidence path never changes.
func composeServicesReportPath(dataDir, runID, phase string) string {
	name := "services.json"
	if phase != "" {
		name = "services." + phase + ".json"
	}
	return filepath.Join(run.Dir(dataDir, runID), "compose", name)
}

// composeServicesAttemptLogDir is where TeardownAttempt writes this
// attempt's per-service compose logs -- namespaced by phase for the same
// reason as composeServicesReportPath above, so two phases' own attempt-1
// logs (each numbered from 1 independently, see
// ComposeServicesLifecycle's own doc comment on why attempts restart per
// phase) never collide on disk either.
func composeServicesAttemptLogDir(dataDir, runID string, attempt int, phase string) string {
	dir := filepath.Join(run.Dir(dataDir, runID), fmt.Sprintf("attempt-%d", attempt), "compose")
	if phase != "" {
		dir = filepath.Join(dir, phase)
	}
	return dir
}

// composeServicesProjectDir is where the synthesized compose project file
// (and nothing else -- see Synthesize's own round-trip test for why an
// empty, dedicated project directory matters) is written for this run.
func composeServicesProjectDir(dataDir, runID string) string {
	return filepath.Join(run.Dir(dataDir, runID), "compose")
}

// composeServicesNetworkName derives this run's dedicated, --internal
// compose-services network name from its runID -- one network per run,
// exactly like relayNetworkName/registryProxyNetworkName's own per-run
// derivation.
func composeServicesNetworkName(runID string) string {
	return "bg-compose-" + runID
}

// composeServicesProjectName derives this attempt's own compose project
// name. The runID+attempt pairing (not runID alone) is load-bearing: it's
// what lets EnsureForAttempt tear down a leftover previous attempt's
// project unambiguously even if that attempt's own TeardownAttempt never
// ran (a worker panic, a process crash) -- see EnsureForAttempt's own doc
// comment.
//
// composeServicesProjectSlug, not runID itself, supplies the identifier:
// Docker Compose project names are restricted to lowercase letters,
// digits, dashes, and underscores, and must start with a letter or digit
// (https://docs.docker.com/compose/how-tos/project-name/), while a run ID
// may contain spaces, colons, uppercase letters, or "/" (see
// docker_test.go's own TestLaunchSpecAcceptsRunIDsFactorydAlreadyGenerates)
// -- interpolating one of those directly here would make every `docker
// compose -p` call for this attempt fail before any service starts.
func composeServicesProjectName(runID string, attempt int) string {
	return fmt.Sprintf("bg-%s-a%d", composeServicesProjectSlug(runID), attempt)
}

// composeServicesProjectSlugSafe matches exactly what Docker Compose itself
// requires of a project name (see composeServicesProjectName's own doc
// comment): if runID already satisfies it, composeServicesProjectSlug
// returns runID completely unchanged, so the overwhelmingly common case (a
// runID that already looks like this) never gains a needless hash suffix.
var composeServicesProjectSlugSafe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// composeServicesProjectSlugUnsafeChar matches every character Docker
// Compose's own project-name rule disallows, for sanitizing a runID that
// fails composeServicesProjectSlugSafe.
var composeServicesProjectSlugUnsafeChar = regexp.MustCompile(`[^a-z0-9_-]`)

// composeServicesProjectSlug derives a Docker Compose-safe project-name
// identifier from runID -- the real runID is never lost: it's still
// recorded verbatim in this run's own services.json and, since this fix,
// stamped as the "buildgate.run" label on every synthesized
// service (see BeginComposeServicesLifecycle), which is what orphan
// reconciliation actually keys off rather than trying to parse it back out
// of a project name (see composeservices_orphans.go).
//
// A runID that already satisfies Compose's own rule is returned unchanged
// -- no hash suffix -- so the common case stays exactly as readable as
// before this fix. Only a runID that needs sanitizing gets one, appended
// specifically to keep two different runIDs that happen to sanitize to the
// same string from colliding on one Compose project name (e.g. "a/b" and
// literal "a-b" would otherwise both become "a-b").
func composeServicesProjectSlug(runID string) string {
	if composeServicesProjectSlugSafe.MatchString(runID) {
		return runID
	}
	sanitized := strings.ToLower(composeServicesProjectSlugUnsafeChar.ReplaceAllString(runID, "-"))
	sanitized = strings.Trim(sanitized, "-_")
	if sanitized == "" {
		sanitized = "run"
	}
	first := sanitized[0]
	if (first < 'a' || first > 'z') && (first < '0' || first > '9') {
		sanitized = "r" + sanitized
	}
	sum := sha256.Sum256([]byte(runID))
	return sanitized + "-" + hex.EncodeToString(sum[:])[:8]
}

// disabledReport writes reason into services.json and returns a lifecycle
// in the disabled state -- the non-error outcome for a run whose target
// repo has no compose file (a rejected one is rejectedReport's error
// instead). Every later lifecycle method is a documented no-op against it.
func disabledReport(dockerBinary, runID, dataDir, phase, reason string) (*ComposeServicesLifecycle, error) {
	report := ComposeServicesReport{Enabled: false, DisabledReason: reason}
	if err := writeComposeServicesReport(dataDir, runID, phase, &report); err != nil {
		return nil, fmt.Errorf("record disabled compose services reason: %w", err)
	}
	return &ComposeServicesLifecycle{dockerBinary: dockerBinary, runID: runID, dataDir: dataDir, phase: phase, disabled: true, disabledReason: reason}, nil
}

// ErrComposeServicesRejected marks a run whose target repo HAS a compose
// file that this package refuses to launch as-is (a service ParseFile
// rejects, a dangling depends_on, two services producing the same worker
// environment variable). The run must halt before its build: launching
// none of the services would let every "service unreachable" failure count
// against the code, and launching only the acceptable ones is not offered
// either (see BeginComposeServicesLifecycle). The wrapping error names each
// rejected service and its reason.
var ErrComposeServicesRejected = errors.New("compose file rejected")

// rejectedReport records reason into services.json, like disabledReport,
// then returns ErrComposeServicesRejected wrapped with that reason and the
// operator's fix.
func rejectedReport(dataDir, runID, phase, reason string, rejected, skipped []composeservices.Rejection) error {
	report := ComposeServicesReport{Enabled: false, DisabledReason: reason, Rejected: rejected, Skipped: skipped}
	if err := writeComposeServicesReport(dataDir, runID, phase, &report); err != nil {
		return fmt.Errorf("record rejected compose services reason: %w", err)
	}
	return fmt.Errorf("%w: %s; no compose service was launched and the build did not start -- fix the target repo's compose file (or the operator's compose_services_* settings) and retry", ErrComposeServicesRejected, reason)
}

// composeServicesGateName is the host-global lock
// AcquireComposeServicesGate takes (internal/modelhost.AcquireNamed).
const composeServicesGateName = "compose-services"

// AcquireComposeServicesGate takes one of concurrency host-wide slots for a
// run whose compose file will launch sidecars, waiting (onWait reports each
// new holder) until one frees or ctx ends. daemon, worker and `factoryd
// run` are separate processes over possibly separate data dirs, and two
// sidecar stacks plus two workers rarely fit one Docker VM, so the slot is
// a flock under the operator's home, not in a data dir.
//
// A run takes it once, before any phase and before its own timeout starts,
// and holds it until the run ends: taken per phase inside a Temporal
// Activity, the wait counted against that Activity's own timeout and a
// contended slot became a halt instead of a delay. A nil Handle with a nil
// error means no slot applies: concurrency <= 0, no compose file, a file
// that launches no service, or a file BeginComposeServicesLifecycle will
// reject -- that run halts at its build rather than queueing first. ctx
// bounds the wait; an expired wait names the holder.
func AcquireComposeServicesGate(ctx context.Context, spec ComposeServicesSpec, runID string, concurrency int, onWait func(holder string)) (*modelhost.Handle, error) {
	if concurrency <= 0 || len(spec.ComposeYAML) == 0 {
		return nil, nil
	}
	if verdict, err := ValidateComposeServices(spec); err != nil || verdict.Reason != "" || len(verdict.Services) == 0 {
		return nil, nil
	}
	return AcquireComposeServicesSlot(ctx, runID, concurrency, onWait)
}

// AcquireComposeServicesSlot takes the host-wide compose sidecars slot
// unconditionally -- AcquireComposeServicesGate's slot without its
// launches-anything check, for a caller that cannot rebuild the run's own
// compose options (the daemon's recovery of a crashed submission) and so
// must assume sidecars. A wait that ends names the holder, and says
// whether it timed out or was canceled.
func AcquireComposeServicesSlot(ctx context.Context, runID string, concurrency int, onWait func(holder string)) (*modelhost.Handle, error) {
	holder := "another run"
	handle, err := modelhost.AcquireNamed(ctx, composeServicesGateName, runID, concurrency, func(h string) {
		holder = h
		if onWait != nil {
			onWait(h)
		}
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return nil, fmt.Errorf("timed out waiting for the host-wide compose sidecar slot (compose_services_concurrency) held by %s: %w", holder, err)
	case errors.Is(err, context.Canceled):
		return nil, fmt.Errorf("canceled while waiting for the host-wide compose sidecar slot held by %s: %w", holder, err)
	}
	return handle, err
}

// ComposeServicesVerdict is ValidateComposeServices' result: what a run
// would launch, or why it would be rejected.
type ComposeServicesVerdict struct {
	// Services are the launchable services, with depends_on edges into a
	// build-skipped service already removed. Empty when Reason is set.
	Services          []composeservices.ServiceSpec
	Rejected          []composeservices.Rejection
	Skipped           []composeservices.Rejection
	WorkerEnvironment []string
	// Forwards: see composeServicesForwards.
	Forwards []string
	// Reason is non-empty when the compose file must be rejected as a
	// whole (see BeginComposeServicesLifecycle's rejection rules).
	Reason string
}

// ValidateComposeServices applies every check BeginComposeServicesLifecycle
// runs before touching Docker -- ParseFile, the dangling-depends_on check,
// the BG_SERVICE_* collision check -- so `factoryd doctor -target-repo`
// reaches the same verdict a run would. spec.ComposeYAML must be
// non-empty; the error is only ParseFile's own non-content failure.
func ValidateComposeServices(spec ComposeServicesSpec) (ComposeServicesVerdict, error) {
	// ReservedWorkerAliases threaded in here, not left to a caller-supplied
	// spec.ParseOptions.ReservedAliases: composeservices can't import this
	// package's own reserved-alias constants directly (this package
	// already imports composeservices, so the reverse would cycle -- see
	// composeservices.Options.ReservedAliases' own doc comment), and this
	// is the one call site that closes that gap.
	parseOptions := spec.ParseOptions
	parseOptions.ReservedAliases = append(append([]string(nil), parseOptions.ReservedAliases...), ReservedWorkerAliases...)
	// The ceiling a service's own mem_limit is checked against is the
	// limit Synthesize applies, never a separately configured value.
	parseOptions.MemoryCeiling = spec.SynthesizeOptions.MemoryLimit
	services, rejected, skipped, err := composeservices.ParseFile(spec.ComposeYAML, parseOptions)
	if err != nil {
		return ComposeServicesVerdict{}, fmt.Errorf("parse compose file: %w", err)
	}
	verdict := ComposeServicesVerdict{Rejected: rejected, Skipped: skipped}
	if len(rejected) != 0 {
		reasons := make([]string, len(rejected))
		for i, r := range rejected {
			reasons[i] = r.String()
		}
		verdict.Reason = strings.Join(reasons, "; ")
		return verdict, nil
	}
	workerEnvironment, workerEnvErr := validateComposeServicesWorkerEnvironment(spec.WorkerEnvironment)
	if workerEnvErr != nil {
		verdict.Reason = workerEnvErr.Error()
		return verdict, nil
	}

	allowed := make(map[string]bool, len(services))
	skippedNames := make(map[string]bool, len(skipped))
	for _, s := range services {
		allowed[s.Name] = true
	}
	for _, s := range skipped {
		skippedNames[s.Service] = true
	}
	launchable := make([]composeservices.ServiceSpec, len(services))
	for i, s := range services {
		kept := make([]composeservices.ServiceDependency, 0, len(s.DependsOn))
		for _, dep := range s.DependsOn {
			switch {
			case allowed[dep.Name]:
				kept = append(kept, dep)
			case skippedNames[dep.Name]:
				// Dropped, not rejected: a build-skipped dependency
				// target is absent from the synthesized file entirely
				// (see BeginComposeServicesLifecycle's doc comment).
			default:
				verdict.Reason = fmt.Sprintf("service %q depends on %q, which is neither an allowed nor a skipped service", s.Name, dep.Name)
				return verdict, nil
			}
		}
		s.DependsOn = kept
		launchable[i] = s
	}

	// Every launchable service emits BG_SERVICE_<NAME> (always) and
	// BG_SERVICE_<NAME>_PORT (when it has a reachable port at all -- see
	// aliasReachablePort). Two distinct service names can produce the
	// same variable name in two ways: (a) ComposeServicesEnvVarName's own
	// normalization collapses two different names to the same suffix
	// (e.g. "event-bus" and "event_bus" both become EVENT_BUS -- a
	// pre-existing risk this loop now also catches, not something new
	// here), or (b) new in this package: one service's bare alias
	// variable collides with a DIFFERENT service's derived _PORT
	// variable (e.g. "db" with a port, alongside a second service
	// literally named "db-port" -- BG_SERVICE_DB_PORT would then mean
	// two different things to whatever reads the worker's environment,
	// and a process environment cannot expose both). Caught here, before
	// any Docker calls, and rejects the whole run with the exact
	// colliding names rather than silently letting one clobber the
	// other in ApplyToWorkerLaunch's env slice.
	envVarOwner := make(map[string]string, len(launchable)*2)
	for _, s := range launchable {
		alias := ComposeServicesEnvVarName(s.Name)
		if owner, exists := envVarOwner[alias]; exists {
			verdict.Reason = fmt.Sprintf("services %q and %q both produce the worker environment variable %s", owner, s.Name, alias)
			return verdict, nil
		}
		envVarOwner[alias] = s.Name
		if aliasReachablePort(s) == 0 {
			continue
		}
		portVar := alias + "_PORT"
		if owner, exists := envVarOwner[portVar]; exists {
			verdict.Reason = fmt.Sprintf("services %q and %q both produce the worker environment variable %s", owner, s.Name, portVar)
			return verdict, nil
		}
		envVarOwner[portVar] = s.Name
	}
	forwards, reason := composeServicesForwards(launchable)
	if reason != "" {
		verdict.Reason = reason
		return verdict, nil
	}
	for _, env := range workerEnvironment {
		key := strings.SplitN(env, "=", 2)[0]
		if owner, exists := envVarOwner[key]; exists {
			verdict.Reason = fmt.Sprintf("operator Compose worker environment variable %s collides with service %q", key, owner)
			return verdict, nil
		}
	}
	verdict.Services = launchable
	verdict.WorkerEnvironment = workerEnvironment
	verdict.Forwards = forwards
	return verdict, nil
}

// composeServicesForwards lists each launchable service's published TCP
// ports as "<host port>=<service>:<container port>", sorted by host port,
// or returns why two services cannot share the worker's loopback: the same
// host port published twice, which `docker compose up` refuses too.
func composeServicesForwards(services []composeservices.ServiceSpec) ([]string, string) {
	owner := map[int]string{}
	target := map[int]string{}
	for _, s := range services {
		for _, p := range s.PublishedPorts {
			if other, taken := owner[p.Host]; taken {
				return nil, fmt.Sprintf("services %q and %q both publish port %d; the worker's localhost can forward it to only one", other, s.Name, p.Host)
			}
			owner[p.Host] = s.Name
			target[p.Host] = s.Name + ":" + strconv.Itoa(p.Target)
		}
	}
	hosts := make([]int, 0, len(owner))
	for host := range owner {
		hosts = append(hosts, host)
	}
	sort.Ints(hosts)
	out := make([]string, len(hosts))
	for i, host := range hosts {
		out[i] = strconv.Itoa(host) + "=" + target[host]
	}
	return out, ""
}

// BeginComposeServicesLifecycle validates and, unless disabled, prepares
// this run's compose project: parses spec.ComposeYAML through
// composeservices.ParseFile, applies the default-on disable rule (see
// below), and -- only when nothing disabled the run -- creates the
// dedicated internal network, synthesizes the project file, pulls the
// images when any is missing locally, and records each image's resolved
// digest.
//
// A target repo with no compose file at all under any of
// ComposeServicesFileNames disables compose services for the run; that is
// not an error. Each of these instead rejects the ENTIRE run with an error
// wrapping ErrComposeServicesRejected (no partial launch of only the
// services that were fine):
//   - ParseFile returns any rejected services or a whole-file rejection.
//   - after removing depends_on edges into a build-skipped service (the
//     one case ParseFile itself only skips, not rejects -- see its own
//     doc comment), any remaining service's depends_on still names
//     something ParseFile never returned in services at all. ParseFile
//     cannot check this itself: it has no view of what got skipped
//     file-wide, only of one service at a time.
//   - two services produce the same worker environment variable.
//
// Both the disable and every rejection reason are recorded into
// services.json (see ComposeServicesReport).
func BeginComposeServicesLifecycle(spec ComposeServicesSpec, dockerBinary, runID, dataDir string, hooks ComposeServicesHooks) (*ComposeServicesLifecycle, error) {
	if len(spec.ComposeYAML) == 0 {
		return disabledReport(dockerBinary, runID, dataDir, spec.Phase, "no compose file found in the target repository")
	}

	verdict, err := ValidateComposeServices(spec)
	if err != nil {
		return nil, err
	}
	if verdict.Reason != "" {
		return nil, rejectedReport(dataDir, runID, spec.Phase, verdict.Reason, verdict.Rejected, verdict.Skipped)
	}
	launchable, skipped := verdict.Services, verdict.Skipped

	// ownershipLabels is stamped on both the network (below) and every
	// synthesized service (via SynthesizeOptions.Labels below) -- the
	// data-dir/run pair is what lets composeservices_orphans.go confirm
	// which installation, and which run, a project/network actually
	// belongs to without parsing either back out of a name (see
	// composeServicesProjectName's own doc comment).
	ownershipLabels := map[string]string{
		labelKey("owner"):              "factoryd",
		labelKey("compose-services"):   "true",
		composeServicesDataDirLabelKey: dataDirLabel(dataDir),
		composeServicesRunLabelKey:     runID,
	}

	networkName := composeServicesNetworkName(runID)
	createNetwork := hooks.CreateNetwork
	if createNetwork == nil {
		createNetwork = composeServicesCreateNetwork
	}
	if err := createNetwork(context.Background(), dockerBinary, networkName, ownershipLabels); err != nil {
		return nil, fmt.Errorf("create compose services network: %w", err)
	}
	// From here on, every remaining step (synthesize, write the project
	// file, pull, record the report) can fail before a *ComposeServicesLifecycle
	// is ever handed back to the caller -- and a caller with no lifecycle
	// in hand has nothing to call Cleanup on. Without this rollback the
	// network above would leak on any such failure, and because its name
	// is fixed to this runID (composeServicesNetworkName), a retry of the
	// same run (a Temporal activity retry, in particular) would then fail
	// immediately trying to create it again. begunSuccessfully is set just
	// before Begin's own successful return below, the only case this
	// rollback must not run.
	begunSuccessfully := false
	removeNetwork := hooks.RemoveNetwork
	if removeNetwork == nil {
		removeNetwork = composeServicesRemoveNetwork
	}
	defer func() {
		if !begunSuccessfully {
			_ = removeNetwork(context.Background(), dockerBinary, networkName)
			if projectDir := composeServicesProjectDir(dataDir, runID); projectDir != "" {
				_ = os.RemoveAll(filepath.Join(projectDir, "bind-inputs"))
			}
		}
	}()

	// synthOpts is spec.SynthesizeOptions plus this run's own ownership
	// labels -- spec is passed by value into Begin, so spec.SynthesizeOptions
	// is already this call's own private copy and safe to extend here
	// without mutating anything the caller holds.
	projectDir := composeServicesProjectDir(dataDir, runID)
	if err := os.MkdirAll(projectDir, 0o750); err != nil {
		return nil, fmt.Errorf("create compose project directory: %w", err)
	}
	launchable, bindInputDir, err := materializeComposeBindInputs(launchable, spec.BaseGitDir, spec.BaseSHA, filepath.Join(projectDir, "bind-inputs"))
	if err != nil {
		return nil, fmt.Errorf("materialize compose bind inputs: %w", err)
	}
	synthOpts := spec.SynthesizeOptions
	synthOpts.Labels = ownershipLabels
	synthOpts.BindSourceDir = bindInputDir
	composeYAML, err := composeservices.Synthesize(launchable, networkName, synthOpts)
	if err != nil {
		return nil, fmt.Errorf("synthesize compose project: %w", err)
	}
	composeFilePath := filepath.Join(projectDir, "compose.yml")
	if err := os.WriteFile(composeFilePath, composeYAML, 0o640); err != nil {
		return nil, fmt.Errorf("write synthesized compose file: %w", err)
	}

	digest := hooks.ImageDigest
	if digest == nil {
		digest = composeServicesImageDigest
	}
	// Pull only when an image is missing locally -- compose's own default
	// `up` policy. Begin runs once per phase (build, verify, full suite,
	// each gate), and pulling every time multiplied registry requests
	// (Docker Hub rate-limits anonymous pulls, so a later phase was the
	// likely one to fail) and let a moving tag change image between two
	// phases of one run. An image with no registry digest (e.g. built
	// locally) also counts as missing. `docker pull <image>` refreshes a
	// moving tag deliberately.
	// Only the missing services are pulled: pulling the whole project would
	// refresh images already present, which is exactly what must not move
	// mid-run.
	var missing []string
	for _, s := range launchable {
		if _, err := digest(context.Background(), dockerBinary, s.Image); err != nil {
			missing = append(missing, s.Name)
		}
	}
	if len(missing) > 0 {
		pull := hooks.Pull
		if pull == nil {
			pull = composeServicesPull
		}
		pullProjectName := "bg-" + composeServicesProjectSlug(runID)
		if err := pull(context.Background(), dockerBinary, pullProjectName, composeFilePath, projectDir, missing); err != nil {
			return nil, fmt.Errorf("pull compose images: %w", err)
		}
	}

	records := make([]ComposeServiceRecord, len(launchable))
	for i, s := range launchable {
		imageDigest, digestErr := digest(context.Background(), dockerBinary, s.Image)
		if digestErr != nil {
			imageDigest = ""
		}
		records[i] = ComposeServiceRecord{Name: s.Name, Alias: s.Name, Port: aliasReachablePort(s), Image: s.Image, Digest: imageDigest}
	}

	readyTimeout := spec.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = ComposeServicesReadyTimeout
	}
	workerEnvironment := verdict.WorkerEnvironment
	forwards := verdict.Forwards

	report := ComposeServicesReport{Enabled: true, NetworkName: networkName, Services: records, Skipped: skipped, Forwards: forwards}
	if err := writeComposeServicesReport(dataDir, runID, spec.Phase, &report); err != nil {
		return nil, fmt.Errorf("record compose services report: %w", err)
	}

	begunSuccessfully = true
	return &ComposeServicesLifecycle{
		dockerBinary:      dockerBinary,
		runID:             runID,
		dataDir:           dataDir,
		phase:             spec.Phase,
		hooks:             hooks,
		networkName:       networkName,
		composeDir:        projectDir,
		composeFilePath:   composeFilePath,
		bindInputDir:      bindInputDir,
		readyTimeout:      readyTimeout,
		workerEnvironment: workerEnvironment,
		forwards:          forwards,
		services:          launchable,
	}, nil
}

// Disabled reports whether this run's compose services are inactive
// because no compose file was present (see BeginComposeServicesLifecycle's
// doc comment).
// Every other method on a disabled lifecycle is a documented no-op.
func (l *ComposeServicesLifecycle) Disabled() bool {
	return l == nil || l.disabled
}

// DisabledReason returns why this lifecycle is disabled, or "" if it isn't.
func (l *ComposeServicesLifecycle) DisabledReason() string {
	if l == nil {
		return ""
	}
	return l.disabledReason
}

// NetworkName returns this run's dedicated compose-services network, or ""
// when disabled.
func (l *ComposeServicesLifecycle) NetworkName() string {
	if l.Disabled() {
		return ""
	}
	return l.networkName
}

// LaunchedServices returns the alias each successfully-launched service is
// reachable at on NetworkName() -- the caller uses this to compute the
// worker's BG_SERVICE_* environment (see cmd/factoryd's own worker-launch
// wiring). Empty when disabled.
func (l *ComposeServicesLifecycle) LaunchedServices() []ComposeServiceRecord {
	if l.Disabled() {
		return nil
	}
	out := make([]ComposeServiceRecord, len(l.services))
	for i, s := range l.services {
		out[i] = ComposeServiceRecord{Name: s.Name, Alias: s.Name, Port: aliasReachablePort(s)}
	}
	return out
}

// aliasReachablePort is the port a container-network client (the worker,
// or another compose service) should use when reaching a service by its
// Alias: the service's own "x-bg-service-port" override
// (composeservices.ServiceSpec.AliasPort) when the compose file set one,
// else the first container-side port from its `ports:` mapping, else 0
// when the service declares no port at all -- e.g. the compose file's own
// image has a well-known default a caller is expected to already know, or
// the service isn't meant to be dialed at all (a one-shot migration
// container, say). 0 means ApplyToWorkerLaunch omits BG_SERVICE_<NAME>_PORT
// entirely rather than emitting an empty or zero value.
func aliasReachablePort(s composeservices.ServiceSpec) int {
	if s.AliasPort != 0 {
		return s.AliasPort
	}
	if len(s.Ports) == 0 {
		return 0
	}
	port, err := strconv.Atoi(s.Ports[0])
	if err != nil {
		return 0
	}
	return port
}

// EnsureForAttempt brings this attempt's own compose project up, first
// tearing down any leftover project from the immediately preceding attempt
// (attempt-1) -- needed because a worker crash or timeout can leave
// TeardownAttempt never having run for that earlier attempt, and
// composeServicesProjectName's own runID+attempt pairing means that
// leftover project would otherwise just sit there consuming resources
// alongside this attempt's fresh one rather than being reclaimed. "Not
// found" tearing that down is expected on every ordinary run's first
// attempt with nothing to tear down, and is therefore ignored rather than
// treated as this call's own failure.
func (l *ComposeServicesLifecycle) EnsureForAttempt(ctx context.Context, attempt int) error {
	if l.Disabled() {
		return nil
	}
	l.lastAttempt = attempt

	down := l.hooks.Down
	if down == nil {
		down = composeServicesDown
	}
	if attempt > 1 {
		prevProject := composeServicesProjectName(l.runID, attempt-1)
		_ = down(ctx, l.dockerBinary, prevProject, l.composeFilePath, l.composeDir) // best-effort; confirmed below, not trusted on its own
		// Confirm, don't trust: `down`'s own exit status is ignored just
		// above (it's expected to report "not found" on the ordinary first
		// EnsureForAttempt call, and this method has no way to tell that
		// apart from a real failure) -- but proceeding to start the new
		// attempt while the previous attempt's containers are still up
		// would put stale and fresh containers on the same network under
		// the same service aliases, exactly the false-accept-from-
		// leftover-state failure this whole lifecycle exists to prevent
		// (found via review, GitHub Codex App, PR #131 round 2, P1).
		// Mirrors Cleanup's own confirm-before-trusting-exit-status pattern
		// via the same ProjectContainersPresent hook, rather than
		// introducing a second way to ask the same question.
		containersPresent := l.hooks.ProjectContainersPresent
		if containersPresent == nil {
			containersPresent = composeProjectContainersPresent
		}
		present, checkErr := containersPresent(ctx, l.dockerBinary, prevProject)
		if checkErr != nil {
			return fmt.Errorf("confirm attempt %d's project %q is gone before starting attempt %d: %w", attempt-1, prevProject, attempt, checkErr)
		}
		if present {
			return fmt.Errorf("attempt %d's project %q still has containers after teardown; refusing to start attempt %d on the same network", attempt-1, prevProject, attempt)
		}
	}

	up := l.hooks.Up
	if up == nil {
		up = composeServicesUp
	}
	project := composeServicesProjectName(l.runID, attempt)
	started := time.Now()
	upErr := up(ctx, l.dockerBinary, project, l.composeFilePath, l.composeDir, l.readyTimeout)
	if upErr == nil && l.byAddress {
		upErr = l.resolveAddresses(ctx, project)
	}
	record := &ComposeAttemptRecord{UpDuration: time.Since(started).String()}
	if upErr != nil {
		record.UpError = upErr.Error()
	}
	if upErr != nil {
		// A failed `up --wait` is exactly when the services' own output
		// matters most (a crashed Kafka, a Postgres that never went
		// healthy), and no TeardownAttempt follows this attempt: the
		// callers skip straight to the next attempt, whose own defensive
		// teardown above does not capture logs. So capture them here.
		logDir, logErr := l.captureLogs(ctx, project, attempt)
		if logErr == nil {
			upErr = fmt.Errorf("%w (service logs: %s)", upErr, logDir)
		}
		upErr = errors.Join(upErr, logErr)
	}
	if err := l.recordAttempt(attempt, record); err != nil {
		return errors.Join(upErr, err)
	}
	return upErr
}

// captureLogs writes each launched service's compose logs for project into
// this attempt's log directory and returns that directory. A service with
// nothing to log (it never started) is skipped, not an error.
func (l *ComposeServicesLifecycle) captureLogs(ctx context.Context, project string, attempt int) (string, error) {
	logs := l.hooks.Logs
	if logs == nil {
		logs = composeServicesLogs
	}
	logDir := composeServicesAttemptLogDir(l.dataDir, l.runID, attempt, l.phase)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		return "", fmt.Errorf("create compose log directory: %w", err)
	}
	for _, s := range l.services {
		output, logErr := logs(ctx, l.dockerBinary, project, l.composeFilePath, l.composeDir, s.Name)
		if logErr != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(logDir, s.Name+".log"), output, 0o640)
	}
	return logDir, nil
}

// TeardownAttempt captures every launched service's own compose logs, then
// tears the attempt's project down. Must run even when the attempt itself
// panicked or timed out -- the caller (cmd/factoryd's runSandboxWithRetries)
// is responsible for invoking this from the same defer/recover path it
// already must use for RegistryProxyLifecycle; this method
// introduces no panic-recovery machinery of its own.
func (l *ComposeServicesLifecycle) TeardownAttempt(ctx context.Context, attempt int) error {
	if l.Disabled() {
		return nil
	}
	project := composeServicesProjectName(l.runID, attempt)
	if _, err := l.captureLogs(ctx, project, attempt); err != nil {
		return err
	}

	down := l.hooks.Down
	if down == nil {
		down = composeServicesDown
	}
	started := time.Now()
	downErr := down(ctx, l.dockerBinary, project, l.composeFilePath, l.composeDir)
	record := &ComposeAttemptRecord{DownDuration: time.Since(started).String()}
	if downErr != nil {
		record.DownError = downErr.Error()
	}
	if err := l.recordAttempt(attempt, record); err != nil {
		return errors.Join(downErr, err)
	}
	return downErr
}

// recordAttempt merges fields into services.json's Attempts[attempt] entry
// -- called separately by EnsureForAttempt (up) and TeardownAttempt (down)
// for the same attempt, so neither overwrites the other's own half of the
// record.
func (l *ComposeServicesLifecycle) recordAttempt(attempt int, fields *ComposeAttemptRecord) error {
	report, err := readComposeServicesReport(l.dataDir, l.runID, l.phase)
	if err != nil {
		return err
	}
	if report.Attempts == nil {
		report.Attempts = map[string]*ComposeAttemptRecord{}
	}
	key := fmt.Sprintf("%d", attempt)
	existing, ok := report.Attempts[key]
	if !ok {
		existing = &ComposeAttemptRecord{}
	}
	if fields.UpDuration != "" {
		existing.UpDuration = fields.UpDuration
	}
	if fields.UpError != "" {
		existing.UpError = fields.UpError
	}
	if fields.DownDuration != "" {
		existing.DownDuration = fields.DownDuration
	}
	if fields.DownError != "" {
		existing.DownError = fields.DownError
	}
	report.Attempts[key] = existing
	return writeComposeServicesReport(l.dataDir, l.runID, l.phase, report)
}

// Cleanup is the deferred backstop for this run's compose project and
// network -- RegistryProxyLifecycle.Cleanup's own contract exactly: bounded by ComposeServicesCleanupTimeout,
// idempotent on success, and returning an error wrapping
// ErrCleanupUnconfirmed whenever a resource's removal could not be
// confirmed (a timeout, or a Docker CLI error on the confirming list call)
// -- never on an ordinary "already gone" outcome. Tears down whichever
// attempt was last active via EnsureForAttempt/TeardownAttempt (or, if
// none was ever recorded, defensively attempts attempt 1's project too, in
// case EnsureForAttempt itself never got the chance to set lastAttempt)
// before removing the network, matching the container-then-network
// ordering `docker network rm` itself requires (it fails outright while
// any container remains attached).
func (l *ComposeServicesLifecycle) Cleanup(ctx context.Context) error {
	if l == nil || l.disabled {
		return nil
	}
	boundedCtx, cancel := context.WithTimeout(context.Background(), ComposeServicesCleanupTimeout)
	defer cancel()
	_ = ctx // Cleanup deliberately does not inherit the caller's context, because a canceled or wedged worker must not turn credential/resource-bearing teardown into a skipped or unbounded operation.

	down := l.hooks.Down
	if down == nil {
		down = composeServicesDown
	}
	containersPresent := l.hooks.ProjectContainersPresent
	if containersPresent == nil {
		containersPresent = composeProjectContainersPresent
	}
	attempts := map[int]bool{1: true}
	if l.lastAttempt > 0 {
		attempts[l.lastAttempt] = true
	}
	attemptNums := make([]int, 0, len(attempts))
	for a := range attempts {
		attemptNums = append(attemptNums, a)
	}
	sort.Ints(attemptNums)
	for _, attempt := range attemptNums {
		project := composeServicesProjectName(l.runID, attempt)
		_ = down(boundedCtx, l.dockerBinary, project, l.composeFilePath, l.composeDir)
		present, checkErr := containersPresent(boundedCtx, l.dockerBinary, project)
		if checkErr != nil {
			return fmt.Errorf("%w: confirm compose project %q removal: %v", ErrCleanupUnconfirmed, project, checkErr)
		}
		if present {
			return fmt.Errorf("%w: compose project %q containers remain after down", ErrCleanupUnconfirmed, project)
		}
	}

	removeNetwork := l.hooks.RemoveNetwork
	if removeNetwork == nil {
		removeNetwork = composeServicesRemoveNetwork
	}
	networkPresent := l.hooks.NetworkPresent
	if networkPresent == nil {
		networkPresent = relayNetworkPresent
	}
	_ = removeNetwork(boundedCtx, l.dockerBinary, l.networkName)
	present, checkErr := networkPresent(boundedCtx, l.dockerBinary, l.networkName)
	if checkErr != nil {
		return fmt.Errorf("%w: confirm compose network %q removal: %v", ErrCleanupUnconfirmed, l.networkName, checkErr)
	}
	if present {
		return fmt.Errorf("%w: compose network %q remains after removal", ErrCleanupUnconfirmed, l.networkName)
	}
	if l.bindInputDir != "" {
		if err := os.RemoveAll(l.bindInputDir); err != nil {
			return fmt.Errorf("remove materialized compose bind inputs: %w", err)
		}
	}
	return nil
}

// composeServicesEnvVarSuffix turns a compose service name into the suffix
// of its BG_SERVICE_<NAME> worker environment variable: uppercased, with
// every non-alphanumeric character replaced by "_" (e.g. "event-bus" ->
// "EVENT_BUS").
var composeServicesNonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]`)

func ComposeServicesEnvVarName(serviceName string) string {
	return "BG_SERVICE_" + composeServicesNonAlphanumeric.ReplaceAllString(strings.ToUpper(serviceName), "_")
}

// ComposeServiceWorkerEnv is the BG_SERVICE_* environment one launched
// service gives the worker, exactly as ApplyToWorkerLaunch sets it.
func ComposeServiceWorkerEnv(s composeservices.ServiceSpec) []string {
	env := []string{ComposeServicesEnvVarName(s.Name) + "=" + s.Name}
	if port := aliasReachablePort(s); port != 0 {
		env = append(env, ComposeServicesEnvVarName(s.Name)+"_PORT="+strconv.Itoa(port))
	}
	return env
}

var composeServicesWorkerEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateComposeServicesWorkerEnvironment(env map[string]string) ([]string, error) {
	keys := sortedKeys(env)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if !composeServicesWorkerEnvKey.MatchString(key) {
			return nil, fmt.Errorf("operator Compose worker environment key %q is not a valid environment variable name", key)
		}
		if IsForbiddenCredentialEnvKey(key) || protectedComposeWorkerEnvironmentKey(key) {
			return nil, fmt.Errorf("operator Compose worker environment key %q is reserved for factory or sandbox control", key)
		}
		value := env[key]
		if strings.IndexByte(value, 0) >= 0 || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("operator Compose worker environment value for %q contains a forbidden control character", key)
		}
		out = append(out, key+"="+value)
	}
	return out, nil
}

// protectedComposeWorkerEnvironmentKey reports whether key names a
// variable the factory, the sandbox, a harness, a package manager or the
// network path owns. Compared upper-cased: npm_config_registry and
// https_proxy are read in lower case. LaunchSpec.Validate separately
// rejects any key the launch already sets, so this list is the early,
// doctor-visible check rather than the only one.
func protectedComposeWorkerEnvironmentKey(key string) bool {
	key = strings.ToUpper(key)
	switch key {
	case "PATH", "HOME", "USER", "SHELL", "PWD", "OLDPWD", "TMPDIR", "TMP", "TEMP", "TERM",
		"GOMODCACHE", "GOCACHE", "GOTMPDIR", "GOPROXY", "GOSUMDB", "GOFLAGS", "GOENV",
		"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY",
		"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE",
		"NODE_OPTIONS":
		return true
	}
	for _, prefix := range []string{
		"ANTHROPIC_", "OPENAI_", "FACTORY", "SANDBOX_", "GIT_", "SF_", "COPILOT_", "PI_", "PIFORK_", "CODEX_",
		"INFERENCE_RELAY_", "NPM_CONFIG_", "PIP_", "XDG_", "LD_", "DYLD_",
	} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// ApplyToWorkerLaunch is compose services' own PrepareWorker-shaped call --
// distinct from RegistryProxyLifecycle's own method of that
// name. It adds the worker's compose-network setting plus its
// BG_SERVICE_*/BG_COMPOSE_SERVICES environment and, when the compose file
// publishes ports, BG_COMPOSE_FORWARDS with the command wrapped in
// bg-forward, leaving s.Network itself untouched. Run promotes ComposeNetwork to the primary network only when
// s.Network is the private "none" network; when a relay or registry-proxy
// network is already primary, Run attaches ComposeNetwork as a second
// internal network. l may be nil (no compose services configured for this run
// at all, as opposed to configured-but-Disabled -- e.g. a target repo with no
// compose file) -- both cases add BG_COMPOSE_SERVICES=disabled: <reason> and
// nothing else, so a worker script can always find the variable rather than
// needing to handle its absence as a third state.
func (l *ComposeServicesLifecycle) ApplyToWorkerLaunch(s LaunchSpec) LaunchSpec {
	if l.Disabled() {
		reason := l.DisabledReason()
		if reason == "" {
			// l is nil here: no ComposeServicesSpec was even given for this
			// run (as opposed to one that resolved to Disabled with its own
			// reason -- see BeginComposeServicesLifecycle's own doc
			// comment, which always sets a non-empty reason in that case).
			reason = "compose services not configured for this run"
		}
		s.Environment = append(s.Environment, "BG_COMPOSE_SERVICES=disabled: "+reason)
		return s
	}
	if l.byAddress {
		return l.applyByAddress(s)
	}
	s.ComposeNetwork = l.NetworkName()
	s.Environment = append(s.Environment, "BG_COMPOSE_SERVICES=up")
	for _, svc := range l.LaunchedServices() {
		s.Environment = append(s.Environment, ComposeServicesEnvVarName(svc.Name)+"="+svc.Alias)
		// Additive: BG_SERVICE_<NAME> above keeps carrying only the bare
		// alias, unchanged for any existing consumer. This second variable
		// is the only place the derived/overridden port is exposed to the
		// worker, and is omitted entirely (not emitted as "0" or empty)
		// when svc.Port is 0 -- see aliasReachablePort's own doc comment
		// for when that happens.
		if svc.Port != 0 {
			s.Environment = append(s.Environment, ComposeServicesEnvVarName(svc.Name)+"_PORT="+strconv.Itoa(svc.Port))
		}
	}
	s.UnrecordedEnvironment = append(s.UnrecordedEnvironment, l.workerEnvironment...)
	if len(l.forwards) > 0 {
		// The worker image's bg-forward binds localhost at each published
		// port before it runs the command as its child: a target's tests
		// that default to what `docker compose up` publishes on a
		// developer machine (localhost:5433) reach the sidecar unchanged.
		s.Environment = append(s.Environment, ComposeForwardsEnvVar+"="+strings.Join(l.forwards, ","))
		s.Command = append([]string{WorkerForwarderPath, "--"}, s.Command...)
	}
	return s
}

// ComposeForwardsEnvVar and WorkerForwarderPath are the contract with
// cmd/bg-forward, which internal/sandbox/Dockerfile installs in every
// worker image (the pifork and project images build FROM it).
const (
	ComposeForwardsEnvVar = "BG_COMPOSE_FORWARDS"
	WorkerForwarderPath   = "/usr/local/bin/bg-forward"
)

func writeComposeServicesReport(dataDir, runID, phase string, report *ComposeServicesReport) error {
	path := composeServicesReportPath(dataDir, runID, phase)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	content, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o640)
}

// ComposeServicesLaunched returns the names of the services phase launched
// for runID, sorted, or nil when it launched none or recorded no report.
func ComposeServicesLaunched(dataDir, runID, phase string) []string {
	report, err := readComposeServicesReport(dataDir, runID, phase)
	if err != nil || !report.Enabled {
		return nil
	}
	names := make([]string, len(report.Services))
	for i, s := range report.Services {
		names[i] = s.Name
	}
	sort.Strings(names)
	return names
}

func readComposeServicesReport(dataDir, runID, phase string) (*ComposeServicesReport, error) {
	content, err := os.ReadFile(composeServicesReportPath(dataDir, runID, phase))
	if err != nil {
		return nil, err
	}
	var report ComposeServicesReport
	if err := json.Unmarshal(content, &report); err != nil {
		return nil, err
	}
	return &report, nil
}

// composeProjectContainersPresent mirrors relayContainerPresent's own
// list-and-check shape, filtered by compose's own project label instead of
// this package's container-name convention: a compose project's containers
// aren't named by this package directly, only labeled by `docker compose`
// itself with com.docker.compose.project=<name>.
func composeProjectContainersPresent(ctx context.Context, dockerBinary, project string) (bool, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "ps", "-a", "--filter", "label=com.docker.compose.project="+project, "--format", "{{.Names}}")
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err != nil {
		if boundedCtx.Err() != nil {
			return false, fmt.Errorf("list compose project containers timed out: %w", boundedCtx.Err())
		}
		return false, fmt.Errorf("list compose project containers failed: %w", err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func composeServicesCreateNetwork(ctx context.Context, dockerBinary, networkName string, labels map[string]string) error {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	args := []string{"network", "create", "--internal"}
	for _, key := range sortedKeys(labels) {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, networkName)
	cmd := exec.CommandContext(boundedCtx, dockerBinary, args...)
	cmd.Env = dockerClientEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		if boundedCtx.Err() != nil {
			return fmt.Errorf("create compose services network timed out: %w", boundedCtx.Err())
		}
		return fmt.Errorf("create compose services network: %w: %s", err, out)
	}
	return nil
}

func composeServicesRemoveNetwork(ctx context.Context, dockerBinary, networkName string) error {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "network", "rm", networkName)
	cmd.Env = dockerClientEnv()
	_ = cmd.Run() // best-effort: caller confirms actual removal via relayNetworkPresent
	return nil
}

func composeServicesPull(ctx context.Context, dockerBinary, projectName, composeFile, projectDir string, services []string) error {
	args := append([]string{"compose", "-p", projectName, "-f", composeFile, "--project-directory", projectDir, "pull"}, services...)
	cmd := exec.CommandContext(ctx, dockerBinary, args...)
	cmd.Env = dockerClientEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker compose pull: %w: %s", err, out)
	}
	return nil
}

func composeServicesImageDigest(ctx context.Context, dockerBinary, image string) (string, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "image", "inspect", "--format", "{{index .RepoDigests 0}}", image)
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("docker image inspect: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func composeServicesUp(ctx context.Context, dockerBinary, projectName, composeFile, projectDir string, waitTimeout time.Duration) error {
	boundedCtx, cancel := context.WithTimeout(ctx, waitTimeout+15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "compose", "-p", projectName, "-f", composeFile, "--project-directory", projectDir,
		"up", "--wait", "--wait-timeout", fmt.Sprintf("%d", int(waitTimeout.Seconds())), "-d")
	cmd.Env = dockerClientEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		if boundedCtx.Err() != nil {
			return fmt.Errorf("docker compose up timed out: %w", boundedCtx.Err())
		}
		return fmt.Errorf("docker compose up: %w: %s", err, composeUpFailureText(out))
	}
	return nil
}

// composeProgressLine matches compose's own per-resource progress lines
// ("Container bg-x-a1-kafka-1 Started"), which say nothing about a failure.
var composeProgressLine = regexp.MustCompile(`^(Container|Volume|Network) \S+ (Creating|Created|Starting|Started|Waiting|Healthy|Running|Recreate|Recreated|Stopping|Stopped|Removing|Removed)$`)

// composeUpFailureText keeps only the lines of a failed `up --wait`'s
// output that explain the failure ("container ...-kafka-1 exited (1)", a
// daemon error): the ~25 progress lines before them would otherwise flood
// the halt reason, `status` and the notification. Falls back to the last
// non-empty line when every line is progress.
func composeUpFailureText(out []byte) string {
	var kept []string
	last := ""
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		last = line
		if !composeProgressLine.MatchString(line) {
			kept = append(kept, line)
		}
	}
	if len(kept) == 0 {
		return last
	}
	return strings.Join(kept, "; ")
}

func composeServicesDown(ctx context.Context, dockerBinary, projectName, composeFile, projectDir string) error {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "compose", "-p", projectName, "-f", composeFile, "--project-directory", projectDir,
		"down", "-v", "--remove-orphans")
	cmd.Env = dockerClientEnv()
	_ = cmd.Run() // best-effort; caller confirms actual removal via composeProjectContainersPresent
	return nil
}

func composeServicesLogs(ctx context.Context, dockerBinary, projectName, composeFile, projectDir, service string) ([]byte, error) {
	boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(boundedCtx, dockerBinary, "compose", "-p", projectName, "-f", composeFile, "--project-directory", projectDir,
		"logs", "--no-color", service)
	cmd.Env = dockerClientEnv()
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose logs: %w", err)
	}
	return out, nil
}
