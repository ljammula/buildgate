package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"buildgate/internal/run"
)

// See internal/registryproxy's own package doc comment for the full design.
// This file is the Docker launch/teardown half; ReconcileRelayOrphans
// (relay.go) reaps what a crash leaves behind. The registry
// proxy injects no credential (every upstream is a public, unauthenticated
// registry), meters no budget, and fronts several fixed upstream hosts
// instead of one operator-chosen one.

const (
	registryProxyContainerPort = 8092
	registryProxyWorkerHost    = "registry-proxy"

	// registryProxyEgressNetworkName mirrors relayEgressNetworkName exactly
	// (see its own doc comment): the one persistent, non-internal network
	// every registry-proxy container's real internet egress goes through,
	// kept separate from relayEgressNetworkName so a registry-proxy
	// container is never reachable from whatever else attaches to the
	// relay's own egress network, and vice versa.
	registryProxyEgressNetworkName = "factoryd-registryproxy-egress"

	// Fixed, well-known route prefixes: RegistryProxyPolicy.Routes may name
	// any prefix, but only these are recognized by
	// RegistryProxyLifecycle.PrepareWorker to set the matching package
	// manager's own registry environment variable (see its own doc
	// comment). An operator-configured route using a different prefix still
	// works as a proxied route, it just needs its own manual env wiring in
	// the worker command/script.
	npmRoutePrefix  = "/npm/"
	pypiRoutePrefix = "/pypi/"
	// pypiFilesRoutePrefix fronts PyPI's own file CDN directly: pypi.org's
	// simple index never serves file bytes itself, only HTML/JSON pages
	// naming files hosted at files.pythonhosted.org -- see pypiRoutePrefix's
	// RewriteHrefHosts entry in DefaultRegistryProxyRoutes, which rewrites
	// those absolute links into ones under this prefix so a client that
	// follows them stays inside this proxy.
	pypiFilesRoutePrefix = "/pypi-files/"
	goProxyRoutePrefix   = "/gomodproxy/"
	// goSumDBRoutePrefix implements the Go module proxy protocol's own
	// checksum-database passthrough (https://go.dev/ref/mod#goproxy-protocol):
	// when GOSUMDB names a real database (its default, "sum.golang.org") and
	// GOPROXY points at an actual proxy URL, the go command itself requests
	// sumdb data at "<GOPROXY>/sumdb/<sumdb-name>/<rest>" instead of dialing
	// the database directly -- no separate env var is needed to enable this,
	// only a route that actually serves it. Deliberately a MORE specific
	// prefix than goProxyRoutePrefix and listed before it in
	// DefaultRegistryProxyRoutes (internal/registryproxy.Server.matchRoute
	// returns the first configured route whose prefix matches, so order is
	// what lets this narrower prefix win over the general module-proxy one
	// it's nested under). Found via review: an earlier version of this
	// package set GOSUMDB=off instead, disabling checksum verification
	// entirely rather than actually proxying it -- this route is what makes
	// GOSUMDB=sum.golang.org (RegistryProxyLifecycle.PrepareWorker's actual
	// default) work through a worker with no other route to the internet.
	goSumDBRoutePrefix = "/gomodproxy/sumdb/sum.golang.org/"
)

// RegistryProxyRoute is the credential-free, plain-data description of one
// proxied route -- a CLI-flag-friendly counterpart to
// internal/registryproxy.Route (which carries a parsed *url.URL and isn't
// itself a natural fit for a Docker container's argv).
type RegistryProxyRoute struct {
	Prefix string `json:"prefix"`
	// Upstream must be an absolute http(s) URL with no user info, query, or
	// fragment -- every registry this proxy fronts is public and
	// unauthenticated, so there is no credential to ever carry here.
	Upstream string `json:"upstream"`
	// AllowedHosts is host[:port] authorities (see
	// internal/registryproxy.Route.AllowedHosts) this route's upstream may
	// redirect to, beyond its own host. An entry may also carry a
	// "->/local/prefix/" suffix (cmd/registry-proxy's own -route flag
	// syntax, parsed there into internal/registryproxy.Route.
	// RewriteHrefHosts): PyPI's simple index needs this to rewrite its own
	// file links, which always point at files.pythonhosted.org, not
	// pypi.org itself.
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
}

