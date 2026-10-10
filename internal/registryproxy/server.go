// Package registryproxy implements a read-only, caching, allowlisted forward
// proxy for package-registry HTTP traffic (npm, PyPI, the Go module proxy),
// launched per-run next to a sandboxed worker -- see internal/sandbox/registryproxy.go for the Docker
// launch/teardown/orphan-reconciliation side of that lifecycle.
//
// DESIGN. A sandboxed build_app.py worker runs with no network route at all
// (internal/sandbox/docker.go) except, optionally, the factory-owned
// inference relay's internal network. That leaves a project needing any
// dependency the canonical sandbox image doesn't already bake unable to
// build inside it (internal/sandbox/Dockerfile.project's own doc comment)
// unless an operator builds a whole bespoke per-project image on the host
// first. This package is the smaller alternative: a per-run container on
// that same internal network, fronting a fixed, operator-declared allowlist
// of package-registry upstreams (npm's registry.npmjs.org, PyPI's
// pypi.org/simple and files.pythonhosted.org, the Go module proxy's
// proxy.golang.org and sum.golang.org), so `npm install`/`pip install`/
// `go build` inside the sandbox can resolve real packages without a
// per-project image and without giving the worker any other route out.
//
// Routing is by a fixed path prefix this server owns (e.g. "/npm/"), never
// by the inbound request's Host header or an absolute-URI request line --
// so a worker can never smuggle an arbitrary upstream host through this
// proxy no matter what it sends as Host or path (see matchRoute). Only
// GET/HEAD are served; every other method (CONNECT included) is rejected
// before any routing decision is made, so this can never become a generic
// tunnel. No request body is ever read or forwarded. A redirect is only
// followed when its target host is in the SAME route's own allowed-host set
// (needed for PyPI, whose simple index commonly redirects a file download
// from pypi.org onto files.pythonhosted.org); a redirect anywhere else is
// refused and reported to the caller as a gateway error, never relayed
// verbatim. Successful GET bodies are cached on the container's own local
// disk (a tmpfs in production -- see internal/sandbox/registryproxy.go),
// keyed by a SHA-256 hash of the route and forwarded path, so a
// traversal-crafted request path can never select an arbitrary cache
// filename. The cache is bounded by MaxCacheBytes (oldest entries evicted
// first) and MaxObjectBytes (a single response too large to fit is streamed
// to the caller but never written to disk) -- and, because a fresh
// container is launched per run and torn down with it (never reused across
// runs), the cache can never carry a poisoned entry from one run into
// another. Concurrency into each upstream is bounded (MaxConcurrentUpstream)
// so a worker cannot use this proxy to fan out unbounded parallel upstream
// traffic. There is no listing or enumeration endpoint: a path that matches
// no configured route prefix gets a flat 404, never a directory listing of
// the cache or of configured routes.
//
// This proxy injects no credential (every upstream
// here is a public, unauthenticated registry) and forwards no client
// request headers upstream at all -- only a fixed User-Agent -- which rules
// out header-based cache poisoning or upstream request smuggling via
// anything the worker sends.
package registryproxy

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Route maps one fixed, proxy-owned path prefix onto one upstream base URL.
// AllowedHosts is the set of hosts (the upstream's own host plus any other
// host this route's upstream is known to redirect to, e.g. PyPI's file
// CDN) a redirect response may be followed to; every other redirect target
// is refused.
type Route struct {
	// Prefix is the path prefix a client request must begin with, always
	// starting and ending with "/" (e.g. "/npm/"). The matched remainder
	// (the request path with this prefix stripped) is appended to Upstream's
	// own path to build the outbound request.
	Prefix string
	// Upstream is the fixed base URL this route forwards to. Its own Host is
	// implicitly allowed for redirects even if AllowedHosts omits it.
	Upstream *url.URL
	// AllowedHosts is an additional set of host[:port] authorities (matched
	// case-insensitively against the redirect target's exact Host, port
	// included -- not bare hostname, so an allowed hostname on an
	// unexpected port is still refused) a redirect from this route's
	// upstream may target. Nil/empty means only Upstream's own host[:port]
	// is allowed.
	AllowedHosts map[string]bool
	// RewriteHrefHosts rewrites this route's own response bodies (PEP 503
	// HTML or PEP 691 JSON simple-index pages only -- see
	// selectHrefRewriter) so a link to one of these host[:port]
	// authorities becomes a path-relative link under the given proxy
	// prefix instead, letting a client that follows it stay inside this
	// proxy rather than trying (and failing, since the worker has no
	// other route out) to reach the real host directly. Needed for PyPI:
	// pypi.org's own simple index links every file at
	// files.pythonhosted.org, so without this the index page it returns
	// is otherwise unusable to a sandboxed pip with no other network
	// route. A host absent from this map is left completely untouched,
	// including one present in AllowedHosts for redirect-following
	// purposes only (the two lists serve different needs and are not
	// required to agree). Nil/empty disables rewriting for this route
	// entirely -- the response body is passed through byte-for-byte.
	RewriteHrefHosts map[string]string
	// LocalDir, when set, is a read-only directory laid out like the Go
	// module proxy's paths under this route (a GOMODCACHE's cache/download
	// directory). A request for a file it holds is answered from it; any
	// other request about a module it holds is a 404; neither reaches the
	// upstream. Everything else goes to the upstream as before.
	// internal/sandbox mounts a repository's private modules here, so a
	// module only the operator can fetch is served without the worker, or
	// this process, holding a credential, and without its name being sent
	// to the public proxy (see serveLocal).
	LocalDir string
}

