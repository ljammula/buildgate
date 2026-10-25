package registryproxy

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newRouteServer is a Server with one route at prefix onto upstreamURL.
func newRouteServer(t *testing.T, prefix, upstreamURL string, allowedHosts map[string]bool, logger *log.Logger) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Routes:                []Route{{Prefix: prefix, Upstream: mustURL(t, upstreamURL), AllowedHosts: allowedHosts}},
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		MaxCacheBytes:         1 << 20,
		MaxObjectBytes:        1 << 18,
		MaxConcurrentUpstream: 4,
		UpstreamTimeout:       5 * time.Second,
		Logger:                logger,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func get(s *Server, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// The Maven route serves Maven Central's layout under an upstream with a base
// path (/maven2), byte for byte: the POM, the jar, its checksum and
// maven-metadata.xml, none rewritten.
func TestMavenRouteServesTheRepositoryLayoutUnchanged(t *testing.T) {
	files := map[string]struct{ contentType, body string }{
		"/maven2/junit/junit/4.13.2/junit-4.13.2.pom":      {"text/xml", `<project><url>https://repo.maven.apache.org/x</url></project>`},
		"/maven2/junit/junit/4.13.2/junit-4.13.2.jar":      {"application/java-archive", "PK\x03\x04jar-bytes"},
		"/maven2/junit/junit/4.13.2/junit-4.13.2.jar.sha1": {"text/plain", "4e8d5c0e4c9d4c4e"},
		"/maven2/junit/junit/maven-metadata.xml":           {"text/xml", `<metadata><versioning><latest>4.13.2</latest></versioning></metadata>`},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", file.contentType)
		fmt.Fprint(w, file.body)
	}))
	defer upstream.Close()
	s := newRouteServer(t, "/maven/", upstream.URL+"/maven2", nil, nil)

	for path, file := range files {
		rec := get(s, "/maven/"+strings.TrimPrefix(path, "/maven2/"))
		if rec.Code != http.StatusOK || rec.Body.String() != file.body || rec.Header().Get("Content-Type") != file.contentType {
			t.Errorf("%s: status %d, type %q, body %q; want 200, %q, the upstream's bytes", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.String(), file.contentType)
		}
	}
	if rec := get(s, "/maven/nonexistent/x/1/x-1.pom"); rec.Code != http.StatusNotFound {
		t.Errorf("a missing artifact: status %d, want the upstream's 404", rec.Code)
	}
}

// The RubyGems route serves Bundler's compact index (/versions, /info/<gem>)
// and gem downloads byte for byte: the compact index names no absolute URL,
// and Bundler checks each info file against the MD5 the versions file lists,
// so a rewritten body would fail its check.
func TestRubyGemsRouteServesTheCompactIndexAndGemsUnchanged(t *testing.T) {
	files := map[string]struct{ contentType, body string }{
		"/versions":               {"text/plain; charset=utf-8", "created_at: 2026-10-24T00:00:00Z\n---\nrake 13.2.1 0f5b7c3a\n"},
		"/info/rake":              {"text/plain; charset=utf-8", "---\n13.2.1 |checksum:46cb38dae65d7d74b6020a4ac9d48afed8eb8149c040eccf0523bec91907059d,ruby:>= 2.3\n"},
		"/gems/rake-13.2.1.gem":   {"application/octet-stream", "\x00gem-bytes\xff"},
		"/specs.4.8.gz":           {"application/octet-stream", "\x1f\x8bspecs"},
		"/api/v1/dependencies":    {"application/octet-stream", ""},
		"/quick/Marshal.4.8/x.rz": {"binary/octet-stream", "x\x9cmarshal"},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", file.contentType)
		fmt.Fprint(w, file.body)
	}))
	defer upstream.Close()
	s := newRouteServer(t, "/rubygems/", upstream.URL, nil, nil)

	for path, file := range files {
		rec := get(s, "/rubygems"+path)
		if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), []byte(file.body)) {
			t.Errorf("%s: status %d, body %q; want 200 and the upstream's bytes", path, rec.Code, rec.Body.String())
		}
	}
}

// A redirect is followed only to a host the route allows, for both new
// routes: by default neither allows any host beyond its upstream's own.
func TestMavenAndRubyGemsRoutesFollowOnlyAllowedRedirects(t *testing.T) {
	for _, prefix := range []string{"/maven/", "/rubygems/"} {
		t.Run(strings.Trim(prefix, "/"), func(t *testing.T) {
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "redirected-bytes")
			}))
			defer target.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
			}))
			defer upstream.Close()

			refusing := newRouteServer(t, prefix, upstream.URL, nil, nil)
			rec := get(refusing, prefix+"a/b")
			if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "redirected-bytes") || rec.Header().Get("Location") != "" {
				t.Errorf("redirect to a host the route does not allow: status %d, body %q; want 502 and nothing relayed", rec.Code, rec.Body.String())
			}

			allowing := newRouteServer(t, prefix, upstream.URL, map[string]bool{mustURL(t, target.URL).Host: true}, nil)
			if rec := get(allowing, prefix+"a/b"); rec.Code != http.StatusOK || rec.Body.String() != "redirected-bytes" {
				t.Errorf("redirect to an allowed host: status %d, body %q; want 200 and the target's bytes", rec.Code, rec.Body.String())
			}
		})
	}
}

// An upstream failure on either new route is counted and logged exactly as on
// the others: internal/sandbox reads the log line, not the route.
func TestMavenAndRubyGemsUpstreamFailuresAreCounted(t *testing.T) {
	for _, prefix := range []string{"/maven/", "/rubygems/"} {
		t.Run(strings.Trim(prefix, "/"), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			}))
			defer upstream.Close()
			var logged bytes.Buffer
			s := newRouteServer(t, prefix, upstream.URL, nil, log.New(&logged, "", 0))
			if rec := get(s, prefix+"x"); rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status %d, want the upstream's 503 relayed", rec.Code)
			}
			if s.UpstreamFailures() != 1 || !strings.Contains(logged.String(), UpstreamFailureLogPrefix+"1") {
				t.Errorf("failures %d, log %q; want 1 and its log line", s.UpstreamFailures(), logged.String())
			}
		})
	}
}
