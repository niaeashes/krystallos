package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"gitsynth/internal/ident"
	"gitsynth/internal/store"
)

// Actor is an authenticated identity.
type Actor struct {
	store.Actor
	// Kind is the kind of the principal at the root: user, bot or service.
	Kind ident.Kind `json:"kind"`
	// Admin is true for users who administer the instance.
	Admin bool `json:"admin"`
	// Credential is the credential the actor authenticated with. It is
	// empty for actors that did not come through Authenticate, such as the
	// admin CLI.
	Credential CredentialRef `json:"-"`
}

// CredentialRef names the credential a request came with.
type CredentialRef struct {
	ID string
	// Kind is "token" (a standing token), "browser" (a signed-in browser),
	// "session" (a credential bound to a session) or "oauth_access" (an
	// OAuth access token).
	Kind string
	// ExpiresAt is RFC 3339, or empty for a credential that never expires.
	ExpiresAt string
	// Audience is where the credential is accepted, if it is bound to one:
	// an MCP endpoint's URL, or AudienceGit.
	Audience string
	// Grant is the OAuth grant an access token belongs to, with the grant's
	// own credential and its expiry.
	Grant, GrantCredential, GrantExpiresAt string
}

// ID parses the actor's identity.
func (a Actor) ID() ident.ID {
	id, _ := ident.Parse(a.Identity)
	return id
}

// Standard reports whether the actor is a user's root identity: the person's
// own hand, not something derived from it.
func (a Actor) Standard() bool { return a.Kind == ident.User && a.Identity == a.Principal }

// System is the server's own actor.
var System = Actor{Actor: SystemActor, Kind: ident.Service, Admin: true}

// ---- initialization ----

// Initialize sets the server's host, which every identity is qualified
// with, and registers the built-in service. It writes the log's first event
// and runs once.
func (a *App) Initialize(ctx context.Context, host string) error {
	if !ident.ValidHost(host) {
		return errf(http.StatusBadRequest, "invalid host %q", host)
	}
	_, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return errf(http.StatusConflict, "this data directory is already initialized")
		}
		if _, err := tx.Append("server.initialized", store.Scope{}, map[string]any{"host": host}); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES ('host', ?)`, host); err != nil {
			return err
		}
		m := Manifest{Name: "gitsynth", Description: "The server's own behavior: merge commits, reconciliation.", Permissions: []string{}}
		if _, err := createPrincipalTx(tx, ident.Service, "gitsynth", false, "", mustJSON(m)); err != nil {
			return err
		}
		_, err := tx.Append("service.registered", store.Scope{}, map[string]any{"id": "service:gitsynth", "builtin": true})
		return err
	})
	return err
}

// Host is the host identities are qualified with.
func (a *App) Host(ctx context.Context) (string, error) {
	host, err := a.Store.Meta(ctx, "host")
	if err == nil && host == "" {
		err = errors.New("this data directory is not initialized; run `gitsynth admin init` first")
	}
	return host, err
}

// ---- principals ----

func createPrincipalTx(tx *store.Tx, kind ident.Kind, name string, admin bool, owner, manifest string) (ident.ID, error) {
	if !ident.ValidSegment(name) {
		return ident.ID{}, errf(http.StatusBadRequest, "invalid name %q: use lowercase letters, digits, '.', '_' and '-'", name)
	}
	var taken int
	if err := tx.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM principals WHERE name = ?`, name).Scan(&taken); err != nil {
		return ident.ID{}, err
	}
	if taken > 0 {
		return ident.ID{}, errf(http.StatusConflict, "the name %q is already taken", name)
	}
	id := ident.Root(kind, name)
	if _, err := tx.Exec(`INSERT INTO principals (id, kind, name, admin, owner, manifest) VALUES (?, ?, ?, ?, ?, ?)`,
		id.String(), string(kind), name, admin, owner, manifest); err != nil {
		return ident.ID{}, err
	}
	if _, err := tx.Exec(`INSERT INTO identities (id, principal) VALUES (?, ?)`, id.String(), id.String()); err != nil {
		return ident.ID{}, err
	}
	return id, nil
}

