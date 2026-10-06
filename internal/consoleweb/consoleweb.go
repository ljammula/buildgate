// Package consoleweb embeds the Flutter Web console build
// (console/build/web) so `factoryd serve` can serve it directly at its own
// address -- same origin as the API, no separate Flutter dev server, no
// -cors-allow-origin needed. dist/ ships only a tracked .gitkeep by
// default; `make console-build` (needs Flutter -- not part of `make
// verify`, which must keep working on a machine with none installed)
// populates it with the real compiled bundle before a release build. The
// placeholder page shown until then lives in placeholderHTML below, not in
// dist/, so a real build never has to rewrite a tracked file (a rebuild of
// unchanged console sources previously dirtied `make install` this way).
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

// fsys is dist/ itself, not embed's own dist/-prefixed root, so a request
// for "/main.dart.js" resolves to "main.dart.js" inside it.
var fsys = mustSub(distFS, "dist")

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
// console-build` has run. Byte-identical to the page this package used to
// serve from a tracked dist/index.html.
const placeholderHTML = `<!doctype html>
<html lang="en">
<head><meta charset="utf-8"><title>Factory Console not built in</title></head>
<body>
<h1>Factory Console is not embedded in this factoryd binary</h1>
<p>This is a placeholder. To have <code>factoryd serve</code> serve the real
console at this same address, build it with Flutter installed:</p>
<pre>make console-build</pre>
<p>then rebuild/reinstall factoryd. Alternatively, run the console's own dev
server against this API -- see <code>console/README.md</code>.</p>
</body>
</html>
`

// Embedded reports whether this binary was built with a real console
// bundle (make console-build) rather than serving placeholderHTML.
// main.dart.js is the Flutter Web build's own entry point script, present
// in every real build. index.html is required too (found via adversarial
// review): a checkout built before dist/index.html stopped being tracked
// loses that file on pull but keeps its gitignored main.dart.js, and
// without this check such a binary would answer / with a raw directory
// listing of the bundle instead of the console or the placeholder.
func Embedded() bool {
	if _, err := fs.Stat(fsys, "index.html"); err != nil {
		return false
	}
	for _, entry := range bundleEntryPoints {
		if _, err := fs.Stat(fsys, entry); err == nil {
			return true
		}
	}
	return false
}

// bundleEntryPoints names the file each console build always writes
// beside index.html: the Flutter Web build's entry script, and the build
// manifest of the React console (console-react/, `make
// console-react-build`), which is embedded in place of the Flutter one
// while it is being proven.
var bundleEntryPoints = []string{"main.dart.js", ".vite/manifest.json"}

// Handler serves the embedded console bundle with SPA fallback: a request
// path with no file extension that doesn't match a real embedded file
// serves index.html instead of 404, so the console's own path-based deep
// links (e.g. /requests/<id>, see console/lib/main.dart's
// usePathUrlStrategy) work on a hard reload/refresh, not just client-side
// navigation.
func Handler() http.Handler {
	fileServer := http.FileServer(http.FS(fsys))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := path.Clean(r.URL.Path)
		trimmed := strings.TrimPrefix(clean, "/")
		// An unbuilt binary has no index.html to serve, so an explicit
		// /index.html gets the placeholder too rather than a bare 404.
		if trimmed == "index.html" && !Embedded() {
			trimmed = ""
		}
		if trimmed != "" && path.Ext(trimmed) == "" {
			if _, err := fs.Stat(fsys, trimmed); err != nil {
				trimmed = ""
			}
		}
		if trimmed == "" {
			if !Embedded() {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, placeholderHTML)
				return
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}
