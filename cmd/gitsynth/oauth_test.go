package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/descope/virtualwebauthn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// oauthAgent is an OAuth client of gitsynth's MCP endpoints, the way Claude
// Code is one: it finds the authorization server from a 401, registers,
// sends the person's browser to authorize, and trades the code for tokens.
type oauthAgent struct {
	t        *testing.T
	h        *harness
	clientID string
	redirect string // where the code comes back: a loopback listener's URL
	http     *http.Client
}

func newOAuthAgent(t *testing.T, h *harness, clientID, redirect string) *oauthAgent {
	return &oauthAgent{t: t, h: h, clientID: clientID, redirect: redirect, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// local turns one of the server's own URLs (at its public localhost
// address) into one the test can reach.
func (h *harness) local(u string) string {
	return strings.Replace(u, h.url, h.base, 1)
}

func pkce() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// authorize sends the browser to the authorization endpoint and returns
// where it lands: the consent page's request ID, or the redirect back to
// the client when the request is refused.
func (o *oauthAgent) authorize(params url.Values) (requestID string, back *url.URL) {
	o.t.Helper()
	resp, err := o.http.Get(o.h.base + "/oauth/authorize?" + params.Encode())
	if err != nil {
		o.t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		o.t.Fatalf("authorize: %d %s", resp.StatusCode, raw)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		o.t.Fatal(err)
	}
	if strings.HasPrefix(loc.String(), o.h.url+"/oauth/consent?") {
		return loc.Query().Get("request"), nil
	}
	return "", loc
}

func (o *oauthAgent) authParams(resource, challenge string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {o.clientID}, "redirect_uri": {o.redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st-123"}, "resource": {resource},
	}
}

// token posts to the token endpoint and returns the status and the JSON.
func (o *oauthAgent) token(form url.Values) (int, map[string]any) {
	o.t.Helper()
	resp, err := o.http.PostForm(o.h.base+"/oauth/token", form)
	if err != nil {
		o.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

type tokens struct{ access, refresh string }

func (o *oauthAgent) exchange(code, verifier, resource string) tokens {
	o.t.Helper()
	code0, out := o.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {o.redirect},
		"client_id": {o.clientID}, "code_verifier": {verifier}, "resource": {resource},
	})
	if code0 != http.StatusOK || out["token_type"] != "Bearer" || out["expires_in"] != float64(3600) {
		o.t.Fatalf("code exchange: %d %v", code0, out)
	}
	return tokens{out["access_token"].(string), out["refresh_token"].(string)}
}

func (o *oauthAgent) refresh(refresh string) (int, map[string]any) {
	return o.token(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {o.clientID}})
}

// connect runs the whole flow for one identity's endpoint with a signed-in
// browser approving, and returns the tokens.
func (o *oauthAgent) connect(b *browser, resource string, create bool) tokens {
	o.t.Helper()
	verifier, challenge := pkce()
	reqID, back := o.authorize(o.authParams(resource, challenge))
	if back != nil {
		o.t.Fatalf("authorize refused: %v", back)
	}
	var approved struct {
		Redirect string `json:"redirect"`
	}
	json.Unmarshal(b.json("POST", "/api/oauth/requests/"+reqID+"/approve", map[string]bool{"create": create}, http.StatusOK), &approved)
	return o.exchange(codeFrom(o.t, o.follow(approved.Redirect)), verifier, resource)
}

// follow goes where the consent page sends the browser after the person
// answers: gitsynth's one-time /oauth/return URL, whose 302 names the
// client's redirect URI with the answer. It returns that.
func (o *oauthAgent) follow(redirect string) *url.URL {
	o.t.Helper()
	if !strings.HasPrefix(redirect, o.h.url+"/oauth/return/ort_") {
		o.t.Fatalf("the consent page should be sent to gitsynth's return URL, not to the client: %s", redirect)
	}
	resp, err := o.http.Get(o.h.local(redirect))
	if err != nil {
		o.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		o.t.Fatalf("the return URL should answer 302, got %d", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		o.t.Fatal(err)
	}
	return loc
}

func codeFrom(t *testing.T, u *url.URL) string {
	t.Helper()
	if u.Query().Get("state") != "st-123" || u.Query().Get("code") == "" {
		t.Fatalf("the redirect should carry the code and the state: %s", u)
	}
	return u.Query().Get("code")
}

// tokenHolder is an MCP client's current access token, which a refresh
// replaces under a live connection.
type tokenHolder struct {
	mu  sync.Mutex
	tok string
}

func (h *tokenHolder) set(tok string) { h.mu.Lock(); h.tok = tok; h.mu.Unlock() }

func (h *tokenHolder) RoundTrip(r *http.Request) (*http.Response, error) {
	h.mu.Lock()
	tok := h.tok
	h.mu.Unlock()
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+tok)
	return http.DefaultTransport.RoundTrip(r)
}

func connectHolder(t *testing.T, endpoint string, holder *tokenHolder) mcpClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "gitsynth-test", Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: endpoint, HTTPClient: &http.Client{Transport: holder}, MaxRetries: -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", endpoint, err)
	}
	t.Cleanup(func() { cs.Close() })
	return mcpClient{t: t, cs: cs}
}

