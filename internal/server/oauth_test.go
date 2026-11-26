package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const claudeAppRedirect = "claude://claude.ai/mcp-auth-callback/sdk"

// oauthSend sends a request to h and returns the recorded answer, whatever its
// status.
func oauthSend(h http.Handler, method, path, contentType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://localhost:3000"+path, strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// register registers a client by Dynamic Client Registration.
func register(h http.Handler, redirects ...string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"client_name": "Claude", "redirect_uris": redirects, "token_endpoint_auth_method": "none"})
	return oauthSend(h, "POST", "/oauth/register", "application/json", string(body))
}

// TestRegisterAppScheme: Dynamic Client Registration accepts the redirect
// URI the Claude desktop app sends, and refuses every scheme the browser
// handles itself.
func TestRegisterAppScheme(t *testing.T) {
	s, _ := newTestServer(t, "http://localhost:3000")
	h := s.Handler()

	w := register(h, claudeAppRedirect)
	var reg struct {
		ClientID     string   `json:"client_id"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	json.Unmarshal(w.Body.Bytes(), &reg)
	if w.Code != http.StatusCreated || reg.ClientID == "" || len(reg.RedirectURIs) != 1 || reg.RedirectURIs[0] != claudeAppRedirect {
		t.Fatalf("registering %s: %d %s", claudeAppRedirect, w.Code, w.Body)
	}

	for _, bad := range []string{
		"javascript:alert(document.cookie)", "javascript://claude.ai/%0aalert(1)", "JAVASCRIPT://x/%0aalert(1)",
		"vbscript:msgbox(1)", "data:text/html,<script>alert(1)</script>", "data://claude.ai/x",
		"file:///etc/passwd", "blob:http://localhost:3000/x", "filesystem:http://localhost:3000/temporary/x",
		"about:blank", "view-source:http://localhost:3000/", "ws://claude.ai/cb", "wss://claude.ai/cb", "ftp://claude.ai/cb",
		"chrome-extension://abc/cb", "intent://cb/x", "http://claude.ai/cb", "claude:cb",
	} {
		w := register(h, bad)
		var out map[string]string
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != http.StatusBadRequest || out["error"] != "invalid_client_metadata" {
			t.Errorf("registering %q should be refused with invalid_client_metadata: %d %s", bad, w.Code, w.Body)
		}
	}
}

// TestAuthorizeAppScheme: authorize accepts the app-scheme redirect URI the
// client registered, exactly, and answers anything else itself, without
// redirecting.
func TestAuthorizeAppScheme(t *testing.T) {
	s, _ := newTestServer(t, "http://localhost:3000")
	h := s.Handler()
	var reg struct {
		ClientID string `json:"client_id"`
	}
	json.Unmarshal(register(h, claudeAppRedirect).Body.Bytes(), &reg)

	params := func(redirect string) string {
		return url.Values{
			"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect}, "state": {"st"},
			"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"},
			"resource": {"http://localhost:3000/mcp/user/alice/claude"},
		}.Encode()
	}
	w := oauthSend(h, "GET", "/oauth/authorize?"+params(claudeAppRedirect), "", "")
	if w.Code != http.StatusFound || !strings.HasPrefix(w.Header().Get("Location"), "http://localhost:3000/oauth/consent?request=oar_") {
		t.Fatalf("authorize with the registered app-scheme redirect should lead to the consent page: %d %q", w.Code, w.Header().Get("Location"))
	}
	for _, other := range []string{
		"claude://claude.ai/mcp-auth-callback/other", "claude://claude.ai/mcp-auth-callback/sdk/",
		"claude://claude.ai:8080/mcp-auth-callback/sdk", "claude://evil.example/mcp-auth-callback/sdk",
		"claude-dev://claude.ai/mcp-auth-callback/sdk", "https://claude.ai/mcp-auth-callback/sdk",
		"javascript://claude.ai/mcp-auth-callback/sdk",
	} {
		w := oauthSend(h, "GET", "/oauth/authorize?"+params(other), "", "")
		if w.Code != http.StatusBadRequest || w.Header().Get("Location") != "" {
			t.Errorf("authorize with %q should be refused without redirecting: %d %q", other, w.Code, w.Header().Get("Location"))
		}
	}
	// A request that goes wrong after the redirect URI is checked goes back
	// to the app, with a 302 like every answer.
	bad := strings.Replace(params(claudeAppRedirect), "response_type=code", "response_type=token", 1)
	w = oauthSend(h, "GET", "/oauth/authorize?"+bad, "", "")
	back, _ := url.Parse(w.Header().Get("Location"))
	if w.Code != http.StatusFound || back.Scheme != "claude" || back.Query().Get("error") != "unsupported_response_type" || back.Query().Get("state") != "st" {
		t.Fatalf("an unsupported response type should go back to the app: %d %q", w.Code, w.Header().Get("Location"))
	}
}

// TestWithParams: the answer is added to a redirect URI as a query, and
// the rest of the URI stays as the client registered it.
func TestWithParams(t *testing.T) {
	for raw, want := range map[string]string{
		claudeAppRedirect:                  claudeAppRedirect + "?code=c&iss=https%3A%2F%2Fgit.example.com&state=s",
		"com.example.app:/callback":        "com.example.app:/callback?code=c&iss=https%3A%2F%2Fgit.example.com&state=s",
		"com.example.app:///callback":      "com.example.app:///callback?code=c&iss=https%3A%2F%2Fgit.example.com&state=s",
		"http://127.0.0.1:54321/callback":  "http://127.0.0.1:54321/callback?code=c&iss=https%3A%2F%2Fgit.example.com&state=s",
		"https://app.example.com/cb?tab=1": "https://app.example.com/cb?code=c&iss=https%3A%2F%2Fgit.example.com&state=s&tab=1",
	} {
		if got := withParams(raw, map[string]string{"code": "c", "state": "s", "iss": "https://git.example.com"}); got != want {
			t.Errorf("withParams(%q) = %q, want %q", raw, got, want)
		}
	}
}
