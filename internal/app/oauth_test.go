package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPublicAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.215.14": true, "2606:2800:21f:cb07:6820:80da:af6b:8b2c": true,
		"127.0.0.1": false, "::1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false,
		"169.254.169.254": false, "fe80::1": false, "fd00::1": false, "100.64.0.1": false, "0.0.0.0": false,
		"::": false, "224.0.0.1": false, "::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "64:ff9b::a00:1": false,
	} {
		if got := publicAddr(netip.MustParseAddr(addr), false); got != want {
			t.Errorf("publicAddr(%s) = %v, want %v", addr, got, want)
		}
	}
	if !publicAddr(netip.MustParseAddr("127.0.0.1"), true) {
		t.Error("loopback should be allowed when the test seam asks for it")
	}
}

func TestCheckMetadataURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://claude.ai/oauth/claude-code-client-metadata": true,
		"http://claude.ai/oauth/metadata":                     false,
		"https://claude.ai":                                   false,
		"https://claude.ai/":                                  false,
		"https://user:pw@claude.ai/x":                         false,
		"https://claude.ai/x#frag":                            false,
	} {
		if err := checkMetadataURL(raw); (err == nil) != ok {
			t.Errorf("checkMetadataURL(%q) = %v, want ok=%v", raw, err, ok)
		}
	}
}

// TestCheckRedirectURI: https, http on loopback, and a native app's
// private-use scheme are accepted; http elsewhere, and every scheme the
// browser handles itself, are not.
func TestCheckRedirectURI(t *testing.T) {
	for _, ok := range []string{
		"https://app.example.com/cb",
		"https://app.example.com/cb?x=1",
		"http://localhost/callback",
		"http://127.0.0.1:8080/callback",
		"http://[::1]:8080/callback",
		"claude://claude.ai/mcp-auth-callback/sdk",
		"com.example.app:/oauth2redirect/example-provider",
		"com.example.app:///callback",
		"my-app+dev.1://cb/x",
	} {
		if err := checkRedirectURI(ok); err != nil {
			t.Errorf("checkRedirectURI(%q) = %v, want it accepted", ok, err)
		}
	}
	for _, bad := range []string{
		// http off loopback, and https without a host.
		"http://evil.example/cb", "http://localhost.evil.example/cb", "http:/cb", "https:/cb", "https:cb",
		// Schemes the browser handles itself.
		"javascript:alert(1)", "javascript://claude.ai/%0aalert(1)", "JavaScript://x/%0aalert(1)",
		"vbscript:msgbox(1)", "livescript:x", "mocha:x",
		"data:text/html,<script>alert(1)</script>", "data://x/y",
		"file:///etc/passwd", "file://host/share",
		"blob:https://gitsynth.example.com/0f1e", "filesystem:https://gitsynth.example.com/temporary/x",
		"about:blank", "about://blank/", "view-source:https://gitsynth.example.com/",
		"ws://localhost/cb", "wss://example.com/cb", "ftp://example.com/cb",
		"mhtml:https://example.com/x", "jar:https://example.com/x.jar!/y",
		"chrome://settings/", "chrome-extension://abc/cb", "moz-extension://abc/cb", "safari-web-extension://abc/cb",
		"resource://gre/x", "intent://scan/#Intent;scheme=zxing;end", "android-app://com.example/https/x",
		// Not a URI an app can get a query on, or not a URI at all.
		"com.example.app:callback", "claude:", "1app://cb/x", "-app://cb/x", "://cb/x", "/callback", "",
		" claude://claude.ai/cb", "java\tscript:alert(1)", "claude://claude.ai/cb\n",
		// A fragment or a user, in any scheme.
		"claude://claude.ai/cb#x", "claude://claude.ai/cb#", "claude://user@claude.ai/cb", "https://u:p@app.example.com/cb",
	} {
		if err := checkRedirectURI(bad); err == nil {
			t.Errorf("checkRedirectURI(%q) accepted it", bad)
		}
	}
}