// fails reports whether a tool call fails, at the protocol or as a tool.
func (c mcpClient) fails(name string, args map[string]any) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := c.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	return err != nil || res.IsError
}

// postStatus posts an empty JSON-RPC message with a token and returns the
// status and the WWW-Authenticate header.
func postStatus(t *testing.T, endpoint, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", endpoint, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate")
}

// passkeyBrowser registers a passkey for the user with the link from admin
// init, then signs in with it in a fresh browser, as the person would.
func passkeyBrowser(t *testing.T, h *harness, regToken string) *browser {
	t.Helper()
	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "gitsynth", Origin: h.url}
	authn := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte("user:alice")})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	var begin struct {
		Ceremony string          `json:"ceremony"`
		Options  json.RawMessage `json:"options"`
	}
	b := newBrowser(t, h.base)
	json.Unmarshal(b.json("POST", "/api/auth/register/begin", map[string]string{"token": regToken}, http.StatusOK), &begin)
	attOpts, err := virtualwebauthn.ParseAttestationOptions(string(begin.Options))
	if err != nil {
		t.Fatal(err)
	}
	if code, raw := b.do("POST", "/api/auth/register/finish?ceremony="+begin.Ceremony, strings.NewReader(virtualwebauthn.CreateAttestationResponse(rp, authn, cred, *attOpts)), false); code != http.StatusOK {
		t.Fatalf("register: %d %s", code, raw)
	}
	authn.AddCredential(cred)
	b = newBrowser(t, h.base)
	json.Unmarshal(b.json("POST", "/api/auth/login/begin", nil, http.StatusOK), &begin)
	asOpts, err := virtualwebauthn.ParseAssertionOptions(string(begin.Options))
	if err != nil {
		t.Fatal(err)
	}
	if code, raw := b.do("POST", "/api/auth/login/finish?ceremony="+begin.Ceremony, strings.NewReader(virtualwebauthn.CreateAssertionResponse(rp, authn, cred, *asOpts)), false); code != http.StatusOK {
		t.Fatalf("sign in: %d %s", code, raw)
	}
	return b
}

