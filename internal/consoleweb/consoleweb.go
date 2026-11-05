// Package consoleweb embeds the React console's production build
// (console/dist, copied to dist/ here by `make console-build`) so `factoryd
// serve` serves it at its own address: same origin as the API, no separate
// dev server, no -cors-allow-origin needed. dist/ ships only a tracked
// .gitkeep; `make console-build` (needs Node and npm -- not part of `make
// verify`, which must keep working on a machine with neither) populates it
// with the compiled bundle before a release build. The placeholder page
// shown until then lives in placeholderHTML below, not in dist/, so a real
// build never rewrites a tracked file.
package consoleweb

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// dist is dist/ itself, not embed's own dist/-prefixed root, so a request
// for "/assets/index.js" resolves to "assets/index.js" inside it.
var dist = mustSub(distFS, "dist")

// Bundle is a console build to serve: this binary's embedded one (Embedded,
// Handler), or any other file tree a test hands NewBundle.
type Bundle struct{ fsys fs.FS }

// NewBundle is the console build whose files are f, laid out as dist/ is.
func NewBundle(f fs.FS) Bundle { return Bundle{fsys: f} }

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		// dist/ is always embedded (it ships .gitkeep even unbuilt) --
		// only a broken build of this package itself could reach this.
		panic(err)
	}
	return sub
}

// placeholderHTML is served for the root path (and any SPA route, via the
// same fallback that serves index.html once built) until `make
// console-build` has run.
const placeholderHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Buildgate console is not embedded in this factoryd binary</title></head>
<body>
<h1>Buildgate console is not embedded in this factoryd binary</h1>
<p>This is a placeholder. To have <code>factoryd serve</code> serve the real
console at this same address, build it with Node and npm installed:</p>
<pre>make console-build</pre>
<p>then rebuild/reinstall factoryd. Alternatively, run the console's own dev
server against this API -- see <code>console/README.md</code>.</p>
</body>
</html>
`

// Embedded reports whether this binary was built with a real console
// bundle (make console-build) rather than serving placeholderHTML. A Vite
// build always writes index.html and .vite/manifest.json; both are
// required. index.html alone is not proof (a hand-placed file), and a
// manifest without index.html is a stale or partial bundle that would
// answer / with a raw directory listing instead of the console or the
// placeholder.
func Embedded() bool { return NewBundle(dist).Embedded() }

// Embedded reports whether b is a real console build: see the package
// function Embedded.
func (b Bundle) Embedded() bool {
	for _, name := range []string{"index.html", ".vite/manifest.json"} {
		if _, err := fs.Stat(b.fsys, name); err != nil {
			return false
		}
	}
	return true
}

// contentTypes are the bundle's file types that Go's own table
// (mime.TypeByExtension) lacks. Without one, the type would come from the
// host's mime.types, which differs between machines or is absent.
var contentTypes = map[string]string{
	// The web app manifest that makes the console installable as an app.
	".webmanifest": "application/manifest+json",
}

// Handler serves the embedded console bundle with SPA fallback: a request
// path with no file extension that doesn't match a real embedded file
// serves index.html instead of 404, so the console's own path-based deep
// links (the path builders of console/src/routes/paths.ts) work on a hard reload/refresh, not just client-side
// navigation.
func Handler() http.Handler { return NewBundle(dist).Handler() }

// Handler serves b with the SPA fallback the package function Handler
// describes.
func (b Bundle) Handler() http.Handler {
	fileServer := http.FileServer(http.FS(b.fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		trimmed := strings.TrimPrefix(clean, "/")
		// An unbuilt binary has no index.html to serve, so an explicit
		// /index.html gets the placeholder too rather than a bare 404.
		if trimmed == "index.html" && !b.Embedded() {
			trimmed = ""
		}
		if trimmed != "" && path.Ext(trimmed) == "" {
			if _, err := fs.Stat(b.fsys, trimmed); err != nil {
				trimmed = ""
			}
		}
		if trimmed == "" {
			if !b.Embedded() {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, placeholderHTML)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		if contentType, ok := contentTypes[path.Ext(trimmed)]; ok {
			w.Header().Set("Content-Type", contentType)
		}
		fileServer.ServeHTTP(w, r)
	})
}
