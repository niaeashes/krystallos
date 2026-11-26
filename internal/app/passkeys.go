package app

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"

	"gitsynth/internal/ident"
	"gitsynth/internal/store"
)

// A user's root identity signs in with a passkey, so an agent running on the
// same machine cannot act as the person: a passkey needs the person to be
// there.

// PasskeyConfig is the relying party the passkeys are bound to.
type PasskeyConfig struct {
	RPID    string
	Origins []string
}

type ceremony struct {
	session   webauthn.SessionData
	principal string // the user registering, "user:alice"
	linkHash  string // the one-time link a registration redeems
	invite    *invite
	expires   time.Time
}

type invite struct {
	name    string
	org     string
	permSet string
}

type passkeys struct {
	wa         *webauthn.WebAuthn
	mu         sync.Mutex
	ceremonies map[string]ceremony
}

// EnablePasskeys configures WebAuthn. Without it, passkey endpoints answer
// that sign-in is unavailable.
func (a *App) EnablePasskeys(cfg PasskeyConfig) error {
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          cfg.RPID,
		RPDisplayName: "gitsynth",
		RPOrigins:     cfg.Origins,
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationPreferred,
		},
	})
	if err != nil {
		return err
	}
	a.pk = &passkeys{wa: wa, ceremonies: map[string]ceremony{}}
	return nil
}

// maxCeremonies bounds the sign-ins and registrations in progress. Starting
// a sign-in needs no credential, so without it a client calling begin in a
// loop could grow the map, and the memory it holds, without limit.
const maxCeremonies = 1000

// put keeps a ceremony for its finish to take. Expired ones are swept out
// only when the map is full; take never hands one out anyway.
func (p *passkeys) put(c ceremony) (string, error) {
	now := time.Now()
	c.expires = now.Add(5 * time.Minute)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.ceremonies) >= maxCeremonies {
		for k, v := range p.ceremonies {
			if now.After(v.expires) {
				delete(p.ceremonies, k)
			}
		}
		if len(p.ceremonies) >= maxCeremonies {
			return "", errf(http.StatusServiceUnavailable, "too many sign-ins in progress; try again in a minute")
		}
	}
	id := randomString(20)
	p.ceremonies[id] = c
	return id, nil
}

func (p *passkeys) take(id string) (ceremony, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.ceremonies[id]
	delete(p.ceremonies, id)
	if !ok || time.Now().After(c.expires) {
		return ceremony{}, false
	}
	return c, true
}

// passkeyUser is a user as WebAuthn sees it. The user handle stored in the
// authenticator is the user's root identity, "user:alice".
type passkeyUser struct {
	principal string
	creds     []webauthn.Credential
}

func (u passkeyUser) WebAuthnID() []byte                         { return []byte(u.principal) }
func (u passkeyUser) WebAuthnName() string                       { return strings.TrimPrefix(u.principal, "user:") }
func (u passkeyUser) WebAuthnDisplayName() string                { return strings.TrimPrefix(u.principal, "user:") }
func (u passkeyUser) WebAuthnCredentials() []webauthn.Credential { return u.creds }