// Config configures one Server.
type Config struct {
	Routes []Route
	// CacheDir is a local, writable directory this process owns exclusively
	// (see this package's own doc comment on why that makes cross-run
	// poisoning structurally impossible). Required.
	CacheDir string
	// MaxCacheBytes bounds the cache directory's total size; the oldest
	// entries (by last write) are evicted first to make room for a new one.
	// Must be positive.
	MaxCacheBytes int64
	// MaxObjectBytes bounds a single response's cacheable size. A response
	// exceeding it is still streamed to the caller in full, just never
	// written to disk. Must be positive and not exceed MaxCacheBytes.
	MaxObjectBytes int64
	// MaxConcurrentUpstream bounds how many upstream fetches may be in
	// flight at once, across every route. Must be positive.
	MaxConcurrentUpstream int
	// UpstreamTimeout bounds one upstream request/response. Must be
	// positive.
	UpstreamTimeout time.Duration
	Logger          *log.Logger
	// CABundlePath, when set, names a PEM file whose certificate(s) are
	// added to this process's own outbound http.Client trust store, on top
	// of the platform's default system roots: additive, not SSL_CERT_FILE,
	// which would replace the image's own public roots. Populated from the FACTORYD_EGRESS_CA_BUNDLE
	// environment variable by cmd/registry-proxy, set by internal/sandbox.
	// LaunchRegistryProxy when an operator configures -egress-ca-bundle.
	CABundlePath string
}

// Server is the read-only caching registry proxy's http.Handler.
type Server struct {
	routes          []Route
	cacheDir        string
	maxCacheBytes   int64
	maxObjectBytes  int64
	upstreamTimeout time.Duration
	client          *http.Client
	sem             chan struct{}
	logger          *log.Logger

	cacheMu    sync.Mutex
	cacheTotal int64
	cacheEntry map[string]cacheStat
}

type cacheStat struct {
	size    int64
	written time.Time
}