// CreateUser adds a user. Only instance admins do this directly; everyone
// else joins through an invitation.
func (a *App) CreateUser(ctx context.Context, by Actor, name string, admin bool) (ident.ID, error) {
	if !by.Admin {
		return ident.ID{}, errf(http.StatusForbidden, "only instance admins may add users")
	}
	var id ident.ID
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		var err error
		if id, err = createPrincipalTx(tx, ident.User, name, admin, "", ""); err != nil {
			return err
		}
		_, err = tx.Append("user.created", store.Scope{}, map[string]any{"id": id.String(), "admin": admin})
		return err
	})
	return id, err
}

// CreateBot adds a bot owned by a user ("user:alice"), an org ("org:acme")
// or the server ("server"). The owner is a relation, not part of the bot's
// name, so a bot can change hands without changing its identity.
func (a *App) CreateBot(ctx context.Context, by Actor, name, owner string) (ident.ID, error) {
	var scope store.Scope
	switch {
	case strings.HasPrefix(owner, "user:"):
		if !by.Standard() || by.Principal != owner {
			return ident.ID{}, errf(http.StatusForbidden, "only %s, by hand, may create bots it owns", owner)
		}
	case strings.HasPrefix(owner, "org:"):
		org := strings.TrimPrefix(owner, "org:")
		if err := a.require(ctx, a.Store.DB(), by, org, "", PermOrgAdmin); err != nil {
			return ident.ID{}, err
		}
		scope.Org = org
	case owner == "server":
		if !by.Admin || !by.Standard() {
			return ident.ID{}, errf(http.StatusForbidden, "only instance admins, by hand, may create server-owned bots")
		}
	default:
		return ident.ID{}, errf(http.StatusBadRequest, "owner must be user:NAME, org:NAME or server")
	}
	var id ident.ID
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		var err error
		if id, err = createPrincipalTx(tx, ident.Bot, name, false, owner, ""); err != nil {
			return err
		}
		_, err = tx.Append("bot.created", scope, map[string]any{"id": id.String(), "owner": owner})
		return err
	})
	return id, err
}

// Manifest describes a service: what it is, which permissions it asks for in
// the orgs it is installed into, and which events it wants.
type Manifest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Permissions []string `json:"permissions"`
	Events      []string `json:"events,omitempty"`
}

// RegisterService adds a service to the server from its manifest.
func (a *App) RegisterService(ctx context.Context, by Actor, m Manifest) (ident.ID, error) {
	if !by.Admin || !by.Standard() {
		return ident.ID{}, errf(http.StatusForbidden, "only instance admins, by hand, may register services")
	}
	for _, p := range m.Permissions {
		if !slices.Contains(allPerms, p) || p == PermOrgAdmin || p == PermRepoAdmin {
			return ident.ID{}, errf(http.StatusBadRequest, "a service cannot ask for %q", p)
		}
	}
	if m.Permissions == nil {
		m.Permissions = []string{}
	}
	var id ident.ID
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		var err error
		if id, err = createPrincipalTx(tx, ident.Service, m.Name, false, "", mustJSON(m)); err != nil {
			return err
		}
		_, err = tx.Append("service.registered", store.Scope{}, map[string]any{"id": id.String(), "manifest": m})
		return err
	})
	return id, err
}

func (a *App) manifest(ctx context.Context, q querier, service string) (Manifest, error) {
	var m Manifest
	var raw string
	err := q.QueryRowContext(ctx, `SELECT manifest FROM principals WHERE id = ? AND kind = 'service'`, service).Scan(&raw)
	if err != nil {
		return m, notFound("service "+service, err)
	}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return m, err
	}
	if m.Permissions == nil {
		m.Permissions = []string{}
	}
	return m, nil
}

