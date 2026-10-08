package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RegistryProxyStarter/RegistryProxyHooks are an injectable seam that makes
// RegistryProxyLifecycle unit-testable without a Docker daemon.
type RegistryProxyStarter func(context.Context, string, RegistryProxySpec) (*RegistryProxyHandle, error)

type RegistryProxyHooks struct {
	StartRegistryProxy   RegistryProxyStarter
	CleanupRegistryProxy func(context.Context, *RegistryProxyHandle) error
	// Address returns the proxy container's address on its internal
	// network, for a worker that reaches it by address; nil asks Docker.
	Address func(ctx context.Context, dockerBinary, container, network string) (string, error)
}

// RegistryProxyCleanupTimeout bounds Cleanup's own teardown call.
const RegistryProxyCleanupTimeout = 30 * time.Second

// RegistryProxyLifecycle owns exactly one factory-owned registry proxy for
// the lifetime of one *run*, reused across its attempts. The registry
// proxy keeps no per-run budget, but reusing one container across
// every attempt of a run avoids paying container-launch cost (and a
// fresh, empty cache) on every retry.
type RegistryProxyLifecycle struct {
	spec         RegistryProxySpec
	dockerBinary string
	hooks        RegistryProxyHooks

	stopHeartbeat func()
	handle        *RegistryProxyHandle
	facts         RegistryProxyLaunchFacts
	// byAddress is set for a worker launched through a sandbox runtime: it
	// joins no network, so it is given the proxy's address (resolved once,
	// in Ensure) instead of the proxy's alias on a shared network.
	byAddress bool
	address   string
}

// BeginRegistryProxyLifecycle validates spec
// owns the same run/data directory the worker attempts use, records the
// owner heartbeat before any container can exist, and returns a lifecycle
// whose Cleanup the caller must run before treating the run as finished.
func BeginRegistryProxyLifecycle(spec RegistryProxySpec, dockerBinary, workerRunID, workerDataDir string, hooks RegistryProxyHooks) (*RegistryProxyLifecycle, error) {
	if spec.RunID != workerRunID || spec.DataDir != workerDataDir {
		return nil, fmt.Errorf("registry proxy spec (run %q, data dir %q) does not match worker spec (run %q, data dir %q)", spec.RunID, spec.DataDir, workerRunID, workerDataDir)
	}
	if err := writeOwnerHeartbeat(workerDataDir, workerRunID); err != nil {
		return nil, fmt.Errorf("record registry proxy owner: %w", err)
	}
	return &RegistryProxyLifecycle{
		spec:          spec,
		dockerBinary:  dockerBinary,
		hooks:         hooks,
		stopHeartbeat: startOwnerHeartbeat(workerDataDir, workerRunID),
	}, nil
}

// Ensure starts this run's registry proxy if it is not running yet, and is
// a no-op once one is. existingInternalNetwork, when non-empty, is this
// run's own relay's already-started internal network (see
// RegistryProxySpec.ExistingInternalNetwork's own doc comment) -- the
// caller passes it only once the relay lifecycle it came from has itself
// already been Ensure()d, so the network genuinely exists by the time this
// call attaches to it. Ignored (has no effect) once this lifecycle's own
// registry proxy is already running, exactly like every other argument
// here on a no-op call.
func (l *RegistryProxyLifecycle) Ensure(ctx context.Context, existingInternalNetwork string) error {
	if l.handle != nil {
		return nil
	}
	l.spec.ExistingInternalNetwork = existingInternalNetwork
	start := l.hooks.StartRegistryProxy
	if start == nil {
		start = LaunchRegistryProxy
	}
	handle, err := start(ctx, l.dockerBinary, l.spec)
	if err != nil {
		return err
	}
	if handle == nil {
		return errors.New("registry proxy starter returned a nil handle")
	}
	l.handle = handle
	l.facts = handle.Facts()
	if l.byAddress {
		return l.resolveAddress(ctx)
	}
	return nil
}

// resolveAddress records the proxy container's address on its internal
// network. The container lives for the whole run, so so does the address.
func (l *RegistryProxyLifecycle) resolveAddress(ctx context.Context) error {
	address := l.hooks.Address
	if address == nil {
		address = ContainerAddress
	}
	ip, err := address(ctx, l.dockerBinary, l.facts.ContainerName, l.facts.NetworkName)
	if err != nil {
		return fmt.Errorf("resolve registry proxy address: %w", err)
	}
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("registry proxy container %q has no address on network %q (got %q)", l.facts.ContainerName, l.facts.NetworkName, ip)
	}
	l.address = ip
	return nil
}

