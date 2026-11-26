package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
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

func TestRedirectAllowed(t *testing.T) {
	reg := []string{"http://localhost/callback", "http://127.0.0.1/callback", "https://app.example.com/cb"}
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
	} {
		if got := redirectAllowed(reg, uri); got != want {
			t.Errorf("redirectAllowed(%q) = %v, want %v", uri, got, want)
		}
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
