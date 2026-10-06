// Package composeservices reduces a target project's own docker-compose
// file to a strictly allow-listed subset of service definitions that a
// sandboxed run's host-side provisioner may safely launch as sidecars next
// to the worker container, on the same internal network -- see
// internal/sandbox/registryproxy.go and internal/sandbox/relay.go for the
// two existing sidecars this is structurally a third instance of.
//
// DESIGN. Many target repos declare docker-compose services (Postgres,
// Redis, a mock auth service) that their own test suite needs running and
// reachable. Unlike a package manifest, which only says what to install, a
// compose file can specify `privileged: true`, host bind-mounts,
// `network_mode: host`, and arbitrary images -- so naively parsing it and
// using it to configure the worker's own container privileges would let
// content in the target repo (which the sandboxed worker itself writes)
// influence its own containment on a later run. This package is the
// boundary that closes that: it treats a compose file as a *request*, never
// as *configuration*, by keeping to a positive field allow-list and hard-
// rejecting everything else with a reason a caller can surface to both the
// model (so it can adapt) and the operator (so a repeated rejection is a
// deliberate decision to widen the allow-list, not a silent workaround).
//
// IMPLEMENTATION. This package loads the compose file through
// github.com/compose-spec/compose-go/v2 (the same library `docker compose`
// itself is built on) rather than hand-walking raw yaml.v3 nodes, so the
// definition of a "field" is the real, versioned compose schema instead of
// a schema this package invented. That upgrade introduces a new risk of
// its own: compose-go's default loader resolves `${VAR}` against
// os.Environ(), reads a `.env` file from the project directory, resolves
// `env_file`/`label_file` from arbitrary host paths at load time (confirmed
// empirically: with the loader's default options, a service naming a
// nonexistent env_file path causes the loader to `stat` that exact host
// path before this package ever sees a parsed model), and can follow
// `extends`/`include` to open arbitrary host paths -- any of which would
// let a target-repo-controlled compose file read the *operator's machine*,
// not just be validated. loadIsolated (below) closes all of these:
// environment lookups are answered only from the base commit's own `.env`
// content the caller hands in (never os.Environ(), never a `.env` read
// from any host path); the compose file is loaded from bytes with its
// working directory pointed at a fresh, empty temp directory (never the
// operator's cwd or the worker's own checkout); and SkipInclude,
// SkipExtends, SkipResolveEnvironment and SkipResolveLabels are all set so
// the loader never dereferences env_file/label_file/extends/include from
// disk at all -- confirmed those Skip options leave the *fields themselves*
// on the parsed model (compose-go's SkipExtends does not clear
// ServiceConfig.Extends, and SkipInclude does not remove the top-level
// "include" key from the raw model), so extendsOrIncludePresent below
// checks for their survival explicitly rather than trusting the Skip
// options alone to make the file inert.
//
// Every exported field of compose-go's types.ServiceConfig is classified
// in exactly one of allowedFields or rejectedFields below; the package's
// own TestServiceConfigFieldsAreFullyClassified test enumerates that
// struct by reflection and fails if a field is unclassified (or a
// go.mod bump to compose-go adds a new one), which is how this package
// keeps its old walker's core invariant -- every field present is either
// allow-listed or explicitly rejected, nothing silently dropped -- now
// that the fields live on someone else's struct instead of a raw yaml.Node
// map this package owned outright.
//
// A service naming `build:` is the one exception to "reject the whole
// service": since a compose file needing to build an image is a reasonable,
// common thing to want and has no privilege implication by itself (the
// image never runs here -- see rejectedFields' own note on Build), it is
// skipped rather than rejected, reported separately, and does not affect
// any other service in the same file.
//
// This package is pure and has no Docker dependency by design -- the
// Docker launch/teardown side (like LaunchRegistryProxy)
// is a separate, later piece in internal/sandbox that consumes ParseFile's
// output. Provenance -- reading the compose file (and its .env) from the
// target branch's base commit rather than the worker's own writable
// checkout, so a worker-authored edit only takes effect after a human
// merges it -- is that caller's responsibility, not this package's;
// ParseFile only ever sees bytes it's handed.
package composeservices

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	interp "github.com/compose-spec/compose-go/v2/interpolation"
	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	units "github.com/docker/go-units"
)

