package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"gitsynth/internal/ident"
	"gitsynth/internal/store"
)

// Orgs lists the orgs the actor holds any grant in (all of them for admins).
func (a *App) Orgs(ctx context.Context, actor Actor) ([]string, error) {
	if actor.Admin {
		return queryStrings(ctx, a.Store.DB(), `SELECT name FROM orgs ORDER BY name`)
	}
	return queryStrings(ctx, a.Store.DB(),
		`SELECT org FROM org_grants WHERE identity = ?
		 UNION SELECT r.org FROM repo_grants g JOIN repos r ON r.full_name = g.repo WHERE g.identity = ?
		 ORDER BY 1`, actor.Identity, actor.Identity)
}

// ReposOf lists an org's repositories the actor may read.
func (a *App) ReposOf(ctx context.Context, actor Actor, org string) ([]Repo, error) {
	all, err := a.Repos(ctx)
	if err != nil {
		return nil, err
	}
	out := []Repo{}
	for _, r := range all {
		if r.Org != org {
			continue
		}
		ok := actor.Admin
		if !ok {
			if ok, err = a.Can(ctx, a.Store.DB(), actor, r.Org, r.FullName, PermRepoRead); err != nil {
				return nil, err
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// ChangeSetSummary is one row of a change set list.
type ChangeSetSummary struct {
	Number int    `json:"number"`
	Issue  int    `json:"issue"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Merged bool   `json:"merged"`
}

// ChangeSets lists a repository's change sets.
func (a *App) ChangeSets(ctx context.Context, fullName string) ([]ChangeSetSummary, error) {
	rows, err := a.Store.DB().QueryContext(ctx,
		`SELECT number, issue, title, author, branch, base, merged_commit != '' FROM changesets WHERE repo = ? ORDER BY number DESC`, fullName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ChangeSetSummary{}
	for rows.Next() {
		var c ChangeSetSummary
		if err := rows.Scan(&c.Number, &c.Issue, &c.Title, &c.Author, &c.Branch, &c.Base, &c.Merged); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IdentityInfo is one identity in a derivation tree.
type IdentityInfo struct {
	ID       string `json:"id"`
	Parent   string `json:"parent,omitempty"`
	Disabled bool   `json:"disabled"`
}

func (a *App) treeOf(ctx context.Context, principal string) ([]IdentityInfo, error) {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT id, parent, disabled FROM identities WHERE principal = ? ORDER BY id`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IdentityInfo{}
	for rows.Next() {
		var i IdentityInfo
		if err := rows.Scan(&i.ID, &i.Parent, &i.Disabled); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// MyIdentities lists the identities in the actor's own tree: everything
// derived from the actor's root (for a derived actor, from the actor itself).
func (a *App) MyIdentities(ctx context.Context, actor Actor) ([]IdentityInfo, error) {
	all, err := a.treeOf(ctx, actor.Principal)
	if err != nil {
		return nil, err
	}
	me := actor.ID()
	out := []IdentityInfo{}
	for _, i := range all {
		if id, err := ident.Parse(i.ID); err == nil && (actor.Standard() || id.Within(me)) {
			out = append(out, i)
		}
	}
	return out, nil
}

// BotInfo is a bot with its owner and its identities.
type BotInfo struct {
	ID         string         `json:"id"`
	Owner      string         `json:"owner"`
	Identities []IdentityInfo `json:"identities"`
}

// ManagedBots lists the bots the actor manages: its own, its orgs' and,
// for instance admins, the server's.
func (a *App) ManagedBots(ctx context.Context, actor Actor) ([]BotInfo, error) {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT id, owner FROM principals WHERE kind = 'bot' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var all []BotInfo
	for rows.Next() {
		var b BotInfo
		if err := rows.Scan(&b.ID, &b.Owner); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, b)
	}
	rows.Close()
	out := []BotInfo{}
	for _, b := range all {
		ok, err := a.manages(ctx, a.Store.DB(), actor, ident.MustParse(b.ID))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if b.Identities, err = a.treeOf(ctx, b.ID); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// ServiceInfo is a registered service.
type ServiceInfo struct {
	ID       string   `json:"id"`
	Manifest Manifest `json:"manifest"`
	Orgs     []string `json:"orgs"`
}

// Services lists the registered services and the orgs each is installed in.
func (a *App) Services(ctx context.Context) ([]ServiceInfo, error) {
	ids, err := queryStrings(ctx, a.Store.DB(), `SELECT id FROM principals WHERE kind = 'service' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	out := []ServiceInfo{}
	for _, id := range ids {
		s := ServiceInfo{ID: id}
		if s.Manifest, err = a.manifest(ctx, a.Store.DB(), id); err != nil {
			return nil, err
		}
		if s.Orgs, err = queryStrings(ctx, a.Store.DB(), `SELECT org FROM service_installs WHERE service = ? ORDER BY org`, id); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// CredentialInfo describes a credential without its secret. Kind is
// "token", "browser" or "session"; a session's credential carries the
// session's ID and label.
type CredentialInfo struct {
	ID        string `json:"id"`
	Identity  string `json:"identity"`
	Kind      string `json:"kind"`
	Session   string `json:"session,omitempty"`
	Label     string `json:"label,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Revoked   bool   `json:"revoked"`
}

// MyCredentials lists the credentials the actor controls: those of its own
// tree and of the bots it manages.
func (a *App) MyCredentials(ctx context.Context, actor Actor) ([]CredentialInfo, error) {
	// OAuth's credentials are shown as the grants they belong to.
	rows, err := a.Store.DB().QueryContext(ctx,
		`SELECT c.id, c.identity, c.kind, c.session, COALESCE(s.label, ''), c.expires_at, c.revoked
		 FROM credentials c LEFT JOIN sessions s ON c.kind = 'session' AND s.id = c.session
		 WHERE c.kind NOT LIKE 'oauth\_%' ESCAPE '\'
		 ORDER BY c.rowid DESC`)
	if err != nil {
		return nil, err
	}
	var all []CredentialInfo
	for rows.Next() {
		var c CredentialInfo
		if err := rows.Scan(&c.ID, &c.Identity, &c.Kind, &c.Session, &c.Label, &c.ExpiresAt, &c.Revoked); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, c)
	}
	rows.Close()
	out := []CredentialInfo{}
	me := actor.ID()
	for _, c := range all {
		id, err := ident.Parse(c.Identity)
		if err != nil {
			continue
		}
		ok := id.Within(me) && (c.Kind != "browser" || actor.Standard())
		if !ok && id.Kind != ident.User {
			if ok, err = a.controls(ctx, a.Store.DB(), actor, id); err != nil {
				return nil, err
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// RevokeCredential revokes a credential the actor controls.
func (a *App) RevokeCredential(ctx context.Context, actor Actor, id string) error {
	_, err := a.Store.Write(ctx, actor.Actor, func(tx *store.Tx) error {
		var identity string
		err := tx.QueryRowContext(ctx, `SELECT identity FROM credentials WHERE id = ?`, id).Scan(&identity)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(http.StatusNotFound, "credential %s not found", id)
		}
		if err != nil {
			return err
		}
		target, err := ident.Parse(identity)
		if err != nil {
			return err
		}
		ok, err := a.controls(ctx, tx, actor, target)
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusNotFound, "credential %s not found", id)
		}
		if _, err := tx.Exec(`UPDATE credentials SET revoked = 1 WHERE id = ?`, id); err != nil {
			return err
		}
		_, err = tx.Append("credential.revoked", store.Scope{}, map[string]any{"id": id, "identity": identity})
		return err
	})
	return err
}
