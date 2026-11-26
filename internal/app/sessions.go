package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"gitsynth/internal/store"
)

// A session is one run of an identity: an agent's run, or a subagent's run
// under it. Identities are made on purpose and last; sessions are opened
// freely, by any credential of an identity, for that same identity. Each
// session gets a short-lived credential of its own, so everything done
// through it carries the session's ID. A session opened with a session's
// credential is that session's child, so the record keeps the tree of runs.
//
// A session's credential never outlives the credential that opened it, and
// stops with it: authenticating walks up the openers and refuses when any
// of them is revoked or expired. Revoking a session's own credential ends
// the session, and every credential bound to it.

// Credential kinds. A session's own credential, and any other credential
// bound to a session (a git credential), holds the session's ID in the
// session column.
const (
	credToken        = "token"
	credBrowser      = "browser"
	credSession      = "session"
	credOAuthGrant   = "oauth_grant"
	credOAuthAccess  = "oauth_access"
	credOAuthRefresh = "oauth_refresh"
)

const (
	defaultSessionTTL = 8 * time.Hour
	maxSessionTTL     = 24 * time.Hour
	// maxSessionDepth bounds how deep sessions nest, and so how far
	// authenticating a session's credential walks up its openers.
	maxSessionDepth = 16
	// maxSessionLabel bounds a session's label, in bytes.
	maxSessionLabel = 200
	// maxGitCredentialTTL bounds a git credential's life.
	maxGitCredentialTTL = time.Hour
)

// Session is a session as recorded.
type Session struct {
	ID       string `json:"id"`
	Identity string `json:"identity"`
	// Parent is the session this one was opened from, or empty for a
	// session opened with a standing token, a signed-in browser or an
	// OAuth grant.
	Parent string `json:"parent"`
	// Label is an outside ID, such as a Claude Code session ID, that ties
	// the session's events to the run's own log.
	Label    string `json:"label"`
	OpenedAt string `json:"opened_at"`
}

// OpenedSession is a freshly opened session with its credential. The token
// is shown once and only its hash is stored.
type OpenedSession struct {
	Session   Session `json:"session"`
	Token     string  `json:"token,omitempty"`
	ExpiresAt string  `json:"expires_at"`
	// CommitterEmail is the canonical address to commit with under the
	// session's identity.
	CommitterEmail string `json:"committer_email"`
	// Actor acts as the session, for callers in this process.
	Actor Actor `json:"-"`
}

// OpenSession opens a session of the actor's own identity, as a child of the
// actor's session when the actor came with a session's credential. ttlHours
// is the credential's lifetime: 8 hours if zero, at most 24, and never past
// the expiry of the actor's credential.
func (a *App) OpenSession(ctx context.Context, by Actor, label string, ttlHours float64) (OpenedSession, error) {
	if by.Credential.ID == "" {
		return OpenedSession{}, errf(http.StatusBadRequest, "a session is opened with a credential of the identity")
	}
	label, err := sessionLabel(label)
	if err != nil {
		return OpenedSession{}, err
	}
	if ttlHours < 0 || ttlHours > maxSessionTTL.Hours() {
		return OpenedSession{}, errf(http.StatusBadRequest, "ttl_hours must be at most %d (0 for the default, %d)", int(maxSessionTTL.Hours()), int(defaultSessionTTL.Hours()))
	}
	ttl := defaultSessionTTL
	if ttlHours > 0 {
		ttl = time.Duration(ttlHours * float64(time.Hour))
	}
	expires, err := capExpiry(time.Now().UTC().Add(ttl), by.Credential.ExpiresAt)
	if err != nil {
		return OpenedSession{}, err
	}
	return a.openSession(ctx, by, by.Credential.ID, label, expires)
}