// ServiceSpec is one compose service that survived validation: only the
// fields this package allow-lists, nothing else from the source document.
type ServiceSpec struct {
	Name  string
	Image string
	// Platform is an explicitly requested Linux platform, or empty when the
	// source left platform selection to Docker. Only the operator-approved
	// Linux platforms survive parsing; Synthesize emits this value so a
	// source request cannot silently select a host platform.
	Platform    string
	Environment map[string]string
	// Ports lists this service's declared container-side ports (e.g.
	// "5432"); a provisioner exposes these only as an alias on the
	// worker's internal network, never publishes them to the host --
	// see internal/sandbox/registryproxy.go's own internal-network
	// convention. The host side of any "host:container" mapping is
	// never re-emitted by a synthesizer; it is kept only in
	// PublishedPorts.
	Ports []string
	// PublishedPorts is each fixed "host:container" TCP mapping. Nothing
	// is published on the host: the worker's own loopback forwards
	// localhost:<Host> to <service>:<Target> (cmd/bg-forward), so a test
	// that defaults to the port a developer's `docker compose up`
	// publishes reaches the sidecar unchanged. A mapping with no host
	// port (a random one) or a UDP one is not kept.
	PublishedPorts []PublishedPort
	Command        []string
	// Entrypoint is kept separate from Command, never merged into it: real
	// Compose semantics have entrypoint override the image's own ENTRYPOINT
	// while command only ever supplies arguments to whatever ENTRYPOINT is
	// in effect, so collapsing the two would invoke an image's own
	// ENTRYPOINT (when Entrypoint itself is unset) with the wrong
	// arguments. nil means "not set" (the image's own value applies,
	// unmodified); a non-nil, possibly empty, slice is an explicit
	// override -- see parseOneService's own handling of compose-go's
	// nil-vs-empty ShellCommand distinction.
	Entrypoint  []string
	Healthcheck *Healthcheck
	// DependsOn preserves each dependency's own validated condition
	// (service_started, service_healthy, or service_completed_successfully)
	// exactly as the source file declared it -- compose-go's own loader
	// already defaults a short-form `depends_on: [name]` entry's condition
	// to "service_started" before ParseFile ever sees it (see
	// transform.transformDependsOn), so there is no separate "no condition
	// to preserve" case for Synthesize to re-derive from healthcheck
	// presence: every entry here already carries the real condition to
	// honor.
	DependsOn []ServiceDependency
	// NamedVolumes lists every accepted named-volume or tmpfs mount, each
	// keeping enough of the source declaration for Synthesize to preserve
	// volume sharing and tmpfs-vs-disk identity -- see VolumeMount's own
	// doc comment.
	NamedVolumes []VolumeMount
	// Hostname sets the container's own internal hostname (e.g. visible
	// via `hostname` inside the container) -- purely cosmetic/informational,
	// grants no capability and widens nothing, so it's allow-listed rather
	// than rejected. A real compose file needing multi-listener setups
	// (Kafka's own KRaft config is a common case) sets this routinely.
	Hostname string
	// User, WorkingDir, ReadOnly, Tmpfs, ShmSize, and Init are all
	// allow-listed fields (see allowedFields) that only ever narrow or
	// redirect a container's own behavior, never widen its privileges or
	// escape the sandbox's own network -- each is threaded straight
	// through to Synthesize so the launched container actually matches
	// what ParseFile validated, rather than silently differing from it.
	User       string
	WorkingDir string
	ReadOnly   bool
	// Tmpfs lists container paths mounted as tmpfs via the short-form
	// top-level `tmpfs:` field -- distinct from a `volumes:` entry of type
	// "tmpfs" (see VolumeMount.Type), which compose also allows.
	Tmpfs   []string
	ShmSize int64
	Init    bool
	// AliasPort overrides the port a provisioner treats as this service's
	// reachable, container-network port -- see internal/sandbox's own
	// derivation, which otherwise defaults to the first entry of Ports.
	// Zero means "not set, use that default derivation instead". Sourced
	// from the one narrow "x-bg-service-port" extension key this package
	// reads (see aliasPortExtensionKey); every other "x-*" extension key
	// remains rejected. It exists because some images (Kafka's multi-
	// listener setups are the motivating case) advertise a different port
	// to container-network clients than the one published in `ports:`, and
	// nothing in the compose spec's own standard fields can express that.
	AliasPort int
	// MemLimit is the service's own `mem_limit` in bytes, 0 when unset.
	// ParseFile accepts it only at or below Options.MemoryCeiling, so it
	// can only narrow the operator's limit; see EffectiveMemoryBytes.
	MemLimit int64
}

// ServiceDependency is one accepted `depends_on` entry: the name of the
// service depended on, plus the validated condition ParseFile requires it
// to wait for. See ServiceSpec.DependsOn's own doc comment for why the
// condition is preserved here rather than re-derived later.
type ServiceDependency struct {
	Name      string
	Condition string
}

// VolumeMount is one accepted named-volume or tmpfs mount from a service's
// `volumes:` block, keeping its source identity and mount type -- reducing
// every mount to only its target path (the old behavior) lost both: two
// services intentionally sharing one named volume ended up with two
// separate, unshared volumes once Synthesize manufactured a fresh name per
// (service, target) pair, and a real tmpfs mount silently became a
// disk-backed named volume. See decodeNamedVolumes and Synthesize.
type VolumeMount struct {
	// Type is "volume", "tmpfs", or "bind". Bind mounts are represented
	// separately from named volumes so the lifecycle can materialize their
	// source from the immutable base commit before synthesis.
	Type string
	// Source is the named volume's own name from the source compose file's
	// top-level `volumes:` block (e.g. "pgdata" for a "pgdata:/data" mount),
	// preserved so Synthesize can re-emit that same name and let two
	// services referencing it share the synthesized file's own volume too.
	// Empty for a tmpfs mount (which has no named source) and for an
	// anonymous named volume (a bare "- /data" entry with no name of its
	// own) -- Synthesize derives a name in that case exactly as it always
	// has.
	Source string
	Target string
	// ReadOnly records the source declaration, but bind mounts are emitted
	// read-only regardless of this value. Named volumes keep their original
	// mutability semantics.
	ReadOnly bool
}

// PublishedPort is one "host:container" TCP port mapping; see
// ServiceSpec.PublishedPorts.
type PublishedPort struct {
	Host   int
	Target int
}

// Healthcheck is the allow-listed subset of a compose service's own
// healthcheck block, used by a provisioner to know when a sidecar is ready
// rather than guessing with a fixed sleep.
type Healthcheck struct {
	Test     []string
	Interval string
	Timeout  string
	Retries  int
	// Disabled mirrors the source file's own `healthcheck: {disable: true}`
	// -- when true, Test/Interval/Timeout/Retries carry no meaning and
	// Synthesize must emit `disable: true` instead of a test. Represented
	// as a field on a non-nil Healthcheck, not a nil Healthcheck, so
	// "explicitly disabled" stays distinguishable from "no healthcheck
	// declared at all" (the latter is a nil ServiceSpec.Healthcheck).
	Disabled bool
}

// Rejection names one compose service, or the whole file, that ParseFile
// refused -- along with why, so a caller can surface it verbatim rather
// than silently dropping it.
type Rejection struct {
	Service string
	Reason  string
}

func (r Rejection) String() string { return fmt.Sprintf("%s: %s", r.Service, r.Reason) }

// wholeFileRejectionService is the Rejection.Service value used when the
// problem isn't attributable to one service -- the file declares more
// services than MaxServices allows, or an `include:` directive survived
// loading -- and so nothing in the file is returned in services, only this
// one Rejection.
const wholeFileRejectionService = "<compose file>"

// DefaultMaxServices is the MaxServices a caller gets by passing a zero
// Options.MaxServices.
const DefaultMaxServices = 8