// NewServer validates config and returns a ready-to-serve Server.
func NewServer(config Config) (*Server, error) {
	if len(config.Routes) == 0 {
		return nil, errors.New("registry proxy requires at least one route")
	}
	seen := map[string]bool{}
	for _, route := range config.Routes {
		if route.Prefix == "" || !strings.HasPrefix(route.Prefix, "/") || !strings.HasSuffix(route.Prefix, "/") {
			return nil, fmt.Errorf("registry proxy route prefix %q must start and end with /", route.Prefix)
		}
		if seen[route.Prefix] {
			return nil, fmt.Errorf("registry proxy route prefix %q is configured more than once", route.Prefix)
		}
		seen[route.Prefix] = true
		if route.Upstream == nil || (route.Upstream.Scheme != "http" && route.Upstream.Scheme != "https") || route.Upstream.Host == "" {
			return nil, fmt.Errorf("registry proxy route %q upstream must be an absolute HTTP(S) URL", route.Prefix)
		}
		for host, prefix := range route.RewriteHrefHosts {
			if host == "" {
				return nil, fmt.Errorf("registry proxy route %q has an empty rewrite host", route.Prefix)
			}
			if prefix == "" || !strings.HasPrefix(prefix, "/") || !strings.HasSuffix(prefix, "/") {
				return nil, fmt.Errorf("registry proxy route %q rewrite prefix for host %q must start and end with /", route.Prefix, host)
			}
		}
	}
	if config.CacheDir == "" || !filepath.IsAbs(config.CacheDir) {
		return nil, errors.New("registry proxy cache directory must be an absolute path")
	}
	if config.MaxCacheBytes <= 0 {
		return nil, errors.New("registry proxy max cache bytes must be positive")
	}
	if config.MaxObjectBytes <= 0 || config.MaxObjectBytes > config.MaxCacheBytes {
		return nil, errors.New("registry proxy max object bytes must be positive and not exceed max cache bytes")
	}
	if config.MaxConcurrentUpstream <= 0 {
		return nil, errors.New("registry proxy max concurrent upstream requests must be positive")
	}
	if config.UpstreamTimeout <= 0 {
		return nil, errors.New("registry proxy upstream timeout must be positive")
	}
	if err := os.MkdirAll(config.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create registry proxy cache directory: %w", err)
	}
	logger := config.Logger
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	transport, err := outboundTransport(config.CABundlePath)
	if err != nil {
		return nil, err
	}
	s := &Server{
		routes:          append([]Route(nil), config.Routes...),
		cacheDir:        config.CacheDir,
		maxCacheBytes:   config.MaxCacheBytes,
		maxObjectBytes:  config.MaxObjectBytes,
		upstreamTimeout: config.UpstreamTimeout,
		sem:             make(chan struct{}, config.MaxConcurrentUpstream),
		logger:          logger,
		cacheEntry:      map[string]cacheStat{},
	}
	s.client = &http.Client{
		CheckRedirect: s.checkRedirect,
		Transport:     transport,
	}
	return s, nil
}

// outboundTransport returns http.DefaultTransport when caBundlePath is
// empty, or a Transport whose TLS RootCAs is the platform's default system
// cert pool PLUS caBundlePath's certificate(s) when set -- see internal/
// relay's own outboundTransport (identical logic, kept per-package rather
// than shared since neither package otherwise depends on the other).
func outboundTransport(caBundlePath string) (http.RoundTripper, error) {
	if caBundlePath == "" {
		return http.DefaultTransport, nil
	}
	pem, err := os.ReadFile(caBundlePath)
	if err != nil {
		return nil, fmt.Errorf("read CA bundle: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("CA bundle %s: no PEM certificate found", caBundlePath)
	}
	// Clone http.DefaultTransport, not a bare &http.Transport{}: its
	// Proxy, timeouts and HTTP/2 settings would otherwise be lost.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{RootCAs: pool}
	return t, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	status := http.StatusInternalServerError
	defer func() {
		s.logger.Printf("method=%q path=%q status=%d duration=%s", r.Method, r.URL.Path, status, time.Since(started))
	}()

	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		status = http.StatusMethodNotAllowed
		writeError(w, status, "method is not allowed")
		return
	}
	// No request body is ever read or forwarded, matching this proxy's
	// GET/HEAD-only contract -- discard whatever a client sent rather than
	// silently ignoring it while leaving the connection holding it open.
	if r.Body != nil {
		_ = r.Body.Close()
	}

	route, rest, ok := s.matchRoute(r.URL.Path)
	if !ok {
		// No listing/enumeration endpoint: an unmatched path is a flat 404,
		// never a directory of configured routes or cached objects.
		status = http.StatusNotFound
		writeError(w, status, "not found")
		return
	}

	if handled, localStatus := s.serveLocal(w, r, route, rest); handled {
		status = localStatus
		return
	}

	key := cacheKey(route.Prefix, rest)
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if served, cachedStatus := s.serveFromCache(w, r, key); served {
			status = cachedStatus
			return
		}
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-time.After(s.upstreamTimeout):
		status = http.StatusServiceUnavailable
		writeError(w, status, "registry proxy is at its concurrent upstream request limit")
		return
	case <-r.Context().Done():
		status = http.StatusServiceUnavailable
		writeError(w, status, "request canceled while waiting for an upstream slot")
		return
	}

	status = s.fetchAndServe(w, r, route, rest, key)
}