// InstallService installs a registered service into an org, granting it the
// permissions its manifest asks for there.
func (a *App) InstallService(ctx context.Context, by Actor, org, service string) error {
	if service == SystemActor.Identity {
		return errf(http.StatusBadRequest, "the built-in service is part of every org already")
	}
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if err := a.require(ctx, tx, by, org, "", PermOrgAdmin); err != nil {
			return err
		}
		m, err := a.manifest(ctx, tx, service)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO service_installs (service, org, perms) VALUES (?, ?, ?)
			ON CONFLICT (service, org) DO UPDATE SET perms = excluded.perms`, service, org, mustJSON(m.Permissions)); err != nil {
			return err
		}
		_, err = tx.Append("service.installed", store.Scope{Org: org}, map[string]any{"service": service, "permissions": m.Permissions})
		return err
	})
	return err
}

// manages reports whether the actor administers a principal: a user is
// managed by its own root identity, a bot by its owner (the user by hand,
// the org's admins, or the instance admins), a service by the instance
// admins.
func (a *App) manages(ctx context.Context, q querier, by Actor, principal ident.ID) (bool, error) {
	if by.Identity == SystemActor.Identity {
		return true, nil
	}
	switch principal.Kind {
	case ident.User:
		return by.Standard() && by.Principal == principal.String(), nil
	case ident.Service:
		return by.Admin && by.Standard(), nil
	}
	var owner string
	err := q.QueryRowContext(ctx, `SELECT owner FROM principals WHERE id = ?`, principal.String()).Scan(&owner)
	if err != nil {
		return false, notFound(principal.String(), err)
	}
	switch {
	case strings.HasPrefix(owner, "user:"):
		return by.Standard() && by.Principal == owner, nil
	case strings.HasPrefix(owner, "org:"):
		return a.Can(ctx, q, by, strings.TrimPrefix(owner, "org:"), "", PermOrgAdmin)
	case owner == "server":
		return by.Admin && by.Standard(), nil
	}
	return false, nil
}

// controls reports whether the actor may act for target: revoke its
// credentials, disable it, register addresses for it, commit as it. An
// identity controls everything derived from it; a principal's manager
// controls the whole tree.
func (a *App) controls(ctx context.Context, q querier, by Actor, target ident.ID) (bool, error) {
	if target.Below(by.ID()) {
		return true, nil
	}
	return a.manages(ctx, q, by, target.RootID())
}

// makes reports whether the actor may make things that last in target's
// tree: derive identities, issue standing tokens, enable a disabled
// identity. Identities are made on purpose, so this is for the person (a
// user's root identity, in its own tree), for a bot's managers acting as
// their own root identity (in the bot's tree), for the instance admins on a
// service, and for the admin CLI. A derived identity never does it, nor
// does a bot or a service for itself.
func (a *App) makes(ctx context.Context, q querier, by Actor, target ident.ID) (bool, error) {
	if by.Identity == SystemActor.Identity {
		return true, nil
	}
	if !by.Standard() {
		return false, nil
	}
	return a.manages(ctx, q, by, target.RootID())
}

// identityActive checks that an identity exists and that neither it nor any
// identity it was derived from is disabled: nothing new is issued or opened
// at or below a disabled identity.
func (a *App) identityActive(ctx context.Context, q querier, id ident.ID) error {
	if err := a.identityExists(ctx, q, id.String()); err != nil {
		return err
	}
	for _, up := range id.Chain()[1:] {
		var disabled bool
		if err := q.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, up.String()).Scan(&disabled); err != nil {
			return notFound("identity "+up.String(), err)
		}
		if disabled {
			return errf(http.StatusConflict, "identity %s is disabled", up)
		}
	}
	return nil
}

func (a *App) identityExists(ctx context.Context, q querier, id string) error {
	var disabled bool
	err := q.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, id).Scan(&disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return errf(http.StatusNotFound, "identity %q not found", id)
	}
	if err != nil {
		return err
	}
	if disabled {
		return errf(http.StatusConflict, "identity %q is disabled", id)
	}
	return nil
}

// ---- derivation ----

// Derive creates an identity below parent (below the actor if parent is
// empty). Identities are made on purpose: a user derives in their own tree,
// by hand, and a bot's managers derive in the bot's tree. An identity never
// derives on its own, so an agent cannot create identities; another run of
// it opens a session instead. Services hold a single identity.
func (a *App) Derive(ctx context.Context, by Actor, parent, label string) (ident.ID, error) {
	p := by.ID()
	if parent != "" {
		var err error
		if p, err = ident.Parse(parent); err != nil {
			return ident.ID{}, errf(http.StatusBadRequest, "%v", err)
		}
	}
	if p.Kind == ident.Service {
		return ident.ID{}, errf(http.StatusBadRequest, "services hold a single identity")
	}
	child, err := p.Child(label)
	if err != nil {
		return ident.ID{}, errf(http.StatusBadRequest, "%v", err)
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		return a.deriveTx(ctx, tx, by, child)
	})
	return child, err
}

// deriveTx creates child below its parent, which must exist and be active,
// if by may make identities there.
func (a *App) deriveTx(ctx context.Context, tx *store.Tx, by Actor, child ident.ID) error {
	p, ok := child.Parent()
	if !ok {
		return errf(http.StatusBadRequest, "%s is a root identity", child)
	}
	ok, err := a.makes(ctx, tx, by, p)
	if err != nil {
		return err
	}
	if !ok {
		return errf(http.StatusForbidden, "%s may not derive identities below %s: identities are made by the person, or by a bot's owner; another run opens a session instead", by.Identity, p)
	}
	if err := a.identityActive(ctx, tx, p); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO identities (id, principal, parent) VALUES (?, ?, ?)`, child.String(), p.RootID().String(), p.String()); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return errf(http.StatusConflict, "identity %s already exists", child)
		}
		return err
	}
	_, err = tx.Append("identity.derived", store.Scope{}, map[string]any{"id": child.String(), "parent": p.String()})
	return err
}