// Options configures ParseFile.
type Options struct {
	// AllowedImageRegistries lists registry/namespace prefixes an image
	// reference must start with (e.g. "docker.io/library/", "ghcr.io/my-org/"
	// -- an image with no registry prefix at all is implicitly Docker Hub's
	// "docker.io/library/"). No entries means fail closed: nothing is
	// allowed.
	AllowedImageRegistries []string
	// EnvFileContent is the raw bytes of the target repo's own top-level
	// `.env` file at the base commit, or nil if it has none. It is the
	// *only* source of `${VAR}` interpolation values ParseFile uses --
	// never os.Environ(), never a `.env` read from any host path -- so the
	// caller must read it from the same provenance-controlled commit as
	// composeYAML itself, not from disk. See this package's doc comment.
	EnvFileContent []byte
	// MaxServices caps how many services a compose file may declare before
	// ParseFile refuses the whole file outright, without parsing any
	// individual service. Zero means DefaultMaxServices.
	MaxServices int
	// ReservedAliases names additional service names (and, via
	// networkAliasReason, `networks: aliases:` entries) ParseFile refuses
	// on top of its own built-in "relay"/"worker" defaults -- the caller
	// (internal/sandbox.ComposeServicesLifecycle) passes its own
	// sandbox.ReservedWorkerAliases here rather than this package
	// importing internal/sandbox directly, which would cycle back
	// (internal/sandbox already imports this package to drive the
	// lifecycle that calls ParseFile in the first place).
	ReservedAliases []string
	// RequireDigest, opt-in and false by default (sessionconfig's
	// compose_services_require_digest), refuses any service whose image is
	// not pinned by digest ("...@sha256:<64 hex>"), on top of the
	// registry/namespace allow-list above. Sidecar images are not
	// digest-pinned by default -- a tag can move between two runs of the
	// same accepted ticket, unlike the worker/relay canonical images,
	// which are always digest-pinned unconditionally (see
	// containment-matrix.md's Package registry row for the accepted
	// residual this flag lets an operator close per-run).
	RequireDigest bool
	// MemoryCeiling is the operator's per-service memory limit
	// (compose_services_memory, e.g. "512m"). A service's own `mem_limit`
	// above it is rejected; with no ceiling configured, any `mem_limit`
	// is rejected (fail closed). internal/sandbox sets it from the same
	// SynthesizeOptions.MemoryLimit Synthesize applies.
	MemoryCeiling string
}

