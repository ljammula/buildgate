package registryproxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func newTestServer(t *testing.T, upstream *httptest.Server, allowedHosts map[string]bool) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Routes: []Route{
			{Prefix: "/npm/", Upstream: mustURL(t, upstream.URL), AllowedHosts: allowedHosts},
		},
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		MaxCacheBytes:         1 << 20,
		MaxObjectBytes:        1 << 18,
		MaxConcurrentUpstream: 4,
		UpstreamTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

// TestMethodNotAllowed covers the "worker uses CONNECT/POST to tunnel
// through this proxy" attack: only GET/HEAD are ever routed at all.
func TestMethodNotAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must never be contacted for a disallowed method")
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodConnect, "TRACE"} {
		req := httptest.NewRequest(method, "/npm/left-pad", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("method %s: got status %d, want 405", method, rec.Code)
		}
	}
}

// TestUnmatchedPathIsNotFoundNotListing covers "no listing/enumeration
// endpoint": a path outside every configured route must 404, never
// enumerate configured routes or cached content.
func TestUnmatchedPathIsNotFoundNotListing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must never be contacted for an unmatched path")
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	for _, p := range []string{"/", "/pypi/foo", "/npm", "/cache/", "/routes"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %s: got status %d, want 404", p, rec.Code)
		}
	}
}

// TestHostHeaderIgnoredForRouting covers Host-header / absolute-URI
// smuggling: routing must depend only on the fixed configured prefix table,
// never on whatever Host the client sends.
func TestHostHeaderIgnoredForRouting(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	req := httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil)
	req.Host = "evil.example.com"
	req.Header.Set("Host", "evil.example.com")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if atomic.LoadInt32(&upstreamHits) != 1 {
		t.Fatalf("expected exactly one upstream hit, got %d", upstreamHits)
	}
}

// TestPathTraversalRejected covers path traversal in the cache key / route
// matching: a ".."-bearing request path must never reach the upstream or
// select a cache file outside the cache directory.
func TestPathTraversalRejected(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("upstream must never be contacted for a traversal path, got %q", r.URL.Path)
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	req := httptest.NewRequest(http.MethodGet, "/npm/../../etc/passwd", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	// net/http/httptest's request URL is already Clean()d by the router in
	// real net/http muxing, but this handler is exercised directly, so it
	// must reject this itself; a raw "/npm/../../etc/passwd" either fails
	// matchRoute's own Clean comparison or never matches the "/npm/" prefix
	// once cleaned. Either way, the upstream must never see it.
	if rec.Code == http.StatusOK {
		t.Fatalf("traversal path unexpectedly served: %d", rec.Code)
	}
}

// TestRedirectToAllowlistedHostFollowed covers the PyPI-style case: a
// redirect to a host explicitly allowlisted for this route is followed.
func TestRedirectToAllowlistedHostFollowed(t *testing.T) {
	file := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "file-bytes")
	}))
	defer file.Close()
	fileHost := mustURL(t, file.URL).Host

	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, file.URL+"/left-pad-1.0.tgz", http.StatusFound)
	}))
	defer index.Close()

	s := newTestServer(t, index, map[string]bool{fileHost: true})
	req := httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "file-bytes" {
		t.Fatalf("got body %q, want %q", rec.Body.String(), "file-bytes")
	}
}

// TestRedirectToNonAllowlistedHostRefused covers "redirect to
// non-allowlisted host": the proxy must refuse it, not relay a raw redirect
// pointing the worker at an arbitrary host it could then try to reach.
func TestRedirectToNonAllowlistedHostRefused(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("the non-allowlisted redirect target must never actually be contacted")
	}))
	defer evil.Close()

	index := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusFound)
	}))
	defer index.Close()

	// No AllowedHosts configured beyond the route's own upstream host.
	s := newTestServer(t, index, nil)
	req := httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound || rec.Code == http.StatusOK {
		t.Fatalf("got status %d, want a gateway failure, not a followed or relayed redirect", rec.Code)
	}
	if strings.Contains(rec.Header().Get("Location"), evil.URL) {
		t.Fatalf("must not relay a Location header pointing at a non-allowlisted host")
	}
}

// TestCachePreventsSecondUpstreamHit covers ordinary cache behavior and
// doubles as the "many parallel requests" concern: a second request for the
// same key must not re-hit the upstream at all.
func TestCachePreventsSecondUpstreamHit(t *testing.T) {
	var upstreamHits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		fmt.Fprint(w, "left-pad-contents")
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || rec.Body.String() != "left-pad-contents" {
			t.Fatalf("request %d: got status %d body %q", i, rec.Code, rec.Body.String())
		}
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 1 {
		t.Fatalf("expected exactly 1 upstream hit across 3 cached requests, got %d", got)
	}
}