// TestOAuthMCP binds an agent to user:alice/claude the way Claude Code
// would: it finds the authorization server from the endpoint's 401,
// registers, and the person approves in a browser signed in with a passkey,
// creating the identity there. The agent then works through a stateful MCP
// session: its calls are recorded under the session the MCP session
// opened, which outlives refreshing the access token; a subagent's child
// session is opened by tool and named on later calls; and a git credential
// from a tool pushes for real. The access token works nowhere else, a
// reused refresh token kills the grant, and revoking the grant or
// disabling the identity stops the agent. A client known by a Client ID
// Metadata Document goes through the same flow, and static tokens keep
// working, at the identity's endpoint and at the old /mcp.
func TestOAuthMCP(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	h := newHarness(t, dir)

	// A client's metadata document, served over TLS on loopback; the
	// server trusts it through its test seam.
	var docURL string
	docs := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := docURL
		if r.URL.Path == "/liar.json" {
			id = "https://elsewhere.example/client.json"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"client_id": id, "client_name": "Metadata Agent", "redirect_uris": []string{"http://localhost/callback"},
			"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
			"token_endpoint_auth_method": "none",
		})
	}))
	defer docs.Close()
	docURL = docs.URL + "/client.json"
	caFile := filepath.Join(dir, "metadata-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: docs.Certificate().Raw}), 0o644); err != nil {
		t.Fatal(err)
	}
	h.serverEnv = append(h.serverEnv, "GITSYNTH_TEST_CLIENT_METADATA_CA="+caFile)

	out := h.run("admin", "init", "--data", h.data, "--user", "alice")
	aliceTok, regToken := tokenFrom(t, out), linkToken(t, out)
	h.start()
	alice := h.client(aliceTok)
	ok[map[string]any](t, alice, "POST", "/api/orgs", map[string]string{"name": "acme"})
	ok[map[string]any](t, alice, "POST", "/api/orgs/acme/repos", map[string]string{"name": "app"})
	aliceWork := filepath.Join(dir, "alice")
	h.git(dir, "clone", h.cloneURL(aliceTok), aliceWork)
	h.commitAs(aliceWork, "alice", "user:alice@localhost", "README.md", "# app\n", "initial commit")
	h.git(aliceWork, "push", "origin", "HEAD:main")

	endpoint := h.base + "/mcp/user/alice/claude"
	resource := h.url + "/mcp/user/alice/claude"

	// Discovery: the 401 names the protected resource metadata, which
	// names the authorization server.
	code, wwwAuth := postStatus(t, endpoint, "")
	prmURL := h.url + "/.well-known/oauth-protected-resource/mcp/user/alice/claude"
	if code != http.StatusUnauthorized || !strings.Contains(wwwAuth, `resource_metadata="`+prmURL+`"`) {
		t.Fatalf("the endpoint should answer 401 pointing at %s: %d %q", prmURL, code, wwwAuth)
	}
	anon := h.client("")
	prm := ok[map[string]any](t, anon, "GET", strings.TrimPrefix(h.local(prmURL), h.base), nil)
	if prm["resource"] != resource || prm["authorization_servers"].([]any)[0] != h.url {
		t.Fatalf("unexpected protected resource metadata: %v", prm)
	}
	asm := ok[map[string]any](t, anon, "GET", "/.well-known/oauth-authorization-server", nil)
	if asm["issuer"] != h.url || asm["registration_endpoint"] != h.url+"/oauth/register" || asm["client_id_metadata_document_supported"] != true ||
		len(asm["code_challenge_methods_supported"].([]any)) != 1 || asm["code_challenge_methods_supported"].([]any)[0] != "S256" ||
		asm["token_endpoint_auth_methods_supported"].([]any)[0] != "none" {
		t.Fatalf("unexpected authorization server metadata: %v", asm)
	}
	// The old endpoint does not start OAuth.
	if code, wwwAuth := postStatus(t, h.base+"/mcp", ""); code != http.StatusUnauthorized || strings.Contains(wwwAuth, "resource_metadata") {
		t.Fatalf("the old /mcp should answer a plain 401: %d %q", code, wwwAuth)
	}

	// Dynamic Client Registration, of a native app on loopback.
	if code, _ := anon.call("POST", "/oauth/register", map[string]any{"redirect_uris": []string{"http://evil.example/cb"}}); code != http.StatusBadRequest {
		t.Fatalf("an http redirect off loopback should be refused, got %d", code)
	}
	code, raw := anon.call("POST", "/oauth/register", map[string]any{
		"client_name": "Test Agent", "redirect_uris": []string{"http://127.0.0.1/callback"}, "token_endpoint_auth_method": "none",
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}, "application_type": "native",
	})
	var reg map[string]any
	json.Unmarshal(raw, &reg)
	if code != http.StatusCreated || reg["client_id"] == "" {
		t.Fatalf("registration: %d %s", code, raw)
	}
	agent := newOAuthAgent(t, h, reg["client_id"].(string), "http://127.0.0.1:54321/callback")

	// What authorize refuses: a redirect the client did not register is
	// answered there; the rest goes back to the client.
	verifier, challenge := pkce()
	bad := agent.authParams(resource, challenge)
	bad.Set("redirect_uri", "http://127.0.0.1:54321/elsewhere")
	if resp, _ := agent.http.Get(h.base + "/oauth/authorize?" + bad.Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unregistered redirect should be refused without redirecting, got %d", resp.StatusCode)
	}
	bad = agent.authParams(resource, challenge)
	bad.Del("code_challenge")
	if _, back := agent.authorize(bad); back == nil || back.Query().Get("error") != "invalid_request" || back.Query().Get("state") != "st-123" || back.Query().Get("iss") != h.url {
		t.Fatalf("a request without PKCE should go back with invalid_request: %v", back)
	}
	if _, back := agent.authorize(agent.authParams(h.url+"/mcp/user/alice", challenge)); back == nil || back.Query().Get("error") != "invalid_target" {
		t.Fatalf("the person's own identity cannot be bound to a client: %v", back)
	}

	// The browser lands on the consent page, which needs the person.
	reqID, back := agent.authorize(agent.authParams(resource, challenge))
	if back != nil || reqID == "" {
		t.Fatalf("authorize should lead to the consent page: %v", back)
	}
	newBrowser(t, h.base).json("GET", "/api/oauth/requests/"+reqID, nil, http.StatusUnauthorized)
	expectStatus(t, alice, "GET", "/api/oauth/requests/"+reqID, nil, http.StatusForbidden)
	b := passkeyBrowser(t, h, regToken)
	var view struct {
		Client struct {
			Name, Via string
		} `json:"client"`
		Identity     string   `json:"identity"`
		Create       []string `json:"create"`
		RedirectKind string   `json:"redirect_kind"`
		RedirectHost string   `json:"redirect_host"`
		Problem      string   `json:"problem"`
	}
	json.Unmarshal(b.json("GET", "/api/oauth/requests/"+reqID, nil, http.StatusOK), &view)
	if view.Client.Name != "Test Agent" || view.Client.Via != "registration" || view.Identity != "user:alice/claude" ||
		len(view.Create) != 1 || view.Create[0] != "user:alice/claude" || view.RedirectKind != "loopback" ||
		view.RedirectHost != "127.0.0.1:54321" || view.Problem != "" {
		t.Fatalf("unexpected consent view: %+v", view)
	}
	// The identity does not exist yet: approving creates it, only when asked.
	b.json("POST", "/api/oauth/requests/"+reqID+"/approve", map[string]bool{}, http.StatusConflict)
	var approved struct {
		Redirect string `json:"redirect"`
	}
	json.Unmarshal(b.json("POST", "/api/oauth/requests/"+reqID+"/approve", map[string]bool{"create": true}, http.StatusOK), &approved)
	back = agent.follow(approved.Redirect)
	if !strings.HasPrefix(back.String(), "http://127.0.0.1:54321/callback?") || back.Query().Get("iss") != h.url {
		t.Fatalf("approving should send the browser back to the client: %s", back)
	}
	if resp, err := agent.http.Get(h.local(approved.Redirect)); err != nil || resp.Body.Close() != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("the return URL should work once: %v %v", resp, err)
	}
	authCode := codeFrom(t, back)
	if c, out := agent.token(url.Values{"grant_type": {"authorization_code"}, "code": {authCode}, "redirect_uri": {agent.redirect},
		"client_id": {agent.clientID}, "code_verifier": {verifier}, "resource": {h.url + "/mcp/user/alice/other"}}); c != http.StatusBadRequest || out["error"] != "invalid_target" {
		t.Fatalf("a code is good only for its resource: %d %v", c, out)
	}
	// That attempt used the code up; a new authorization gets a fresh one.
	first := agent.connect(b, resource, false)

	// The agent works through a stateful MCP session.
	holder := &tokenHolder{tok: first.access}
	c := connectHolder(t, endpoint, holder)
	me := c.call("whoami", map[string]any{}, false)
	sesID, _ := me["session"].(string)
	if me["identity"] != "user:alice/claude" || !strings.HasPrefix(sesID, "ses_") || me["connection_session"] != sesID {
		t.Fatalf("the MCP session should act as a session of user:alice/claude: %v", me)
	}
	issue := c.call("create_issue", map[string]any{"repo": "acme/app", "title": "from the agent"}, false)

	// The access token is good at its endpoint only.
	for _, other := range []string{h.base + "/mcp/user/alice", h.base + "/mcp/user/alice/other", h.base + "/mcp"} {
		if code, _ := postStatus(t, other, first.access); code != http.StatusUnauthorized {
			t.Fatalf("the access token should be refused at %s, got %d", other, code)
		}
	}
	expectStatus(t, h.client(first.access), "GET", "/api/me", nil, http.StatusUnauthorized)
	expectStatus(t, h.client(first.refresh), "GET", "/api/me", nil, http.StatusUnauthorized)
	if code, _ := postStatus(t, endpoint, first.refresh); code != http.StatusUnauthorized {
		t.Fatalf("a refresh token should not work at the endpoint, got %d", code)
	}
	if out, err := h.gitErr(dir, "ls-remote", h.cloneURL(first.access)); err == nil {
		t.Fatalf("the access token should not work for git:\n%s", out)
	}

	// A subagent sharing the connection opens a child session and names it.
	sub := c.call("open_session", map[string]any{"label": "explorer"}, false)
	subSes := sub["session"].(map[string]any)
	subID, _ := subSes["id"].(string)
	if subSes["parent"] != sesID || sub["token"] != nil {
		t.Fatalf("the child session should be below the MCP session's, with no token handed out: %v", sub)
	}
	if subMe := c.call("whoami", map[string]any{"session": subID}, false); subMe["session"] != subID || subMe["identity"] != "user:alice/claude" {
		t.Fatalf("naming the child session should act as it: %v", subMe)
	}
	c.call("comment_issue", map[string]any{"repo": "acme/app", "number": issue["number"], "body": "from the subagent", "session": subID}, false)
	if res := c.call("whoami", map[string]any{"session": "ses_nosuchsession"}, true); !strings.Contains(res["error"].(string), "not found") {
		t.Fatalf("an unknown session cannot be named: %v", res)
	}

	// The subagent pushes with a git credential bound to its session.
	gc := c.call("git_credential", map[string]any{"session": subID}, false)
	gitTok, _ := gc["token"].(string)
	if gc["session"] != subID || gc["committer_email"] != "user:alice/claude@localhost" || gc["git_url"] != h.url+"/git/" || gitTok == "" {
		t.Fatalf("unexpected git credential: %v", gc)
	}
	expectStatus(t, h.client(gitTok), "GET", "/api/me", nil, http.StatusUnauthorized)
	work := filepath.Join(dir, "agent")
	h.git(dir, "clone", h.cloneURL(gitTok), work)
	h.commitAs(work, "claude", "user:alice/claude@localhost", "notes.md", "notes\n", "add notes")
	h.git(work, "push", "origin", "HEAD:agent-notes")
	pushed := h.git(work, "rev-parse", "HEAD")

	// Refreshing rotates the tokens; the MCP session goes on as the same
	// session.
	code, refreshed := agent.refresh(first.refresh)
	if code != http.StatusOK {
		t.Fatalf("refresh: %d %v", code, refreshed)
	}
	second := tokens{refreshed["access_token"].(string), refreshed["refresh_token"].(string)}
	holder.set(second.access)
	if me := c.call("whoami", map[string]any{}, false); me["session"] != sesID {
		t.Fatalf("the MCP session should survive a refresh: %v", me)
	}

	// Everything is recorded under the sessions.
	evs := h.events(alice)
	requireEvent(t, evs, "the MCP session's session opened", func(e event) bool {
		return e.Type == "session.opened" && e.Identity == "user:alice/claude" && e.Payload["id"] == sesID && e.Payload["label"] == "gitsynth-test 1.0"
	})
	requireEvent(t, evs, "the issue under the MCP session", func(e event) bool {
		return e.Type == "issue.created" && e.Identity == "user:alice/claude" && e.Session == sesID
	})
	requireEvent(t, evs, "the child session opened under it", func(e event) bool {
		return e.Type == "session.opened" && e.Session == sesID && e.Payload["id"] == subID && e.Payload["parent"] == sesID
	})
	requireEvent(t, evs, "the comment under the child session", func(e event) bool {
		return e.Type == "issue.commented" && e.Session == subID
	})
	requireEvent(t, evs, "the push under the child session", func(e event) bool {
		return pushRecorded("user:alice/claude", "refs/heads/agent-notes", pushed)(e) && e.Session == subID
	})
	for _, typ := range []string{"oauth.client_registered", "oauth.granted", "oauth.tokens_issued", "identity.derived"} {
		requireEvent(t, evs, typ, func(e event) bool { return e.Type == typ })
	}

	// A refresh token works once; presenting it again revokes the grant.
	if code, out := agent.refresh(first.refresh); code != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("a used refresh token should be refused: %d %v", code, out)
	}
	if !c.fails("whoami", map[string]any{}) {
		t.Fatal("the grant should be revoked after a refresh token was reused")
	}
	if code, out := agent.refresh(second.refresh); code != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("the revoked grant's refresh token should be refused: %d %v", code, out)
	}
	if _, err := h.gitErr(dir, "ls-remote", h.cloneURL(gitTok)); err == nil {
		t.Fatal("the git credential should stop with its grant")
	}

	// A second connection to the same identity: the person sees it, and
	// revoking it stops it.
	third := agent.connect(b, resource, false)
	c3 := connectHolder(t, endpoint, &tokenHolder{tok: third.access})
	c3.call("whoami", map[string]any{}, false)
	var grants []map[string]any
	json.Unmarshal(b.json("GET", "/api/oauth/grants", nil, http.StatusOK), &grants)
	if len(grants) != 1 || grants[0]["identity"] != "user:alice/claude" || grants[0]["client_name"] != "Test Agent" {
		t.Fatalf("the live grant should be listed: %v", grants)
	}
	b.json("POST", "/api/oauth/grants/"+grants[0]["id"].(string)+"/revoke", nil, http.StatusOK)
	if !c3.fails("whoami", map[string]any{}) {
		t.Fatal("revoking the grant should stop the agent")
	}
	if code, _ := agent.refresh(third.refresh); code != http.StatusBadRequest {
		t.Fatalf("a revoked grant cannot be refreshed, got %d", code)
	}

	// Disabling the identity stops a connection too.
	fourth := agent.connect(b, resource, false)
	c4 := connectHolder(t, endpoint, &tokenHolder{tok: fourth.access})
	c4.call("whoami", map[string]any{}, false)
	b.json("POST", "/api/identities/disable", map[string]string{"id": "user:alice/claude"}, http.StatusOK)
	if !c4.fails("whoami", map[string]any{}) {
		t.Fatal("disabling the identity should stop the agent")
	}
	if code, _ := agent.refresh(fourth.refresh); code != http.StatusBadRequest {
		t.Fatalf("a disabled identity's grant cannot be refreshed, got %d", code)
	}
	b.json("POST", "/api/identities/enable", map[string]string{"id": "user:alice/claude"}, http.StatusOK)

	// The person can decline.
	_, challenge = pkce()
	reqID, _ = agent.authorize(agent.authParams(resource, challenge))
	var denied struct {
		Redirect string `json:"redirect"`
	}
	json.Unmarshal(b.json("POST", "/api/oauth/requests/"+reqID+"/deny", nil, http.StatusOK), &denied)
	if u := agent.follow(denied.Redirect); u.Query().Get("error") != "access_denied" || u.Query().Get("state") != "st-123" || u.Query().Get("iss") != h.url {
		t.Fatalf("declining should tell the client access_denied: %s", u)
	}

	// A desktop app with no loopback listener, such as the Claude desktop
	// app, registers a private-use scheme. Schemes the browser handles
	// itself are refused.
	for _, bad := range []string{"javascript:alert(document.cookie)", "data:text/html,<script>alert(1)</script>", "file:///etc/passwd"} {
		if code, raw := anon.call("POST", "/oauth/register", map[string]any{"redirect_uris": []string{bad}}); code != http.StatusBadRequest {
			t.Fatalf("registering %q should be refused: %d %s", bad, code, raw)
		}
	}
	const appRedirect = "claude://claude.ai/mcp-auth-callback/sdk"
	code, raw = anon.call("POST", "/oauth/register", map[string]any{
		"client_name": "Desktop Agent", "redirect_uris": []string{appRedirect}, "token_endpoint_auth_method": "none",
	})
	json.Unmarshal(raw, &reg)
	if code != http.StatusCreated {
		t.Fatalf("registering %s: %d %s", appRedirect, code, raw)
	}
	desktop := newOAuthAgent(t, h, reg["client_id"].(string), appRedirect)
	desktopResource := h.url + "/mcp/user/alice/desktop"
	_, challenge = pkce()
	for _, other := range []string{"claude://claude.ai/mcp-auth-callback/other", "claude-dev://claude.ai/mcp-auth-callback/sdk"} {
		p := desktop.authParams(desktopResource, challenge)
		p.Set("redirect_uri", other)
		if resp, _ := desktop.http.Get(h.base + "/oauth/authorize?" + p.Encode()); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("authorize with %s should be refused without redirecting, got %d", other, resp.StatusCode)
		}
	}
	reqID, back = desktop.authorize(desktop.authParams(desktopResource, challenge))
	if back != nil {
		t.Fatalf("authorize with the registered app scheme: %v", back)
	}
	var aview struct {
		RedirectKind   string `json:"redirect_kind"`
		RedirectScheme string `json:"redirect_scheme"`
		RedirectHost   string `json:"redirect_host"`
	}
	json.Unmarshal(b.json("GET", "/api/oauth/requests/"+reqID, nil, http.StatusOK), &aview)
	if aview.RedirectKind != "app" || aview.RedirectScheme != "claude" || aview.RedirectHost != "" {
		t.Fatalf("the consent page should say the answer goes to whichever app opens claude: links: %+v", aview)
	}
	json.Unmarshal(b.json("POST", "/api/oauth/requests/"+reqID+"/deny", nil, http.StatusOK), &denied)
	if u := desktop.follow(denied.Redirect); !strings.HasPrefix(u.String(), appRedirect+"?") || u.Query().Get("error") != "access_denied" ||
		u.Query().Get("state") != "st-123" || u.Query().Get("iss") != h.url {
		t.Fatalf("declining should send access_denied to the app: %s", u)
	}
	viaApp := desktop.connect(b, desktopResource, true)
	if me := connectHolder(t, h.base+"/mcp/user/alice/desktop", &tokenHolder{tok: viaApp.access}).call("whoami", map[string]any{}, false); me["identity"] != "user:alice/desktop" {
		t.Fatalf("the desktop app's client should act as user:alice/desktop: %v", me)
	}

	// A client known by its metadata document, which must name itself.
	liar := newOAuthAgent(t, h, docs.URL+"/liar.json", "http://localhost:43210/callback")
	if resp, _ := liar.http.Get(h.base + "/oauth/authorize?" + liar.authParams(resource, challenge).Encode()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a document naming another client_id should be refused, got %d", resp.StatusCode)
	}
	cimd := newOAuthAgent(t, h, docURL, "http://localhost:43210/callback")
	cimdResource := h.url + "/mcp/user/alice/cimd"
	verifier, challenge = pkce()
	reqID, back = cimd.authorize(cimd.authParams(cimdResource, challenge))
	if back != nil {
		t.Fatalf("authorize with a metadata document: %v", back)
	}
	var cview struct {
		Client struct {
			Name, Via, Host string
		} `json:"client"`
	}
	json.Unmarshal(b.json("GET", "/api/oauth/requests/"+reqID, nil, http.StatusOK), &cview)
	if cview.Client.Name != "Metadata Agent" || cview.Client.Via != "metadata_document" || cview.Client.Host != strings.TrimPrefix(docs.URL, "https://") {
		t.Fatalf("unexpected consent view for a metadata document: %+v", cview)
	}
	json.Unmarshal(b.json("POST", "/api/oauth/requests/"+reqID+"/approve", map[string]bool{"create": true}, http.StatusOK), &approved)
	cimdCode := codeFrom(t, cimd.follow(approved.Redirect))
	wrong, _ := pkce()
	if c, out := cimd.token(url.Values{"grant_type": {"authorization_code"}, "code": {cimdCode}, "redirect_uri": {cimd.redirect},
		"client_id": {cimd.clientID}, "code_verifier": {wrong}}); c != http.StatusBadRequest || out["error"] != "invalid_grant" {
		t.Fatalf("a wrong code verifier should be refused: %d %v", c, out)
	}
	if c, _ := cimd.token(url.Values{"grant_type": {"authorization_code"}, "code": {cimdCode}, "redirect_uri": {cimd.redirect},
		"client_id": {cimd.clientID}, "code_verifier": {verifier}}); c != http.StatusBadRequest {
		t.Fatalf("a code works once, got %d", c)
	}
	viaDoc := cimd.connect(b, cimdResource, false)
	if me := connectHolder(t, h.base+"/mcp/user/alice/cimd", &tokenHolder{tok: viaDoc.access}).call("whoami", map[string]any{}, false); me["identity"] != "user:alice/cimd" {
		t.Fatalf("the metadata document's client should act as user:alice/cimd: %v", me)
	}

	// Static tokens keep working: at the identity's endpoint, where each
	// connection still opens a session, and at the old /mcp.
	static := ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"})["token"].(string)
	sc := connectMCP(t, endpoint, static)
	if me := sc.call("whoami", map[string]any{}, false); me["identity"] != "user:alice/claude" || !strings.HasPrefix(me["session"].(string), "ses_") {
		t.Fatalf("a static token at the identity's endpoint should get a session: %v", me)
	}
	if code, _ := postStatus(t, h.base+"/mcp/user/alice/cimd", static); code != http.StatusUnauthorized {
		t.Fatalf("a static token should be refused at another identity's endpoint, got %d", code)
	}
	// A connection can name only sessions below its own.
	if res := sc.call("whoami", map[string]any{"session": subID}, true); !strings.Contains(res["error"].(string), "not below") {
		t.Fatalf("another connection's session cannot be named: %v", res)
	}
	// Ending the MCP session closes its session, and what is bound to it.
	staticMe := sc.call("whoami", map[string]any{}, false)
	staticGit := sc.call("git_credential", map[string]any{}, false)["token"].(string)
	h.git(dir, "ls-remote", h.cloneURL(staticGit))
	sc.cs.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := h.gitErr(dir, "ls-remote", h.cloneURL(staticGit)); err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the git credential should stop when its MCP session ends")
		}
		time.Sleep(50 * time.Millisecond)
	}
	requireEvent(t, h.events(alice), "the session closed with its MCP session", func(e event) bool {
		return e.Type == "session.closed" && e.Payload["id"] == staticMe["session"]
	})
	if me := connectMCP(t, h.base+"/mcp", static).call("whoami", map[string]any{}, false); me["identity"] != "user:alice/claude" || me["session"] != "" {
		t.Fatalf("the old /mcp should still serve a static token: %v", me)
	}
}