// OpenMCPSession opens the session an MCP session acts as, when a client
// initializes at an identity's endpoint. It is opened by what lasts across
// the client's requests: the grant's own credential for an OAuth client,
// whose access tokens come and go, or else the credential the client
// connected with. It lasts as long as that credential did when it opened,
// and ends earlier when the MCP session ends (CloseSession). Its token is
// never handed out: the returned session's Actor acts as it.
func (a *App) OpenMCPSession(ctx context.Context, by Actor, label string) (OpenedSession, error) {
	label, err := sessionLabel(label)
	if err != nil {
		return OpenedSession{}, err
	}
	opener, until := by.Credential.ID, by.Credential.ExpiresAt
	if by.Credential.Grant != "" {
		opener, until = by.Credential.GrantCredential, by.Credential.GrantExpiresAt
	}
	if opener == "" {
		return OpenedSession{}, errf(http.StatusBadRequest, "a session is opened with a credential of the identity")
	}
	if until != "" && expired(until) {
		return OpenedSession{}, errf(http.StatusUnauthorized, "token expired")
	}
	opened, err := a.openSession(ctx, by, opener, label, until)
	opened.Token = ""
	return opened, err
}

// sessionLabel checks a session's label.
func sessionLabel(label string) (string, error) {
	label = strings.TrimSpace(label)
	if len(label) > maxSessionLabel {
		return "", errf(http.StatusBadRequest, "label is longer than %d bytes", maxSessionLabel)
	}
	if strings.ContainsFunc(label, unicode.IsControl) {
		return "", errf(http.StatusBadRequest, "label must not contain control characters")
	}
	return label, nil
}

// capExpiry returns the earlier of want and limit (RFC 3339, or empty for
// none) as RFC 3339, and refuses one that has already passed.
func capExpiry(want time.Time, limit string) (string, error) {
	if limit != "" {
		t, err := time.Parse(time.RFC3339, limit)
		if err != nil {
			return "", err
		}
		if t.Before(want) {
			want = t
		}
	}
	if !want.After(time.Now()) {
		return "", errf(http.StatusUnauthorized, "token expired")
	}
	return want.UTC().Format(time.RFC3339), nil
}

// openSession opens a session of by's identity, opened by the credential
// opener (by's own, or its grant's), as a child of by's session when by
// came with a session's credential. expires is RFC 3339, or empty for a
// session whose credential does not expire on its own.
func (a *App) openSession(ctx context.Context, by Actor, opener, label, expires string) (OpenedSession, error) {
	parent := ""
	if by.Credential.Kind == credSession {
		parent = by.Session
	}
	now := time.Now().UTC()
	out := OpenedSession{
		Session: Session{
			ID: "ses_" + randomString(10), Identity: by.Identity, Parent: parent, Label: label,
			OpenedAt: now.Format(time.RFC3339),
		},
		Token:          "gs_" + randomString(32),
		ExpiresAt:      expires,
		CommitterEmail: a.CommitEmail(ctx, by.Identity),
	}
	credID := "cred_" + randomString(10)
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		// Authenticate checked all of this before the request got here;
		// check it again where nothing can change in between.
		if err := a.identityActive(ctx, tx, by.ID()); err != nil {
			return err
		}
		var revoked bool
		var until string
		if err := tx.QueryRowContext(ctx, `SELECT revoked, expires_at FROM credentials WHERE id = ?`, opener).Scan(&revoked, &until); err != nil {
			return notFound("credential "+opener, err)
		}
		if revoked {
			return errf(http.StatusUnauthorized, "token revoked")
		}
		if expired(until) {
			return errf(http.StatusUnauthorized, "token expired")
		}
		if parent != "" {
			depth, err := sessionDepth(ctx, tx, parent)
			if err != nil {
				return err
			}
			if depth >= maxSessionDepth {
				return errf(http.StatusBadRequest, "sessions nest at most %d deep", maxSessionDepth)
			}
		}
		s := out.Session
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, session, expires_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			credID, s.Identity, hashToken(out.Token), s.ID, out.ExpiresAt, credSession); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO sessions (id, identity, parent, opened_by, credential, label, opened_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			s.ID, s.Identity, s.Parent, opener, credID, s.Label, s.OpenedAt); err != nil {
			return err
		}
		_, err := tx.Append("session.opened", store.Scope{}, map[string]any{
			"id": s.ID, "parent": s.Parent, "label": s.Label, "credential": credID, "expires_at": out.ExpiresAt,
		})
		return err
	})
	if err != nil {
		return OpenedSession{}, err
	}
	out.Actor = by
	out.Actor.Session = out.Session.ID
	out.Actor.Credential = CredentialRef{ID: credID, Kind: credSession, ExpiresAt: out.ExpiresAt}
	return out, nil
}