// ParseFile validates composeYAML (the raw bytes of a docker-compose.yml)
// against ParseFile's allow-list. A service that fails any check is
// returned in rejected, never in services -- there is no partial-allow of
// a single service. A service naming `build:` is instead returned in
// skipped (see this package's doc comment on why that's not a rejection).
// A whole-file problem -- too many services, or an `include:` directive
// surviving the loader -- produces a single Rejection with Service set to
// "<compose file>" and no services or skipped entries at all.
func ParseFile(composeYAML []byte, opts Options) (services []ServiceSpec, rejected []Rejection, skipped []Rejection, err error) {
	maxServices := opts.MaxServices
	if maxServices == 0 {
		maxServices = DefaultMaxServices
	}

	envFile, envErr := dotenv.UnmarshalBytesWithLookup(opts.EnvFileContent, nil)
	if envErr != nil {
		return nil, nil, nil, fmt.Errorf("parse .env content: %w", envErr)
	}

	tempDir, tmpErr := os.MkdirTemp("", "composeservices-")
	if tmpErr != nil {
		return nil, nil, nil, fmt.Errorf("create isolated working directory: %w", tmpErr)
	}
	defer os.RemoveAll(tempDir)

	dict, project, loadErr := loadIsolated(composeYAML, envFile, tempDir)
	if loadErr != nil {
		// Any loader error -- malformed YAML, a schema violation, an
		// internal compose-go consistency check -- is surfaced as an
		// ordinary whole-file rejection reason rather than a Go error a
		// caller might treat differently (e.g. retry, or log to a
		// different channel): it's caused by the content of composeYAML,
		// not this package's environment, so it belongs in the same
		// reporting path as every other rejection.
		return nil, []Rejection{{Service: wholeFileRejectionService, Reason: loadErr.Error()}}, nil, nil
	}

	if reason, present := topLevelIncludePresent(dict); present {
		return nil, []Rejection{{Service: wholeFileRejectionService, Reason: reason}}, nil, nil
	}

	if len(project.Services) > maxServices {
		return nil, []Rejection{{
			Service: wholeFileRejectionService,
			Reason:  fmt.Sprintf("declares %d services, more than the maximum of %d allowed", len(project.Services), maxServices),
		}}, nil, nil
	}

	names := make([]string, 0, len(project.Services))
	for name := range project.Services {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic order regardless of map iteration

	reserved := reservedAliasSet(opts.ReservedAliases)
	for _, name := range names {
		svc := project.Services[name]

		if svc.Build != nil {
			skipped = append(skipped, Rejection{
				Service: name,
				Reason:  "services built from a Dockerfile are not supported yet -- run that service as an in-sandbox process, or provision it from a pre-built, allow-listed image instead",
			})
			continue
		}

		spec, reason := parseOneService(name, svc, tempDir, opts.AllowedImageRegistries, reserved, opts.RequireDigest, opts.MemoryCeiling)
		if reason != "" {
			rejected = append(rejected, Rejection{Service: name, Reason: reason})
			continue
		}
		services = append(services, spec)
	}

	return services, rejected, skipped, nil
}

// loadIsolated loads composeYAML through compose-go with every host-read
// avenue this package knows about closed off -- see the package doc
// comment for what each of these does and why it's needed. It returns both
// the raw model dict (topLevelIncludePresent needs it: `include:` is a
// top-level directive, not a field on types.ServiceConfig, so it's not
// observable on the typed Project the second return value gives) and the
// typed Project itself.
func loadIsolated(composeYAML []byte, envFile map[string]string, workingDir string) (map[string]any, *types.Project, error) {
	ctx := context.Background()
	configDetails := types.ConfigDetails{
		WorkingDir: workingDir,
		ConfigFiles: []types.ConfigFile{
			{Filename: "docker-compose.yml", Content: composeYAML},
		},
		Environment: types.Mapping(envFile),
	}

	optFuncs := []func(*loader.Options){func(o *loader.Options) {
		o.SkipInclude = true
		o.SkipExtends = true
		// Both default to reading env_file/label_file paths straight off
		// disk at load time -- confirmed empirically, see package doc
		// comment. EnvFiles and LabelFiles are rejected fields regardless,
		// but that check runs on the *result* of loading, which is too
		// late to stop this read.
		o.SkipResolveEnvironment = true
		o.SkipResolveLabels = true
		// This package validates each service's own fields, not whether
		// the file is a fully coherent, runnable compose project -- a
		// depends_on/volumes_from/network referencing an undeclared name
		// elsewhere in the same file is the real compose CLI's concern,
		// not a security one, and the old hand-walker never checked it
		// either. Left on, this failed the whole load (no per-service
		// detail at all) for otherwise-fine fixtures.
		o.SkipConsistencyCheck = true
		o.SetProjectName("composeservices", true)
		// Explicit, rather than relying on ToOptions' default of
		// configDetails.LookupEnv: makes the "only ever envFile, never
		// os.Environ()" guarantee visible at the call site instead of
		// resting on a compose-go default this package doesn't own.
		o.Interpolate = &interp.Options{
			LookupValue: func(key string) (string, bool) {
				v, ok := envFile[key]
				return v, ok
			},
		}
	}}

	dict, err := loader.LoadModelWithContext(ctx, configDetails, optFuncs...)
	if err != nil {
		return nil, nil, err
	}
	project, err := loader.ModelToProject(dict, loader.ToOptions(&configDetails, optFuncs), configDetails)
	if err != nil {
		return nil, nil, err
	}
	return dict, project, nil
}

// topLevelIncludePresent reports whether the raw model still carries an
// `include:` key after loading with SkipInclude -- confirmed empirically
// that SkipInclude prevents the referenced files from being read, but does
// not remove the key itself from the model.
func topLevelIncludePresent(dict map[string]any) (reason string, present bool) {
	if _, ok := dict["include"]; ok {
		return "\"include\" is never allowed -- it can load compose files from arbitrary host paths", true
	}
	return "", false
}

// baseReservedAliases are service names this package refuses regardless of
// any other field, or of what a caller's own Options.ReservedAliases adds
// to them -- a provisioner reaches the sandbox's other sidecars by exactly
// these names on the shared internal network, so a compose service
// claiming one would either collide outright or, via a Networks alias (see
// networkAliasReason below), let a target-repo-authored service impersonate
// a sidecar the worker is meant to trust unconditionally. "relay" and
// "worker" are reserved defensively even though neither is a literal alias
// in internal/sandbox today; the real sidecar aliases
// (internal/sandbox.ReservedWorkerAliases -- relayWorkerHost and
// registryProxyWorkerHost) arrive per-call via Options.ReservedAliases, see
// that field's own doc comment for why this package can't just import them
// directly.
var baseReservedAliases = map[string]bool{"relay": true, "worker": true}

// reservedAliasSet merges baseReservedAliases with a caller's own
// Options.ReservedAliases into the one set ParseFile checks against for
// this call.
func reservedAliasSet(extra []string) map[string]bool {
	set := make(map[string]bool, len(baseReservedAliases)+len(extra))
	for name := range baseReservedAliases {
		set[name] = true
	}
	for _, name := range extra {
		set[name] = true
	}
	return set
}

func reservedAliasReason(name string) string {
	return fmt.Sprintf("service name %q is reserved for the sandbox's own internal-network alias and may not be reused", name)
}

// networkAliasReason checks a service's `networks:` block for an alias
// that collides with reservedAliases -- without this, a service named
// something innocuous could still set `networks.<net>.aliases:
// [registry-proxy]` and have traffic meant for the real sidecar routed to
// it instead. Not explicitly called out in this package's original
// allow-list proposal (Networks was to be allow-listed opaquely), but
// following through on why reservedAliases exists at all makes this the
// same class of attack, so it gets the same check.
func networkAliasReason(networks map[string]*types.ServiceNetworkConfig, reserved map[string]bool) string {
	names := make([]string, 0, len(networks))
	for name := range networks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := networks[name]
		if cfg == nil {
			continue
		}
		for _, alias := range cfg.Aliases {
			if reserved[alias] {
				return fmt.Sprintf("network alias %q is reserved for the sandbox's own internal-network alias and may not be reused", alias)
			}
		}
	}
	return ""
}

// allowedDependsOnConditions are the three conditions the compose spec
// itself documents for `depends_on` -- anything else (an unknown string
// compose-go's own loader let through, or a future spec addition this
// package hasn't reviewed) is rejected rather than silently accepted, per
// this package's fail-closed allow-list philosophy for every other field.
var allowedDependsOnConditions = map[string]bool{
	types.ServiceConditionStarted:               true,
	types.ServiceConditionHealthy:               true,
	types.ServiceConditionCompletedSuccessfully: true,
}

// allowedFields are the exported types.ServiceConfig field names this
// package accepts. See parseOneService for how each is actually validated
// and copied into a ServiceSpec -- being allow-listed here only means "not
// rejected outright"; several (Volumes, Networks, Ports, Image) still get
// their own content checked.
var allowedFields = map[string]bool{
	"Name":            true, // structural: the compose map key, not document content
	"Image":           true,
	"Platform":        true, // only linux/amd64 and linux/arm64 are accepted
	"Environment":     true,
	"Command":         true,
	"Entrypoint":      true,
	"HealthCheck":     true,
	"DependsOn":       true,
	"Volumes":         true, // named volumes and tmpfs only; bind-mount source is rejected, see parseOneService
	"Hostname":        true,
	"Networks":        true, // accepted opaquely except for a reserved-alias check, see networkAliasReason
	"User":            true,
	"Tmpfs":           true,
	"ShmSize":         true,
	"ReadOnly":        true,
	"Expose":          true,
	"Restart":         true,
	"WorkingDir":      true,
	"Init":            true,
	"Ports":           true, // never published on the host; see ServiceSpec.PublishedPorts
	"StopSignal":      true, // trivially safe: at most a bad signal name, which docker itself refuses on stop
	"StopGracePeriod": true, // trivially safe: a duration, not a capability
	"Extensions":      true, // only the one narrow "x-bg-service-port" key is read; every other "x-*" key is still rejected, see decodeExtensions
	"MemLimit":        true, // accepted only at or below Options.MemoryCeiling, see memLimitReason
	"ContainerName":   true, // inert source metadata; never emitted by Synthesize
}