// TestOversizedResponseNotCached covers "oversized response filling the
// disk": a response larger than MaxObjectBytes must still reach the client
// in full but must never be written to the cache, and a later request for
// it must hit the upstream again (proving no partial/oversized entry was
// cached).
func TestOversizedResponseNotCached(t *testing.T) {
	var upstreamHits int32
	const objectSize = 1 << 19 // larger than the 1<<18 MaxObjectBytes in newTestServer
	payload := strings.Repeat("x", objectSize)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamHits, 1)
		fmt.Fprint(w, payload)
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/npm/big-package", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got status %d", i, rec.Code)
		}
		if rec.Body.Len() != objectSize {
			t.Fatalf("request %d: got %d bytes, want %d (full body must still reach the client)", i, rec.Body.Len(), objectSize)
		}
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 2 {
		t.Fatalf("expected 2 upstream hits (oversized response must never be cached), got %d", got)
	}
}

// TestCacheEvictsOldestWhenOverCap exercises the size cap: once the cache
// would exceed MaxCacheBytes, the oldest entry is evicted to make room.
func TestCacheEvictsOldestWhenOverCap(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, strings.Repeat("y", 1000))
	}))
	defer upstream.Close()
	s, err := NewServer(Config{
		Routes:                []Route{{Prefix: "/npm/", Upstream: mustURL(t, upstream.URL)}},
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		MaxCacheBytes:         2500, // fits ~2 entries of 1000 bytes, not 3
		MaxObjectBytes:        2000,
		MaxConcurrentUpstream: 4,
		UpstreamTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	for _, pkg := range []string{"a", "b", "c"} {
		req := httptest.NewRequest(http.MethodGet, "/npm/"+pkg, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("package %s: got status %d", pkg, rec.Code)
		}
		time.Sleep(2 * time.Millisecond) // ensure distinct written timestamps
	}
	s.cacheMu.Lock()
	total := s.cacheTotal
	entries := len(s.cacheEntry)
	s.cacheMu.Unlock()
	if total > 2500 {
		t.Fatalf("cache total %d exceeds cap 2500", total)
	}
	if entries >= 3 {
		t.Fatalf("expected eviction to keep fewer than 3 entries, got %d", entries)
	}
}

// TestCacheKeyIsHashNotRawPath is a targeted check that the on-disk cache
// filename is a hash, never a client-controlled path fragment -- the second
// half of the path-traversal defense (matchRoute rejects the request
// entirely, this ensures even a permitted path never leaks into a
// filesystem path segment).
func TestCacheKeyIsHashNotRawPath(t *testing.T) {
	key := cacheKey("/npm/", "../../../etc/passwd")
	if strings.ContainsAny(key, "./\\") {
		t.Fatalf("cache key %q must be a pure hex hash", key)
	}
	if len(key) != 64 {
		t.Fatalf("cache key %q must be a 32-byte sha256 hex digest", key)
	}
}

// TestConcurrentRequestsBoundedNoUpstreamOverload covers "DoS via many
// parallel requests": with MaxConcurrentUpstream small, many concurrent
// distinct-key requests must all still complete (none hang forever), and
// the proxy itself must not crash or exceed the configured slot count
// concurrently at the upstream.
func TestConcurrentRequestsBoundedNoUpstreamOverload(t *testing.T) {
	var inFlight, maxInFlight int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		fmt.Fprint(w, "ok")
	}))
	defer upstream.Close()
	s, err := NewServer(Config{
		Routes:                []Route{{Prefix: "/npm/", Upstream: mustURL(t, upstream.URL)}},
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		MaxCacheBytes:         1 << 20,
		MaxObjectBytes:        1 << 18,
		MaxConcurrentUpstream: 2,
		UpstreamTimeout:       2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/npm/pkg-%d", i), nil)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("request %d: got status %d", i, rec.Code)
			}
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&maxInFlight); got > 2 {
		t.Fatalf("observed %d concurrent upstream requests, want <= 2 (MaxConcurrentUpstream)", got)
	}
}

// TestHeadServesCachedHeadersOnly covers HEAD semantics against a cached
// entry: no body bytes for a HEAD request.
func TestHeadServesCachedHeadersOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "body-bytes")
	}))
	defer upstream.Close()
	s := newTestServer(t, upstream, nil)

	get := httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil)
	s.ServeHTTP(httptest.NewRecorder(), get)

	head := httptest.NewRequest(http.MethodHead, "/npm/left-pad", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, head)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD response must have no body, got %d bytes", rec.Body.Len())
	}
}