// CloseSession ends the session act acts as (act came from an opened
// session's Actor or from DescendantSession): its own credential is
// revoked, and with it every session opened below it and every credential
// bound to it. reason goes into the event.
func (a *App) CloseSession(ctx context.Context, act Actor, reason string) error {
	if act.Session == "" || act.Credential.Kind != credSession {
		return errf(http.StatusBadRequest, "no session to close")
	}
	_, err := a.Store.Write(ctx, act.Actor, func(tx *store.Tx) error {
		res, err := tx.Exec(`UPDATE credentials SET revoked = 1 WHERE id = ? AND revoked = 0`, act.Credential.ID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return err
		}
		_, err = tx.Append("session.closed", store.Scope{}, map[string]any{"id": act.Session, "credential": act.Credential.ID, "reason": reason})
		return err
	})
	return err
}

// SessionAlive checks that the session act acts as still works: its own
// credential and every credential that opened a session above it are
// neither revoked nor expired, and its identity is not disabled.
func (a *App) SessionAlive(ctx context.Context, act Actor) error {
	db := a.Store.DB()
	if _, err := sessionDepth(ctx, db, act.Session); err != nil {
		return err
	}
	for _, id := range act.ID().Chain() {
		var disabled bool
		if err := db.QueryRowContext(ctx, `SELECT disabled FROM identities WHERE id = ?`, id.String()).Scan(&disabled); err != nil {
			return notFound("identity "+id.String(), err)
		}
		if disabled {
			return errf(http.StatusUnauthorized, "identity %s is disabled", id)
		}
	}
	return nil
}

// DescendantSession resolves a session opened below root's session, so a
// run that shares root's connection (a subagent) can act as the session it
// opened. Only a session of root's own identity, somewhere below root's
// session and still working, can be named.
func (a *App) DescendantSession(ctx context.Context, root Actor, id string) (Actor, error) {
	if root.Session == "" {
		return Actor{}, errf(http.StatusBadRequest, "this connection has no session, so it cannot act as one below it")
	}
	db := a.Store.DB()
	var identity, parent, cred, expires string
	err := db.QueryRowContext(ctx,
		`SELECT s.identity, s.parent, s.credential, c.expires_at FROM sessions s JOIN credentials c ON c.id = s.credential WHERE s.id = ?`, id).
		Scan(&identity, &parent, &cred, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && identity != root.Identity) {
		return Actor{}, errf(http.StatusNotFound, "session %s not found", id)
	}
	if err != nil {
		return Actor{}, err
	}
	for depth := 0; parent != root.Session; depth++ {
		if parent == "" || depth >= maxSessionDepth {
			return Actor{}, errf(http.StatusForbidden, "session %s is not below this connection's session %s", id, root.Session)
		}
		if err := db.QueryRowContext(ctx, `SELECT parent FROM sessions WHERE id = ?`, parent).Scan(&parent); err != nil {
			return Actor{}, notFound("session", err)
		}
	}
	if _, err := sessionDepth(ctx, db, id); err != nil {
		return Actor{}, err
	}
	act := root
	act.Session = id
	act.Credential = CredentialRef{ID: cred, Kind: credSession, ExpiresAt: expires}
	return act, nil
}

