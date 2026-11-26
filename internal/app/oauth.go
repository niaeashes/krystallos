package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"gitsynth/internal/ident"
	"gitsynth/internal/store"
)

// gitsynth is the OAuth 2.1 authorization server for its own MCP endpoints,
// following MCP's authorization spec. A client (Claude Code, say) asks to
// act as one identity at that identity's endpoint; the person approves it in
// a signed-in browser, which makes a grant; the client trades the code for
// an access token, whose audience is the endpoint's URL, and a refresh
// token, which rotates on every use. The grant has a credential of its own,
// which lasts while the client keeps refreshing: sessions the client opens
// hang on it, so they outlive any one access token. Revoking the grant, or
// disabling the identity, revokes all of it.
//
// What waits for the person (authorization requests), codes, answers on
// their way back to the client and fetched Client ID Metadata Documents
// live in memory; grants, tokens and clients are recorded.

const (
	oauthRequestTTL = 10 * time.Minute
	oauthCodeTTL    = 2 * time.Minute
	oauthReturnTTL  = time.Minute
	// OAuthAccessTTL and OAuthRefreshTTL are how long tokens last. Each
	// refresh moves the grant's expiry to the new refresh token's.
	OAuthAccessTTL  = time.Hour
	OAuthRefreshTTL = 30 * 24 * time.Hour

	maxRedirectURIs  = 10
	maxClientName    = 200
	maxOAuthURI      = 2000
	maxOAuthRequests = 1000
	// maxUnusedClients bounds how many clients Dynamic Client Registration
	// accepts in an hour without any of them being approved, since anyone
	// may register one.
	maxUnusedClients = 200
)

// OAuthError is an error in OAuth's own form (RFC 6749 §5.2): a code such
// as invalid_grant, a description, and the HTTP status to answer with.
type OAuthError struct {
	Status      int
	Code        string
	Description string
}

func (e *OAuthError) Error() string { return e.Code + ": " + e.Description }

func oauthErr(status int, code, format string, args ...any) error {
	return &OAuthError{Status: status, Code: code, Description: fmt.Sprintf(format, args...)}
}

// OAuthClient is a client of the MCP endpoints.
type OAuthClient struct {
	ID           string   `json:"client_id"`
	Name         string   `json:"client_name,omitempty"`
	RedirectURIs []string `json:"redirect_uris"`
	// Via is "registration" (Dynamic Client Registration; the name is the
	// client's own claim) or "metadata_document" (the client_id is the
	// https URL of its metadata, so its host vouches for it).
	Via string `json:"via"`
}

const (
	viaRegistration     = "registration"
	viaMetadataDocument = "metadata_document"
)

// oauthMem is the authorization server's state in memory.
type oauthMem struct {
	mu       sync.Mutex
	requests map[string]*AuthRequest
	codes    map[string]*authCode      // by the code's hash
	returns  map[string]*pendingReturn // answers on their way to the client
	docs     map[string]cachedDoc      // Client ID Metadata Documents by URL
	fetch    *metadataFetcher
}

// pendingReturn is where /oauth/return/{id} sends the browser.
type pendingReturn struct {
	location string
	expires  time.Time
}

// AuthRequest is an authorization request waiting for the person.
type AuthRequest struct {
	ID            string
	Client        OAuthClient
	RedirectURI   string
	State         string
	CodeChallenge string
	Resource      string
	Identity      string
	expires       time.Time
}

type authCode struct {
	client, redirectURI, challenge, resource string
	grant, identity, approvedBy              string
	expires                                  time.Time
	used                                     bool
}

// ---- clients ----

// ClientRegistration is a Dynamic Client Registration request (RFC 7591).
// Fields gitsynth has no use for, such as logo_uri, are ignored.
type ClientRegistration struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
}

