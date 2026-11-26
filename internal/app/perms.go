package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"krystallos/internal/ident"
)

// Permissions. Each is one operation on one kind of resource.
const (
	PermRepoRead       = "repo.read"
	PermRepoPush       = "repo.push"
	PermRepoAdmin      = "repo.admin"
	PermOrgAdmin       = "org.admin"
	PermIdeaWrite      = "idea.write"
	PermIssueWrite     = "issue.write"
	PermPlanWrite      = "plan.write"
	PermChangeSetWrite = "changeset.write"
	PermChangeSetMerge = "changeset.merge"
	PermReviewWrite    = "review.write"
	PermReviewApprove  = "review.approve"
	PermCheckWrite     = "check.write"
)

var allPerms = []string{
	PermRepoRead, PermRepoPush, PermRepoAdmin, PermOrgAdmin, PermIdeaWrite, PermIssueWrite,
	PermPlanWrite, PermChangeSetWrite, PermChangeSetMerge, PermReviewWrite, PermReviewApprove, PermCheckWrite,
}

// builtinSets are the permission sets every repository has.
var builtinSets = func() map[string][]string {
	read := []string{PermRepoRead}
	write := append(slices.Clone(read), PermRepoPush, PermIdeaWrite, PermIssueWrite, PermPlanWrite,
		PermChangeSetWrite, PermReviewWrite, PermCheckWrite)
	maintain := append(slices.Clone(write), PermChangeSetMerge, PermReviewApprove)
	admin := append(slices.Clone(maintain), PermRepoAdmin, PermOrgAdmin)
	return map[string][]string{"read": read, "write": write, "maintain": maintain, "admin": admin}
}()

// Can reports whether the actor holds perm on the repository (or, with an
// empty repo, on the org). Permission sets are granted to identities, and a
// derived identity can never do more than the one it was derived from: an
// identity's effective permissions are its own grants intersected with its
// parent's effective permissions. A derived identity with no grants of its
// own inherits its parent's; a root with no grants has none.
func (a *App) Can(ctx context.Context, q querier, actor Actor, org, repo, perm string) (bool, error) {
	if actor.Identity == SystemActor.Identity {
		return true, nil
	}
	id, err := ident.Parse(actor.Identity)
	if err != nil {
		return false, nil
	}
	for _, level := range id.Chain() {
		perms, has, err := a.directPerms(ctx, q, level, org, repo)
		if err != nil {
			return false, err
		}
		if has && !slices.Contains(perms, perm) {
			return false, nil
		}
		if !has && level.IsRoot() {
			return false, nil
		}
	}
	return true, nil
}

// directPerms is what one identity was granted itself on a repository or
// org: its org grant, its repository grant, and, for a service, what its
// installation in the org approved.
func (a *App) directPerms(ctx context.Context, q querier, id ident.ID, org, repo string) ([]string, bool, error) {
	var perms []string
	has := false
	var set string
	err := q.QueryRowContext(ctx, `SELECT perm_set FROM org_grants WHERE org = ? AND identity = ?`, org, id.String()).Scan(&set)
	switch {
	case err == nil:
		perms, has = append(perms, builtinSets[set]...), true
	case !errors.Is(err, sql.ErrNoRows):
		return nil, false, err
	}
	if repo != "" {
		err = q.QueryRowContext(ctx, `SELECT perm_set FROM repo_grants WHERE repo = ? AND identity = ?`, repo, id.String()).Scan(&set)
		switch {
		case err == nil:
			p, err := a.setPerms(ctx, q, repo, set)
			if err != nil {
				return nil, false, err
			}
			perms, has = append(perms, p...), true
		case !errors.Is(err, sql.ErrNoRows):
			return nil, false, err
		}
	}
	if id.Kind == ident.Service && id.IsRoot() {
		var raw string
		err = q.QueryRowContext(ctx, `SELECT perms FROM service_installs WHERE service = ? AND org = ?`, id.String(), org).Scan(&raw)
		switch {
		case err == nil:
			var p []string
			if err := json.Unmarshal([]byte(raw), &p); err != nil {
				return nil, false, err
			}
			perms, has = append(perms, p...), true
		case !errors.Is(err, sql.ErrNoRows):
			return nil, false, err
		}
	}
	return perms, has, nil
}

func (a *App) setPerms(ctx context.Context, q querier, repo, set string) ([]string, error) {
	if perms, ok := builtinSets[set]; ok {
		return perms, nil
	}
	var raw string
	err := q.QueryRowContext(ctx, `SELECT perms FROM perm_sets WHERE repo = ? AND name = ?`, repo, set).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var perms []string
	return perms, json.Unmarshal([]byte(raw), &perms)
}

func (a *App) require(ctx context.Context, q querier, actor Actor, org, repo, perm string) error {
	ok, err := a.Can(ctx, q, actor, org, repo, perm)
	if err != nil {
		return err
	}
	if !ok {
		where := org
		if repo != "" {
			where = repo
		}
		return errf(http.StatusForbidden, "%s lacks %s on %s", actor.Identity, perm, where)
	}
	return nil
}

// Allowed loads a repository and reports whether the actor holds perm on it.
func (a *App) Allowed(ctx context.Context, actor Actor, fullName, perm string) (Repo, bool, error) {
	repo, err := a.Repo(ctx, a.Store.DB(), fullName)
	if err != nil {
		return Repo{}, false, err
	}
	ok, err := a.Can(ctx, a.Store.DB(), actor, repo.Org, repo.FullName, perm)
	return repo, ok, err
}

// CanSeeOrg reports whether the actor's principal holds any grant in the
// org, or is installed there.
func (a *App) CanSeeOrg(ctx context.Context, actor Actor, org string) (bool, error) {
	if actor.Admin || actor.Identity == SystemActor.Identity {
		return true, nil
	}
	var n int
	err := a.Store.DB().QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM org_grants WHERE org = ? AND identity = ?)
		      + (SELECT COUNT(*) FROM repo_grants g JOIN repos r ON r.full_name = g.repo WHERE r.org = ? AND g.identity = ?)
		      + (SELECT COUNT(*) FROM service_installs WHERE org = ? AND service = ?)`,
		org, actor.Principal, org, actor.Principal, org, actor.Principal).Scan(&n)
	return n > 0, err
}