// GitCredential is a short-lived credential for git, bound to a session.
type GitCredential struct {
	ID        string `json:"id"`
	Identity  string `json:"identity"`
	Session   string `json:"session"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
	// CommitterEmail is the address to commit with.
	CommitterEmail string `json:"committer_email"`
}

// IssueGitCredential issues a credential for git to the session act acts
// as. It lasts an hour at most and never past the session's own credential,
// is accepted only at git's endpoints, and stops when the session ends.
// What is pushed with it carries the session.
func (a *App) IssueGitCredential(ctx context.Context, act Actor) (GitCredential, error) {
	if act.Session == "" || act.Credential.Kind != credSession {
		return GitCredential{}, errf(http.StatusBadRequest, "a git credential is bound to a session; open one first")
	}
	expires, err := capExpiry(time.Now().UTC().Add(maxGitCredentialTTL), act.Credential.ExpiresAt)
	if err != nil {
		return GitCredential{}, err
	}
	out := GitCredential{
		ID: "cred_" + randomString(10), Identity: act.Identity, Session: act.Session,
		Token: "gs_" + randomString(32), ExpiresAt: expires, CommitterEmail: a.CommitEmail(ctx, act.Identity),
	}
	_, err = a.Store.Write(ctx, act.Actor, func(tx *store.Tx) error {
		if err := a.identityActive(ctx, tx, act.ID()); err != nil {
			return err
		}
		if _, err := sessionDepth(ctx, tx, act.Session); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, session, expires_at, kind) VALUES (?, ?, ?, ?, ?, ?)`,
			out.ID, out.Identity, hashToken(out.Token), out.Session, out.ExpiresAt, credSession); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO credential_audiences (credential, audience) VALUES (?, ?)`, out.ID, AudienceGit); err != nil {
			return err
		}
		_, err := tx.Append("credential.issued", store.Scope{}, map[string]any{
			"id": out.ID, "identity": out.Identity, "session": out.Session, "expires_at": out.ExpiresAt, "audience": AudienceGit,
		})
		return err
	})
	if err != nil {
		return GitCredential{}, err
	}
	return out, nil
}

// sessionDepth walks from a session up through the credentials that opened
// it, and the sessions those belong to, until it reaches a credential that
// is not a session's (a standing token, a signed-in browser, an OAuth
// grant). It refuses when the own credential of any session on the way, or
// any opener, is revoked or expired: revoking or expiring a credential
// stops every session opened below it, and ending a session stops every
// credential bound to it. It returns how many sessions deep the session
// is: 1 for one opened with a standing token.
func sessionDepth(ctx context.Context, q querier, session string) (int, error) {
	for depth := 1; depth <= maxSessionDepth; depth++ {
		var openedBy, own string
		err := q.QueryRowContext(ctx, `SELECT opened_by, credential FROM sessions WHERE id = ?`, session).Scan(&openedBy, &own)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errf(http.StatusUnauthorized, "session %s not found", session)
		}
		if err != nil {
			return 0, err
		}
		var ownExpires string
		var ownRevoked bool
		err = q.QueryRowContext(ctx, `SELECT expires_at, revoked FROM credentials WHERE id = ?`, own).Scan(&ownExpires, &ownRevoked)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && ownRevoked) {
			return 0, errf(http.StatusUnauthorized, "session %s has ended", session)
		}
		if err != nil {
			return 0, err
		}
		if expired(ownExpires) {
			return 0, errf(http.StatusUnauthorized, "session %s has expired", session)
		}
		var kind, above, expires string
		var revoked bool
		err = q.QueryRowContext(ctx, `SELECT kind, session, expires_at, revoked FROM credentials WHERE id = ?`, openedBy).
			Scan(&kind, &above, &expires, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errf(http.StatusUnauthorized, "the credential that opened session %s is gone", session)
		}
		if err != nil {
			return 0, err
		}
		if revoked {
			return 0, errf(http.StatusUnauthorized, "the credential that opened session %s was revoked", session)
		}
		if expired(expires) {
			return 0, errf(http.StatusUnauthorized, "the credential that opened session %s has expired", session)
		}
		if kind != credSession {
			return depth, nil
		}
		session = above
	}
	return 0, errf(http.StatusUnauthorized, "sessions nest more than %d deep", maxSessionDepth)
}

// expired reports whether an RFC 3339 expiry has passed. Empty means the
// credential never expires; an expiry that does not parse counts as passed.
func expired(at string) bool {
	if at == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, at)
	return err != nil || !time.Now().Before(t)
}