// rejectedFields are the exported types.ServiceConfig field names this
// package refuses outright when set to a non-default value: each one is
// either a real way to widen a container's privileges or escape the
// internal network this is meant to confine sidecars to, or a feature this
// package hasn't reviewed closely enough to allow through. "The field was
// set" is itself the rejection reason for the first group; for the second,
// rejecting is the conservative default until someone reviews it in.
//
// Build is the one field in this set that does not produce a Rejection --
// ParseFile special-cases it into skipped before this map is ever
// consulted, see ParseFile's own comment. It's still listed here (not in
// allowedFields) because "present and unreviewed for the allow-list" is
// true of it too; only the *consequence* differs.
var rejectedFields = map[string]string{
	// --- explicit widen-or-escape fields ---
	"Privileged":        "privileged containers are never allowed",
	"CapAdd":            "adding capabilities is never allowed",
	"CapDrop":           "dropping capabilities is not implemented yet -- TODO(compose services v1 follow-on): CapDrop only narrows a container's capabilities and could plausibly be allow-listed, but v1 keeps the allow-list minimal",
	"NetworkMode":       "a service may not opt out of the sandbox's own internal network",
	"Build":             "services built from a Dockerfile are skipped, not rejected -- see ParseFile",
	"Extends":           "\"extends\" is never allowed -- it can load compose files from arbitrary host paths",
	"SecurityOpt":       "overriding security options is never allowed",
	"Pid":               "sharing another container's or the host's PID namespace is never allowed",
	"Ipc":               "sharing another container's or the host's IPC namespace is never allowed",
	"Uts":               "sharing another container's or the host's UTS namespace is never allowed",
	"Devices":           "exposing host devices is never allowed",
	"DeviceCgroupRules": "changing device cgroup rules is never allowed",
	"Sysctls":           "changing kernel parameters is never allowed",
	"Ulimits":           "overriding resource ulimits is not implemented yet -- TODO(compose services v1 follow-on): these only ever narrow a limit, never grant one, but v1 keeps the allow-list minimal",
	"Labels":            "setting container labels is never allowed",
	"Logging":           "overriding the logging driver is never allowed",
	"DNS":               "overriding DNS servers is never allowed",
	"DNSOpts":           "overriding DNS options is never allowed",
	"DNSSearch":         "overriding the DNS search domain is never allowed",
	"Links":             "legacy container links are never allowed",
	"Runtime":           "overriding the container runtime is never allowed",
	"OomKillDisable":    "disabling the OOM killer is never allowed",
	"OomScoreAdj":       "adjusting the OOM score is never allowed",
	"CgroupParent":      "overriding the cgroup parent is never allowed",
	"Secrets":           "compose secrets are not implemented yet -- TODO(compose services v1 follow-on)",
	"Configs":           "compose configs are not implemented yet -- TODO(compose services v1 follow-on)",
	"EnvFiles":          "\"env_file\" is never allowed -- it reads an arbitrary host path; declare variables under \"environment\" instead",
	"Profiles":          "compose profiles are not implemented yet -- TODO(compose services v1 follow-on): this package always loads every service, so a profile-gated service would behave inconsistently with real compose",
	"ExternalLinks":     "linking to a container outside the sandbox is never allowed",
	"GroupAdd":          "adding supplementary groups is never allowed",
	"Deploy":            "the \"deploy\" block (Swarm/replica config) is not implemented yet -- TODO(compose services v1 follow-on)",
	"VolumesFrom":       "mounting another container's volumes is never allowed",
	"ExtraHosts":        "adding host entries is never allowed",
	"UserNSMode":        "overriding the user namespace is never allowed",

	// --- unreviewed / not needed: rejected as the conservative default ---
	"Annotations":    "container annotations are not implemented yet -- TODO(compose services v1 follow-on): unreviewed",
	"Attach":         "unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"Develop":        "compose's \"develop\" (watch) block is unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"BlkioConfig":    "block IO limits are unreviewed -- TODO(compose services v1 follow-on)",
	"Cgroup":         "overriding the cgroup namespace mode is unreviewed -- TODO(compose services v1 follow-on)",
	"CPUCount":       "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPUPercent":     "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPUPeriod":      "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPUQuota":       "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPURTPeriod":    "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPURTRuntime":   "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPUS":           "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CPUSet":         "pinning CPUs is unreviewed -- TODO(compose services v1 follow-on)",
	"CPUShares":      "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"CredentialSpec": "Windows credential specs are unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"Dockerfile":     "only meaningful alongside \"build\", which is skipped rather than read -- TODO(compose services v1 follow-on)",
	"DomainName":     "unreviewed and not needed -- TODO(compose services v1 follow-on): cosmetic like Hostname, but not requested, kept minimal",
	"Provider":       "compose \"provider\" services are unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"Gpus":           "GPU device passthrough is unreviewed -- TODO(compose services v1 follow-on)",
	"Isolation":      "overriding container isolation mode is unreviewed -- TODO(compose services v1 follow-on)",
	"LabelFiles":     "\"label_file\" is never allowed -- it reads an arbitrary host path, the same concern as env_file",
	"CustomLabels":   "internal compose-go bookkeeping, never actually populated from a compose file's own YAML -- rejected out of caution since it's never read by this package either",
	"LogDriver":      "overriding the logging driver is never allowed (legacy alias of \"logging\")",
	"LogOpt":         "overriding logging options is never allowed (legacy alias of \"logging\")",
	"MemReservation": "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"MemSwapLimit":   "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"MemSwappiness":  "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"MacAddress":     "overriding the container's MAC address is unreviewed -- TODO(compose services v1 follow-on)",
	"Models":         "compose \"models\" (AI model providers) are unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"Net":            "legacy/unused field, never populated by a real compose file -- rejected for completeness",
	"PidsLimit":      "resource limits are unreviewed -- TODO(compose services v1 follow-on): only ever narrows, could plausibly be allow-listed later",
	"PullPolicy":     "unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"Scale":          "replica count is unreviewed and not needed -- TODO(compose services v1 follow-on)",
	"StdinOpen":      "unreviewed and not needed -- TODO(compose services v1 follow-on): harmless, but kept minimal",
	"StorageOpt":     "storage driver options are unreviewed -- TODO(compose services v1 follow-on)",
	"Sysctl":         "changing kernel parameters is never allowed", // defensive: not a real field name, kept in case of a future rename
	"Tty":            "unreviewed and not needed -- TODO(compose services v1 follow-on): harmless, but kept minimal",
	"UseAPISocket":   "granting access to the Docker API socket is never allowed",
	"VolumeDriver":   "overriding the volume driver is unreviewed -- TODO(compose services v1 follow-on)",
	"PreStart":       "lifecycle hooks run arbitrary commands, unreviewed and never allowed",
	"PostStart":      "lifecycle hooks run arbitrary commands, unreviewed and never allowed",
	"PreStop":        "lifecycle hooks run arbitrary commands, unreviewed and never allowed",
}

