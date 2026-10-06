// registry-proxy is the standalone binary front-end for
// internal/registryproxy's read-only, caching, allowlisted package-registry
// proxy -- launched per-run by internal/sandbox.LaunchRegistryProxy.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"buildgate/internal/registryproxy"
)

func main() {
	if err := realMain(os.Args[1:]); err != nil {
		log.Fatalf("registry-proxy: %v", err)
	}
}

// caBundleEnvironmentVariable names the PEM file whose certificates are added
// to this proxy's outbound trust roots (internal/sandbox's
// egressCABundleDockerArgs sets it).
const caBundleEnvironmentVariable = "FACTORYD_EGRESS_CA_BUNDLE"

// routeFlag accumulates repeated -route flags of the form
// "prefix=upstream[,allowed-host[->rewrite-prefix],...]", e.g.
// "/npm/=https://registry.npmjs.org" or, for PyPI (whose simple index
// links every file at files.pythonhosted.org, not pypi.org itself):
// "/pypi/=https://pypi.org/simple,files.pythonhosted.org->/pypi-files/".
// A plain "host" entry only allows a redirect to that host; a
// "host->prefix" entry ALSO rewrites the response body's own
// href/url links to that host into a path-relative link under prefix
// (registryproxy.Route.RewriteHrefHosts) -- see that field's own doc
// comment for exactly which response shapes this applies to.
type routeFlag struct {
	routes []registryproxy.Route
}

func (f *routeFlag) String() string { return "" }

func (f *routeFlag) Set(value string) error {
	prefix, rest, ok := strings.Cut(value, "=")
	if !ok || prefix == "" || rest == "" {
		return fmt.Errorf("route %q must be \"prefix=upstream[,allowed-host[->rewrite-prefix],...]\"", value)
	}
	parts := strings.Split(rest, ",")
	upstream, err := url.Parse(parts[0])
	if err != nil {
		return fmt.Errorf("route %q: parse upstream: %w", value, err)
	}
	var allowed map[string]bool
	var rewrite map[string]string
	for _, entry := range parts[1:] {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		host, rewritePrefix, hasRewrite := strings.Cut(entry, "->")
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" {
			return fmt.Errorf("route %q has an empty allowed host", value)
		}
		if allowed == nil {
			allowed = map[string]bool{}
		}
		allowed[host] = true
		if hasRewrite {
			rewritePrefix = strings.TrimSpace(rewritePrefix)
			if rewritePrefix == "" {
				return fmt.Errorf("route %q: rewrite target for host %q must not be empty", value, host)
			}
			if rewrite == nil {
				rewrite = map[string]string{}
			}
			rewrite[host] = rewritePrefix
		}
	}
	f.routes = append(f.routes, registryproxy.Route{Prefix: prefix, Upstream: upstream, AllowedHosts: allowed, RewriteHrefHosts: rewrite})
	return nil
}

func realMain(args []string) error {
	flags := flag.NewFlagSet("registry-proxy", flag.ContinueOnError)
	addr := flags.String("addr", ":8092", "HTTP listen address")
	cacheDir := flags.String("cache-dir", "/cache", "local, container-private directory for cached responses")
	maxCacheBytes := flags.Int64("max-cache-bytes", 1<<30, "maximum total size of cached responses")
	maxObjectBytes := flags.Int64("max-object-bytes", 256<<20, "maximum size of a single response eligible for caching; larger responses are still streamed to the client, just never cached")
	maxConcurrentUpstream := flags.Int("max-concurrent-upstream", 16, "maximum upstream requests in flight at once, across every route")
	upstreamTimeout := flags.Duration("upstream-timeout", 60*time.Second, "maximum duration of one upstream request")
	var routes routeFlag
	flags.Var(&routes, "route", `repeatable: "prefix=upstream[,allowed-host,...]", e.g. "/npm/=https://registry.npmjs.org"`)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(routes.routes) == 0 {
		flags.Usage()
		return fmt.Errorf("at least one -route is required")
	}

	server, err := registryproxy.NewServer(registryproxy.Config{
		Routes:                routes.routes,
		CacheDir:              *cacheDir,
		MaxCacheBytes:         *maxCacheBytes,
		MaxObjectBytes:        *maxObjectBytes,
		MaxConcurrentUpstream: *maxConcurrentUpstream,
		UpstreamTimeout:       *upstreamTimeout,
		Logger:                log.New(os.Stderr, "", log.LstdFlags),
		CABundlePath:          os.Getenv(caBundleEnvironmentVariable),
	})
	if err != nil {
		return fmt.Errorf("configure registry proxy: %w", err)
	}

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *addr, err)
	}
	// Fixed line internal/sandbox.waitRegistryProxyListening polls
	// `docker logs` for.
	log.Printf("serving registry proxy on %s", *addr)
	return http.Serve(listener, server)
}