func (a *App) passkeyUser(ctx context.Context, principal string) (passkeyUser, error) {
	u := passkeyUser{principal: principal}
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT data FROM passkeys WHERE principal = ?`, principal)
	if err != nil {
		return u, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return u, err
		}
		var c webauthn.Credential
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			return u, err
		}
		u.creds = append(u.creds, c)
	}
	return u, rows.Err()
}

func (a *App) passkeysEnabled() error {
	if a.pk == nil {
		return errf(http.StatusServiceUnavailable, "passkey sign-in is not configured on this server")
	}
	return nil
}

// userID accepts "alice" or "user:alice" and returns "user:alice".
func userID(name string) string {
	if strings.HasPrefix(name, "user:") {
		return name
	}
	return "user:" + name
}

// ---- one-time links ----

// IssueEnrollmentLink creates a one-time link for an existing user. Purpose
// "register" lets them add a passkey; "login" signs them in once, as a
// recovery path that only someone with the admin CLI can open.
func (a *App) IssueEnrollmentLink(ctx context.Context, by Actor, user, purpose string, ttl time.Duration) (string, error) {
	if purpose != "register" && purpose != "login" {
		return "", errf(http.StatusBadRequest, "purpose must be register or login")
	}
	if !by.Admin {
		return "", errf(http.StatusForbidden, "only instance admins may issue these links")
	}
	principal := userID(user)
	var kind string
	err := a.Store.DB().QueryRowContext(ctx, `SELECT kind FROM principals WHERE id = ?`, principal).Scan(&kind)
	if err != nil {
		return "", notFound(principal, err)
	}
	if kind != string(ident.User) {
		return "", errf(http.StatusBadRequest, "only users sign in with passkeys")
	}
	return a.issueLink(ctx, by, principal, purpose, "", "", ttl)
}

// IssueInvite creates a one-time link that lets someone join as a new user.
// An instance admin may invite anyone; an org admin may invite people into
// their org, who then get permSet there.
func (a *App) IssueInvite(ctx context.Context, by Actor, org, permSet string, ttl time.Duration) (string, error) {
	if by.Identity != SystemActor.Identity && !by.Standard() {
		return "", errf(http.StatusForbidden, "only a person, by hand, may invite people")
	}
	if org == "" {
		if !by.Admin {
			return "", errf(http.StatusForbidden, "only instance admins may invite people without an org")
		}
		permSet = ""
	} else {
		if err := a.require(ctx, a.Store.DB(), by, org, "", PermOrgAdmin); err != nil {
			return "", err
		}
		if permSet == "" {
			permSet = "write"
		}
		if _, ok := builtinSets[permSet]; !ok {
			return "", errf(http.StatusBadRequest, "unknown permission set %q", permSet)
		}
	}
	return a.issueLink(ctx, by, "", "invite", org, permSet, ttl)
}

func (a *App) issueLink(ctx context.Context, by Actor, principal, purpose, org, permSet string, ttl time.Duration) (string, error) {
	token := "gsl_" + randomString(32)
	expires := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO enrollment_links (hash, principal, purpose, org, perm_set, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
			hashToken(token), principal, purpose, org, permSet, expires); err != nil {
			return err
		}
		payload := map[string]any{"purpose": purpose, "expires_at": expires}
		if principal != "" {
			payload["user"] = principal
		}
		if permSet != "" {
			payload["set"] = permSet
		}
		_, err := tx.Append("link.issued", store.Scope{Org: org}, payload)
		return err
	})
	return token, err
}

// LinkInfo says what a one-time link is for, so the page can ask for a name
// when it is an invitation.
type LinkInfo struct {
	Purpose string `json:"purpose"`
	User    string `json:"user,omitempty"`
	Org     string `json:"org,omitempty"`
}

type link struct {
	LinkInfo
	permSet string
}

func (a *App) lookupLink(ctx context.Context, q querier, token string, purposes ...string) (link, error) {
	var l link
	var expires string
	var used bool
	err := q.QueryRowContext(ctx, `SELECT principal, purpose, org, perm_set, expires_at, used FROM enrollment_links WHERE hash = ?`, hashToken(token)).
		Scan(&l.User, &l.Purpose, &l.Org, &l.permSet, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return link{}, errf(http.StatusUnauthorized, "this link is not valid")
	}
	if err != nil {
		return link{}, err
	}
	t, err := time.Parse(time.RFC3339, expires)
	if used || err != nil || time.Now().After(t) {
		return link{}, errf(http.StatusUnauthorized, "this link has expired or was already used")
	}
	for _, p := range purposes {
		if l.Purpose == p {
			return l, nil
		}
	}
	return link{}, errf(http.StatusUnauthorized, "this link is not valid here")
}

// InspectLink reports what a link is for, without using it.
func (a *App) InspectLink(ctx context.Context, token string) (LinkInfo, error) {
	l, err := a.lookupLink(ctx, a.Store.DB(), token, "register", "invite", "login")
	return l.LinkInfo, err
}

// ---- registration ----

// BeginPasskeyRegistration starts adding a passkey through a one-time link.
// For an invitation, name is the user name the person picks.
func (a *App) BeginPasskeyRegistration(ctx context.Context, linkToken, name string) (string, *protocol.CredentialCreation, error) {
	if err := a.passkeysEnabled(); err != nil {
		return "", nil, err
	}
	l, err := a.lookupLink(ctx, a.Store.DB(), linkToken, "register", "invite")
	if err != nil {
		return "", nil, err
	}
	c := ceremony{principal: l.User, linkHash: hashToken(linkToken)}
	if l.Purpose == "invite" {
		if !ident.ValidSegment(name) {
			return "", nil, errf(http.StatusBadRequest, "pick a user name: lowercase letters, digits, '.', '_' and '-'")
		}
		var taken int
		if err := a.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM principals WHERE name = ?`, name).Scan(&taken); err != nil {
			return "", nil, err
		}
		if taken > 0 {
			return "", nil, errf(http.StatusConflict, "the name %q is already taken", name)
		}
		c.principal = userID(name)
		c.invite = &invite{name: name, org: l.Org, permSet: l.permSet}
	}
	user, err := a.passkeyUser(ctx, c.principal)
	if err != nil {
		return "", nil, err
	}
	var exclude []protocol.CredentialDescriptor
	for _, cred := range user.creds {
		exclude = append(exclude, cred.Descriptor())
	}
	opts, session, err := a.pk.wa.BeginRegistration(user, webauthn.WithExclusions(exclude),
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementRequired))
	if err != nil {
		return "", nil, err
	}
	c.session = *session
	id, err := a.pk.put(c)
	if err != nil {
		return "", nil, err
	}
	return id, opts, nil
}

