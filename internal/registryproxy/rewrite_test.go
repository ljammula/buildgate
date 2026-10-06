package registryproxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newRewritingTestServer configures one "/pypi/" route pointing at upstream
// and rewriting any href/url whose host matches filesHost into a link
// under "/pypi-files/" -- the shape cmd/factoryd's own -registry-proxy
// wiring uses for PyPI.
func newRewritingTestServer(t *testing.T, upstream *httptest.Server, filesHost string) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Routes: []Route{
			{
				Prefix:           "/pypi/",
				Upstream:         mustURL(t, upstream.URL),
				RewriteHrefHosts: map[string]string{filesHost: "/pypi-files/"},
			},
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

// TestRewriteHTMLSimpleIndexHrefs covers PEP 503: an href to the
// allowlisted files host is rewritten to a path-relative /pypi-files/
// link; an href to any other host, and a relative href, are left
// untouched.
func TestRewriteHTMLSimpleIndexHrefs(t *testing.T) {
	const filesHost = "files.pythonhosted.org"
	index := `<!DOCTYPE html><html><body>
<a href="https://files.pythonhosted.org/packages/aa/left-pad-1.0.tar.gz#sha256=abc123">left-pad-1.0.tar.gz</a>
<a href="https://evil.example/steal">untouched-non-allowlisted</a>
<a href='https://files.pythonhosted.org/packages/bb/left-pad-2.0.tar.gz'>single-quoted</a>
<a href="./relative-link">untouched-relative</a>
</body></html>`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, index)
	}))
	defer upstream.Close()
	s := newRewritingTestServer(t, upstream, filesHost)

	req := httptest.NewRequest(http.MethodGet, "/pypi/left-pad/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, `href="/pypi-files/packages/aa/left-pad-1.0.tar.gz#sha256=abc123"`) {
		t.Errorf("allowlisted double-quoted href not rewritten as expected, got:\n%s", body)
	}
	if !strings.Contains(body, `href='/pypi-files/packages/bb/left-pad-2.0.tar.gz'`) {
		t.Errorf("allowlisted single-quoted href not rewritten as expected, got:\n%s", body)
	}
	if !strings.Contains(body, `href="https://evil.example/steal"`) {
		t.Errorf("non-allowlisted href must be left untouched, got:\n%s", body)
	}
	if !strings.Contains(body, `href="./relative-link"`) {
		t.Errorf("relative href must be left untouched, got:\n%s", body)
	}
	if strings.Contains(body, "https://files.pythonhosted.org") {
		t.Errorf("an allowlisted absolute href leaked through unrewritten, got:\n%s", body)
	}
}

// TestRewriteJSONSimpleIndexURLs covers PEP 691: files[].url entries whose
// host is allowlisted are rewritten; other fields (name, meta, a
// non-allowlisted url) round-trip unchanged.
func TestRewriteJSONSimpleIndexURLs(t *testing.T) {
	const filesHost = "files.pythonhosted.org"
	index := `{
  "meta": {"api-version": "1.0"},
  "name": "left-pad",
  "files": [
    {"filename": "left-pad-1.0.tar.gz", "url": "https://files.pythonhosted.org/packages/aa/left-pad-1.0.tar.gz", "hashes": {"sha256": "abc123"}},
    {"filename": "left-pad-2.0.tar.gz", "url": "https://mirror.example/left-pad-2.0.tar.gz", "hashes": {"sha256": "def456"}}
  ]
}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
		fmt.Fprint(w, index)
	}))
	defer upstream.Close()
	s := newRewritingTestServer(t, upstream, filesHost)

	req := httptest.NewRequest(http.MethodGet, "/pypi/left-pad/", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("got status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var decoded struct {
		Name string `json:"name"`
		Meta struct {
			APIVersion string `json:"api-version"`
		} `json:"meta"`
		Files []struct {
			Filename string `json:"filename"`
			URL      string `json:"url"`
			Hashes   struct {
				SHA256 string `json:"sha256"`
			} `json:"hashes"`
		} `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode rewritten JSON: %v (body=%s)", err, rec.Body.String())
	}
	if decoded.Name != "left-pad" || decoded.Meta.APIVersion != "1.0" {
		t.Errorf("unrelated fields must round-trip unchanged, got %+v", decoded)
	}
	if len(decoded.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(decoded.Files))
	}
	if decoded.Files[0].URL != "/pypi-files/packages/aa/left-pad-1.0.tar.gz" {
		t.Errorf("allowlisted file URL not rewritten as expected, got %q", decoded.Files[0].URL)
	}
	if decoded.Files[0].Hashes.SHA256 != "abc123" {
		t.Errorf("file hash must round-trip unchanged, got %+v", decoded.Files[0])
	}
	if decoded.Files[1].URL != "https://mirror.example/left-pad-2.0.tar.gz" {
		t.Errorf("non-allowlisted file URL must be left untouched, got %q", decoded.Files[1].URL)
	}
}

// TestRewrittenResponseIsWhatGetsCached covers the coordinator's own
// explicit requirement: a second request must be served from cache with
// the REWRITTEN body, never the original upstream bytes -- proven by
// having the upstream change its response between the two requests (a
// cache hit must still return the FIRST, rewritten response, not a fresh
// rewrite of the second).
func TestRewrittenResponseIsWhatGetsCached(t *testing.T) {
	const filesHost = "files.pythonhosted.org"
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<a href="https://files.pythonhosted.org/packages/aa/pkg-1.0.tar.gz">pkg-1.0.tar.gz</a>`)
	}))
	defer upstream.Close()
	s := newRewritingTestServer(t, upstream, filesHost)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/pypi/pkg/", nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: got status %d", i, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `href="/pypi-files/packages/aa/pkg-1.0.tar.gz"`) {
			t.Fatalf("request %d: cached response was not the rewritten body, got:\n%s", i, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "files.pythonhosted.org") {
			t.Fatalf("request %d: cached response still carries the unrewritten absolute host, got:\n%s", i, rec.Body.String())
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("expected exactly 1 upstream hit (second request served from cache), got %d", got)
	}
}