// RegisterClient registers a public client by Dynamic Client Registration.
// Anyone may; the person sees the client's name marked as its own claim.
func (a *App) RegisterClient(ctx context.Context, reg ClientRegistration) (OAuthClient, time.Time, error) {
	c := OAuthClient{ID: "gsc_" + randomString(16), Via: viaRegistration}
	if err := checkClientMetadata(&c, reg.ClientName, reg.RedirectURIs, reg.TokenEndpointAuthMethod, reg.GrantTypes, reg.ResponseTypes); err != nil {
		return OAuthClient{}, time.Time{}, &OAuthError{Status: http.StatusBadRequest, Code: "invalid_client_metadata", Description: err.Error()}
	}
	now := time.Now().UTC()
	_, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
		var unused int
		err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM oauth_clients c WHERE c.via = ? AND c.registered_at > ?
			 AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client = c.id)`,
			viaRegistration, now.Add(-time.Hour).Format(time.RFC3339)).Scan(&unused)
		if err != nil {
			return err
		}
		if unused >= maxUnusedClients {
			return oauthErr(http.StatusTooManyRequests, "temporarily_unavailable", "too many clients registered without being approved; try again later")
		}
		if _, err := tx.Exec(`INSERT INTO oauth_clients (id, name, redirect_uris, via, registered_at) VALUES (?, ?, ?, ?, ?)`,
			c.ID, c.Name, mustJSON(c.RedirectURIs), c.Via, now.Format(time.RFC3339)); err != nil {
			return err
		}
		_, err = tx.Append("oauth.client_registered", store.Scope{}, map[string]any{
			"client_id": c.ID, "client_name": c.Name, "redirect_uris": c.RedirectURIs, "via": c.Via,
		})
		return err
	})
	if err != nil {
		return OAuthClient{}, time.Time{}, err
	}
	return c, now, nil
}

// checkClientMetadata checks what a client says about itself, from a
// registration or a metadata document, and fills c from it. Only public
// clients using the authorization code flow are supported.
func checkClientMetadata(c *OAuthClient, name string, redirects []string, authMethod string, grantTypes, responseTypes []string) error {
	name = strings.TrimSpace(name)
	if len(name) > maxClientName || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("client_name must be at most %d bytes, without control characters", maxClientName)
	}
	if len(redirects) == 0 || len(redirects) > maxRedirectURIs {
		return fmt.Errorf("redirect_uris must list between 1 and %d URIs", maxRedirectURIs)
	}
	for _, u := range redirects {
		if err := checkRedirectURI(u); err != nil {
			return err
		}
	}
	if authMethod != "" && authMethod != "none" {
		return fmt.Errorf("token_endpoint_auth_method %q is not supported: clients are public (none)", authMethod)
	}
	for _, g := range grantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return fmt.Errorf("grant type %q is not supported", g)
		}
	}
	if len(grantTypes) > 0 && !contains(grantTypes, "authorization_code") {
		return errors.New("grant_types must include authorization_code")
	}
	for _, r := range responseTypes {
		if r != "code" {
			return fmt.Errorf("response type %q is not supported", r)
		}
	}
	c.Name, c.RedirectURIs = name, redirects
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Redirect kinds: where the browser takes the answer.
const (
	// RedirectWeb is an https URL: a site.
	RedirectWeb = "web"
	// RedirectLoopback is an http URL on a loopback address, where a native
	// app such as a CLI listens (RFC 8252 §7.3).
	RedirectLoopback = "loopback"
	// RedirectApp is a private-use URI scheme, such as claude:, which the
	// operating system hands to whichever app registered it (RFC 8252 §7.1).
	RedirectApp = "app"
)

var schemeRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// browserSchemes are the schemes a browser handles itself instead of handing
// them to an app: going to one runs script, shows a local file or a browser
// page, or opens another kind of connection. None is a place a native app
// receives a code, so a redirect URI never uses one. The browser's
// handling of a few (javascript:, data:) is how a redirect becomes script
// in gitsynth's origin; the rest are refused for the same reason, that
// they act inside the browser.
var browserSchemes = map[string]bool{
	// Script, and content the browser renders itself.
	"javascript": true, "vbscript": true, "livescript": true, "mocha": true,
	"data": true, "blob": true, "filesystem": true, "view-source": true,
	"about": true, "file": true, "mhtml": true, "jar": true,
	// Other connections the browser makes itself.
	"ws": true, "wss": true, "ftp": true,
	// The browser's own pages, and its ways of opening apps by name.
	"chrome": true, "chrome-untrusted": true, "chrome-search": true, "edge": true,
	"opera": true, "brave": true, "vivaldi": true, "resource": true, "moz-icon": true,
	"intent": true, "android-app": true,
}

// browserScheme reports whether a (lowercase) scheme is one the browser
// handles itself, extensions' schemes (chrome-extension:, moz-extension:)
// included.
func browserScheme(scheme string) bool {
	return browserSchemes[scheme] || strings.HasSuffix(scheme, "-extension")
}

// redirectKind says which kind of redirect URI u is, or why it is not one:
// an https URL, an http URL on a loopback address, or a private-use URI
// scheme for a native app. A private-use one may name a host
// (claude://claude.ai/callback) or not (com.example.app:/callback, the form
// RFC 8252 §7.1 itself gives): for the operating system only the scheme
// counts, so the host decides nothing either way. It must be hierarchical,
// with a path starting with /, so that the code is added as a query.
func redirectKind(u *url.URL) (string, error) {
	switch {
	case u.Scheme == "https":
		if u.Host == "" {
			return "", errors.New("an https redirect URI needs a host")
		}
		return RedirectWeb, nil
	case u.Scheme == "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", errors.New("an http redirect URI must be on localhost; anywhere else use https")
		}
		return RedirectLoopback, nil
	case !schemeRe.MatchString(u.Scheme):
		return "", errors.New("a redirect URI needs a scheme")
	case browserScheme(u.Scheme):
		return "", fmt.Errorf("%s: is handled by the browser itself, not by an app", u.Scheme)
	case u.Opaque != "" || !strings.HasPrefix(u.Path, "/"):
		return "", fmt.Errorf("an app's redirect URI must have a path, as in %s:/callback or %s://host/callback", u.Scheme, u.Scheme)
	}
	return RedirectApp, nil
}

// checkRedirectURI accepts an https URL; an http URL on a loopback address,
// which is how a CLI receives the code; or a URI with a private-use scheme,
// which is how a desktop or mobile app does. PKCE (S256, always required)
// is what keeps a code that reaches the wrong app useless.
func checkRedirectURI(raw string) error {
	if len(raw) > maxOAuthURI {
		return fmt.Errorf("redirect URI is longer than %d bytes", maxOAuthURI)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || strings.Contains(raw, "#") || u.User != nil {
		return fmt.Errorf("invalid redirect URI %q: it must parse, without a fragment or a user", raw)
	}
	if _, err := redirectKind(u); err != nil {
		return fmt.Errorf("redirect URI %q: %v", raw, err)
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// redirectAllowed reports whether uri is one of the client's redirect URIs,
// exactly. Only a loopback one may differ, in its port alone (RFC 8252
// §7.3): a native app listens on whatever port is free when it asks. An
// https or private-use one matches only exactly.
func redirectAllowed(registered []string, uri string) bool {
	got, err := url.Parse(uri)
	if err != nil || checkRedirectURI(uri) != nil {
		return false
	}
	for _, r := range registered {
		if r == uri {
			return true
		}
		want, err := url.Parse(r)
		if err != nil || want.Scheme != "http" || got.Scheme != "http" || !isLoopbackHost(want.Hostname()) || !isLoopbackHost(got.Hostname()) {
			continue
		}
		if strings.EqualFold(want.Hostname(), got.Hostname()) && want.EscapedPath() == got.EscapedPath() && want.RawQuery == got.RawQuery {
			return true
		}
	}
	return false
}

// AllowsRedirect reports whether the client may be sent to uri.
func (c OAuthClient) AllowsRedirect(uri string) bool { return redirectAllowed(c.RedirectURIs, uri) }

// isMetadataURL reports whether a client_id names a Client ID Metadata
// Document: an https URL with a path.
func isMetadataURL(clientID string) bool {
	return strings.HasPrefix(clientID, "https://")
}

// LookupClient finds a client by its client_id: a registered one, or the
// one a Client ID Metadata Document describes, fetched (or taken from a
// recent fetch) and checked.
func (a *App) LookupClient(ctx context.Context, clientID string) (OAuthClient, error) {
	if clientID == "" {
		return OAuthClient{}, oauthErr(http.StatusBadRequest, "invalid_request", "client_id is required")
	}
	if isMetadataURL(clientID) {
		return a.metadataClient(ctx, clientID)
	}
	var c OAuthClient
	var uris string
	err := a.Store.DB().QueryRowContext(ctx, `SELECT id, name, redirect_uris, via FROM oauth_clients WHERE id = ? AND via = ?`, clientID, viaRegistration).
		Scan(&c.ID, &c.Name, &uris, &c.Via)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, oauthErr(http.StatusUnauthorized, "invalid_client", "unknown client %q", clientID)
	}
	if err != nil {
		return OAuthClient{}, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return OAuthClient{}, err
	}
	return c, nil
}

// ---- authorization ----

// StartAuthorization keeps an authorization request until the person
// approves or denies it, and returns its ID. The caller has checked the
// client and redirect URI (errors there are shown, never redirected) and
// the rest (errors there go back to the client).
func (a *App) StartAuthorization(req AuthRequest) string {
	m := &a.oauth
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.requests == nil {
		m.requests = map[string]*AuthRequest{}
	}
	now := time.Now()
	for id, r := range m.requests {
		if now.After(r.expires) {
			delete(m.requests, id)
		}
	}
	if len(m.requests) >= maxOAuthRequests {
		// Drop the one closest to expiring rather than refuse the person.
		var oldest string
		for id, r := range m.requests {
			if oldest == "" || r.expires.Before(m.requests[oldest].expires) {
				oldest = id
			}
		}
		delete(m.requests, oldest)
	}
	req.ID = "oar_" + randomString(16)
	req.expires = now.Add(oauthRequestTTL)
	m.requests[req.ID] = &req
	return req.ID
}

func (a *App) authRequest(id string) (*AuthRequest, error) {
	m := &a.oauth
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.requests[id]
	if r == nil || time.Now().After(r.expires) {
		return nil, errf(http.StatusNotFound, "this authorization request has expired or was already answered; start again from the client")
	}
	return r, nil
}

func (a *App) dropAuthRequest(id string) {
	a.oauth.mu.Lock()
	delete(a.oauth.requests, id)
	a.oauth.mu.Unlock()
}

// AuthRequestView is what the consent page shows.
type AuthRequestView struct {
	ID     string `json:"id"`
	Client struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Via  string `json:"via"`
		// Host is the host of a metadata document's URL: who vouches for
		// the client's name.
		Host string `json:"host,omitempty"`
	} `json:"client"`
	// RedirectKind is where the browser takes the answer: RedirectWeb, to
	// the site at RedirectHost; RedirectLoopback, to a program on this
	// computer listening at RedirectHost; or RedirectApp, to whichever app
	// on this computer the operating system hands RedirectScheme links to.
	RedirectKind   string `json:"redirect_kind"`
	RedirectHost   string `json:"redirect_host,omitempty"`
	RedirectScheme string `json:"redirect_scheme,omitempty"`
	Identity       string `json:"identity"`
	Resource       string `json:"resource"`
	// Create lists the identities approving creates, outermost first.
	Create []string `json:"create,omitempty"`
	// Problem says why the request cannot be approved, if it cannot.
	Problem string `json:"problem,omitempty"`
}

// consenter checks that by is a person in a signed-in browser: only they can
// bind a client to an identity.
func consenter(by Actor) error {
	if by.Credential.Kind != credBrowser || !by.Standard() {
		return errf(http.StatusForbidden, "binding a client to an identity needs the person, signed in in a browser")
	}
	return nil
}

// AuthorizationRequest describes a waiting request to the signed-in person.
func (a *App) AuthorizationRequest(ctx context.Context, by Actor, id string) (AuthRequestView, error) {
	if err := consenter(by); err != nil {
		return AuthRequestView{}, err
	}
	r, err := a.authRequest(id)
	if err != nil {
		return AuthRequestView{}, err
	}
	v := AuthRequestView{ID: r.ID, Identity: r.Identity, Resource: r.Resource}
	v.Client.ID, v.Client.Name, v.Client.Via = r.Client.ID, r.Client.Name, r.Client.Via
	if r.Client.Via == viaMetadataDocument {
		if u, err := url.Parse(r.Client.ID); err == nil {
			v.Client.Host = u.Host
		}
	}
	if u, err := url.Parse(r.RedirectURI); err == nil {
		v.RedirectKind, _ = redirectKind(u)
		if v.RedirectKind == RedirectApp {
			v.RedirectScheme = u.Scheme
		} else {
			v.RedirectHost = u.Host
		}
	}
	target := ident.MustParse(r.Identity)
	if !target.Below(by.ID()) {
		v.Problem = fmt.Sprintf("%s is not one of your identities; sign in as %s to approve this", r.Identity, target.RootID())
		return v, nil
	}
	db := a.Store.DB()
	for _, id := range target.Chain() {
		var disabled bool
		err := db.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, id.String()).Scan(&disabled)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			v.Create = append(v.Create, id.String())
		case err != nil:
			return AuthRequestView{}, err
		case disabled:
			v.Problem = fmt.Sprintf("%s is disabled; enable it on the Identities page first", id)
		}
	}
	// Chain lists the identity first and its root last; creating goes the
	// other way.
	for i, j := 0, len(v.Create)-1; i < j; i, j = i+1, j-1 {
		v.Create[i], v.Create[j] = v.Create[j], v.Create[i]
	}
	return v, nil
}

// ApproveAuthorization binds the request's client to its identity: it
// records the grant, creating the identity first when create is set and it
// does not exist yet, and returns the request with a code for the client.
// The code works once, for a short time, and only while the browser that
// approved it is still signed in.
func (a *App) ApproveAuthorization(ctx context.Context, by Actor, id string, create bool) (AuthRequest, string, error) {
	if err := consenter(by); err != nil {
		return AuthRequest{}, "", err
	}
	r, err := a.authRequest(id)
	if err != nil {
		return AuthRequest{}, "", err
	}
	target := ident.MustParse(r.Identity)
	if !target.Below(by.ID()) {
		return AuthRequest{}, "", errf(http.StatusForbidden, "%s is not one of your identities", r.Identity)
	}
	grant, grantCred := "gnt_"+randomString(12), "cred_"+randomString(10)
	now := time.Now().UTC()
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		var missing []ident.ID
		for _, up := range target.Chain() {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM identities WHERE id = ?`, up.String()).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				missing = append([]ident.ID{up}, missing...)
			}
		}
		if len(missing) > 0 && !create {
			return errf(http.StatusConflict, "identity %s does not exist yet; approve with create to make it", target)
		}
		for _, m := range missing {
			if err := a.deriveTx(ctx, tx, by, m); err != nil {
				return err
			}
		}
		if err := a.identityActive(ctx, tx, target); err != nil {
			return err
		}
		if r.Client.Via == viaMetadataDocument {
			res, err := tx.Exec(`INSERT INTO oauth_clients (id, name, redirect_uris, via, registered_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
				r.Client.ID, r.Client.Name, mustJSON(r.Client.RedirectURIs), r.Client.Via, now.Format(time.RFC3339))
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				if _, err := tx.Append("oauth.client_registered", store.Scope{}, map[string]any{
					"client_id": r.Client.ID, "client_name": r.Client.Name, "redirect_uris": r.Client.RedirectURIs, "via": r.Client.Via,
				}); err != nil {
					return err
				}
			}
		}
		// Until the client trades its code, the grant lasts only as long as
		// the code could; the first tokens extend it.
		until := now.Add(oauthCodeTTL + time.Minute).Format(time.RFC3339)
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, expires_at, kind) VALUES (?, ?, ?, ?, ?)`,
			grantCred, r.Identity, hashToken("gsg_"+randomString(32)), until, credOAuthGrant); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO credential_audiences (credential, audience, grant_id, client) VALUES (?, ?, ?, ?)`,
			grantCred, r.Resource, grant, r.Client.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO oauth_grants (id, client, identity, resource, credential, approved_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			grant, r.Client.ID, r.Identity, r.Resource, grantCred, by.Credential.ID, now.Format(time.RFC3339)); err != nil {
			return err
		}
		_, err := tx.Append("oauth.granted", store.Scope{}, map[string]any{
			"grant": grant, "client_id": r.Client.ID, "client_name": r.Client.Name, "identity": r.Identity,
			"resource": r.Resource, "credential": grantCred,
		})
		return err
	})
	if err != nil {
		return AuthRequest{}, "", err
	}
	a.dropAuthRequest(id)
	code := "gsa_" + randomString(32)
	m := &a.oauth
	m.mu.Lock()
	if m.codes == nil {
		m.codes = map[string]*authCode{}
	}
	for h, c := range m.codes {
		if time.Now().After(c.expires) {
			delete(m.codes, h)
		}
	}
	m.codes[hashToken(code)] = &authCode{
		client: r.Client.ID, redirectURI: r.RedirectURI, challenge: r.CodeChallenge, resource: r.Resource,
		grant: grant, identity: r.Identity, approvedBy: by.Credential.ID, expires: time.Now().Add(oauthCodeTTL),
	}
	m.mu.Unlock()
	return *r, code, nil
}

// DenyAuthorization drops a waiting request and returns it, so the client
// can be told.
func (a *App) DenyAuthorization(ctx context.Context, by Actor, id string) (AuthRequest, error) {
	if err := consenter(by); err != nil {
		return AuthRequest{}, err
	}
	r, err := a.authRequest(id)
	if err != nil {
		return AuthRequest{}, err
	}
	a.dropAuthRequest(id)
	return *r, nil
}

// KeepReturn keeps where the browser goes once the person has answered (the
// client's redirect URI, with the code or the error) and returns a one-time
// ID for it. /oauth/return/{id} sends the browser there with a 302, so the
// web UI only ever navigates to gitsynth itself: a redirect URI that should
// not have been accepted still never runs as script in gitsynth's origin.
// The ID works once, within a minute.
func (a *App) KeepReturn(location string) string {
	m := &a.oauth
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.returns == nil {
		m.returns = map[string]*pendingReturn{}
	}
	now := time.Now()
	for id, r := range m.returns {
		if now.After(r.expires) {
			delete(m.returns, id)
		}
	}
	if len(m.returns) >= maxOAuthRequests {
		var oldest string
		for id, r := range m.returns {
			if oldest == "" || r.expires.Before(m.returns[oldest].expires) {
				oldest = id
			}
		}
		delete(m.returns, oldest)
	}
	id := "ort_" + randomString(16)
	m.returns[id] = &pendingReturn{location: location, expires: now.Add(oauthReturnTTL)}
	return id
}

// TakeReturn gives where a return ID sends the browser, and forgets it.
func (a *App) TakeReturn(id string) (string, bool) {
	m := &a.oauth
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.returns[id]
	if r == nil {
		return "", false
	}
	delete(m.returns, id)
	if time.Now().After(r.expires) {
		return "", false
	}
	return r.location, true
}

// ---- tokens ----

// TokenResponse is the token endpoint's answer (RFC 6749 §5.1).
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
}

var verifierRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

// ExchangeCode trades an authorization code for tokens. clientID,
// redirectURI and verifier must match what the code was issued for; so must
// resource, when the client sends it.
func (a *App) ExchangeCode(ctx context.Context, code, clientID, redirectURI, verifier, resource string) (TokenResponse, error) {
	if code == "" || clientID == "" || redirectURI == "" || verifier == "" {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_request", "code, client_id, redirect_uri and code_verifier are required")
	}
	m := &a.oauth
	m.mu.Lock()
	c := m.codes[hashToken(code)]
	reused := false
	if c != nil {
		reused = c.used
		c.used = true
	}
	m.mu.Unlock()
	if c == nil || time.Now().After(c.expires) {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the code is invalid or has expired")
	}
	if reused {
		// A code used twice was intercepted somewhere: whatever the first
		// use got stops (RFC 6749 §4.1.2).
		if err := a.revokeGrant(ctx, c.grant, "authorization code reused"); err != nil {
			return TokenResponse{}, err
		}
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the code was already used")
	}
	if clientID != c.client || redirectURI != c.redirectURI {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the code was issued to another client or redirect URI")
	}
	if !verifierRe.MatchString(verifier) {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "code_verifier is malformed")
	}
	sum := sha256.Sum256([]byte(verifier))
	if subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(c.challenge)) != 1 {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "code_verifier does not match the code challenge")
	}
	if resource != "" && resource != c.resource {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_target", "the code was issued for %s", c.resource)
	}
	var out TokenResponse
	_, err := a.Store.Write(ctx, identityActor(c.identity), func(tx *store.Tx) error {
		var revoked bool
		var until string
		err := tx.QueryRowContext(ctx, `SELECT revoked, expires_at FROM credentials WHERE id = ?`, c.approvedBy).Scan(&revoked, &until)
		if err != nil || revoked || expired(until) {
			return oauthErr(http.StatusBadRequest, "invalid_grant", "the browser that approved this has signed out")
		}
		g, err := loadGrant(ctx, tx, c.grant)
		if err != nil {
			return err
		}
		out, err = a.issueTokensTx(ctx, tx, g, "")
		return err
	})
	return out, err
}

// Refresh trades a refresh token for new tokens. The old refresh token
// stops; presenting it again means it leaked, so that revokes the whole
// grant (OAuth 2.1 §4.3.1).
func (a *App) Refresh(ctx context.Context, refreshToken, clientID, resource string) (TokenResponse, error) {
	if refreshToken == "" || clientID == "" {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_request", "refresh_token and client_id are required")
	}
	var credID, kind, expires, grantID, client string
	var revoked bool
	err := a.Store.DB().QueryRowContext(ctx,
		`SELECT c.id, c.kind, c.expires_at, c.revoked, COALESCE(b.grant_id, ''), COALESCE(b.client, '')
		 FROM credentials c LEFT JOIN credential_audiences b ON b.credential = c.id WHERE c.hash = ?`, hashToken(refreshToken)).
		Scan(&credID, &kind, &expires, &revoked, &grantID, &client)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (kind != credOAuthRefresh || grantID == "")) {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "invalid refresh token")
	}
	if err != nil {
		return TokenResponse{}, err
	}
	if client != clientID {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the refresh token was issued to another client")
	}
	if revoked {
		if err := a.revokeGrant(ctx, grantID, "refresh token reused"); err != nil {
			return TokenResponse{}, err
		}
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the refresh token was already used or revoked")
	}
	if expired(expires) {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the refresh token has expired")
	}
	var out TokenResponse
	var g grantRow
	if g, err = loadGrant(ctx, a.Store.DB(), grantID); err != nil {
		return TokenResponse{}, err
	}
	if resource != "" && resource != g.resource {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_target", "the grant is for %s", g.resource)
	}
	_, err = a.Store.Write(ctx, identityActor(g.identity), func(tx *store.Tx) error {
		res, err := tx.Exec(`UPDATE credentials SET revoked = 1 WHERE id = ? AND revoked = 0`, credID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Another request rotated it first.
			return oauthErr(http.StatusBadRequest, "invalid_grant", "the refresh token was already used")
		}
		out, err = a.issueTokensTx(ctx, tx, g, credID)
		return err
	})
	return out, err
}

type grantRow struct {
	id, client, identity, resource, credential string
}

func loadGrant(ctx context.Context, q querier, id string) (grantRow, error) {
	g := grantRow{id: id}
	err := q.QueryRowContext(ctx, `SELECT client, identity, resource, credential FROM oauth_grants WHERE id = ?`, id).
		Scan(&g.client, &g.identity, &g.resource, &g.credential)
	if errors.Is(err, sql.ErrNoRows) {
		return g, oauthErr(http.StatusBadRequest, "invalid_grant", "the grant is gone")
	}
	return g, err
}

// issueTokensTx issues an access and a refresh token on a grant that is
// still live, and moves the grant's expiry to the refresh token's.
func (a *App) issueTokensTx(ctx context.Context, tx *store.Tx, g grantRow, rotated string) (TokenResponse, error) {
	gc, err := grantCredential(ctx, tx, g.id)
	if err != nil {
		return TokenResponse{}, err
	}
	if gc.revoked || expired(gc.expiresAt) {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "the grant has been revoked or has expired")
	}
	if err := a.identityActive(ctx, tx, ident.MustParse(g.identity)); err != nil {
		return TokenResponse{}, oauthErr(http.StatusBadRequest, "invalid_grant", "%s", err.Error())
	}
	now := time.Now().UTC()
	access, refresh := "gso_"+randomString(32), "gsr_"+randomString(32)
	accessID, refreshID := "cred_"+randomString(10), "cred_"+randomString(10)
	accessUntil := now.Add(OAuthAccessTTL).Format(time.RFC3339)
	refreshUntil := now.Add(OAuthRefreshTTL).Format(time.RFC3339)
	for _, c := range []struct{ id, token, until, kind string }{
		{accessID, access, accessUntil, credOAuthAccess},
		{refreshID, refresh, refreshUntil, credOAuthRefresh},
	} {
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, expires_at, kind) VALUES (?, ?, ?, ?, ?)`,
			c.id, g.identity, hashToken(c.token), c.until, c.kind); err != nil {
			return TokenResponse{}, err
		}
		if _, err := tx.Exec(`INSERT INTO credential_audiences (credential, audience, grant_id, client) VALUES (?, ?, ?, ?)`,
			c.id, g.resource, g.id, g.client); err != nil {
			return TokenResponse{}, err
		}
	}
	if _, err := tx.Exec(`UPDATE credentials SET expires_at = ? WHERE id = ?`, refreshUntil, g.credential); err != nil {
		return TokenResponse{}, err
	}
	payload := map[string]any{"grant": g.id, "access": accessID, "refresh": refreshID, "expires_at": accessUntil, "grant_expires_at": refreshUntil}
	if rotated != "" {
		payload["rotated"] = rotated
	}
	if _, err := tx.Append("oauth.tokens_issued", store.Scope{}, payload); err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(OAuthAccessTTL.Seconds()), RefreshToken: refresh}, nil
}

