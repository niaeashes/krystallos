package server

import (
	"io/fs"
	"net/http"
	"path"
	"strings"

	"gitsynth/internal/app"
)

// webUI serves the built web UI from files. A file that exists is served as
// it is; any other path gets index.html, so a reload on a deep link such as
// /org/repo/issues/1 lands in the UI's own router. The API, git and MCP
// never fall back to the page: their clients get a 404, not HTML.
//
// It is registered as "/" rather than "GET /", because "GET /" conflicts
// with "/mcp", which takes every method. It answers only GET and HEAD, as
// "GET /" would.
func webUI(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		p := r.URL.Path
		switch {
		case under(p, "/api"):
			writeError(w, &app.Error{Code: http.StatusNotFound, Msg: "not found"})
			return
		case under(p, "/git"), under(p, "/mcp"), under(p, "/.well-known"):
			http.NotFound(w, r)
			return
		}

		w.Header().Set("X-Content-Type-Options", "nosniff")
		name := strings.TrimPrefix(path.Clean(p), "/")
		asset := strings.HasPrefix(name, "assets/")
		if !isFile(files, name) {
			if asset {
				// A page from an older build asking for a chunk this build
				// does not have: HTML in its place would only fail to parse.
				http.NotFound(w, r)
				return
			}
			name = "index.html"
		}
		if asset {
			// Vite names these after their content, so a new build brings
			// new names and a cached copy never goes stale.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if path.Ext(name) == ".html" {
			w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
		}
		http.ServeFileFS(w, r, files, name)
	})
}

// under reports whether p is prefix itself or a path below it.
func under(p, prefix string) bool {
	return p == prefix || strings.HasPrefix(p, prefix+"/")
}

// isFile reports whether name is a regular file in files. Directories are
// never served, so there are no listings.
func isFile(files fs.FS, name string) bool {
	info, err := fs.Stat(files, name)
	return err == nil && info.Mode().IsRegular()
}