// PrepareWorker points workerSpec at this run's already-started registry
// proxy: its own internal network, and the package-manager environment
// variables matching whichever of the fixed, well-known route prefixes
// (npmRoutePrefix/pypiRoutePrefix/goProxyRoutePrefix) this policy
// configured. A route using any other prefix is still proxied correctly by
// the container itself, it just gets no automatic env wiring here -- the
// caller's own build/verify script would need to reference it directly.
func (l *RegistryProxyLifecycle) PrepareWorker(workerSpec LaunchSpec) (LaunchSpec, error) {
	if l.handle == nil {
		return LaunchSpec{}, errors.New("registry proxy lifecycle has no started registry proxy")
	}
	facts := l.handle.Facts()
	// Accepts either this proxy's own dedicated network or a relay's
	// shared one (RegistryProxySpec.ExistingInternalNetwork's own doc
	// comment) -- never either mechanism's real-internet-routable egress
	// network, which would defeat this whole check's purpose.
	sharedRelayNetwork := strings.HasPrefix(facts.NetworkName, "factoryd-relay-") && facts.NetworkName != relayEgressNetworkName
	ownNetwork := strings.HasPrefix(facts.NetworkName, "factoryd-registryproxy-") && facts.NetworkName != registryProxyEgressNetworkName
	if !sharedRelayNetwork && !ownNetwork {
		return LaunchSpec{}, fmt.Errorf("registry proxy returned unsafe worker network %q", facts.NetworkName)
	}
	if facts.WorkerBaseURL != "http://"+registryProxyWorkerHost+":"+strconv.Itoa(registryProxyContainerPort) || l.handle.WorkerBaseURL != facts.WorkerBaseURL {
		return LaunchSpec{}, errors.New("registry proxy returned unsafe worker base URL")
	}
	baseURL := facts.WorkerBaseURL
	if l.byAddress {
		// No shared network: the worker reaches the proxy at its address,
		// which the runtime's policy admits on the proxy's port alone.
		baseURL = "http://" + net.JoinHostPort(l.address, strconv.Itoa(registryProxyContainerPort))
		workerSpec.Sidecars = append(workerSpec.Sidecars, SidecarEndpoint{Name: registryProxyWorkerHost, IP: l.address, Ports: []int{registryProxyContainerPort}})
	} else {
		workerSpec.Network = facts.NetworkName
	}
	return l.withRegistryEnvironment(workerSpec, baseURL)
}

// withRegistryEnvironment points the worker's package tooling at the proxy's
// routes under baseURL, replacing whatever the caller set for them.
func (l *RegistryProxyLifecycle) withRegistryEnvironment(workerSpec LaunchSpec, baseURL string) (LaunchSpec, error) {

	forbidden := map[string]bool{}
	var additions []string
	for _, route := range l.spec.Routes {
		base := strings.TrimRight(baseURL, "/") + route.Prefix
		switch route.Prefix {
		case npmRoutePrefix:
			forbidden["npm_config_registry"] = true
			additions = append(additions, "npm_config_registry="+base)
		case pypiRoutePrefix:
			forbidden["PIP_INDEX_URL"] = true
			forbidden["PIP_TRUSTED_HOST"] = true
			// pip ignores a plain-HTTP index unless its host is trusted;
			// the proxy is the factory's own, reached on a private network.
			additions = append(additions, "PIP_INDEX_URL="+base, "PIP_TRUSTED_HOST="+urlHost(baseURL))
		case goProxyRoutePrefix:
			forbidden["GOPROXY"] = true
			forbidden["GOSUMDB"] = true
			forbidden["GOMODCACHE"] = true
			forbidden["GOCACHE"] = true
			forbidden["GOFLAGS"] = true
			forbidden["GONOPROXY"] = true
			// GOMODCACHE must move off the image's baked, read-only
			// /usr/local/gomodcache for GOPROXY to mean anything: a
			// module the image lacks has to land somewhere writable, and
			// the memory-backed $HOME tmpfs is too small for a real
			// target repo's graph -- see LaunchSpec.ScratchDir. The
			// baked cache is then unused for this run; every module comes
			// through the proxy, which is the whole point of the proxy.
			scratch, err := EnsureScratchDir(workerSpec.DataDir, workerSpec.RunID, workerSpec.Name, workerSpec.User)
			if err != nil {
				return LaunchSpec{}, err
			}
			workerSpec.ScratchDir = scratch
			additions = append(additions,
				"GOPROXY="+strings.TrimRight(base, "/"),
				"GOMODCACHE="+WorkerScratchMount+"/gomodcache",
				// GOCACHE too: compiling a real target repo's dependency
				// graph (~1 GB of build cache for the Flutter + Go app repo's backend) filled
				// the memory-backed /home/worker tmpfs the image defaults
				// GOCACHE into -- the verify gate died on "no space left
				// on device" right after the module cache fix landed
				// (found live 2026-09-10, attempt 6). Same disk-backed
				// scratch directory, which is this container's alone (see
				// RunScratchDir): canonical verify must never compile or
				// replay a cache the build phase wrote.
				"GOCACHE="+WorkerScratchMount+"/go-build",
				// The go command extracts modules read-only (0444 files
				// in 0555 directories); on the host that makes the
				// scratch directory undeletable by a plain RemoveAll
				// once the run ends (found live 2026-09-10). -modcacherw
				// is Go's own documented answer for a cache that is
				// cleaned up by something other than `go clean`.
				"GOFLAGS="+withModCacheRW(workerSpec.Environment),
				// sum.golang.org (Go's own default GOSUMDB), not "off":
				// goSumDBRoutePrefix registers a route serving exactly
				// "<this GOPROXY value>/sumdb/sum.golang.org/...", which is
				// the Go module proxy protocol's own documented passthrough
				// (https://go.dev/ref/mod#goproxy-protocol) -- the go
				// command automatically requests checksum data there
				// instead of dialing sum.golang.org directly whenever
				// GOSUMDB names a real database and GOPROXY points at an
				// actual proxy, so checksum verification stays enabled with
				// no other env var needed. Real per-token-manager
				// misconfiguration this replaces: an earlier version set
				// GOSUMDB=off here because this route did not exist yet,
				// silently disabling checksum verification entirely rather
				// than actually proxying it.
				"GOSUMDB=sum.golang.org",
				// Every module through the proxy, a private one included:
				// a Makefile that exports GOPRIVATE would otherwise send
				// the go command straight to the module's host, which a
				// worker cannot reach and has no credential for. GOPRIVATE
				// still keeps such a module out of the checksum database.
				"GONOPROXY=none",
			)
		case goSumDBRoutePrefix:
			// No env var of its own: this route exists purely so the
			// passthrough request GOSUMDB=sum.golang.org/GOPROXY together
			// trigger (see the goProxyRoutePrefix case above) has
			// somewhere real to land. Explicitly listed (not left to
			// default, matching this switch's own doc comment) so a
			// reader doesn't have to wonder why this prefix is silently
			// skipped.
		case pypiFilesRoutePrefix:
			// No env var of its own either: pip never requests this route
			// directly by name -- it's only ever reached via a link the
			// pypiRoutePrefix route's own response rewriting (internal/
			// registryproxy.Route.RewriteHrefHosts) already pointed here.
		}
	}
	environment := make([]string, 0, len(workerSpec.Environment)+len(additions))
	for _, value := range workerSpec.Environment {
		key, _, found := strings.Cut(value, "=")
		if found && forbidden[key] {
			continue
		}
		environment = append(environment, value)
	}
	workerSpec.Environment = append(environment, additions...)
	return workerSpec, nil
}