// serveLocal answers a request from route.LocalDir and reports whether it
// did, with the status it wrote. A regular file rest names is served. Any
// other path of a module the directory holds (its version list, its latest
// version, a version it lacks) is a 404 from here: a module that is served
// locally is one whose name is not for the upstream, so nothing about it is
// asked there. Everything else is left for the upstream. The directory is
// opened as an os.Root, so no path and no symlink inside it reaches a file
// outside it.
func (s *Server) serveLocal(w http.ResponseWriter, r *http.Request, route Route, rest string) (handled bool, status int) {
	if route.LocalDir == "" || rest == "" {
		return false, 0
	}
	root, err := os.OpenRoot(route.LocalDir)
	if err != nil {
		return false, 0
	}
	defer root.Close()
	if file, err := root.Open(filepath.FromSlash(rest)); err == nil {
		defer file.Close()
		if info, err := file.Stat(); err == nil && info.Mode().IsRegular() {
			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			recorder.Header().Set("Content-Type", localContentType(rest))
			http.ServeContent(recorder, r, "", info.ModTime(), file)
			return true, recorder.status
		}
	}
	if module := localModulePath(rest); module != "" {
		if info, err := root.Stat(filepath.FromSlash(module)); err == nil && info.IsDir() {
			writeError(w, http.StatusNotFound, "not found")
			return true, http.StatusNotFound
		}
	}
	return false, 0
}

// localModulePath is the module a Go module proxy path asks about
// ("<module>/@v/<file>" or "<module>/@latest"), "" for any other path.
func localModulePath(rest string) string {
	if module, _, found := strings.Cut(rest, "/@v/"); found {
		return module
	}
	if module, found := strings.CutSuffix(rest, "/@latest"); found {
		return module
	}
	return ""
}

// statusRecorder notes the status a handler writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// localContentType is the type the Go module proxy protocol gives each file.
func localContentType(name string) string {
	switch path.Ext(name) {
	case ".zip":
		return "application/zip"
	case ".info":
		return "application/json"
	default:
		return "text/plain; charset=utf-8"
	}
}

// matchRoute finds the configured route whose prefix the cleaned request
// path begins with, and returns the remainder after that prefix. path.Clean
// rejects ".."-style traversal from ever reaching the upstream request or
// the cache key: a request path that Clean changes is refused outright.
func (s *Server) matchRoute(requestPath string) (Route, string, bool) {
	cleaned := path.Clean(requestPath)
	if cleaned != requestPath && cleaned+"/" != requestPath {
		return Route{}, "", false
	}
	for _, route := range s.routes {
		if strings.HasPrefix(requestPath, route.Prefix) {
			return route, strings.TrimPrefix(requestPath, route.Prefix), true
		}
	}
	return Route{}, "", false
}

