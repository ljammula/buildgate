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
	// UpstreamFailures returns how many requests the proxy container has
	// failed because its upstream failed, and whether that is known; nil
	// reads the container's log.
	UpstreamFailures func(ctx context.Context, dockerBinary, container string) (count int, known bool)
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

// UpstreamFailures is how many requests this run's registry proxy has failed
// since it started because a package registry could not be reached, did not
// answer in time or answered 5xx (internal/registryproxy counts them), and
// whether that is known. It is not known without a started proxy or when the
// proxy's log cannot be read: a caller must then decide nothing from it. A
// caller that wants the failures of one launch reads it before and after.
func (l *RegistryProxyLifecycle) UpstreamFailures(ctx context.Context) (count int, known bool) {
	if l == nil || l.handle == nil {
		return 0, false
	}
	read := l.hooks.UpstreamFailures
	if read == nil {
		read = registryProxyUpstreamFailures
	}
	return read(ctx, l.dockerBinary, l.facts.ContainerName)
}

// PrepareWorker points workerSpec at this run's already-started registry
// proxy: its own internal network, and the package-manager environment
// variables matching whichever of the fixed, well-known route prefixes
// (npm, PyPI, Go, Maven, RubyGems) this policy configured. A route using any
// other prefix is still proxied correctly by the container itself, it just
// gets no automatic env wiring here -- the caller's own build/verify script
// would need to reference it directly.
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
			// target repo's graph, so the worker gets the disk-backed
			// per-launch scratch volume (LaunchSpec.Scratch). The baked
			// cache is then unused for this run; every module comes
			// through the proxy, which is the whole point of the proxy.
			workerSpec.Scratch = true
			additions = append(additions,
				"GOPROXY="+strings.TrimRight(base, "/"),
				"GOMODCACHE="+WorkerScratchMount+"/gomodcache",
				// GOCACHE too: compiling a real target repo's dependency
				// graph (~1 GB of build cache for the Flutter + Go app repo's backend) filled
				// the memory-backed /home/worker tmpfs the image defaults
				// GOCACHE into -- the verify gate died on "no space left
				// on device" right after the module cache fix landed
				// (found live 2026-09-10, attempt 6). Same disk-backed
				// scratch volume, which is this container's alone:
				// canonical verify must never compile or replay a cache
				// the build phase wrote.
				"GOCACHE="+WorkerScratchMount+"/go-build",
				// The go command extracts modules read-only (0444 files
				// in 0555 directories); -modcacherw is Go's own documented
				// answer for a cache cleaned up by something other than
				// `go clean` (the stand-in Run removes a host directory).
				"GOFLAGS="+withModCacheRW(workerSpec.Environment),
				// Checksums are verified against sum.golang.org's own key,
				// fetched through goSumDBRoutePrefix. The URL is named
				// explicitly: given only a GOPROXY, the go command first asks
				// "<GOPROXY>/sumdb/sum.golang.org/supported", and on the 404
				// the database itself returns for that path it dials
				// sum.golang.org directly, which a worker cannot reach. With
				// "<name> <url>" it uses the URL as the database and still
				// checks every signed tree head with the named database's
				// built-in key, so the proxy relays data it cannot forge.
				"GOSUMDB=sum.golang.org "+goSumDBURL(base),
				// Every module through the proxy, a private one included:
				// a Makefile that exports GOPRIVATE would otherwise send
				// the go command straight to the module's host, which a
				// worker cannot reach and has no credential for. The
				// modules the proxy holds locally are kept out of the
				// checksum database by GONOSUMDB, below.
				"GONOPROXY=none",
			)
		case goSumDBRoutePrefix:
			// No env var of its own: the goProxyRoutePrefix case names
			// this route's URL in GOSUMDB. Explicitly listed (not left to
			// default, matching this switch's own doc comment) so a
			// reader doesn't have to wonder why this prefix is silently
			// skipped.
		case pypiFilesRoutePrefix:
			// No env var of its own either: pip never requests this route
			// directly by name -- it's only ever reached via a link the
			// pypiRoutePrefix route's own response rewriting (internal/
			// registryproxy.Route.RewriteHrefHosts) already pointed here.
		case mavenRoutePrefix, rubygemsRoutePrefix:
			keys, values := mavenAndRubyGemsEnvironment(route.Prefix, base)
			for _, key := range keys {
				forbidden[key] = true
			}
			additions = append(additions, values...)
			workerSpec.Scratch = true
		}
	}
	if forbidden["GOPROXY"] {
		// The modules the proxy serves from GoModuleDir are this machine's
		// own: none of their names goes to the checksum database either.
		// go.sum holds their hashes, as on the host.
		private, err := goModuleViewPaths(l.spec.GoModuleDir)
		if err != nil {
			return LaunchSpec{}, fmt.Errorf("read the registry proxy's Go module directory: %w", err)
		}
		if len(private) > 0 {
			forbidden["GONOSUMDB"] = true
			additions = append(additions, "GONOSUMDB="+strings.Join(private, ","))
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

// mavenAndRubyGemsEnvironment is the environment the Maven or the RubyGems
// route at base gives the worker, as the keys it replaces and the KEY=value
// entries that replace them. Both put what they download on the disk-backed
// per-launch scratch volume (LaunchSpec.Scratch): the image's own paths are
// read-only, the memory-backed $HOME is too small for a real dependency
// graph, and /tmp is noexec, where a native gem extension could not run.
//
// Maven reads no environment variable for a mirror. The Maven a build derives
// (internal/sandbox/Dockerfile.project's java-replace stage) has global
// settings whose single mirror of every repository is BUILDGATE_MAVEN_MIRROR,
// so this sets that variable and, in MAVEN_ARGS (read by Maven 3.9 and
// later), only the local repository on the scratch volume: no launch names a
// file an image may lack. Those settings replace the distribution's own,
// which carry Maven's block on plain-HTTP mirrors. A Maven an operator's image
// brings keeps its own settings and has no mirror, so it cannot fetch.
// MVNW_REPOURL sends the Maven wrapper's own download of Maven to the same
// route; the Maven it downloads keeps its own settings, with no mirror. A
// repository's .mvn/maven.config or settings.xml can name another
// mirror, but the sandbox's network policy reaches no host but the proxy.
// JAVA_TOOL_OPTIONS moves java.io.tmpdir off the noexec /tmp, where a test
// suite unpacks the native libraries it loads (sqlite-jdbc, Netty, JNA), to
// the scratch volume's java-tmp, which the worker's wrapper creates.
//
// Bundler's mirror.all setting sends every gem source to the route; gems
// are installed under BUNDLE_PATH, and GEM_HOME is where a `gem install`
// writes (it reads no mirror setting: it takes the route as --source).
func mavenAndRubyGemsEnvironment(prefix, base string) (keys, values []string) {
	scratch := WorkerScratchMount
	var entries [][2]string
	switch prefix {
	case mavenRoutePrefix:
		mirror := strings.TrimRight(base, "/")
		entries = [][2]string{
			{"BUILDGATE_MAVEN_MIRROR", mirror},
			{"MAVEN_ARGS", "-Dmaven.repo.local=" + scratch + "/maven-repository"},
			{"MVNW_REPOURL", mirror},
			{"JAVA_TOOL_OPTIONS", "-Djava.io.tmpdir=" + scratch + "/" + workerJavaTmpDir},
		}
	case rubygemsRoutePrefix:
		entries = [][2]string{
			{"BUNDLE_MIRROR__ALL", base},
			{"BUNDLE_PATH", scratch + "/bundle"},
			{"GEM_HOME", scratch + "/gems"},
		}
	}
	for _, entry := range entries {
		keys = append(keys, entry[0])
		values = append(values, entry[0]+"="+entry[1])
	}
	return keys, values
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
