package server

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"gitsynth/internal/app"
	"gitsynth/internal/ident"
)

// gitsynth is the OAuth 2.1 authorization server for its identities' MCP
// endpoints, as MCP's authorization spec lays out:
//
//   - a 401 at an endpoint points at its Protected Resource Metadata
//     (RFC 9728), which names this server as the authorization server;
//   - the Authorization Server Metadata (RFC 8414) lists the endpoints;
//   - clients register with a Client ID Metadata Document, or with Dynamic
//     Client Registration (RFC 7591);
//   - /oauth/authorize takes the person to the consent page in the web UI,
//     where they sign in and approve; the code comes back with PKCE (S256
//     only) and a resource indicator (RFC 8707) naming the endpoint;
//   - /oauth/token trades the code, and later refresh tokens, for tokens
//     whose audience is that endpoint.

// randomID is a random identifier for things that live in memory.
func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// metadata answers with a public JSON document.
func metadata(w http.ResponseWriter, v any) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=300")
	writeJSON(w, http.StatusOK, v)
}

// protectedResource serves the Protected Resource Metadata of an identity's
// MCP endpoint, at /.well-known/oauth-protected-resource/mcp/....
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	id, ok := mcpIdentity(strings.TrimPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	metadata(w, map[string]any{
		"resource":                 s.mcpResource(id),
		"authorization_servers":    []string{s.URL},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "gitsynth MCP for " + id.String(),
	})
}

// authServerMetadata serves the Authorization Server Metadata.
func (s *Server) authServerMetadata(w http.ResponseWriter, r *http.Request) {
	metadata(w, map[string]any{
		"issuer":                                         s.URL,
		"authorization_endpoint":                         s.URL + "/oauth/authorize",
		"token_endpoint":                                 s.URL + "/oauth/token",
		"registration_endpoint":                          s.URL + "/oauth/register",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// oauthError answers in OAuth's error form.
func oauthError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	var oe *app.OAuthError
	var ae *app.Error
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &oe):
		writeJSON(w, oe.Status, map[string]string{"error": oe.Code, "error_description": oe.Description})
	case errors.As(err, &tooBig):
		writeError(w, err)
	case errors.As(err, &ae) && ae.Code < 500:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": ae.Msg})
	default:
		writeError(w, err)
	}
}