func (l *RegistryProxyLifecycle) Facts() RegistryProxyLaunchFacts {
	if l == nil {
		return RegistryProxyLaunchFacts{}
	}
	return l.facts
}

// Cleanup tears this run's registry proxy down and stops the owner
// heartbeat -- idempotent on success.
func (l *RegistryProxyLifecycle) Cleanup() error {
	if l == nil {
		return nil
	}
	if l.handle != nil {
		cleanup := l.hooks.CleanupRegistryProxy
		if cleanup == nil {
			cleanup = CleanupRegistryProxy
		}
		ctx, cancel := context.WithTimeout(context.Background(), RegistryProxyCleanupTimeout)
		defer cancel()
		cleanupErr := cleanup(ctx, l.handle)
		l.facts = l.handle.Facts()
		if cleanupErr != nil {
			return fmt.Errorf("cleanup registry proxy: %w", cleanupErr)
		}
		l.handle = nil
	}
	if l.stopHeartbeat != nil {
		l.stopHeartbeat()
		l.stopHeartbeat = nil
	}
	return nil
}

// Abandon stops the owner heartbeat
// unconditionally, for a caller whose own last cleanup retry has failed and
// is about to discard this lifecycle with nothing left that will ever call
// Cleanup again.
func (l *RegistryProxyLifecycle) Abandon() {
	if l == nil {
		return
	}
	if l.stopHeartbeat != nil {
		l.stopHeartbeat()
		l.stopHeartbeat = nil
	}
}

// CleanupRegistryProxy is the function form for code that stores handles
// behind an interface.
func CleanupRegistryProxy(ctx context.Context, h *RegistryProxyHandle) error {
	if h == nil {
		return nil
	}
	return h.Cleanup(ctx)
}

// withModCacheRW returns the worker's existing GOFLAGS value (from the
// image or the caller's environment) with -modcacherw appended, rather
// than replacing it: a project that builds with -tags or -mod=vendor
// must keep compiling the same target under the proxy (Codex review of
// PR #98).
func withModCacheRW(environment []string) string {
	existing := ""
	for _, value := range environment {
		if v, found := strings.CutPrefix(value, "GOFLAGS="); found {
			existing = v
		}
	}
	for _, flag := range strings.Fields(existing) {
		if flag == "-modcacherw" {
			return existing
		}
	}
	return strings.TrimSpace(existing + " -modcacherw")
}

// urlHost is the host of rawURL without its port, "" when it does not parse.
func urlHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