// DefaultRegistryProxyRoutes returns the documented ecosystems (npm, PyPI,
// Go module proxy, and the Go checksum database's own proxy passthrough)
// pointed at their real public upstreams -- cmd/factoryd's -registry-proxy
// defaults to exactly this when an operator doesn't override individual
// upstreams. goSumDBRoutePrefix is listed before goProxyRoutePrefix
// deliberately -- see that constant's own doc comment for why the order
// matters.
func DefaultRegistryProxyRoutes() []RegistryProxyRoute {
	return []RegistryProxyRoute{
		{Prefix: npmRoutePrefix, Upstream: "https://registry.npmjs.org"},
		// "files.pythonhosted.org", not "files.pythonhosted.org:443": a
		// real redirect Location from pypi.org carries no explicit port
		// for its own default https scheme, and matching is by exact
		// host[:port] authority (internal/registryproxy.Route.
		// AllowedHosts's own doc comment) -- an explicit ":443" here would
		// never actually match a real PyPI redirect.
		{Prefix: pypiRoutePrefix, Upstream: "https://pypi.org/simple", AllowedHosts: []string{"files.pythonhosted.org->" + pypiFilesRoutePrefix}},
		{Prefix: pypiFilesRoutePrefix, Upstream: "https://files.pythonhosted.org"},
		{Prefix: goSumDBRoutePrefix, Upstream: "https://sum.golang.org"},
		// proxy.golang.org serves a large module zip as a 302 to a signed
		// storage.googleapis.com URL (a ~45 MB google.golang.org/api zip
		// does this; a small zip is served inline). Without the redirect
		// host allowlisted, checkRedirect refuses it and the worker sees
		// a 502 for exactly the modules big enough to matter -- found
		// live 2026-09-10 on the first real submit-to-PR run through the
		// proxy, where `go vet` on a real target repo stalled on that one
		// zip. A plain redirect target, no href rewriting: the go command
		// only ever follows the Location the proxy already resolved.
		{Prefix: goProxyRoutePrefix, Upstream: "https://proxy.golang.org", AllowedHosts: []string{"storage.googleapis.com"}},
	}
}

// RegistryProxyRoutesWithUpstreams is DefaultRegistryProxyRoutes with each
// route's upstream replaced by the operator's own (cmd/factoryd's
// -registry-proxy-*-upstream flags, for a corporate mirror). The PyPI
// index route's file-link rewrite follows the pypiFiles host, as before.
// A route whose upstream is left at the public default keeps that
// default's redirect allowlist; a mirror gets none, since where it
// redirects is unknown here. One place, not a second hand-written copy
// of the route table in cmd/factoryd: the previous copy there silently
// dropped the Go route's storage.googleapis.com redirect host the moment
// it was added to the defaults (found live 2026-09-10, second run).
func RegistryProxyRoutesWithUpstreams(npm, pypi, pypiFiles, goProxy, goSumDB string) ([]RegistryProxyRoute, error) {
	pypiFilesURL, err := url.Parse(pypiFiles)
	if err != nil {
		return nil, fmt.Errorf("pypi files upstream: %w", err)
	}
	routes := DefaultRegistryProxyRoutes()
	for i := range routes {
		var upstream string
		switch routes[i].Prefix {
		case npmRoutePrefix:
			upstream = npm
		case pypiRoutePrefix:
			upstream = pypi
			routes[i].AllowedHosts = []string{pypiFilesURL.Host + "->" + pypiFilesRoutePrefix}
		case pypiFilesRoutePrefix:
			upstream = pypiFiles
		case goProxyRoutePrefix:
			upstream = goProxy
		case goSumDBRoutePrefix:
			upstream = goSumDB
		}
		if upstream != routes[i].Upstream && routes[i].Prefix != pypiRoutePrefix {
			routes[i].AllowedHosts = nil
		}
		routes[i].Upstream = upstream
	}
	return routes, nil
}

