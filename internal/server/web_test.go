package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const testIndex = `<!doctype html><div id="root"></div>`

func webFixture() fstest.MapFS {
	return fstest.MapFS{
		"index.html":       {Data: []byte(testIndex)},
		"robots.txt":       {Data: []byte("User-agent: *\n")},
		"assets/a.js":      {Data: []byte("console.log(1)")},
		"docs/readme.html": {Data: []byte("<p>doc</p>")},
	}
}

func serve(h http.Handler, method, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, target, nil))
	return w
}

func TestWebUI(t *testing.T) {
	h := (&Server{Web: webFixture()}).Handler()

	page := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if w.Code != http.StatusOK || w.Body.String() != testIndex {
			t.Fatalf("want index.html, got %d %q", w.Code, w.Body.String())
		}
		hd := w.Header()
		if got := hd.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", got)
		}
		if got := hd.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", got)
		}
		if got := hd.Get("Content-Security-Policy"); got != "frame-ancestors 'none'" {
			t.Errorf("Content-Security-Policy = %q", got)
		}
		if got := hd.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", got)
		}
	}

	t.Run("root", func(t *testing.T) { page(t, serve(h, "GET", "/")) })
	t.Run("deep link", func(t *testing.T) { page(t, serve(h, "GET", "/niaeashes/gitsynth/issues/1")) })
	t.Run("sign-in link", func(t *testing.T) { page(t, serve(h, "GET", "/register?token=abc")) })
	t.Run("directory gets the page, not a listing", func(t *testing.T) {
		page(t, serve(h, "GET", "/docs/"))
		page(t, serve(h, "GET", "/assets"))
	})

	t.Run("hashed asset", func(t *testing.T) {
		w := serve(h, "GET", "/assets/a.js")
		if w.Code != http.StatusOK || w.Body.String() != "console.log(1)" {
			t.Fatalf("got %d %q", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("Cache-Control = %q", got)
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
			t.Errorf("Content-Type = %q", got)
		}
		if got := w.Header().Get("Content-Security-Policy"); got != "" {
			t.Errorf("Content-Security-Policy on a script: %q", got)
		}
	})

	t.Run("other file", func(t *testing.T) {
		w := serve(h, "GET", "/robots.txt")
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Body.String(), "User-agent") {
			t.Fatalf("got %d %q", w.Code, w.Body.String())
		}
		if got := w.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("Cache-Control = %q, want no-cache", got)
		}
	})

	t.Run("head", func(t *testing.T) {
		w := serve(h, "HEAD", "/niaeashes/gitsynth")
		if w.Code != http.StatusOK || w.Body.Len() != 0 {
			t.Fatalf("got %d with %d bytes", w.Code, w.Body.Len())
		}
	})

	notFound := []string{"/assets/missing.js", "/git/x", "/git", "/mcp/x"}
	for _, target := range notFound {
		t.Run("404 "+target, func(t *testing.T) {
			w := serve(h, "GET", target)
			if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "<") {
				t.Fatalf("got %d %q", w.Code, w.Body.String())
			}
		})
	}

	for _, target := range []string{"/api/nope", "/api"} {
		t.Run("json 404 "+target, func(t *testing.T) {
			w := serve(h, "GET", target)
			if w.Code != http.StatusNotFound {
				t.Fatalf("got %d", w.Code)
			}
			if got := w.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
			if !strings.Contains(w.Body.String(), `"error":"not found"`) {
				t.Errorf("body = %q", w.Body.String())
			}
		})
	}

	t.Run("healthz", func(t *testing.T) {
		if w := serve(h, "GET", "/healthz"); w.Code != http.StatusOK || w.Body.String() != "ok\n" {
			t.Fatalf("got %d %q", w.Code, w.Body.String())
		}
	})

	t.Run("post", func(t *testing.T) {
		w := serve(h, "POST", "/")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("got %d", w.Code)
		}
		if got := w.Header().Get("Allow"); got != "GET, HEAD" {
			t.Errorf("Allow = %q", got)
		}
	})
}

func TestNoWebUI(t *testing.T) {
	h := (&Server{}).Handler()
	for _, target := range []string{"/", "/niaeashes/gitsynth/issues/1"} {
		if w := serve(h, "GET", target); w.Code != http.StatusNotFound {
			t.Errorf("GET %s: got %d, want 404", target, w.Code)
		}
	}
}