// checkRedirect is the outbound http.Client's redirect policy: follow a
// redirect only when its target host is allowed for the route the ORIGINAL
// request matched, refuse otherwise. Deliberately fails closed for any
// request whose context this package didn't attach (redirectContext below),
// since without it there is nothing to check the target host against.
func (s *Server) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("registry proxy: too many redirects")
	}
	allowed, ok := req.Context().Value(redirectAllowedHostsKey{}).(map[string]bool)
	if !ok || !allowed[strings.ToLower(req.URL.Host)] {
		return fmt.Errorf("registry proxy: redirect to non-allowlisted host %q refused", req.URL.Host)
	}
	return nil
}

type redirectAllowedHostsKey struct{}

// fetchAndServe issues the upstream request, streams the response to the
// client, and -- unless it exceeds maxObjectBytes -- writes a copy to the
// on-disk cache. Only a GET is ever sent upstream, even for an inbound
// HEAD, so a subsequent GET for the same key gets a filled cache entry
// (HEAD's own response headers are the same for both).
func (s *Server) fetchAndServe(w http.ResponseWriter, r *http.Request, route Route, rest, key string) int {
	// Matched by full host[:port] authority, not bare hostname (found while
	// writing this package's own tests: two distinct httptest servers both
	// bind "127.0.0.1", differing only by port -- bare-hostname matching
	// would have let a redirect to ANY port on an allowed hostname through,
	// including a port an operator never intended to allow).
	allowed := map[string]bool{strings.ToLower(route.Upstream.Host): true}
	for host := range route.AllowedHosts {
		allowed[strings.ToLower(host)] = true
	}
	ctx := context.WithValue(r.Context(), redirectAllowedHostsKey{}, allowed)
	ctx, cancel := context.WithTimeout(ctx, s.upstreamTimeout)
	defer cancel()

	outboundURL := *route.Upstream
	outboundURL.Path = strings.TrimRight(route.Upstream.Path, "/") + "/" + strings.TrimLeft(rest, "/")
	outboundURL.RawQuery = r.URL.RawQuery

	outbound, err := http.NewRequestWithContext(ctx, http.MethodGet, outboundURL.String(), nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, "build upstream request")
		return http.StatusBadGateway
	}
	// No client header is ever forwarded (see this package's own doc
	// comment): only a fixed identifying User-Agent is sent.
	outbound.Header.Set("User-Agent", "buildgate-registry-proxy/1")

	response, err := s.client.Do(outbound)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			writeError(w, http.StatusGatewayTimeout, "upstream request timed out")
			return http.StatusGatewayTimeout
		}
		writeError(w, http.StatusBadGateway, "upstream request failed or was redirected to a non-allowlisted host")
		return http.StatusBadGateway
	}
	defer response.Body.Close()

	if r.Method == http.MethodHead {
		copyResponseHeaders(w.Header(), response.Header, true)
		w.WriteHeader(response.StatusCode)
		return response.StatusCode
	}
	if response.StatusCode != http.StatusOK {
		copyResponseHeaders(w.Header(), response.Header, true)
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return response.StatusCode
	}

	contentType := response.Header.Get("Content-Type")
	if rewrite := selectHrefRewriter(route, contentType); rewrite != nil {
		s.serveRewritten(w, response, rewrite, key, contentType)
		return response.StatusCode
	}
	copyResponseHeaders(w.Header(), response.Header, true)
	w.WriteHeader(response.StatusCode)
	s.copyAndCache(w, response.Body, key, contentType)
	return response.StatusCode
}

// maxRewritableBodyBytes bounds how large a simple-index response
// (PEP 503 HTML or PEP 691 JSON -- always small, hand-authored-scale
// documents, never a package archive itself) this server will buffer
// fully in order to rewrite its href/url links. A response over this
// bound is passed through unrewritten and uncached (found via review:
// caching an unrewritten body here would let a client resolve
// unreachable absolute links straight from the cache on a later request,
// exactly what rewriting exists to prevent -- see this function's own
// caller for why an unrewritten body must never be cached).
const maxRewritableBodyBytes = 8 << 20