// RegistryProxyPolicy is the complete, factory-owned configuration for one
// run's registry proxy: which image to run, which upstreams it may front,
// and its cache/concurrency limits. Unlike RoutePolicy there is no
// credential-bearing counterpart -- every field here is safe to log or
// persist as-is.
type RegistryProxyPolicy struct {
	Image                 string               `json:"image"`
	Routes                []RegistryProxyRoute `json:"routes"`
	CacheBytes            int64                `json:"cache_bytes"`
	MaxObjectBytes        int64                `json:"max_object_bytes"`
	MaxConcurrentUpstream int                  `json:"max_concurrent_upstream"`
	UpstreamTimeout       time.Duration        `json:"upstream_timeout"`
}

// Validate rejects a mutable image, an empty or malformed route table, or a
// disabled/unbounded cache limit -- before Docker is ever invoked.
func (p RegistryProxyPolicy) Validate() error {
	if _, ok := relayImageDigest(p.Image); !ok {
		return errors.New("registry proxy image must be pinned by a sha256 digest")
	}
	if len(p.Routes) == 0 {
		return errors.New("registry proxy requires at least one route")
	}
	seen := map[string]bool{}
	for _, route := range p.Routes {
		if route.Prefix == "" || !strings.HasPrefix(route.Prefix, "/") || !strings.HasSuffix(route.Prefix, "/") {
			return fmt.Errorf("registry proxy route prefix %q must start and end with /", route.Prefix)
		}
		if seen[route.Prefix] {
			return fmt.Errorf("registry proxy route prefix %q is configured more than once", route.Prefix)
		}
		seen[route.Prefix] = true
		if strings.ContainsAny(route.Prefix, "\x00\r\n\t ,") {
			return fmt.Errorf("registry proxy route prefix %q contains an unsafe character", route.Prefix)
		}
		u, err := url.Parse(route.Upstream)
		if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("registry proxy route %q upstream must be an absolute HTTP(S) URL without credentials or query data", route.Prefix)
		}
		if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
			return fmt.Errorf("registry proxy route %q upstream scheme must be http or https", route.Prefix)
		}
		for _, host := range route.AllowedHosts {
			if strings.ContainsAny(host, "\x00\r\n\t ,") {
				return fmt.Errorf("registry proxy route %q allowed host %q contains an unsafe character", route.Prefix, host)
			}
		}
	}
	if p.CacheBytes <= 0 {
		return errors.New("registry proxy cache size must be positive")
	}
	if p.MaxObjectBytes <= 0 || p.MaxObjectBytes > p.CacheBytes {
		return errors.New("registry proxy max object size must be positive and not exceed the cache size")
	}
	if p.MaxConcurrentUpstream <= 0 {
		return errors.New("registry proxy max concurrent upstream requests must be positive")
	}
	if p.UpstreamTimeout <= 0 {
		return errors.New("registry proxy upstream timeout must be positive")
	}
	return nil
}

// RegistryProxySpec is one launch's complete configuration: the
// request-scoped policy plus the run identity its container and network are
// labeled with.
type RegistryProxySpec struct {
	RegistryProxyPolicy
	RunID   string
	DataDir string
	// ExistingInternalNetwork, when set, is an already-created per-run
	// --internal network (this run's relay's own -- see
	// RouteLaunchFacts.NetworkName) that LaunchRegistryProxy attaches the
	// registry-proxy container to instead of creating a second one of its
	// own. No launch site passes one now. Empty (the default)
	// creates a dedicated internal network exactly as before this field
	// existed. Set by cmd/factoryd's own orchestration
	// (sandbox_exec.go), never by an operator flag directly -- it names a
	// network created moments earlier in the same
	// process, not something a caller could supply out of thin air.
	ExistingInternalNetwork string
	// CABundlePath, when set, is a host-side PEM file (e.g. a corporate
	// TLS-interception CA) bind-mounted read-only into the registry-proxy
	// container -- see egressCABundleDockerArgs and RouteSpec.CABundlePath's
	// own doc comment (this mirrors it exactly: a host-side, per-Worker
	// fact, never carried on RegistryProxyPolicy or in Workflow input).
	CABundlePath string
}