// TestNewServerRejectsBadConfig is a table of invalid configurations that
// must be rejected before ServeHTTP ever runs.
func TestNewServerRejectsBadConfig(t *testing.T) {
	validUpstream := mustURL(t, "https://example.com")
	tmp := t.TempDir()
	cases := []struct {
		name   string
		config Config
	}{
		{"no routes", Config{CacheDir: tmp, MaxCacheBytes: 1, MaxObjectBytes: 1, MaxConcurrentUpstream: 1, UpstreamTimeout: time.Second}},
		{"bad prefix", Config{Routes: []Route{{Prefix: "npm", Upstream: validUpstream}}, CacheDir: tmp, MaxCacheBytes: 1, MaxObjectBytes: 1, MaxConcurrentUpstream: 1, UpstreamTimeout: time.Second}},
		{"relative cache dir", Config{Routes: []Route{{Prefix: "/npm/", Upstream: validUpstream}}, CacheDir: "relative", MaxCacheBytes: 1, MaxObjectBytes: 1, MaxConcurrentUpstream: 1, UpstreamTimeout: time.Second}},
		{"object bigger than cache", Config{Routes: []Route{{Prefix: "/npm/", Upstream: validUpstream}}, CacheDir: tmp, MaxCacheBytes: 10, MaxObjectBytes: 20, MaxConcurrentUpstream: 1, UpstreamTimeout: time.Second}},
		{"zero concurrency", Config{Routes: []Route{{Prefix: "/npm/", Upstream: validUpstream}}, CacheDir: tmp, MaxCacheBytes: 10, MaxObjectBytes: 5, MaxConcurrentUpstream: 0, UpstreamTimeout: time.Second}},
	}
	for _, c := range cases {
		if _, err := NewServer(c.config); err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
		}
	}
}

// A route with a LocalDir answers from it for the files it holds, never
// asking the upstream for those, and still proxies everything else.
func TestLocalDirAnswersBeforeTheUpstream(t *testing.T) {
	var upstreamPaths []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPaths = append(upstreamPaths, r.URL.Path)
		if strings.Contains(r.URL.Path, "private") {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "from upstream")
	}))
	defer upstream.Close()
	local := t.TempDir()
	module := filepath.Join(local, "example.com", "private", "mod", "@v")
	if err := os.MkdirAll(module, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(module, "v1.2.3.mod"), []byte("module example.com/private/mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside the directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(module, "v9.9.9.mod")); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(Config{
		Routes:                []Route{{Prefix: "/gomodproxy/", Upstream: mustURL(t, upstream.URL), LocalDir: local}},
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		MaxCacheBytes:         1 << 20,
		MaxObjectBytes:        1 << 18,
		MaxConcurrentUpstream: 4,
		UpstreamTimeout:       5 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	get := func(path string) (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code, rec.Body.String()
	}
	if code, body := get("/gomodproxy/example.com/private/mod/@v/v1.2.3.mod"); code != http.StatusOK || body != "module example.com/private/mod\n" {
		t.Errorf("a file the directory holds: %d %q", code, body)
	}
	if len(upstreamPaths) != 0 {
		t.Errorf("the upstream was asked for a file the directory holds: %v", upstreamPaths)
	}
	if code, body := get("/gomodproxy/example.com/public/mod/@v/v1.0.0.mod"); code != http.StatusOK || body != "from upstream" {
		t.Errorf("a file the directory lacks: %d %q, want the upstream's", code, body)
	}
	// Nothing else about a module the directory holds is asked of the
	// upstream: its version list, its latest version, a version the
	// directory lacks, and a symlink out of the directory (never followed)
	// are a 404 from here, so a private module's name stays here.
	asked := len(upstreamPaths)
	for _, path := range []string{
		"/gomodproxy/example.com/private/mod/@v/list",
		"/gomodproxy/example.com/private/mod/@latest",
		"/gomodproxy/example.com/private/mod/@v/v2.0.0.zip",
		"/gomodproxy/example.com/private/mod/@v/v9.9.9.mod",
	} {
		if code, body := get(path); code != http.StatusNotFound || strings.Contains(body, "outside") {
			t.Errorf("%s: %d %q, want a 404 from the proxy itself", path, code, body)
		}
	}
	if len(upstreamPaths) != asked {
		t.Errorf("the upstream was asked about a module the directory holds: %v", upstreamPaths[asked:])
	}
}