// DisableIdentity stops an identity and everything derived from it: the
// switch to pull on an agent that runs away. Every credential at or below
// it is revoked, sessions included, and the event says which; while it is
// disabled nothing new is issued or opened there.
func (a *App) DisableIdentity(ctx context.Context, by Actor, identity string) error {
	target, err := ident.Parse(identity)
	if err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		ok, err := a.controls(ctx, tx, by, target)
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusForbidden, "%s may not disable %s", by.Identity, target)
		}
		var disabled bool
		err = tx.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, target.String()).Scan(&disabled)
		if err != nil {
			return notFound("identity "+target.String(), err)
		}
		if disabled {
			return errf(http.StatusConflict, "identity %s is already disabled", target)
		}
		if _, err := tx.Exec(`UPDATE identities SET disabled = 1 WHERE id = ?`, target.String()); err != nil {
			return err
		}
		revoked, err := revokeTree(ctx, tx, target)
		if err != nil {
			return err
		}
		_, err = tx.Append("identity.disabled", store.Scope{}, map[string]any{"id": target.String(), "revoked": revoked})
		return err
	})
	return err
}

// revokeTree revokes every credential of an identity and of the identities
// derived from it, and returns their IDs.
func revokeTree(ctx context.Context, tx *store.Tx, target ident.ID) ([]string, error) {
	// The tree is the identity itself and every identity whose name
	// continues it with "/".
	prefix := target.String() + "/"
	const where = `revoked = 0 AND (identity = ? OR substr(identity, 1, ?) = ?)`
	ids, err := queryStrings(ctx, tx, `SELECT id FROM credentials WHERE `+where+` ORDER BY rowid`, target.String(), len(prefix), prefix)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`UPDATE credentials SET revoked = 1 WHERE `+where, target.String(), len(prefix), prefix)
	return ids, err
}

