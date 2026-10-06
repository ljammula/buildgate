package consoleweb

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedFalseWithPlaceholder(t *testing.T) {
	if Embedded() {
		t.Skip("dist/ holds a built bundle (make console-build); placeholder assertions describe the checked-in state")
	}
	// The checked-in dist/ ships only the placeholder page (no
	// main.dart.js) until `make console-build` runs -- this test's own
	// build has not run that, so Embedded must report false.
	if Embedded() {
		t.Fatal("Embedded() = true, want false for the checked-in placeholder dist/")
	}
}

func TestHandlerServesIndexAtRoot(t *testing.T) {
	if Embedded() {
		t.Skip("dist/ holds a built bundle (make console-build); placeholder assertions describe the checked-in state")
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Factory Console") {
		t.Errorf("body = %q, want placeholder index.html content", rec.Body.String())
	}
}

func TestHandlerSPAFallback(t *testing.T) {
	if Embedded() {
		t.Skip("dist/ holds a built bundle (make console-build); placeholder assertions describe the checked-in state")
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/requests/abc", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "Factory Console") {
		t.Errorf("body = %q, want index.html content served for a deep-link path", rec.Body.String())
	}
}

func TestHandlerServesRealFile(t *testing.T) {
	if !Embedded() {
		t.Skip("dist/ ships no index.html until `make console-build` runs -- this test needs a real build")
	}
	// A path that both names a real embedded file and has an extension
	// (index.html) must reach http.FileServer's own normal handling
	// (which redirects an explicit /index.html to /), not this package's
	// SPA fallback -- proves the fallback in Handler only ever engages for
	// an extensionless, not-embedded path (an SPA route), never a real
	// asset request.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))

	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want %d (http.FileServer's own index.html redirect)", rec.Code, http.StatusMovedPermanently)
	}
	if got := rec.Header().Get("Location"); got != "./" {
		t.Errorf("Location = %q, want %q", got, "./")
	}
}

// TestHandlerFallbackOnlyForExtensionlessPaths proves an extensioned but
// genuinely missing path 404s rather than being swallowed by the SPA
// fallback -- only an extensionless path is assumed to be a console route
// (see console/lib/main.dart's usePathUrlStrategy, which never produces
// an extensioned path of its own). The path is one no build ever emits,
// so this holds whether or not `make console-build` has populated dist/
// in the developer's checkout.
func TestHandlerFallbackOnlyForExtensionlessPaths(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/definitely-not-built.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// withFS swaps the package's embedded dist/ view for fake, restoring it
// when the test ends.
func withFS(t *testing.T, fake fstest.MapFS) {
	t.Helper()
	saved := fsys
	fsys = fake
	t.Cleanup(func() { fsys = saved })
}

// TestStaleBundleWithoutIndexServesPlaceholder: a checkout built before
// dist/index.html stopped being tracked keeps its gitignored main.dart.js
// after pulling, but loses index.html. Found via adversarial review: with
// Embedded() checking main.dart.js alone, / answered with a raw directory
// listing of the bundle.
func TestStaleBundleWithoutIndexServesPlaceholder(t *testing.T) {
	withFS(t, fstest.MapFS{"main.dart.js": {Data: []byte("// js")}, "flutter.js": {Data: []byte("// js")}})
	if Embedded() {
		t.Fatal("Embedded() = true for a bundle with no index.html, want false")
	}
	for _, target := range []string{"/", "/requests/abc"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Factory Console is not embedded") {
			t.Errorf("GET %s = %d %q, want the placeholder page", target, rec.Code, rec.Body.String())
		}
	}
}

// TestUnbuiltIndexHTMLServesPlaceholder: before the placeholder moved into
// placeholderHTML, an explicit /index.html reached it through the file
// server; it must not become a bare 404 on an unbuilt binary.
func TestUnbuiltIndexHTMLServesPlaceholder(t *testing.T) {
	withFS(t, fstest.MapFS{".gitkeep": {Data: nil}})
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Factory Console is not embedded") {
		t.Errorf("GET /index.html = %d %q, want the placeholder page", rec.Code, rec.Body.String())
	}
}

// TestReactBundleIsEmbedded: the React console's build has no main.dart.js;
// its manifest marks it as a real bundle, and a deep link and an asset are
// served from it.
func TestReactBundleIsEmbedded(t *testing.T) {
	withFS(t, fstest.MapFS{
		"index.html":             {Data: []byte("<!doctype html><title>Buildgate</title>")},
		".vite/manifest.json":    {Data: []byte("{}")},
		"assets/index-abc123.js": {Data: []byte("// js")},
	})
	if !Embedded() {
		t.Fatal("Embedded() = false for a bundle with index.html and a Vite manifest, want true")
	}
	for _, target := range []string{"/", "/requests/abc", "/app/projects"} {
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<title>Buildgate</title>") {
			t.Errorf("GET %s = %d %q, want the bundle's index.html", target, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "// js" {
		t.Errorf("GET the asset = %d %q, want the file", rec.Code, rec.Body.String())
	}
}

// TestManifestWithoutIndexServesPlaceholder: a manifest alone is a stale
// bundle, like a main.dart.js alone.
func TestManifestWithoutIndexServesPlaceholder(t *testing.T) {
	withFS(t, fstest.MapFS{".vite/manifest.json": {Data: []byte("{}")}})
	if Embedded() {
		t.Fatal("Embedded() = true for a bundle with no index.html, want false")
	}
}
