package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"gitsynth/internal/gitx"
)

// send is a request whose body may be any size, sent with its length
// declared or, with sized false, without, as a chunked body is.
func send(h http.Handler, ctx context.Context, method, path, token string, body []byte, sized bool, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, "http://localhost:3000"+path, bytes.NewReader(body))
	if !sized {
		r.ContentLength = -1
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// TestOversizedBodiesAreRefused sends bodies past the cap, with their length
// declared and without: the API, the passkey finish endpoints, MCP and the
// hooks' endpoint answer 413 in the usual JSON shape.
func TestOversizedBodiesAreRefused(t *testing.T) {
	s, tok := newTestServer(t, "http://localhost:3000")
	h := s.Handler()
	huge := []byte(`{"name":"` + strings.Repeat("a", maxBody) + `"}`)

	for _, tc := range []struct {
		name, path, token string
		h                 http.Handler
		sized             bool
	}{
		{"API, sized", "/api/orgs", tok, h, true},
		{"API, chunked", "/api/orgs", tok, h, false},
		{"passkey registration, sized", "/api/auth/register/finish?ceremony=x", "", h, true},
		{"passkey registration, chunked", "/api/auth/register/finish?ceremony=x", "", h, false},
		{"passkey sign-in, sized", "/api/auth/login/finish?ceremony=x", "", h, true},
		{"passkey sign-in, chunked", "/api/auth/login/finish?ceremony=x", "", h, false},
		{"MCP, sized", "/mcp", tok, h, true},
		{"hooks, sized", "/internal/ref-txn", "", s.InternalHandler(), true},
	} {
		w := send(tc.h, context.Background(), "POST", tc.path, tc.token, huge, tc.sized, map[string]string{"Content-Type": "application/json"})
		var out struct {
			Error string `json:"error"`
		}
		if w.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Error == "" {
			t.Errorf("%s: got %d %q, want 413 with a JSON error", tc.name, w.Code, w.Body.String())
		}
	}

	// Just under the cap is still read.
	w := send(h, context.Background(), "POST", "/api/orgs", tok, []byte(strings.Repeat(" ", maxBody-20)+`{"name":"acme"}`), false, nil)
	if w.Code != http.StatusOK {
		t.Errorf("a body under the cap: got %d %s", w.Code, w.Body.String())
	}
}

// TestGitBodiesAreNotCapped sends upload-pack a request past the cap that
// bodies elsewhere have: git gets all of it.
func TestGitBodiesAreNotCapped(t *testing.T) {
	needGit(t)
	s, tok := newTestServer(t, "http://localhost:3000")
	h := s.Handler()
	call(t, h, "POST", "/api/orgs", tok, `{"name":"acme"}`, nil, nil)
	call(t, h, "POST", "/api/orgs/acme/repos", tok, `{"name":"app"}`, nil, nil)

	var req bytes.Buffer
	req.Write(gitx.PktLine("command=ls-refs\n"))
	req.WriteString("0001")
	prefix := "ref-prefix refs/heads/" + strings.Repeat("x", 60000) + "\n"
	for req.Len() <= maxBody {
		req.Write(gitx.PktLine(prefix))
	}
	req.WriteString("0000")

	for _, sized := range []bool{true, false} {
		w := send(h, context.Background(), "POST", "/git/acme/app.git/git-upload-pack", tok, req.Bytes(), sized,
			map[string]string{"Git-Protocol": "version=2", "Content-Type": "application/x-git-upload-pack-request"})
		if w.Code != http.StatusOK || w.Body.String() != "0000" {
			t.Errorf("ls-refs of %d bytes (length declared: %v): got %d %q, want git's empty answer", req.Len(), sized, w.Code, w.Body.String())
		}
	}
}

// TestGitWaitsForASlot holds the only git slot: a git request waits for it,
// gives up with 503 when the client goes away, and goes ahead once the slot
// is released.
func TestGitWaitsForASlot(t *testing.T) {
	needGit(t)
	s, tok := newTestServer(t, "http://localhost:3000")
	s.MaxGit = 1
	h := s.Handler()
	call(t, h, "POST", "/api/orgs", tok, `{"name":"acme"}`, nil, nil)
	call(t, h, "POST", "/api/orgs/acme/repos", tok, `{"name":"app"}`, nil, nil)

	release, ok := s.acquireGit(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if !ok {
		t.Fatal("the first slot should be free")
	}

	start := func(ctx context.Context, path string, headers map[string]string) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- send(h, ctx, "GET", path, tok, nil, true, headers) }()
		return done
	}
	waiting := func(what string, done <-chan *httptest.ResponseRecorder) {
		t.Helper()
		select {
		case w := <-done:
			t.Fatalf("%s should wait for the slot, got %d %q", what, w.Code, w.Body.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	finish := func(what string, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case w := <-done:
			return w
		case <-time.After(10 * time.Second):
			t.Fatalf("%s did not finish", what)
			return nil
		}
	}

	// A push's ref advertisement writes a preamble before git starts; the
	// wait comes before it, so giving up is still a clean 503.
	ctx, cancel := context.WithCancel(context.Background())
	push := start(ctx, "/git/acme/app.git/info/refs?service=git-receive-pack", nil)
	waiting("a push", push)
	cancel()
	w := finish("a push", push)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
		t.Fatalf("a push given up on: got %d (Retry-After %q) %q, want 503 with Retry-After", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}

	fetch := start(context.Background(), "/git/acme/app.git/info/refs?service=git-upload-pack", map[string]string{"Git-Protocol": "version=2"})
	waiting("a fetch", fetch)
	release()
	w = finish("a fetch", fetch)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "version 2") {
		t.Fatalf("a fetch after the slot was released: got %d %q", w.Code, w.Body.String())
	}
	if n := len(s.gitSlots); n != 0 {
		t.Fatalf("%d slots still held after git exited", n)
	}
	// The server's own git work, here reading refs for the API, does not
	// wait for a slot.
	release, _ = s.acquireGit(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	defer release()
	call(t, h, "GET", "/api/repos/acme/app", tok, "", nil, nil)
}