// serveRewritten buffers response's full body (bounded by
// maxRewritableBodyBytes), rewrites it, and serves + caches the REWRITTEN
// bytes -- never the original. Content-Length is deliberately never copied
// from the upstream response here (copyResponseHeaders' own
// includeContentLength=false): the rewritten body's length differs from
// the original's, and a stale Content-Length would truncate or corrupt
// the response net/http actually sends.
func (s *Server) serveRewritten(w http.ResponseWriter, response *http.Response, rewrite func([]byte) ([]byte, error), key, contentType string) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRewritableBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read upstream response")
		return
	}
	if int64(len(body)) > maxRewritableBodyBytes {
		// Too large to safely rewrite -- serve what upstream sent
		// unmodified, and deliberately do not cache it (see this
		// function's own doc comment).
		copyResponseHeaders(w.Header(), response.Header, false)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		_, _ = io.Copy(io.Discard, response.Body) // drain any remainder
		return
	}
	rewritten, err := rewrite(body)
	if err != nil {
		// A simple-index response this server cannot even parse is
		// itself upstream's problem to explain, not silently passed
		// through with un-rewritten, unreachable absolute links.
		writeError(w, http.StatusBadGateway, "rewrite upstream response")
		return
	}
	copyResponseHeaders(w.Header(), response.Header, false)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(rewritten)
	s.cacheBytes(rewritten, key, contentType)
}

// copyAndCache streams the upstream body to the client and, in the same
// pass, to a temp file capped at maxObjectBytes on disk -- an oversized or
// endless upstream response is still fully delivered to the caller (bounded
// only by the caller's own patience/timeout), but the on-disk temp file
// itself never grows past that cap, so this can never be used to fill the
// cache disk regardless of how large or slow the real response is.
func (s *Server) copyAndCache(w io.Writer, body io.Reader, key, contentType string) {
	tmp, err := os.CreateTemp(s.cacheDir, "tmp-*")
	if err != nil {
		// Cache unavailable is not a client-visible failure -- still serve
		// the response, just uncached.
		_, _ = io.Copy(w, body)
		return
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // no-op once renamed into place
	}()

	capped := &cappedWriter{w: tmp, limit: s.maxObjectBytes}
	multi := io.MultiWriter(w, capped)
	total, copyErr := io.Copy(multi, body)
	if copyErr != nil || capped.exceeded || total > s.maxObjectBytes {
		return // not cached: partial/failed read, or too large
	}
	if err := tmp.Close(); err != nil {
		return
	}
	s.commitCacheEntry(tmpPath, key, total, contentType)
}

// cappedWriter writes at most limit bytes to the underlying writer, then
// silently discards the rest (still reporting a full, successful write to
// its own caller) rather than erroring -- an error here would abort
// io.MultiWriter's copy to the OTHER writer (the real client response) too,
// which must keep receiving the complete body regardless of caching.
type cappedWriter struct {
	w        io.Writer
	limit    int64
	written  int64
	exceeded bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.exceeded {
		return len(p), nil
	}
	remaining := c.limit - c.written
	if int64(len(p)) <= remaining {
		n, err := c.w.Write(p)
		c.written += int64(n)
		if err != nil {
			return n, err
		}
		return len(p), nil
	}
	if remaining > 0 {
		n, err := c.w.Write(p[:remaining])
		c.written += int64(n)
		if err != nil {
			return n, err
		}
	}
	c.exceeded = true
	return len(p), nil
}

// cacheBytes caches an already-fully-in-memory response body -- used for a
// rewritten simple-index page (serveRewritten), which is always small and
// already buffered, unlike copyAndCache's streaming path for an ordinary
// (potentially large) package archive.
func (s *Server) cacheBytes(data []byte, key, contentType string) {
	if int64(len(data)) > s.maxObjectBytes {
		return
	}
	tmp, err := os.CreateTemp(s.cacheDir, "tmp-*")
	if err != nil {
		return
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpPath) // no-op once renamed into place
	}()
	if _, err := tmp.Write(data); err != nil {
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	s.commitCacheEntry(tmpPath, key, int64(len(data)), contentType)
}