// identityActor signs what happens to an identity's OAuth tokens at the
// token endpoint, where no credential of its own was presented.
func identityActor(identity string) store.Actor {
	id := ident.MustParse(identity)
	return store.Actor{Identity: id.String(), Principal: id.RootID().String()}
}

type grantCred struct {
	id, expiresAt string
	revoked       bool
}

// grantCredential reads a grant's own credential.
func grantCredential(ctx context.Context, q querier, grant string) (grantCred, error) {
	var g grantCred
	err := q.QueryRowContext(ctx,
		`SELECT c.id, c.expires_at, c.revoked FROM oauth_grants g JOIN credentials c ON c.id = g.credential WHERE g.id = ?`, grant).
		Scan(&g.id, &g.expiresAt, &g.revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return g, errf(http.StatusUnauthorized, "the grant this token belongs to is gone")
	}
	return g, err
}

func describeAudience(audience string) string {
	if audience == AudienceGit {
		return "git"
	}
	return "the MCP endpoint " + audience
}

// ---- grants ----

// GrantInfo is a client connected to an identity.
type GrantInfo struct {
	ID         string `json:"id"`
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`
	Via        string `json:"via"`
	Identity   string `json:"identity"`
	Resource   string `json:"resource"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at"`
}

// Grants lists the connected clients on identities the actor controls: the
// live grants whose client has traded its code for tokens.
func (a *App) Grants(ctx context.Context, by Actor) ([]GrantInfo, error) {
	rows, err := a.Store.DB().QueryContext(ctx,
		`SELECT g.id, g.client, COALESCE(cl.name, ''), COALESCE(cl.via, ''), g.identity, g.resource, g.created_at, c.expires_at
		 FROM oauth_grants g JOIN credentials c ON c.id = g.credential
		 LEFT JOIN oauth_clients cl ON cl.id = g.client
		 WHERE c.revoked = 0
		   AND EXISTS (SELECT 1 FROM credential_audiences b WHERE b.grant_id = g.id AND b.credential != g.credential)
		 ORDER BY g.created_at DESC, g.id`)
	if err != nil {
		return nil, err
	}
	var all []GrantInfo
	for rows.Next() {
		var g GrantInfo
		if err := rows.Scan(&g.ID, &g.ClientID, &g.ClientName, &g.Via, &g.Identity, &g.Resource, &g.CreatedAt, &g.ExpiresAt); err != nil {
			rows.Close()
			return nil, err
		}
		if !expired(g.ExpiresAt) {
			all = append(all, g)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []GrantInfo{}
	for _, g := range all {
		id, err := ident.Parse(g.Identity)
		if err != nil {
			continue
		}
		ok, err := a.controls(ctx, a.Store.DB(), by, id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, g)
		}
	}
	return out, nil
}

// RevokeGrant disconnects a client: the grant's credential and every token
// issued on it are revoked, and so every session its MCP sessions opened.
func (a *App) RevokeGrant(ctx context.Context, by Actor, grant string) error {
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		g, err := loadGrant(ctx, tx, grant)
		if err != nil {
			return errf(http.StatusNotFound, "grant %s not found", grant)
		}
		ok, err := a.controls(ctx, tx, by, ident.MustParse(g.identity))
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusNotFound, "grant %s not found", grant)
		}
		return revokeGrantTx(ctx, tx, g, "revoked")
	})
	return err
}

// revokeGrant revokes a grant for a reason the server found itself, such
// as a refresh token presented twice. It is signed by the grant's identity.
func (a *App) revokeGrant(ctx context.Context, grant, reason string) error {
	g, err := loadGrant(ctx, a.Store.DB(), grant)
	if err != nil {
		return err
	}
	_, err = a.Store.Write(ctx, identityActor(g.identity), func(tx *store.Tx) error {
		return revokeGrantTx(ctx, tx, g, reason)
	})
	return err
}

func revokeGrantTx(ctx context.Context, tx *store.Tx, g grantRow, reason string) error {
	const where = `revoked = 0 AND (id = ? OR id IN (SELECT credential FROM credential_audiences WHERE grant_id = ?))`
	ids, err := queryStrings(ctx, tx, `SELECT id FROM credentials WHERE `+where+` ORDER BY rowid`, g.credential, g.id)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(`UPDATE credentials SET revoked = 1 WHERE `+where, g.credential, g.id); err != nil {
		return err
	}
	_, err = tx.Append("oauth.grant_revoked", store.Scope{}, map[string]any{
		"grant": g.id, "client_id": g.client, "identity": g.identity, "reason": reason, "revoked": ids,
	})
	return err
}