// aliasPortExtensionKey is the one "x-*" extension key this package reads
// (see ServiceSpec.AliasPort's own doc comment for why). Every other
// extension key is rejected with extensionsRejectionReason below, exactly
// as the whole Extensions field once was -- this is not "allow all x-
// extensions," it is one specific, spelled-out key.
const aliasPortExtensionKey = "x-bg-service-port"

// extensionsRejectionReason is the reason a non-aliasPortExtensionKey "x-*"
// key is rejected with -- unchanged from Extensions' own reason before this
// package started reading aliasPortExtensionKey out of it.
const extensionsRejectionReason = "custom \"x-\" extension fields are unreviewed -- TODO(compose services v1 follow-on)"

func init() {
	// A field must be classified exactly once. This isn't the exhaustiveness
	// test itself (that one reflects over the live compose-go type, see
	// fields_test.go) -- it only guards against a typo inside this file
	// putting the same name in both maps.
	for name := range allowedFields {
		if _, dup := rejectedFields[name]; dup {
			panic(fmt.Sprintf("composeservices: field %q is in both allowedFields and rejectedFields", name))
		}
	}
}

var allowedPlatforms = map[string]bool{
	"linux/amd64": true,
	"linux/arm64": true,
}

func parseOneService(name string, svc types.ServiceConfig, workingDir string, allowedImageRegistries []string, reserved map[string]bool, requireDigest bool, memoryCeiling string) (ServiceSpec, string) {
	if reason := firstRejectedFieldPresent(svc); reason != "" {
		return ServiceSpec{}, reason
	}

	if reserved[name] {
		return ServiceSpec{}, reservedAliasReason(name)
	}
	if reason := networkAliasReason(svc.Networks, reserved); reason != "" {
		return ServiceSpec{}, reason
	}

	spec := ServiceSpec{Name: name, Image: svc.Image, Hostname: svc.Hostname}

	if svc.Platform != "" {
		if !allowedPlatforms[svc.Platform] {
			return ServiceSpec{}, fmt.Sprintf("platform %q is not allow-listed (only linux/amd64 and linux/arm64 are supported)", svc.Platform)
		}
		spec.Platform = svc.Platform
	}

	if spec.Image == "" {
		return ServiceSpec{}, "missing required \"image\""
	}
	if !imageAllowed(spec.Image, allowedImageRegistries) {
		canonical := canonicalizeImageRef(spec.Image)
		return ServiceSpec{}, fmt.Sprintf("image %q is not under an allow-listed registry/namespace -- add %q to compose_services_allowed_registries, or use an image under one already listed", spec.Image, canonical[:strings.LastIndex(canonical, "/")+1])
	}
	if requireDigest && !imageDigestPinned(spec.Image) {
		return ServiceSpec{}, fmt.Sprintf("image %q is not pinned by digest (compose_services_require_digest is set) -- rewrite it as \"<image>@sha256:<digest>\", found via `docker pull <image>` or the registry's own manifest API", spec.Image)
	}

	if svc.MemLimit != 0 {
		if reason := memLimitReason(int64(svc.MemLimit), memoryCeiling); reason != "" {
			return ServiceSpec{}, reason
		}
		spec.MemLimit = int64(svc.MemLimit)
	}

	env, reason := decodeEnvironment(svc.Environment)
	if reason != "" {
		return ServiceSpec{}, reason
	}
	spec.Environment = env

	spec.Ports = make([]string, 0, len(svc.Ports))
	for _, p := range svc.Ports {
		spec.Ports = append(spec.Ports, fmt.Sprintf("%d", p.Target))
		if p.Published == "" || (p.Protocol != "" && p.Protocol != "tcp") {
			continue
		}
		host, err := strconv.Atoi(p.Published)
		if err != nil || host < 1 || host > 65535 || p.Target < 1 || p.Target > 65535 {
			return ServiceSpec{}, fmt.Sprintf("port %q:%d is not a single published TCP port", p.Published, p.Target)
		}
		spec.PublishedPorts = append(spec.PublishedPorts, PublishedPort{Host: host, Target: int(p.Target)})
	}

	aliasPort, reason := decodeExtensions(svc.Extensions)
	if reason != "" {
		return ServiceSpec{}, reason
	}
	spec.AliasPort = aliasPort

	// svc.Command/svc.Entrypoint are compose-go's ShellCommand, whose own
	// nil-vs-non-nil distinction is load-bearing (nil means "not set", a
	// non-nil empty slice means "explicitly cleared" -- see ShellCommand's
	// own doc comment in compose-go): copying only when non-nil, rather
	// than the unconditional `append([]string(nil), svc.Command...)` this
	// replaced, is what preserves that distinction instead of collapsing
	// an explicit `command: []`/`entrypoint: []` down to the same nil as
	// an absent field.
	if svc.Command != nil {
		spec.Command = append([]string{}, svc.Command...)
	}
	if svc.Entrypoint != nil {
		spec.Entrypoint = append([]string{}, svc.Entrypoint...)
	}

	if svc.HealthCheck != nil {
		spec.Healthcheck = decodeHealthcheck(svc.HealthCheck)
	}

	depNames := make([]string, 0, len(svc.DependsOn))
	for dep := range svc.DependsOn {
		depNames = append(depNames, dep)
	}
	sort.Strings(depNames) // deterministic order regardless of the source map's own iteration order
	spec.DependsOn = make([]ServiceDependency, 0, len(depNames))
	for _, dep := range depNames {
		condition := svc.DependsOn[dep].Condition
		if !allowedDependsOnConditions[condition] {
			return ServiceSpec{}, fmt.Sprintf("service %q depends on %q with unsupported condition %q", name, dep, condition)
		}
		spec.DependsOn = append(spec.DependsOn, ServiceDependency{Name: dep, Condition: condition})
	}

	vols, reason := decodeNamedVolumes(svc.Volumes, workingDir)
	if reason != "" {
		return ServiceSpec{}, reason
	}
	spec.NamedVolumes = vols

	spec.User = svc.User
	spec.WorkingDir = svc.WorkingDir
	spec.ReadOnly = svc.ReadOnly
	spec.ShmSize = int64(svc.ShmSize)
	spec.Init = svc.Init != nil && *svc.Init
	if len(svc.Tmpfs) > 0 {
		spec.Tmpfs = append([]string(nil), svc.Tmpfs...)
	}

	return spec, ""
}