// commitCacheEntry evicts oldest entries as needed to make room, then
// renames tmpPath into place as key's cache file. A meta file alongside it
// records contentType, since the raw body alone doesn't carry it back for a
// later cache hit.
func (s *Server) commitCacheEntry(tmpPath, key string, size int64, contentType string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	if existing, ok := s.cacheEntry[key]; ok {
		s.cacheTotal -= existing.size
	}
	for s.cacheTotal+size > s.maxCacheBytes && len(s.cacheEntry) > 0 {
		oldestKey, oldestWritten := "", time.Time{}
		for k, stat := range s.cacheEntry {
			if oldestKey == "" || stat.written.Before(oldestWritten) {
				oldestKey, oldestWritten = k, stat.written
			}
		}
		s.cacheTotal -= s.cacheEntry[oldestKey].size
		delete(s.cacheEntry, oldestKey)
		os.Remove(s.cachePath(oldestKey))
		os.Remove(s.cacheMetaPath(oldestKey))
	}
	if size > s.maxCacheBytes {
		return // cannot ever fit, even in an empty cache
	}
	if err := os.Rename(tmpPath, s.cachePath(key)); err != nil {
		return
	}
	_ = os.WriteFile(s.cacheMetaPath(key), []byte(contentType), 0o600)
	s.cacheEntry[key] = cacheStat{size: size, written: time.Now()}
	s.cacheTotal += size
}

// serveFromCache serves a cached object directly, with no upstream contact
// at all, when key is present. Reports whether it served the request.
func (s *Server) serveFromCache(w http.ResponseWriter, r *http.Request, key string) (served bool, status int) {
	s.cacheMu.Lock()
	_, ok := s.cacheEntry[key]
	s.cacheMu.Unlock()
	if !ok {
		return false, 0
	}
	f, err := os.Open(s.cachePath(key))
	if err != nil {
		return false, 0
	}
	defer f.Close()
	if contentType, err := os.ReadFile(s.cacheMetaPath(key)); err == nil && len(contentType) > 0 {
		w.Header().Set("Content-Type", string(contentType))
	}
	w.Header().Set("X-Registry-Proxy-Cache", "HIT")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
	return true, http.StatusOK
}

// cacheKey hashes the route prefix and remainder into a fixed-width hex
// string used as a filename, so a crafted request path (however it tries to
// traverse or collide) can never select an arbitrary cache filename -- the
// filesystem never sees any client-controlled byte directly.
func cacheKey(prefix, rest string) string {
	sum := sha256.Sum256([]byte(prefix + rest))
	return hex.EncodeToString(sum[:])
}

func (s *Server) cachePath(key string) string     { return filepath.Join(s.cacheDir, key) }
func (s *Server) cacheMetaPath(key string) string { return filepath.Join(s.cacheDir, key+".meta") }

// includeContentLength must be false whenever the body actually written
// differs from the upstream response's own body (i.e. after rewriting) --
// see serveRewritten's own doc comment for why a stale Content-Length would
// corrupt that response.
func copyResponseHeaders(dst, src http.Header, includeContentLength bool) {
	names := []string{"Content-Type", "ETag", "Last-Modified", "Cache-Control"}
	if includeContentLength {
		names = append(names, "Content-Length")
	}
	for _, name := range names {
		if v := src.Get(name); v != "" {
			dst.Set(name, v)
		}
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintln(w, message)
}

// selectHrefRewriter returns the rewrite function matching contentType for
// route, or nil when no rewriting applies -- either route.RewriteHrefHosts
// is empty, or contentType is neither an HTML nor a JSON simple-index
// shape (PEP 503 / PEP 691 respectively; a package archive download served
// by the same route, e.g. a .whl/.tar.gz, has neither content type and
// passes through completely untouched by this mechanism).
func selectHrefRewriter(route Route, contentType string) func([]byte) ([]byte, error) {
	if len(route.RewriteHrefHosts) == 0 {
		return nil
	}
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case strings.Contains(mediaType, "json"):
		return func(body []byte) ([]byte, error) { return rewritePyPIJSONURLs(body, route.RewriteHrefHosts) }
	case strings.Contains(mediaType, "html"):
		return func(body []byte) ([]byte, error) { return rewriteHTMLHrefs(body, route.RewriteHrefHosts), nil }
	default:
		return nil
	}
}