// registerClient is Dynamic Client Registration. Fields gitsynth does not
// use are accepted and ignored.
func (s *Server) registerClient(w http.ResponseWriter, r *http.Request) {
	var reg app.ClientRegistration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, err)
			return
		}
		oauthError(w, &app.OAuthError{Status: http.StatusBadRequest, Code: "invalid_client_metadata", Description: "invalid JSON body: " + err.Error()})
		return
	}
	c, at, err := s.App.RegisterClient(r.Context(), reg)
	if err != nil {
		oauthError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        at.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// canonicalResource checks a resource indicator: the URL of an identity's
// MCP endpoint on this server. It returns the URL in the form tokens are
// bound to, and the identity.
func (s *Server) canonicalResource(raw string) (string, ident.ID, error) {
	bad := func(msg string) (string, ident.ID, error) {
		return "", ident.ID{}, &app.OAuthError{Status: http.StatusBadRequest, Code: "invalid_target", Description: msg}
	}
	if raw == "" {
		return bad("resource is required: the URL of the MCP endpoint, such as " + s.URL + "/mcp/user/alice/claude")
	}
	u, err := url.Parse(raw)
	base, berr := url.Parse(s.URL)
	if err != nil || berr != nil || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
		!strings.EqualFold(u.Scheme, base.Scheme) || !strings.EqualFold(u.Host, base.Host) {
		return bad("resource must be an MCP endpoint on " + s.URL)
	}
	id, ok := mcpIdentity(u.Path)
	if !ok {
		return bad("resource must be an identity's MCP endpoint, " + s.URL + "/mcp/<kind>/<root>/<label>...")
	}
	return s.mcpResource(id), id, nil
}

var challengeRe = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// withParams adds query parameters to a redirect URI.
func withParams(raw string, params map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// authorize starts the authorization code flow. A request with an unknown
// client or a redirect URI the client did not register is answered here,
// never redirected; any other problem goes back to the client. A good one
// waits for the person on the consent page.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	problem := func(msg string) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte("This sign-in request cannot go on: " + msg + "\n"))
	}
	client, err := s.App.LookupClient(r.Context(), q.Get("client_id"))
	if err != nil {
		var oe *app.OAuthError
		if errors.As(err, &oe) {
			problem(oe.Description)
			return
		}
		writeError(w, err)
		return
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(client.RedirectURIs) == 1 {
		redirect = client.RedirectURIs[0]
	}
	if !client.AllowsRedirect(redirect) {
		problem("the redirect_uri is not one the client registered")
		return
	}
	state := q.Get("state")
	back := func(code, desc string) {
		http.Redirect(w, r, withParams(redirect, map[string]string{"error": code, "error_description": desc, "state": state, "iss": s.URL}), http.StatusFound)
	}
	if q.Get("response_type") != "code" {
		back("unsupported_response_type", "only response_type=code is supported")
		return
	}
	if q.Get("code_challenge_method") != "S256" || !challengeRe.MatchString(q.Get("code_challenge")) {
		back("invalid_request", "PKCE is required, with code_challenge_method=S256")
		return
	}
	resource, id, err := s.canonicalResource(q.Get("resource"))
	if err != nil {
		var oe *app.OAuthError
		errors.As(err, &oe)
		back(oe.Code, oe.Description)
		return
	}
	if id.Kind != ident.User || id.IsRoot() {
		back("invalid_target", "a client can be bound to an identity derived from a user, such as user:alice/claude, never to the person's own identity or to a bot's")
		return
	}
	reqID := s.App.StartAuthorization(app.AuthRequest{
		Client: client, RedirectURI: redirect, State: state, CodeChallenge: q.Get("code_challenge"),
		Resource: resource, Identity: id.String(),
	})
	http.Redirect(w, r, s.URL+"/oauth/consent?request="+url.QueryEscape(reqID), http.StatusFound)
}

// token is the token endpoint: the authorization code and refresh token
// grants, for public clients.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if err := r.ParseForm(); err != nil {
		oauthError(w, badBodyOAuth(err))
		return
	}
	f := r.PostForm
	clientID := f.Get("client_id")
	if user, _, ok := r.BasicAuth(); ok && clientID == "" {
		clientID, _ = url.QueryUnescape(user)
	}
	resource := ""
	if raw := f.Get("resource"); raw != "" {
		var err error
		if resource, _, err = s.canonicalResource(raw); err != nil {
			oauthError(w, err)
			return
		}
	}
	var out app.TokenResponse
	var err error
	switch f.Get("grant_type") {
	case "authorization_code":
		out, err = s.App.ExchangeCode(r.Context(), f.Get("code"), clientID, f.Get("redirect_uri"), f.Get("code_verifier"), resource)
	case "refresh_token":
		out, err = s.App.Refresh(r.Context(), f.Get("refresh_token"), clientID, resource)
	default:
		err = &app.OAuthError{Status: http.StatusBadRequest, Code: "unsupported_grant_type", Description: "grant_type must be authorization_code or refresh_token"}
	}
	if err != nil {
		oauthError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func badBodyOAuth(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return err
	}
	return &app.OAuthError{Status: http.StatusBadRequest, Code: "invalid_request", Description: err.Error()}
}

// ---- the consent page's API ----

func (s *Server) authRequest(r *http.Request, actor app.Actor) (any, error) {
	return s.App.AuthorizationRequest(r.Context(), actor, r.PathValue("id"))
}

// approveAuth binds the client to the identity and returns where the
// browser goes next: the client's redirect URI, with the code.
func (s *Server) approveAuth(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Create bool `json:"create"`
	}](r)
	if err != nil {
		return nil, err
	}
	req, code, err := s.App.ApproveAuthorization(r.Context(), actor, r.PathValue("id"), in.Create)
	if err != nil {
		return nil, err
	}
	return map[string]string{"redirect": withParams(req.RedirectURI, map[string]string{"code": code, "state": req.State, "iss": s.URL})}, nil
}

func (s *Server) denyAuth(r *http.Request, actor app.Actor) (any, error) {
	req, err := s.App.DenyAuthorization(r.Context(), actor, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return map[string]string{"redirect": withParams(req.RedirectURI, map[string]string{
		"error": "access_denied", "error_description": "the person declined", "state": req.State, "iss": s.URL,
	})}, nil
}

func (s *Server) listGrants(r *http.Request, actor app.Actor) (any, error) {
	return s.App.Grants(r.Context(), actor)
}

func (s *Server) revokeGrant(r *http.Request, actor app.Actor) (any, error) {
	return map[string]bool{"ok": true}, s.App.RevokeGrant(r.Context(), actor, r.PathValue("id"))
}