// Spec assembles a launchable RegistryProxySpec, mirroring RoutePolicy.Spec.
func (p RegistryProxyPolicy) Spec(runID, dataDir string) RegistryProxySpec {
	return RegistryProxySpec{RegistryProxyPolicy: p, RunID: runID, DataDir: dataDir}
}

// Validate adds the run identity checks to the embedded policy's own.
func (s RegistryProxySpec) Validate() error {
	if err := s.RegistryProxyPolicy.Validate(); err != nil {
		return err
	}
	if !run.ValidID(s.RunID) {
		return errors.New("registry proxy run id is required and must be a valid run id")
	}
	if strings.ContainsAny(s.RunID, "\x00\r\n\t") {
		return errors.New("registry proxy run id contains an unsafe control character")
	}
	if s.DataDir == "" || !strings.HasPrefix(s.DataDir, "/") {
		return errors.New("registry proxy data directory is required and must be absolute")
	}
	if s.ExistingInternalNetwork != "" {
		if strings.ContainsAny(s.ExistingInternalNetwork, "\x00\r\n\t ") {
			return errors.New("registry proxy existing internal network name contains an unsafe character")
		}
		// Rejected the same way LaunchSpec.Validate rejects a worker
		// pointed at either egress network (see its own doc comment): an
		// egress network has a real route to the internet, so attaching
		// the registry-proxy container to it as though it were the
		// shared per-run internal network would defeat this whole
		// mechanism's own isolation.
		if s.ExistingInternalNetwork == relayEgressNetworkName || s.ExistingInternalNetwork == registryProxyEgressNetworkName {
			return errors.New("registry proxy existing internal network must not be an egress network")
		}
	}
	if s.CABundlePath != "" {
		if err := ValidateEgressCABundle(s.CABundlePath); err != nil {
			return err
		}
	}
	return nil
}

// RegistryProxyLaunchFacts contains only safe-to-persist lifecycle
// metadata -- there is no credential to ever exclude, but the shape mirrors
// RouteLaunchFacts for consistency with how a caller already records relay
// evidence.
type RegistryProxyLaunchFacts struct {
	Image         string
	ImageDigest   string
	Hosts         []string
	NetworkName   string
	NetworkID     string
	ContainerName string
	ContainerID   string
	WorkerBaseURL string
	StartedAt     time.Time
}

// RegistryProxyHandle owns one started registry-proxy container and,
// unless it is sharing a relay's own network (ownsNetwork false -- see
// RegistryProxySpec.ExistingInternalNetwork), its internal network too.
// Cleanup is explicit.
type RegistryProxyHandle struct {
	WorkerBaseURL string
	AuditFacts    RegistryProxyLaunchFacts

	dockerBinary  string
	networkName   string
	containerName string
	// ownsNetwork is false when networkName is a relay's own internal
	// network this container only ever attached to (RegistryProxySpec.
	// ExistingInternalNetwork was set) -- Cleanup must never attempt to
	// remove a network it doesn't own, both because that network's
	// lifecycle belongs entirely to its creator and because attempting
	// to remove it here could race the creator's own teardown of the
	// same network.
	ownsNetwork bool
	// caBundleCleanup removes the staged CA bundle --
	// see stageEgressCABundle's doc comment. nil when no bundle was
	// configured.
	caBundleCleanup func()
}

// Facts returns a copy of the audit-safe launch facts.
func (h *RegistryProxyHandle) Facts() RegistryProxyLaunchFacts {
	if h == nil {
		return RegistryProxyLaunchFacts{}
	}
	return h.AuditFacts
}