// hrefAttrPattern matches an HTML href attribute with either quote style --
// PEP 503's simple index is a minimal, hand-specified HTML dialect (bare
// <a href="...">...</a> anchors), not general HTML, so this narrow pattern
// is deliberately not a full HTML parser.
var hrefAttrPattern = regexp.MustCompile(`href\s*=\s*"([^"]*)"|href\s*=\s*'([^']*)'`)

// rewriteHTMLHrefs rewrites every href whose absolute URL's host[:port] is a
// key of allowedHosts (PEP 503's simple index) into a path-relative link
// under the matching proxy prefix; every other href (a relative link, or an
// absolute one to a host not in allowedHosts) is left byte-for-byte
// unchanged.
func rewriteHTMLHrefs(body []byte, allowedHosts map[string]string) []byte {
	return hrefAttrPattern.ReplaceAllFunc(body, func(match []byte) []byte {
		sub := hrefAttrPattern.FindSubmatch(match)
		quote := byte('"')
		value := string(sub[1])
		if value == "" && len(sub[2]) > 0 {
			value = string(sub[2])
			quote = '\''
		}
		rewritten, ok := rewriteHrefValue(value, allowedHosts)
		if !ok {
			return match
		}
		return append([]byte("href="+string(quote)), append([]byte(rewritten), quote)...)
	})
}

// rewritePyPIJSONURLs rewrites a PEP 691 JSON simple-index response's
// files[].url entries the same way rewriteHTMLHrefs rewrites PEP 503 HTML
// hrefs -- see that function's own doc comment. Decodes into a generic
// map so every field this package doesn't specifically know about
// (name, meta, versions, a file's hashes/size/requires-python/...)
// round-trips completely unchanged.
func rewritePyPIJSONURLs(body []byte, allowedHosts map[string]string) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("parse PyPI simple index JSON: %w", err)
	}
	filesRaw, ok := doc["files"]
	if !ok {
		return body, nil // no files array -- nothing to rewrite
	}
	var files []map[string]json.RawMessage
	if err := json.Unmarshal(filesRaw, &files); err != nil {
		return nil, fmt.Errorf("parse PyPI simple index JSON files: %w", err)
	}
	for _, file := range files {
		rawURL, ok := file["url"]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(rawURL, &value); err != nil {
			continue // not a string -- leave whatever it is untouched
		}
		if rewritten, ok := rewriteHrefValue(value, allowedHosts); ok {
			encoded, err := json.Marshal(rewritten)
			if err != nil {
				continue
			}
			file["url"] = encoded
		}
	}
	rewrittenFiles, err := json.Marshal(files)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten PyPI simple index JSON files: %w", err)
	}
	doc["files"] = rewrittenFiles
	return json.Marshal(doc)
}

// rewriteHrefValue rewrites one absolute URL whose host[:port] is a key of
// allowedHosts into a path-relative link under the matching proxy prefix,
// preserving the original path/query/fragment. Reports false (value
// returned is meaningless) for a relative URL or one whose host is not in
// allowedHosts -- the caller leaves those completely untouched.
func rewriteHrefValue(raw string, allowedHosts map[string]string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return "", false
	}
	prefix, ok := allowedHosts[strings.ToLower(u.Host)]
	if !ok {
		return "", false
	}
	rewritten := strings.TrimRight(prefix, "/") + "/" + strings.TrimLeft(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		rewritten += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		rewritten += "#" + u.EscapedFragment()
	}
	return rewritten, true
}