// FinishPasskeyRegistration stores the new passkey and uses up the link. For
// an invitation it also creates the user, in the same transaction.
func (a *App) FinishPasskeyRegistration(ctx context.Context, ceremonyID string, body io.Reader) (Actor, error) {
	if err := a.passkeysEnabled(); err != nil {
		return Actor{}, err
	}
	c, ok := a.pk.take(ceremonyID)
	if !ok || c.principal == "" {
		return Actor{}, errf(http.StatusBadRequest, "registration expired; start again")
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(body)
	if err != nil {
		return Actor{}, errf(http.StatusBadRequest, "invalid passkey response: %v", err)
	}
	user, err := a.passkeyUser(ctx, c.principal)
	if err != nil {
		return Actor{}, err
	}
	cred, err := a.pk.wa.CreateCredential(user, c.session, parsed)
	if err != nil {
		return Actor{}, errf(http.StatusBadRequest, "passkey was not accepted: %v", err)
	}
	actor := Actor{Actor: store.Actor{Identity: c.principal, Principal: c.principal}, Kind: ident.User}
	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	_, err = a.Store.Write(ctx, actor.Actor, func(tx *store.Tx) error {
		res, err := tx.Exec(`UPDATE enrollment_links SET used = 1 WHERE hash = ? AND used = 0`, c.linkHash)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errf(http.StatusUnauthorized, "this link was already used")
		}
		if c.invite != nil {
			if _, err := createPrincipalTx(tx, ident.User, c.invite.name, false, "", ""); err != nil {
				return err
			}
			if _, err := tx.Append("user.joined", store.Scope{Org: c.invite.org}, map[string]any{"id": c.principal}); err != nil {
				return err
			}
			if c.invite.org != "" {
				if _, err := tx.Exec(`INSERT INTO org_grants (org, identity, perm_set) VALUES (?, ?, ?)
					ON CONFLICT (org, identity) DO UPDATE SET perm_set = excluded.perm_set`, c.invite.org, c.principal, c.invite.permSet); err != nil {
					return err
				}
				if _, err := tx.Append("grant.set", store.Scope{Org: c.invite.org}, map[string]any{
					"identity": c.principal, "set": c.invite.permSet, "via": "invitation",
				}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`INSERT INTO passkeys (id, principal, data) VALUES (?, ?, ?)`, credID, c.principal, mustJSON(cred)); err != nil {
			return err
		}
		_, err = tx.Append("passkey.registered", store.Scope{}, map[string]any{"credential_id": credID})
		return err
	})
	if err != nil {
		return Actor{}, err
	}
	return a.standardActor(ctx, c.principal)
}

// ---- sign-in ----

// BeginPasskeyLogin starts a discoverable sign-in: the authenticator says
// whose passkey it holds.
func (a *App) BeginPasskeyLogin(ctx context.Context) (string, *protocol.CredentialAssertion, error) {
	if err := a.passkeysEnabled(); err != nil {
		return "", nil, err
	}
	opts, session, err := a.pk.wa.BeginDiscoverableLogin(webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return "", nil, err
	}
	id, err := a.pk.put(ceremony{session: *session})
	if err != nil {
		return "", nil, err
	}
	return id, opts, nil
}

// FinishPasskeyLogin verifies the assertion and returns the user's root
// identity.
func (a *App) FinishPasskeyLogin(ctx context.Context, ceremonyID string, body io.Reader) (Actor, error) {
	if err := a.passkeysEnabled(); err != nil {
		return Actor{}, err
	}
	c, ok := a.pk.take(ceremonyID)
	if !ok || c.principal != "" {
		return Actor{}, errf(http.StatusBadRequest, "sign-in expired; start again")
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(body)
	if err != nil {
		return Actor{}, errf(http.StatusBadRequest, "invalid passkey response: %v", err)
	}
	user, cred, err := a.pk.wa.ValidatePasskeyLogin(func(_, userHandle []byte) (webauthn.User, error) {
		return a.passkeyUser(ctx, string(userHandle))
	}, c.session, parsed)
	if err != nil {
		return Actor{}, errf(http.StatusUnauthorized, "passkey was not accepted")
	}
	principal := user.(passkeyUser).principal
	credID := base64.RawURLEncoding.EncodeToString(cred.ID)
	// Keep the signature counter current; it is how a cloned key shows.
	if _, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE passkeys SET data = ? WHERE id = ?`, mustJSON(cred), credID)
		return err
	}); err != nil {
		return Actor{}, err
	}
	return a.standardActor(ctx, principal)
}

// RedeemLoginLink signs a user in once through an admin-issued link.
func (a *App) RedeemLoginLink(ctx context.Context, token string) (Actor, error) {
	var principal string
	_, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
		l, err := a.lookupLink(ctx, tx, token, "login")
		if err != nil {
			return err
		}
		principal = l.User
		_, err = tx.Exec(`UPDATE enrollment_links SET used = 1 WHERE hash = ?`, hashToken(token))
		return err
	})
	if err != nil {
		return Actor{}, err
	}
	return a.standardActor(ctx, principal)
}

func (a *App) standardActor(ctx context.Context, principal string) (Actor, error) {
	act := Actor{Actor: store.Actor{Identity: principal, Principal: principal}}
	var kind string
	err := a.Store.DB().QueryRowContext(ctx, `SELECT kind, admin FROM principals WHERE id = ?`, principal).Scan(&kind, &act.Admin)
	act.Kind = ident.Kind(kind)
	return act, notFound(principal, err)
}

// ---- browser sessions ----

// BrowserSession is a signed-in browser. It is a credential like a token,
// kept in a cookie and bound to the user's root identity.
type BrowserSession struct {
	Token   string
	Expires time.Time
}

const browserSessionTTL = 12 * time.Hour

// StartBrowserSession issues the cookie credential after a sign-in.
func (a *App) StartBrowserSession(ctx context.Context, actor Actor, method string) (BrowserSession, error) {
	s := BrowserSession{Token: "gss_" + randomString(32), Expires: time.Now().UTC().Add(browserSessionTTL)}
	id := "sess_" + randomString(10)
	_, err := a.Store.Write(ctx, actor.Actor, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, expires_at, kind) VALUES (?, ?, ?, ?, 'browser')`,
			id, actor.Identity, hashToken(s.Token), s.Expires.Format(time.RFC3339)); err != nil {
			return err
		}
		_, err := tx.Append("browser_session.started", store.Scope{}, map[string]any{"credential_id": id, "method": method})
		return err
	})
	return s, err
}

// EndBrowserSession revokes the cookie credential.
func (a *App) EndBrowserSession(ctx context.Context, actor Actor, token string) error {
	_, err := a.Store.Write(ctx, actor.Actor, func(tx *store.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM credentials WHERE hash = ? AND kind = 'browser'`, hashToken(token)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE credentials SET revoked = 1 WHERE id = ?`, id); err != nil {
			return err
		}
		_, err = tx.Append("browser_session.ended", store.Scope{}, map[string]any{"credential_id": id})
		return err
	})
	return err
}
