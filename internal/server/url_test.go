package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitsynth/internal/accesslog"
	"gitsynth/internal/app"
)

// forged are headers a client can send to pose as reached at another
// address. The server's own URL is the only address it uses.
var forged = map[string]string{
	"Host":              "evil.example",
	"X-Forwarded-Proto": "https",
	"X-Forwarded-Host":  "evil.example",
	"Forwarded":         "proto=https;host=evil.example",
}

// newTestServer is a server at url, initialized with an admin, alice,
// whose token it returns.
func newTestServer(t *testing.T, url string, origins ...string) (*Server, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	a, err := app.Open(ctx, app.Config{DataDir: filepath.Join(dir, "data"), GitBin: "git"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if err := a.Initialize(ctx, "git.example.com"); err != nil {
		t.Fatal(err)
	}
	id, err := a.CreateUser(ctx, app.System, "alice", true)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := a.IssueToken(ctx, app.System, id.String(), 0)
	if err != nil {
		t.Fatal(err)
	}
	access, err := accesslog.Open(filepath.Join(dir, "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { access.Close() })
	return &Server{App: a, Access: access, URL: url, Origins: origins}, tok.Token
}

// call sends a request to h with the given token (if any) and headers, and
// decodes a JSON answer into out (if any).
func call(t *testing.T, h http.Handler, method, path, token, body string, headers map[string]string, out any) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://localhost:3000"+path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		if k == "Host" {
			r.Host = v
		} else {
			r.Header.Set(k, v)
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
	return w
}

func TestCloneURLComesFromTheServerURL(t *testing.T) {
	s, tok := newTestServer(t, "http://localhost:3000")
	h := s.Handler()
	const want = "http://localhost:3000/git/acme/app.git"

	call(t, h, "POST", "/api/orgs", tok, `{"name":"acme"}`, nil, nil)
	var created struct {
		CloneURL string `json:"clone_url"`
	}
	call(t, h, "POST", "/api/orgs/acme/repos", tok, `{"name":"app"}`, forged, &created)
	if created.CloneURL != want {
		t.Errorf("clone URL on create = %q, want %q", created.CloneURL, want)
	}

	for name, headers := range map[string]map[string]string{"plain": nil, "forged": forged} {
		var got struct {
			CloneURL string `json:"clone_url"`
		}
		call(t, h, "GET", "/api/repos/acme/app", tok, "", headers, &got)
		if got.CloneURL != want {
			t.Errorf("%s request: clone URL = %q, want %q", name, got.CloneURL, want)
		}
	}

	s.URL = "https://git.example.com"
	var got struct {
		CloneURL string `json:"clone_url"`
	}
	call(t, s.Handler(), "GET", "/api/repos/acme/app", tok, "", nil, &got)
	if got.CloneURL != "https://git.example.com/git/acme/app.git" {
		t.Errorf("clone URL = %q, want it under https://git.example.com", got.CloneURL)
	}
}

func TestSessionCookieIsSecureWhenTheURLIsHTTPS(t *testing.T) {
	for _, tc := range []struct {
		url     string
		headers map[string]string
		secure  bool
	}{
		{"http://localhost:3000", nil, false},
		{"http://localhost:3000", forged, false},
		{"https://git.example.com", nil, true},
		{"https://git.example.com", map[string]string{"X-Forwarded-Proto": "http"}, true},
	} {
		s, tok := newTestServer(t, tc.url)
		h := s.Handler()
		link, err := s.App.IssueEnrollmentLink(context.Background(), app.System, "alice", "login", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		signIn := call(t, h, "POST", "/api/auth/link", "", `{"token":"`+link+`"}`, tc.headers, nil)
		signOut := call(t, h, "POST", "/api/auth/logout", tok, "", tc.headers, nil)
		for what, w := range map[string]*httptest.ResponseRecorder{"sign-in": signIn, "sign-out": signOut} {
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Name != sessionCookie {
				t.Fatalf("%s at %s: want the session cookie, got %v", what, tc.url, cookies)
			}
			if cookies[0].Secure != tc.secure {
				t.Errorf("%s at %s with headers %v: Secure = %v, want %v", what, tc.url, tc.headers, cookies[0].Secure, tc.secure)
			}
		}
	}
}

func TestInvitationLinkStartsFromAKnownOrigin(t *testing.T) {
	s, tok := newTestServer(t, "https://git.example.com", "http://localhost:5173")
	h := s.Handler()
	for _, tc := range []struct {
		name    string
		headers map[string]string
		want    string
	}{
		{"no origin", nil, "https://git.example.com"},
		{"the server's URL", map[string]string{"Origin": "https://git.example.com"}, "https://git.example.com"},
		{"another accepted origin", map[string]string{"Origin": "http://localhost:5173"}, "http://localhost:5173"},
		{"an unknown origin", map[string]string{"Origin": "https://evil.example"}, "https://git.example.com"},
		{"a near miss", map[string]string{"Origin": "https://git.example.com.evil.example"}, "https://git.example.com"},
		{"forged forwarding headers", forged, "https://git.example.com"},
	} {
		var inv struct {
			URL string `json:"url"`
		}
		call(t, h, "POST", "/api/invitations", tok, `{}`, tc.headers, &inv)
		if !strings.HasPrefix(inv.URL, tc.want+"/register?token=gsl_") {
			t.Errorf("%s: invitation link = %q, want it under %s", tc.name, inv.URL, tc.want)
		}
	}
}
