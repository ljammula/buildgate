package registryproxy

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// failureServer is a Server on one upstream with a short upstream timeout,
// whose log the test can read.
func failureServer(t *testing.T, upstreamURL string, timeout time.Duration) (*Server, *bytes.Buffer) {
	t.Helper()
	logged := &bytes.Buffer{}
	s, err := NewServer(Config{
		Routes:                []Route{{Prefix: "/npm/", Upstream: mustURL(t, upstreamURL)}},
		CacheDir:              filepath.Join(t.TempDir(), "cache"),
		MaxCacheBytes:         1 << 20,
		MaxObjectBytes:        1 << 18,
		MaxConcurrentUpstream: 4,
		UpstreamTimeout:       timeout,
		Logger:                log.New(logged, "", log.LstdFlags),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, logged
}

func serve(s *Server, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func TestAnUpstreamThatCannotBeReachedIsCounted(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	url := upstream.URL
	upstream.Close() // nothing listens there any more: the dial is refused
	s, logged := failureServer(t, url, 5*time.Second)

	for want := int64(1); want <= 2; want++ {
		if rec := serve(s, http.MethodGet, "/npm/left-pad"); rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", rec.Code)
		}
		if got := s.UpstreamFailures(); got != want {
			t.Fatalf("UpstreamFailures = %d, want %d", got, want)
		}
	}
	// The host reads the count from the log: one line per failure, each with
	// the total so far.
	for _, line := range []string{UpstreamFailureLogPrefix + "1\n", UpstreamFailureLogPrefix + "2\n"} {
		if !strings.Contains(logged.String(), " "+line) {
			t.Errorf("log lacks %q:\n%s", line, logged.String())
		}
	}
}

func TestAnUpstreamThatDoesNotAnswerInTimeIsCounted(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer upstream.Close()
	defer close(release)
	s, _ := failureServer(t, upstream.URL, 50*time.Millisecond)

	if rec := serve(s, http.MethodGet, "/npm/left-pad"); rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", rec.Code)
	}
	if got := s.UpstreamFailures(); got != 1 {
		t.Errorf("UpstreamFailures = %d, want 1", got)
	}
}

func TestAnUpstream5xxIsCountedAndRelayed(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		s, _ := failureServer(t, upstream.URL, 5*time.Second)
		if rec := serve(s, method, "/npm/left-pad"); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status = %d, want the upstream's 503", method, rec.Code)
		}
		if got := s.UpstreamFailures(); got != 1 {
			t.Errorf("%s: UpstreamFailures = %d, want 1", method, got)
		}
		upstream.Close()
	}
}

// A response that starts and then stops arriving is the upstream's failure
// too: the client got a truncated package.
func TestAnUpstreamBodyCutShortIsCounted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = fmt.Fprint(w, "partial")
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}))
	defer upstream.Close()
	s, _ := failureServer(t, upstream.URL, 5*time.Second)

	serve(s, http.MethodGet, "/npm/left-pad")
	if got := s.UpstreamFailures(); got != 1 {
		t.Errorf("UpstreamFailures = %d, want 1", got)
	}
}

// What a build can cause on its own says nothing about the registry: a
// package that does not exist, any other 4xx, a request the proxy refuses.
func TestWhatAClientCanCauseIsNotCounted(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the redirect target was contacted")
	}))
	defer elsewhere.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/limited":
			w.WriteHeader(http.StatusTooManyRequests)
		case "/moved":
			http.Redirect(w, r, elsewhere.URL+"/steal", http.StatusFound)
		default:
			_, _ = fmt.Fprint(w, "ok")
		}
	}))
	defer upstream.Close()
	s, logged := failureServer(t, upstream.URL, 5*time.Second)

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"a package that does not exist", http.MethodGet, "/npm/missing", http.StatusNotFound},
		{"another 4xx", http.MethodGet, "/npm/limited", http.StatusTooManyRequests},
		{"a refused method", http.MethodPost, "/npm/left-pad", http.StatusMethodNotAllowed},
		{"a path no route has", http.MethodGet, "/other/left-pad", http.StatusNotFound},
		{"a traversal", http.MethodGet, "/npm/../etc/passwd", http.StatusNotFound},
		{"a redirect to a host the route does not allow", http.MethodGet, "/npm/moved", http.StatusBadGateway},
		{"a served package", http.MethodGet, "/npm/left-pad", http.StatusOK},
		{"a cache hit", http.MethodGet, "/npm/left-pad", http.StatusOK},
	} {
		if rec := serve(s, tc.method, tc.path); rec.Code != tc.want {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
		}
		if got := s.UpstreamFailures(); got != 0 {
			t.Fatalf("%s: UpstreamFailures = %d, want 0", tc.name, got)
		}
	}
	if strings.Contains(logged.String(), UpstreamFailureLogPrefix) {
		t.Errorf("a failure line was logged with none counted:\n%s", logged.String())
	}
}

// A client that gives up on a request is not the upstream failing it.
func TestARequestItsClientAbandonedIsNotCounted(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer upstream.Close()
	defer close(release)
	s, _ := failureServer(t, upstream.URL, 5*time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/npm/left-pad", nil).WithContext(ctx))
	if got := s.UpstreamFailures(); got != 0 {
		t.Errorf("UpstreamFailures = %d, want 0", got)
	}
}
