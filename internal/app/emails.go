package app

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"gitsynth/internal/ident"
	"gitsynth/internal/store"
)

// In Git the identity lives in the committer, and a committer is an email
// address. An address resolves to an identity in two ways: the canonical
// form of the identity ("user:alice/claude@host" with this server's host),
// which needs no registration, or an address the identity registered.

// IdentityEmailAddress is an address registered to an identity.
type IdentityEmailAddress struct {
	Email    string `json:"email"`
	Identity string `json:"identity"`
}

func normalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

func validEmail(email string) bool {
	local, domain, ok := strings.Cut(email, "@")
	return ok && local != "" && domain != "" && !strings.ContainsAny(email, " \t\r\n<>")
}

func (a *App) host(ctx context.Context) string {
	a.hostMu.Lock()
	defer a.hostMu.Unlock()
	if a.hostCache == "" {
		a.hostCache, _ = a.Host(ctx)
	}
	return a.hostCache
}

// CommitEmail is the canonical address an identity commits with.
func (a *App) CommitEmail(ctx context.Context, identity string) string {
	return identity + "@" + a.host(ctx)
}

// ResolveEmail finds the identity an address belongs to.
func (a *App) ResolveEmail(ctx context.Context, q querier, email string) (string, bool, error) {
	email = normalizeEmail(email)
	if id, err := ident.Parse(email); err == nil {
		if id.Host != a.host(ctx) {
			return "", false, nil
		}
		err := a.identityExists(ctx, q, id.String())
		var ae *Error
		if errors.As(err, &ae) && ae.Code == http.StatusNotFound {
			return "", false, nil
		}
		return id.String(), err == nil, nil
	}
	var identity string
	err := q.QueryRowContext(ctx, `SELECT identity FROM identity_email_addresses WHERE email = ?`, email).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return identity, err == nil, err
}

// AddEmailAddress registers an address for an identity the actor is or
// controls. The first identity to register an address holds it.
func (a *App) AddEmailAddress(ctx context.Context, by Actor, identity, email string) (IdentityEmailAddress, error) {
	if identity == "" {
		identity = by.Identity
	}
	target, err := ident.Parse(identity)
	if err != nil {
		return IdentityEmailAddress{}, errf(http.StatusBadRequest, "%v", err)
	}
	email = normalizeEmail(email)
	if !validEmail(email) {
		return IdentityEmailAddress{}, errf(http.StatusBadRequest, "invalid email address %q", email)
	}
	if _, err := ident.Parse(email); err == nil {
		return IdentityEmailAddress{}, errf(http.StatusBadRequest, "%s is an identity's canonical address; it needs no registration", email)
	}
	addr := IdentityEmailAddress{Email: email, Identity: target.String()}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if target.String() != by.Identity {
			ok, err := a.controls(ctx, tx, by, target)
			if err != nil {
				return err
			}
			if !ok {
				return errf(http.StatusForbidden, "%s may not register addresses for %s", by.Identity, target)
			}
		}
		if err := a.identityExists(ctx, tx, target.String()); err != nil {
			return err
		}
		var holder string
		err := tx.QueryRowContext(ctx, `SELECT identity FROM identity_email_addresses WHERE email = ?`, email).Scan(&holder)
		switch {
		case err == nil && holder == addr.Identity:
			return errf(http.StatusConflict, "%s is already registered to %s", email, holder)
		case err == nil:
			return errf(http.StatusConflict, "%s is already registered to another identity", email)
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if _, err := tx.Exec(`INSERT INTO identity_email_addresses (email, identity) VALUES (?, ?)`, email, addr.Identity); err != nil {
			return err
		}
		_, err = tx.Append("identity.email_added", store.Scope{}, addr)
		return err
	})
	return addr, err
}

// RemoveEmailAddress drops a registered address, if the actor is or
// controls the identity holding it.
func (a *App) RemoveEmailAddress(ctx context.Context, by Actor, email string) error {
	email = normalizeEmail(email)
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		var identity string
		err := tx.QueryRowContext(ctx, `SELECT identity FROM identity_email_addresses WHERE email = ?`, email).Scan(&identity)
		if errors.Is(err, sql.ErrNoRows) {
			return errf(http.StatusNotFound, "%s is not registered", email)
		}
		if err != nil {
			return err
		}
		if identity != by.Identity {
			ok, err := a.controls(ctx, tx, by, ident.MustParse(identity))
			if err != nil {
				return err
			}
			if !ok {
				return errf(http.StatusNotFound, "%s is not registered", email)
			}
		}
		if _, err := tx.Exec(`DELETE FROM identity_email_addresses WHERE email = ?`, email); err != nil {
			return err
		}
		_, err = tx.Append("identity.email_removed", store.Scope{}, IdentityEmailAddress{Email: email, Identity: identity})
		return err
	})
	return err
}

// MyEmailAddresses lists the addresses of the identities the actor is or
// controls.
func (a *App) MyEmailAddresses(ctx context.Context, actor Actor) ([]IdentityEmailAddress, error) {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT email, identity FROM identity_email_addresses ORDER BY identity, email`)
	if err != nil {
		return nil, err
	}
	var all []IdentityEmailAddress
	for rows.Next() {
		var e IdentityEmailAddress
		if err := rows.Scan(&e.Email, &e.Identity); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, e)
	}
	rows.Close()
	out := []IdentityEmailAddress{}
	for _, e := range all {
		ok := e.Identity == actor.Identity
		if !ok {
			if ok, err = a.controls(ctx, a.Store.DB(), actor, ident.MustParse(e.Identity)); err != nil {
				return nil, err
			}
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}