// EnableIdentity undoes DisableIdentity, so a role's name is never used up.
// It brings back no credential: a runaway agent's token stays dead, and the
// identity gets fresh ones. Disabling revoked everything at and below the
// identity already; whatever is still unrevoked there (left by a server
// from before disabling revoked anything) is revoked now, before it could
// work again. Enabling is for whoever may make identities there (the
// person, or a bot's managers), never for a derived identity.
func (a *App) EnableIdentity(ctx context.Context, by Actor, identity string) error {
	target, err := ident.Parse(identity)
	if err != nil {
		return errf(http.StatusBadRequest, "%v", err)
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		ok, err := a.makes(ctx, tx, by, target)
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusForbidden, "%s may not enable %s: that is for the person, or for a bot's owner", by.Identity, target)
		}
		var disabled bool
		err = tx.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, target.String()).Scan(&disabled)
		if err != nil {
			return notFound("identity "+target.String(), err)
		}
		if !disabled {
			return errf(http.StatusConflict, "identity %s is not disabled", target)
		}
		if _, err := tx.Exec(`UPDATE identities SET disabled = 0 WHERE id = ?`, target.String()); err != nil {
			return err
		}
		payload := map[string]any{"id": target.String()}
		revoked, err := revokeTree(ctx, tx, target)
		if err != nil {
			return err
		}
		if len(revoked) > 0 {
			payload["revoked"] = revoked
		}
		_, err = tx.Append("identity.enabled", store.Scope{}, payload)
		return err
	})
	return err
}

// ---- credentials ----

// IssuedToken is a freshly issued credential. The token is shown once and
// only its hash is stored.
type IssuedToken struct {
	ID        string `json:"id"`
	Identity  string `json:"identity"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Token     string `json:"token"`
	// CommitterEmail is the canonical address to commit with under this
	// identity, so a session can set GIT_COMMITTER_EMAIL from it.
	CommitterEmail string `json:"committer_email"`
}

// IssueToken issues a standing token for an identity: one that lasts and
// belongs to no session. It follows the same rule as deriving (the person,
// a bot's managers, the admin CLI), because a standing token is how an
// identity is handed to an agent in the first place. Runs of that agent
// open sessions with it.
func (a *App) IssueToken(ctx context.Context, by Actor, identity string, ttl time.Duration) (IssuedToken, error) {
	target, err := ident.Parse(identity)
	if err != nil {
		return IssuedToken{}, errf(http.StatusBadRequest, "%v", err)
	}
	if target.String() == SystemActor.Identity {
		return IssuedToken{}, errf(http.StatusForbidden, "the built-in service has no tokens")
	}
	tok := IssuedToken{ID: "cred_" + randomString(10), Identity: target.String(), Token: "gs_" + randomString(32),
		CommitterEmail: a.CommitEmail(ctx, target.String())}
	if ttl > 0 {
		tok.ExpiresAt = time.Now().UTC().Add(ttl).Format(time.RFC3339)
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		ok, err := a.makes(ctx, tx, by, target)
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusForbidden, "%s may not issue tokens for %s: standing tokens are issued by the person, or by a bot's owner; a run opens a session instead", by.Identity, target)
		}
		if err := a.identityActive(ctx, tx, target); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, expires_at, kind) VALUES (?, ?, ?, ?, 'token')`,
			tok.ID, tok.Identity, hashToken(tok.Token), tok.ExpiresAt); err != nil {
			return err
		}
		_, err = tx.Append("credential.issued", store.Scope{}, map[string]any{
			"id": tok.ID, "identity": tok.Identity, "expires_at": tok.ExpiresAt,
		})
		return err
	})
	return tok, err
}

// AudienceGit is the audience of a credential that works only for git.
const AudienceGit = "git"

// Authenticate resolves a token presented where no audience applies: the
// API, the event stream, the legacy MCP endpoint. A credential bound to an
// audience (an OAuth access token, a git credential) is refused here.
func (a *App) Authenticate(ctx context.Context, token string) (Actor, error) {
	return a.AuthenticateFor(ctx, token, "")
}