// firstRejectedFieldPresent walks rejectedFields in a fixed order and
// returns the reason for the first one set to a non-default value on svc,
// or "" if none are. Using reflect.Value.IsZero generically here (rather
// than a hand-written non-default check per field) is what keeps this in
// sync automatically as rejectedFields grows -- the alternative, a
// per-field switch, is exactly the kind of thing that silently stops
// covering a field when someone adds a new entry to the map and forgets
// the switch case.
func firstRejectedFieldPresent(svc types.ServiceConfig) string {
	v := reflect.ValueOf(svc)
	t := v.Type()
	// Build excepted: ParseFile handles it (skip, not reject) before this
	// is ever called.
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		if name == "Build" {
			continue
		}
		reason, isRejected := rejectedFields[name]
		if !isRejected {
			continue
		}
		if !v.Field(i).IsZero() {
			return reason
		}
	}
	return ""
}

// memLimitReason rejects a service's own mem_limit (in bytes) unless it is
// positive and at or below the operator's ceiling: repo content may only
// narrow a sidecar's containment, never widen it.
func memLimitReason(memLimit int64, ceiling string) string {
	if memLimit < 0 {
		return fmt.Sprintf("mem_limit %d is negative", memLimit)
	}
	ceilingBytes, err := MemoryBytes(ceiling)
	if err != nil {
		return fmt.Sprintf("mem_limit is set but the operator's compose_services_memory %q is not a usable ceiling", ceiling)
	}
	if memLimit > ceilingBytes {
		return fmt.Sprintf("mem_limit %s is above the operator's compose_services_memory %s -- lower it, or raise compose_services_memory", units.BytesSize(float64(memLimit)), ceiling)
	}
	return ""
}

func imageAllowed(image string, allowedImageRegistries []string) bool {
	if len(allowedImageRegistries) == 0 {
		return false // fail closed: no configured allow-list means nothing is allowed
	}
	canonical := canonicalizeImageRef(image)
	for _, prefix := range allowedImageRegistries {
		if strings.HasPrefix(canonical, prefix) {
			return true
		}
	}
	return false
}

// canonicalizeImageRef applies Docker's own implicit-registry convention so
// an allow-list entry like "docker.io/library/" matches the bare
// references real compose files actually write ("postgres:16.4"), not just
// the fully-qualified form nothing writes by hand -- mirrors `docker pull`'s
// own resolution rules closely enough for allow-list matching (not a full
// reference parser: tags/digests are left as-is, only the registry/
// namespace prefix is normalized).
func canonicalizeImageRef(image string) string {
	firstSlash := strings.Index(image, "/")
	if firstSlash == -1 {
		// No "/" at all ("postgres:16.4") -- an official Docker Hub image.
		return "docker.io/library/" + image
	}
	firstComponent := image[:firstSlash]
	looksLikeRegistryHost := strings.Contains(firstComponent, ".") || strings.Contains(firstComponent, ":") || firstComponent == "localhost"
	if looksLikeRegistryHost {
		return image // already fully qualified, e.g. "ghcr.io/org/img" or "registry:5000/img"
	}
	// A namespace with no registry host ("bitnami/postgresql") -- also
	// Docker Hub, just not the "library/" official-images namespace.
	return "docker.io/" + image
}

// imageDigestPinnedPattern matches a trailing "@sha256:<64 hex chars>" --
// the same digest form every sandbox/relay/registry-proxy image this
// codebase launches already requires unconditionally (imageDigest in
// internal/sandbox/docker.go cuts the same suffix off a Validate'd
// LaunchSpec.Image). A tag ("postgres:16.4") or a bare image name never
// matches, regardless of whether a tag is also present alongside the
// digest ("postgres:16.4@sha256:...", which Docker itself accepts and
// resolves by digest, ignoring the tag).
var imageDigestPinnedPattern = regexp.MustCompile(`@sha256:[0-9a-fA-F]{64}$`)

func imageDigestPinned(image string) bool {
	return imageDigestPinnedPattern.MatchString(image)
}

