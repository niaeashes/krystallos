package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/descope/virtualwebauthn"
)

// browser is an HTTP client that keeps cookies, like a signed-in tab.
type browser struct {
	t      *testing.T
	base   string
	client *http.Client
}

func newBrowser(t *testing.T, base string) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: base, client: &http.Client{Jar: jar}}
}

func (b *browser) do(method, path string, body io.Reader, csrf bool) (int, []byte) {
	b.t.Helper()
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf {
		req.Header.Set("X-Gitsynth-CSRF", "1")
	}
	resp, err := b.client.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func (b *browser) json(method, path string, body any, want int) []byte {
	b.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	code, raw := b.do(method, path, rd, true)
	if code != want {
		b.t.Fatalf("%s %s: got %d, want %d: %s", method, path, code, want, raw)
	}
	return raw
}

func linkToken(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "token="); i >= 0 {
			u, err := url.Parse(strings.TrimPrefix(line, "passkey: "))
			if err != nil {
				t.Fatal(err)
			}
			return u.Query().Get("token")
		}
	}
	t.Fatalf("no link in output:\n%s", out)
	return ""
}

// TestBrowserSignIn registers a passkey through a one-time link and signs in
// with it, using a virtual authenticator so the server's WebAuthn checks run
// for real. The server runs with no address flags, so passkeys are bound to
// localhost and accepted from its own URL and from the Vite dev server. It
// then checks the cookie session's CSRF guard, sign-out, and the admin
// recovery link.
func TestBrowserSignIn(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, dir)
	out := h.run("admin", "init", "--data", h.data, "--user", "alice")
	if !strings.Contains(out, "passkey: http://localhost:3000/register?token=") {
		t.Fatalf("admin should link to the default URL of a localhost server:\n%s", out)
	}
	regToken := linkToken(t, out)
	h.start()

	rp := virtualwebauthn.RelyingParty{ID: "localhost", Name: "gitsynth", Origin: h.url}
	vite := virtualwebauthn.RelyingParty{ID: "localhost", Name: "gitsynth", Origin: "http://localhost:5173"}
	authn := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte("user:alice")})
	cred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)

	// Register.
	b := newBrowser(t, h.base)
	var begin struct {
		Ceremony string          `json:"ceremony"`
		Options  json.RawMessage `json:"options"`
	}
	json.Unmarshal(b.json("POST", "/api/auth/register/begin", map[string]string{"token": regToken}, http.StatusOK), &begin)
	attOpts, err := virtualwebauthn.ParseAttestationOptions(string(begin.Options))
	if err != nil {
		t.Fatal(err)
	}
	att := virtualwebauthn.CreateAttestationResponse(rp, authn, cred, *attOpts)
	if code, raw := b.do("POST", "/api/auth/register/finish?ceremony="+begin.Ceremony, strings.NewReader(att), false); code != http.StatusOK {
		t.Fatalf("register finish: %d %s", code, raw)
	}
	authn.AddCredential(cred)
	// The link works once.
	b2 := newBrowser(t, h.base)
	b2.json("POST", "/api/auth/register/begin", map[string]string{"token": regToken}, http.StatusUnauthorized)

	// Sign in with the passkey in a fresh browser.
	b = newBrowser(t, h.base)
	json.Unmarshal(b.json("POST", "/api/auth/login/begin", nil, http.StatusOK), &begin)
	asOpts, err := virtualwebauthn.ParseAssertionOptions(string(begin.Options))
	if err != nil {
		t.Fatal(err)
	}
	as := virtualwebauthn.CreateAssertionResponse(rp, authn, cred, *asOpts)
	if code, raw := b.do("POST", "/api/auth/login/finish?ceremony="+begin.Ceremony, strings.NewReader(as), false); code != http.StatusOK {
		t.Fatalf("login finish: %d %s", code, raw)
	}
	var me map[string]any
	json.Unmarshal(b.json("GET", "/api/me", nil, http.StatusOK), &me)
	if me["identity"] != "user:alice" || me["kind"] != "user" {
		t.Fatalf("signed in as the wrong identity: %v", me)
	}

	// A cookie alone cannot change anything; the CSRF header is required.
	if code, _ := b.do("POST", "/api/orgs", strings.NewReader(`{"name":"acme"}`), false); code != http.StatusForbidden {
		t.Fatalf("cookie request without CSRF header should be refused, got %d", code)
	}
	b.json("POST", "/api/orgs", map[string]string{"name": "acme"}, http.StatusOK)

	// Alice invites someone into acme. They pick a name and create a passkey,
	// at the Vite dev server; the user is created, with write in acme, in the
	// same step.
	var inv struct {
		URL string `json:"url"`
	}
	json.Unmarshal(b.json("POST", "/api/invitations", map[string]string{"org": "acme", "set": "write"}, http.StatusOK), &inv)
	if !strings.HasPrefix(inv.URL, h.url+"/register?token=") {
		t.Fatalf("the invitation should link to the server's URL %s: %s", h.url, inv.URL)
	}
	u, err := url.Parse(inv.URL)
	if err != nil {
		t.Fatal(err)
	}
	invToken := u.Query().Get("token")
	nb := newBrowser(t, h.base)
	var info map[string]string
	json.Unmarshal(nb.json("POST", "/api/auth/link/inspect", map[string]string{"token": invToken}, http.StatusOK), &info)
	if info["purpose"] != "invite" || info["org"] != "acme" {
		t.Fatalf("unexpected link info: %v", info)
	}
	nb.json("POST", "/api/auth/register/begin", map[string]string{"token": invToken, "name": "alice"}, http.StatusConflict)
	json.Unmarshal(nb.json("POST", "/api/auth/register/begin", map[string]string{"token": invToken, "name": "bob"}, http.StatusOK), &begin)
	bobAuthn := virtualwebauthn.NewAuthenticatorWithOptions(virtualwebauthn.AuthenticatorOptions{UserHandle: []byte("user:bob")})
	bobCred := virtualwebauthn.NewCredential(virtualwebauthn.KeyTypeEC2)
	attOpts, err = virtualwebauthn.ParseAttestationOptions(string(begin.Options))
	if err != nil {
		t.Fatal(err)
	}
	if code, raw := nb.do("POST", "/api/auth/register/finish?ceremony="+begin.Ceremony, strings.NewReader(virtualwebauthn.CreateAttestationResponse(vite, bobAuthn, bobCred, *attOpts)), false); code != http.StatusOK {
		t.Fatalf("invited registration: %d %s", code, raw)
	}
	json.Unmarshal(nb.json("GET", "/api/me", nil, http.StatusOK), &me)
	if me["identity"] != "user:bob" || me["admin"] != false {
		t.Fatalf("the invited user should be bob, not an admin: %v", me)
	}
	var bobOrgs map[string][]string
	json.Unmarshal(nb.json("GET", "/api/orgs", nil, http.StatusOK), &bobOrgs)
	if len(bobOrgs["orgs"]) != 1 || bobOrgs["orgs"][0] != "acme" {
		t.Fatalf("bob should be in acme: %v", bobOrgs)
	}
	nb.json("POST", "/api/invitations", map[string]string{}, http.StatusForbidden)

	// A signed-in browser can open a session as well; it opened it, so the
	// session stops when the browser signs out.
	var opened openedSession
	json.Unmarshal(b.json("POST", "/api/sessions", map[string]string{"label": "from the browser"}, http.StatusOK), &opened)
	fromBrowser := h.client(opened.Token)
	if me := ok[map[string]any](t, fromBrowser, "GET", "/api/me", nil); me["identity"] != "user:alice" || me["session"] != opened.Session.ID {
		t.Fatalf("a session opened by the browser should be alice's: %v", me)
	}

	// Signing out revokes the browser's credential, and with it the session
	// the browser opened.
	b.json("POST", "/api/auth/logout", nil, http.StatusOK)
	b.json("GET", "/api/me", nil, http.StatusUnauthorized)
	expectStatus(t, fromBrowser, "GET", "/api/me", nil, http.StatusUnauthorized)

	// The admin recovery link signs in once.
	out = h.run("admin", "login-link", "--data", h.data, "--user", "alice")
	loginToken := linkToken(t, out)
	b = newBrowser(t, h.base)
	b.json("POST", "/api/auth/link", map[string]string{"token": loginToken}, http.StatusOK)
	b.json("GET", "/api/me", nil, http.StatusOK)
	newBrowser(t, h.base).json("POST", "/api/auth/link", map[string]string{"token": loginToken}, http.StatusUnauthorized)

	// Every sign-in is in the event log, under alice.
	tok := tokenFrom(t, h.run("admin", "token", "--data", h.data, "--identity", "user:alice"))
	evs := h.events(h.client(tok))
	for _, want := range []string{"passkey.registered", "browser_session.started", "browser_session.ended"} {
		requireEvent(t, evs, want, func(e event) bool { return e.Type == want && e.Identity == "user:alice" })
	}
	for _, want := range []string{"user.joined", "passkey.registered"} {
		requireEvent(t, evs, want+" by bob", func(e event) bool { return e.Type == want && e.Identity == "user:bob" })
	}

}