func TestRedirectAllowed(t *testing.T) {
	reg := []string{"http://localhost/callback", "http://127.0.0.1/callback", "https://app.example.com/cb",
		"claude://claude.ai/mcp-auth-callback/sdk", "com.example.app:/callback"}
	for uri, want := range map[string]bool{
		"http://localhost:51234/callback": true,
		"http://127.0.0.1:8080/callback":  true,
		"http://localhost/callback":       true,
		"http://localhost:51234/other":    false,
		"http://127.0.0.2:80/callback":    false,
		"https://app.example.com/cb":      true,
		"https://app.example.com:444/cb":  false,
		"https://evil.example.com/cb":     false,
		"http://app.example.com/cb":       false,
		// A private-use scheme matches only exactly: no other port, path,
		// query, host or scheme.
		"claude://claude.ai/mcp-auth-callback/sdk":      true,
		"claude://claude.ai:1234/mcp-auth-callback/sdk": false,
		"claude://claude.ai/mcp-auth-callback/sdk/":     false,
		"claude://claude.ai/mcp-auth-callback/other":    false,
		"claude://claude.ai/mcp-auth-callback/sdk?x=1":  false,
		"claude://evil.example/mcp-auth-callback/sdk":   false,
		"Claude://claude.ai/mcp-auth-callback/sdk":      false,
		"claude-dev://claude.ai/mcp-auth-callback/sdk":  false,
		"https://claude.ai/mcp-auth-callback/sdk":       false,
		"javascript://claude.ai/mcp-auth-callback/sdk":  false,
		"com.example.app:/callback":                     true,
		"com.example.app:///callback":                   false,
		"com.example.app://x/callback":                  false,
	} {
		if got := redirectAllowed(reg, uri); got != want {
			t.Errorf("redirectAllowed(%q) = %v, want %v", uri, got, want)
		}
	}
}

// TestRedirectKind is what the consent page says about where the answer
// goes.
func TestRedirectKind(t *testing.T) {
	for raw, want := range map[string]string{
		"https://app.example.com/cb":               RedirectWeb,
		"http://127.0.0.1:54321/callback":          RedirectLoopback,
		"claude://claude.ai/mcp-auth-callback/sdk": RedirectApp,
		"com.example.app:/callback":                RedirectApp,
	} {
		u, _ := url.Parse(raw)
		if got, err := redirectKind(u); got != want || err != nil {
			t.Errorf("redirectKind(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

// TestReturnOnce: an answer on its way to the client can be taken once,
// and not after it expires.
func TestReturnOnce(t *testing.T) {
	a := &App{}
	loc := "claude://claude.ai/mcp-auth-callback/sdk?code=gsa_x&iss=https%3A%2F%2Fgit.example.com&state=s"
	id := a.KeepReturn(loc)
	if !strings.HasPrefix(id, "ort_") || len(id) < 20 {
		t.Fatalf("unexpected return ID %q", id)
	}
	if other := a.KeepReturn(loc); other == id {
		t.Fatal("each answer should get an ID of its own")
	}
	if got, ok := a.TakeReturn(id); !ok || got != loc {
		t.Fatalf("TakeReturn = %q, %v; want %q", got, ok, loc)
	}
	if _, ok := a.TakeReturn(id); ok {
		t.Fatal("a return ID should work once")
	}
	if _, ok := a.TakeReturn("ort_nosuchid"); ok {
		t.Fatal("an unknown return ID should not work")
	}
	late := a.KeepReturn(loc)
	a.oauth.mu.Lock()
	a.oauth.returns[late].expires = time.Now().Add(-time.Second)
	a.oauth.mu.Unlock()
	if _, ok := a.TakeReturn(late); ok {
		t.Fatal("an expired return ID should not work")
	}
	a.oauth.mu.Lock()
	_, kept := a.oauth.returns[late]
	a.oauth.mu.Unlock()
	if kept {
		t.Fatal("an expired return ID should be forgotten")
	}
}

// TestMetadataFetchGuard serves a valid document over TLS on loopback: the
// guarded fetcher refuses the address, and the test seam lets it through.
func TestMetadataFetchGuard(t *testing.T) {
	var docURL string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"client_id":"` + docURL + `","client_name":"Test","redirect_uris":["http://localhost/callback"],"token_endpoint_auth_method":"none"}`))
	}))
	defer srv.Close()
	docURL = srv.URL + "/client.json"

	_, err := newMetadataFetcher(MetadataFetchConfig{}).get(context.Background(), docURL)
	if err == nil || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("the fetch should refuse a loopback address: %v", err)
	}
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c, err := newMetadataFetcher(MetadataFetchConfig{RootCAs: pool, AllowLoopback: true}).get(context.Background(), docURL)
	if err != nil || c.ID != docURL || c.Name != "Test" || c.Via != viaMetadataDocument {
		t.Fatalf("with the test seam the document should be read: %+v %v", c, err)
	}
}