// LaunchRegistryProxy starts a detached registry-proxy container on
// registryProxyEgressNetworkName (its only route to the real upstream
// registries) and attaches it, under the stable worker-visible alias
// registry-proxy, to either a fresh internal network it creates and owns
// (the default), or -- when spec.ExistingInternalNetwork is set -- an
// already-existing internal network it only attaches to, never creates or
// removes (see that field's own doc comment: this is how one run composes
// a registry proxy with a model relay while the worker still only ever
// joins one network). The worker itself is expected to join only that one
// internal network -- see RegistryProxyLifecycle.PrepareWorker.
func LaunchRegistryProxy(ctx context.Context, dockerBinary string, spec RegistryProxySpec) (*RegistryProxyHandle, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if dockerBinary == "" {
		dockerBinary = "docker"
	}
	if err := ensureRegistryProxyEgressNetwork(ctx, dockerBinary); err != nil {
		return nil, fmt.Errorf("ensure registry proxy egress network: %w", err)
	}
	caBundlePath, caBundleCleanup, err := stageEgressCABundle(spec.CABundlePath, spec.DataDir)
	if err != nil {
		return nil, fmt.Errorf("stage egress CA bundle: %w", err)
	}

	suffix := relayUniqueSuffix()
	containerName := "factoryd-registryproxy-container-" + suffix

	ownsNetwork := spec.ExistingInternalNetwork == ""
	networkName := spec.ExistingInternalNetwork
	if ownsNetwork {
		networkName = "factoryd-registryproxy-" + suffix
	}

	handle := &RegistryProxyHandle{
		dockerBinary:    dockerBinary,
		networkName:     networkName,
		containerName:   containerName,
		ownsNetwork:     ownsNetwork,
		caBundleCleanup: caBundleCleanup,
	}

	if ownsNetwork {
		networkArgs := []string{
			"network", "create", "--internal",
			"--label", labelKey("owner") + "=factoryd",
			"--label", labelKey("registryproxy") + "=true",
			"--label", labelKey("data-dir") + "=" + dataDirLabel(spec.DataDir),
		}
		if spec.RunID != "" {
			networkArgs = append(networkArgs, "--label", labelKey("run")+"="+spec.RunID)
		}
		networkArgs = append(networkArgs, networkName)
		networkOutput, err := runRelayDocker(ctx, dockerBinary, "create registry proxy network", networkArgs...)
		if err != nil {
			// cleanupNetwork alone (see below) never runs caBundleCleanup
			// for a shared-network handle, but this call is reachable only
			// when ownsNetwork is true (this whole block is guarded on
			// it), so cleanupNetwork always runs it here regardless.
			return nil, errors.Join(err, handle.cleanupNetwork(ctx))
		}
		if networkID := strings.TrimSpace(string(networkOutput)); networkID != "" {
			handle.AuditFacts.NetworkID = networkID
		}
	}
	// !ownsNetwork: networkName already exists -- created
	// moments earlier in the same process (see
	// RegistryProxySpec.ExistingInternalNetwork's own doc comment). Nothing
	// to create here; the `network connect` call below attaches this
	// container to it the same way either way.

	containerArgs := []string{
		"run", "--detach", "--rm", "--name", containerName,
		"--label", labelKey("owner") + "=factoryd",
		"--label", labelKey("registryproxy") + "=true",
		"--label", labelKey("data-dir") + "=" + dataDirLabel(spec.DataDir),
		"--network", registryProxyEgressNetworkName,
		// A restricted profile set here, not left to the
		// image's own Dockerfile USER directive.
		"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--user", "65532:65532",
		"--pids-limit", "128", "--memory", "512m", "--cpus", "1",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
	}
	if spec.RunID != "" {
		containerArgs = append(containerArgs, "--label", labelKey("run")+"="+spec.RunID)
	}
	if !ownsNetwork {
		// Read by ReconcileRelayOrphans (relay.go) to decide whether a
		// crash-recovered copy of this container ever owned a network of
		// its own to reclaim -- see removeRelayContainerAndNetwork's own
		// doc comment. Omitted entirely (never set to "false") for an
		// own-network launch, so a container predating this label and a
		// standalone launch both read identically as "not shared."
		containerArgs = append(containerArgs, "--label", labelKey("registryproxy-shared-network")+"=true")
	}
	// The cache lives on its own tmpfs, sized to the configured cache
	// bytes (rounded up to whole MiB, with a floor so a tiny configured
	// cache still gets a usable mount) -- --read-only above means /cache
	// must be an explicit writable mount, never the container's own
	// read-only root.
	cacheMiB := (spec.CacheBytes + (1 << 20) - 1) >> 20
	if cacheMiB < 1 {
		cacheMiB = 1
	}
	containerArgs = append(containerArgs, "--tmpfs", fmt.Sprintf("/cache:rw,noexec,nosuid,size=%dm", cacheMiB))
	containerArgs = append(containerArgs, egressCABundleDockerArgs(caBundlePath)...)

	containerArgs = append(containerArgs,
		spec.Image,
		"-addr", ":"+strconv.Itoa(registryProxyContainerPort),
		"-cache-dir", "/cache",
		"-max-cache-bytes", strconv.FormatInt(spec.CacheBytes, 10),
		"-max-object-bytes", strconv.FormatInt(spec.MaxObjectBytes, 10),
		"-max-concurrent-upstream", strconv.Itoa(spec.MaxConcurrentUpstream),
		"-upstream-timeout", spec.UpstreamTimeout.String(),
	)
	var hosts []string
	for _, route := range spec.Routes {
		routeArg := route.Prefix + "=" + route.Upstream
		if len(route.AllowedHosts) > 0 {
			routeArg += "," + strings.Join(route.AllowedHosts, ",")
		}
		containerArgs = append(containerArgs, "-route", routeArg)
		if u, err := url.Parse(route.Upstream); err == nil {
			hosts = append(hosts, u.Host)
		}
		for _, allowed := range route.AllowedHosts {
			// Strip a "->/local/prefix/" rewrite suffix (see this field's
			// own doc comment) before recording it: the audited Hosts list
			// names real upstream hosts a run could reach, not this
			// proxy's own internal routing detail.
			host, _, _ := strings.Cut(allowed, "->")
			hosts = append(hosts, host)
		}
	}

	containerOutput, err := runRelayDocker(ctx, dockerBinary, "start registry proxy container", containerArgs...)
	if err != nil {
		return nil, errors.Join(err, handle.cleanup(ctx))
	}
	handle.AuditFacts.ContainerID = strings.TrimSpace(string(containerOutput))
	if handle.AuditFacts.ContainerID == "" {
		handle.AuditFacts.ContainerID = containerName
	}

	_, err = runRelayDocker(ctx, dockerBinary, "connect registry proxy network",
		"network", "connect", "--alias", registryProxyWorkerHost, networkName, containerName)
	if err != nil {
		return nil, errors.Join(err, handle.cleanup(ctx))
	}
	if err := confirmRelayRunning(ctx, dockerBinary, containerName); err != nil {
		return nil, errors.Join(err, handle.cleanup(ctx))
	}
	if err := waitRegistryProxyListening(ctx, dockerBinary, containerName); err != nil {
		return nil, errors.Join(err, handle.cleanup(ctx))
	}

	imageDigest, _ := relayImageDigest(spec.Image)
	handle.WorkerBaseURL = "http://" + registryProxyWorkerHost + ":" + strconv.Itoa(registryProxyContainerPort)
	handle.AuditFacts = RegistryProxyLaunchFacts{
		Image:         spec.Image,
		ImageDigest:   imageDigest,
		Hosts:         hosts,
		NetworkName:   networkName,
		NetworkID:     handle.AuditFacts.NetworkID,
		ContainerName: containerName,
		ContainerID:   handle.AuditFacts.ContainerID,
		WorkerBaseURL: handle.WorkerBaseURL,
		StartedAt:     time.Now().UTC(),
	}
	return handle, nil
}