// decodeEnvironment converts compose-go's already-interpolated
// MappingWithEquals into a plain map, rejecting the "bare name, no value"
// form (`environment: [FOO]`, decoded as a nil value) that means "pass
// this through from whatever environment the container runtime is invoked
// in" -- this package's provisioner has no such ambient environment to
// pass through, and silently turning it into an empty string would hide
// that from both the model and the operator.
//
// A literal "$" in a value survives here as a literal "$": compose-go's
// own interpolation step de-escapes "$$" to "$" before this package ever
// sees the value (confirmed empirically: `POSTGRES_PASSWORD: $$ecret` in
// the source YAML decodes to the Go string "$ecret"). A downstream
// synthesizer that writes ServiceSpec.Environment back out to a new
// compose YAML file must re-escape "$" as "$$", or a value containing "$"
// will be misinterpreted as an interpolation reference on the next load.
func decodeEnvironment(env types.MappingWithEquals) (map[string]string, string) {
	out := make(map[string]string, len(env))
	names := make([]string, 0, len(env))
	for k := range env {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		v := env[k]
		if v == nil {
			return nil, fmt.Sprintf("invalid \"environment\": entry %q has no value (pass-through from the caller's own environment is not supported)", k)
		}
		out[k] = *v
	}
	return out, ""
}

// decodeExtensions validates a service's `x-*` extension fields against the
// one narrow exception this package allows (aliasPortExtensionKey): any
// other key present rejects the service with extensionsRejectionReason,
// exactly as the old blanket Extensions rejection did before this package
// started reading aliasPortExtensionKey out of it. aliasPort is 0 when the
// key is absent.
func decodeExtensions(ext types.Extensions) (aliasPort int, reason string) {
	names := make([]string, 0, len(ext))
	for k := range ext {
		names = append(names, k)
	}
	sort.Strings(names) // deterministic order regardless of the source map's own iteration order
	for _, k := range names {
		if k != aliasPortExtensionKey {
			return 0, extensionsRejectionReason
		}
		port, ok := asPort(ext[k])
		if !ok {
			return 0, fmt.Sprintf("%q must be an integer TCP port between 1 and 65535, got %v", aliasPortExtensionKey, ext[k])
		}
		aliasPort = port
	}
	return aliasPort, ""
}

// asPort converts a decoded YAML scalar to an int TCP port, accepting only
// whole numbers in the valid port range. yaml.v3 decodes a plain integer
// scalar into Go's "any" as int (confirmed empirically for this package's
// isolated loader); int64/uint64/float64 are also accepted defensively in
// case a future compose-go version decodes differently, but a string
// (e.g. a quoted "19092") or any other type is rejected -- the field must
// be a real YAML integer, not a string that merely looks like one.
func asPort(v any) (int, bool) {
	var n int64
	switch t := v.(type) {
	case int:
		n = int64(t)
	case int64:
		n = t
	case uint64:
		n = int64(t)
	case float64:
		if t != math.Trunc(t) {
			return 0, false
		}
		n = int64(t)
	default:
		return 0, false
	}
	if n < 1 || n > 65535 {
		return 0, false
	}
	return int(n), true
}

func decodeHealthcheck(hc *types.HealthCheckConfig) *Healthcheck {
	if hc.Disable {
		// Test/Interval/Timeout/Retries are ignored here even if the
		// source file also set them alongside disable:true -- Compose
		// itself treats disable:true as authoritative over any test.
		return &Healthcheck{Disabled: true}
	}
	out := &Healthcheck{Test: append([]string(nil), hc.Test...)}
	if hc.Interval != nil {
		out.Interval = hc.Interval.String()
	}
	if hc.Timeout != nil {
		out.Timeout = hc.Timeout.String()
	}
	if hc.Retries != nil {
		out.Retries = int(*hc.Retries)
	}
	return out
}

// decodeNamedVolumes keeps each named volume, tmpfs, or normalized relative
// bind mount's target path plus, for a named volume, its own source name (see
// VolumeMount's own doc comment on why the source matters). Bind sources are
// relative to compose-go's isolated working directory here; the lifecycle
// later materializes that path from the immutable base commit. Image mounts
// remain rejected as unreviewed.
func decodeNamedVolumes(vols []types.ServiceVolumeConfig, workingDir string) ([]VolumeMount, string) {
	out := make([]VolumeMount, 0, len(vols))
	for _, v := range vols {
		switch v.Type {
		case "volume":
			out = append(out, VolumeMount{Type: "volume", Source: v.Source, Target: v.Target, ReadOnly: v.ReadOnly})
		case "tmpfs":
			out = append(out, VolumeMount{Type: "tmpfs", Target: v.Target})
		case "bind":
			if workingDir == "" {
				return nil, fmt.Sprintf("volume %q is a bind-mount without an isolated compose working directory", v.Source+":"+v.Target)
			}
			if v.Bind != nil && (v.Bind.SELinux != "" || v.Bind.Propagation != "" || v.Bind.Recursive != "" || len(v.Bind.Extensions) != 0) {
				return nil, fmt.Sprintf("volume %q uses unsupported bind-mount options", v.Source+":"+v.Target)
			}
			if v.Source == "" || !filepath.IsAbs(v.Source) {
				return nil, fmt.Sprintf("volume %q is not a normalized relative bind source", v.Source+":"+v.Target)
			}
			rel, err := filepath.Rel(workingDir, v.Source)
			if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Sprintf("volume %q is outside the compose project directory", v.Source+":"+v.Target)
			}
			rel = filepath.ToSlash(filepath.Clean(rel))
			for _, part := range strings.Split(rel, "/") {
				if part == ".git" {
					return nil, fmt.Sprintf("volume %q may not expose git metadata", v.Source+":"+v.Target)
				}
			}
			out = append(out, VolumeMount{Type: "bind", Source: rel, Target: v.Target, ReadOnly: v.ReadOnly})
		default:
			return nil, fmt.Sprintf("volume type %q is not in the allow-list (only named volumes and tmpfs mounts are)", v.Type)
		}
	}
	return out, ""
}
