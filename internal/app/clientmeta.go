package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"
)

// A Client ID Metadata Document is a client's description of itself, served
// at the https URL that is its client_id. Fetching one is a request the
// server makes on behalf of whoever sent the client_id, so it is guarded:
// https only, never to a loopback, private, link-local or otherwise
// non-public address (checked on the address actually dialed, so a name
// that resolves there is refused too), no proxy, no redirects, a size limit
// and a timeout.

const (
	maxMetadataDoc     = 64 << 10
	metadataTimeout    = 10 * time.Second
	metadataCacheTTL   = 10 * time.Minute
	maxMetadataCached  = 256
	maxMetadataURLSize = 2000
)

// MetadataFetchConfig changes how Client ID Metadata Documents are fetched.
// It exists for tests, which serve documents from a local TLS server.
type MetadataFetchConfig struct {
	// RootCAs replaces the system's trusted roots.
	RootCAs *x509.CertPool
	// AllowLoopback lets the fetch reach loopback addresses.
	AllowLoopback bool
}

// ConfigureMetadataFetch replaces the guarded fetcher's settings. It is a
// test seam: a server never needs it.
func (a *App) ConfigureMetadataFetch(cfg MetadataFetchConfig) {
	a.oauth.mu.Lock()
	a.oauth.fetch = newMetadataFetcher(cfg)
	a.oauth.docs = nil
	a.oauth.mu.Unlock()
}

type metadataFetcher struct {
	client *http.Client
}

type cachedDoc struct {
	client  OAuthClient
	fetched time.Time
}

func newMetadataFetcher(cfg MetadataFetchConfig) *metadataFetcher {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return fmt.Errorf("refusing to dial %s", address)
			}
			if !publicAddr(ip, cfg.AllowLoopback) {
				return fmt.Errorf("refusing to fetch client metadata from the non-public address %s", ip)
			}
			return nil
		},
	}
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		TLSClientConfig:        &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    5 * time.Second,
		ResponseHeaderTimeout:  5 * time.Second,
		MaxResponseHeaderBytes: 16 << 10,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      true,
	}
	return &metadataFetcher{client: &http.Client{
		Transport: tr,
		Timeout:   metadataTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("client metadata documents are not followed through redirects")
		},
	}}
}

// nonPublic lists ranges that are not reachable on the public internet, or
// that lead back into one's own network, beyond what netip classifies.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64, which can reach private IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// publicAddr reports whether ip is an address the server may fetch from.
func publicAddr(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	switch {
	case !ip.IsValid(), ip.IsUnspecified(), ip.IsMulticast(), ip.IsLinkLocalUnicast(),
		ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsPrivate():
		return false
	case ip.IsLoopback():
		return allowLoopback
	}
	for _, p := range nonPublic {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// checkMetadataURL checks a client_id that names a metadata document.
func checkMetadataURL(raw string) error {
	if len(raw) > maxMetadataURLSize {
		return fmt.Errorf("client_id is longer than %d bytes", maxMetadataURLSize)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Path == "" || u.Path == "/" {
		return fmt.Errorf("a client_id URL must be https, with a host and a path, and no credentials or fragment")
	}
	return nil
}

// metadataClient returns the client a metadata document describes, from a
// recent fetch or a new one.
func (a *App) metadataClient(ctx context.Context, clientID string) (OAuthClient, error) {
	if err := checkMetadataURL(clientID); err != nil {
		return OAuthClient{}, oauthErr(http.StatusBadRequest, "invalid_client", "%s", err.Error())
	}
	m := &a.oauth
	m.mu.Lock()
	if d, ok := m.docs[clientID]; ok && time.Since(d.fetched) < metadataCacheTTL {
		m.mu.Unlock()
		return d.client, nil
	}
	if m.fetch == nil {
		m.fetch = newMetadataFetcher(MetadataFetchConfig{})
	}
	f := m.fetch
	m.mu.Unlock()

	c, err := f.get(ctx, clientID)
	if err != nil {
		return OAuthClient{}, oauthErr(http.StatusBadRequest, "invalid_client", "client metadata document %s: %v", clientID, err)
	}
	m.mu.Lock()
	if m.docs == nil {
		m.docs = map[string]cachedDoc{}
	}
	if len(m.docs) >= maxMetadataCached {
		for k, d := range m.docs {
			if time.Since(d.fetched) >= metadataCacheTTL || len(m.docs) >= maxMetadataCached {
				delete(m.docs, k)
			}
		}
	}
	m.docs[clientID] = cachedDoc{client: c, fetched: time.Now()}
	m.mu.Unlock()
	return c, nil
}

// clientMetadata is the part of a metadata document krystallos reads.
type clientMetadata struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

func (f *metadataFetcher) get(ctx context.Context, clientID string) (OAuthClient, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)
	if err != nil {
		return OAuthClient{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "krystallos")
	resp, err := f.client.Do(req)
	if err != nil {
		return OAuthClient{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return OAuthClient{}, fmt.Errorf("answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataDoc+1))
	if err != nil {
		return OAuthClient{}, err
	}
	if len(body) > maxMetadataDoc {
		return OAuthClient{}, fmt.Errorf("larger than %d KiB", maxMetadataDoc>>10)
	}
	var doc clientMetadata
	if err := json.Unmarshal(body, &doc); err != nil {
		return OAuthClient{}, fmt.Errorf("not a JSON object: %v", err)
	}
	if doc.ClientID != clientID {
		return OAuthClient{}, fmt.Errorf("its client_id %q is not the URL it was fetched from", doc.ClientID)
	}
	c := OAuthClient{ID: clientID, Via: viaMetadataDocument}
	if err := checkClientMetadata(&c, doc.ClientName, doc.RedirectURIs, doc.TokenEndpointAuthMethod, doc.GrantTypes, doc.ResponseTypes); err != nil {
		return OAuthClient{}, err
	}
	return c, nil
}