// AuthenticateFor resolves a token to its identity, as presented at
// audience: an MCP endpoint's URL, AudienceGit, or "" for anywhere else. A
// credential with no audience works everywhere; one bound to an audience
// works only there. A grant's own credential and a refresh token never
// authenticate a request.
//
// A credential stops working when its identity, or any identity it was
// derived from, is disabled; a session's credential also stops when the
// session's own credential, or any credential that opened a session above
// it, is revoked or expires; an OAuth token stops with its grant.
func (a *App) AuthenticateFor(ctx context.Context, token, audience string) (Actor, error) {
	var act Actor
	var kind, bound string
	var revoked bool
	db := a.Store.DB()
	err := db.QueryRowContext(ctx,
		`SELECT c.id, c.kind, c.expires_at, c.identity, i.principal, c.session, c.revoked, p.kind, p.admin,
		        COALESCE(b.audience, ''), COALESCE(b.grant_id, '')
		 FROM credentials c
		 JOIN identities i ON i.id = c.identity
		 JOIN principals p ON p.id = i.principal
		 LEFT JOIN credential_audiences b ON b.credential = c.id
		 WHERE c.hash = ?`, hashToken(token)).
		Scan(&act.Credential.ID, &act.Credential.Kind, &act.Credential.ExpiresAt, &act.Identity, &act.Principal, &act.Session, &revoked, &kind, &act.Admin,
			&bound, &act.Credential.Grant)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{}, errf(http.StatusUnauthorized, "invalid token")
	}
	if err != nil {
		return Actor{}, err
	}
	act.Kind = ident.Kind(kind)
	act.Admin = act.Admin && act.Kind == ident.User
	switch act.Credential.Kind {
	case credToken, credBrowser, credSession, credOAuthAccess:
	default:
		return Actor{}, errf(http.StatusUnauthorized, "invalid token")
	}
	if act.Credential.Kind == credOAuthAccess && bound == "" {
		return Actor{}, errf(http.StatusUnauthorized, "invalid token")
	}
	// A credential with no audience is good anywhere.
	switch {
	case bound == "" || bound == audience:
	case audience == "":
		return Actor{}, errf(http.StatusUnauthorized, "this token is bound to %s and is accepted only there", describeAudience(bound))
	default:
		return Actor{}, errf(http.StatusUnauthorized, "this token is not for this endpoint")
	}
	if revoked {
		return Actor{}, errf(http.StatusUnauthorized, "token revoked")
	}
	if expired(act.Credential.ExpiresAt) {
		return Actor{}, errf(http.StatusUnauthorized, "token expired")
	}
	act.Credential.Audience = bound
	if act.Credential.Grant != "" {
		g, err := grantCredential(ctx, db, act.Credential.Grant)
		if err != nil {
			return Actor{}, err
		}
		if g.revoked || expired(g.expiresAt) {
			return Actor{}, errf(http.StatusUnauthorized, "the grant this token belongs to has ended")
		}
		act.Credential.GrantCredential = g.id
		act.Credential.GrantExpiresAt = g.expiresAt
	}
	if act.Credential.Kind == credSession {
		if _, err := sessionDepth(ctx, db, act.Session); err != nil {
			return Actor{}, err
		}
	}
	if err := chainEnabled(ctx, db, act.ID()); err != nil {
		return Actor{}, err
	}
	return act, nil
}

// chainEnabled refuses an identity that is disabled, or derived from one
// that is: no credential works there.
func chainEnabled(ctx context.Context, q querier, target ident.ID) error {
	for _, id := range target.Chain() {
		var disabled bool
		if err := q.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, id.String()).Scan(&disabled); err != nil {
			return err
		}
		if disabled {
			return errf(http.StatusUnauthorized, "identity %s is disabled", id)
		}
	}
	return nil
}