// Cleanup removes the registry-proxy container first, confirms it is
// absent, then removes and confirms the internal network: a Docker CLI
// response can be lost while the daemon-side removal still succeeds, or
// vice versa.
func (h *RegistryProxyHandle) Cleanup(ctx context.Context) error {
	if h == nil {
		return nil
	}
	return h.cleanup(ctx)
}

func (h *RegistryProxyHandle) cleanup(ctx context.Context) error {
	// Called here too, not only inside cleanupNetwork below (see
	// caBundleCleanup's own doc comment): a shared-network handle
	// (!ownsNetwork) never calls cleanupNetwork at all, so this is its
	// only path to ever release the staged bundle copy. Safe to also run
	// via cleanupNetwork for an owns-network handle -- os.RemoveAll is
	// idempotent.
	if h.caBundleCleanup != nil {
		h.caBundleCleanup()
	}
	if err := h.removeContainer(ctx); err != nil {
		return err
	}
	if !h.ownsNetwork {
		// Removing the container above already detached it from the
		// shared network automatically (Docker does this on container
		// removal) -- this handle must never attempt `network rm` on a
		// network it doesn't own (see its own doc comment on ownsNetwork):
		// that network's lifecycle belongs entirely to whatever
		// created it.
		return nil
	}
	var errs []error
	if err := h.cleanupNetwork(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (h *RegistryProxyHandle) removeContainer(ctx context.Context) error {
	_, removeErr := runRelayDocker(ctx, h.dockerBinary, "remove registry proxy container", "rm", "-f", h.containerName)
	present, checkErr := relayContainerPresent(ctx, h.dockerBinary, h.containerName)
	if checkErr != nil {
		return fmt.Errorf("%w: confirm registry proxy container %q removal: %v", ErrRelayCleanupUnconfirmed, h.containerName, checkErr)
	}
	if present {
		if removeErr != nil {
			return fmt.Errorf("%w: remove registry proxy container %q: command failed and container remains", ErrRelayCleanupUnconfirmed, h.containerName)
		}
		return fmt.Errorf("%w: registry proxy container %q remains after removal", ErrRelayCleanupUnconfirmed, h.containerName)
	}
	return nil
}

func (h *RegistryProxyHandle) cleanupNetwork(ctx context.Context) error {
	// Reached directly by LaunchRegistryProxy's own network-create failure
	// path, before cleanup() above would ever run -- see its own comment.
	if h.caBundleCleanup != nil {
		h.caBundleCleanup()
	}
	_, removeErr := runRelayDocker(ctx, h.dockerBinary, "remove registry proxy network", "network", "rm", h.networkName)
	present, checkErr := relayNetworkPresent(ctx, h.dockerBinary, h.networkName)
	if checkErr != nil {
		return fmt.Errorf("%w: confirm registry proxy network %q removal: %v", ErrRelayCleanupUnconfirmed, h.networkName, checkErr)
	}
	if present {
		if removeErr != nil {
			return fmt.Errorf("%w: remove registry proxy network %q: command failed and network remains", ErrRelayCleanupUnconfirmed, h.networkName)
		}
		return fmt.Errorf("%w: registry proxy network %q remains after removal", ErrRelayCleanupUnconfirmed, h.networkName)
	}
	return nil
}

// ensureRegistryProxyEgressNetwork mirrors ensureRelayEgressNetwork exactly
// (see its own doc comment): idempotent creation, tolerating the benign
// race of a concurrent LaunchRegistryProxy call creating it first.
func ensureRegistryProxyEgressNetwork(ctx context.Context, dockerBinary string) error {
	present, err := relayNetworkPresent(ctx, dockerBinary, registryProxyEgressNetworkName)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	_, err = runRelayDocker(ctx, dockerBinary, "create registry proxy egress network",
		"network", "create",
		"--label", labelKey("owner")+"=factoryd",
		"--label", labelKey("registryproxy-egress")+"=true",
		registryProxyEgressNetworkName,
	)
	if err == nil {
		return nil
	}
	if present, presentErr := relayNetworkPresent(ctx, dockerBinary, registryProxyEgressNetworkName); presentErr == nil && present {
		return nil
	}
	return err
}

// registryProxyListeningLogLine is the fixed line cmd/registry-proxy logs
// immediately before calling http.Serve -- see waitRelayListening's own
// doc comment for why polling the container's own log is a reliable,
// shell-free readiness signal.
const registryProxyListeningLogLine = "serving registry proxy on"

func waitRegistryProxyListening(ctx context.Context, dockerBinary, containerName string) error {
	deadline := time.Now().Add(relayReadinessTimeout)
	for {
		boundedCtx, cancel := context.WithTimeout(ctx, relayDockerCommandTimeout)
		out, _ := exec.CommandContext(boundedCtx, dockerBinary, "logs", containerName).CombinedOutput()
		cancel()
		if strings.Contains(string(out), registryProxyListeningLogLine) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry proxy container did not report listening within %s", relayReadinessTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(relayReadinessPollInterval):
		}
	}
}
