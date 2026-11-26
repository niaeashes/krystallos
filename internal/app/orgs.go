package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gitsynth/internal/store"
)

// Repo is a repository and its settings.
type Repo struct {
	FullName      string    `json:"full_name"`
	Org           string    `json:"org"`
	Name          string    `json:"name"`
	Path          string    `json:"-"`
	DefaultBranch string    `json:"default_branch"`
	MergeRule     MergeRule `json:"merge_rule"`
}

func (r Repo) scope() store.Scope { return store.Scope{Org: r.Org, Repo: r.FullName} }

// MergeRule is the authorization side of merging: what the records must show
// before a change set may be merged. Whether it holds is computed from
// records, never declared.
type MergeRule struct {
	RequiredApprovals int      `json:"required_approvals"`
	RequiredChecks    []string `json:"required_checks"`
}

// CreateOrg creates an org and makes the caller its admin.
func (a *App) CreateOrg(ctx context.Context, by Actor, name string) error {
	if err := validName("org", name); err != nil {
		return err
	}
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO orgs (name) VALUES (?)`, name); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "org %q already exists", name)
			}
			return err
		}
		if _, err := tx.Exec(`INSERT INTO org_grants (org, identity, perm_set) VALUES (?, ?, 'admin')`, name, by.Identity); err != nil {
			return err
		}
		scope := store.Scope{Org: name}
		if _, err := tx.Append("org.created", scope, map[string]any{"name": name}); err != nil {
			return err
		}
		_, err := tx.Append("grant.set", scope, map[string]any{"identity": by.Identity, "set": "admin"})
		return err
	})
	return err
}

// GrantOrg gives an identity one of the built-in permission sets on every
// repository of the org.
func (a *App) GrantOrg(ctx context.Context, by Actor, org, identity, set string) error {
	if _, ok := builtinSets[set]; !ok {
		return errf(http.StatusBadRequest, "unknown permission set %q", set)
	}
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if err := a.require(ctx, tx, by, org, "", PermOrgAdmin); err != nil {
			return err
		}
		if err := a.identityExists(ctx, tx, identity); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO org_grants (org, identity, perm_set) VALUES (?, ?, ?)
			ON CONFLICT (org, identity) DO UPDATE SET perm_set = excluded.perm_set`, org, identity, set); err != nil {
			return err
		}
		_, err := tx.Append("grant.set", store.Scope{Org: org}, map[string]any{"identity": identity, "set": set})
		return err
	})
	return err
}

// CreateRepo creates a bare repository with gitsynth's hooks installed.
func (a *App) CreateRepo(ctx context.Context, by Actor, org, name string) (Repo, error) {
	if err := validName("repository", name); err != nil {
		return Repo{}, err
	}
	if err := a.require(ctx, a.Store.DB(), by, org, "", PermOrgAdmin); err != nil {
		return Repo{}, err
	}
	repo := Repo{FullName: org + "/" + name, Org: org, Name: name, DefaultBranch: "main", MergeRule: MergeRule{RequiredChecks: []string{}}}
	// The row holds only the directory's name, so the data directory can
	// move as a whole.
	dir := "r_" + randomString(10) + ".git"
	repo.Path = a.repoDir(dir)
	if _, err := a.Git.Output(ctx, "", nil, "init", "--bare", "--initial-branch="+repo.DefaultBranch, repo.Path); err != nil {
		return Repo{}, err
	}
	cleanup := func() { os.RemoveAll(repo.Path) }
	if _, err := a.installHook(repo.Path); err != nil {
		cleanup()
		return Repo{}, err
	}
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO repos (full_name, org, name, path, default_branch) VALUES (?, ?, ?, ?, ?)`,
			repo.FullName, org, name, dir, repo.DefaultBranch); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return errf(http.StatusConflict, "repository %q already exists", repo.FullName)
			}
			return err
		}
		_, err := tx.Append("repo.created", repo.scope(), map[string]any{"name": name, "default_branch": repo.DefaultBranch})
		return err
	})
	if err != nil {
		cleanup()
		return Repo{}, err
	}
	return repo, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// installHook wires the reference-transaction hook, which every ref update
// passes through: pushes, server-side merges and direct git commands alike.
// It reports whether it changed the file. A hook that already says the same,
// and may run, is left alone; otherwise the new one is written beside it and
// renamed over it, so a git running at that moment never reads half a script.
func (a *App) installHook(repoPath string) (bool, error) {
	script := []byte(fmt.Sprintf("#!/bin/sh\nexec %s hook --data %s reference-transaction \"$@\"\n",
		shellQuote(a.cfg.HookBin), shellQuote(a.cfg.DataDir)))
	path := filepath.Join(repoPath, "hooks", "reference-transaction")
	if st, err := os.Stat(path); err == nil && st.Mode().Perm()&0o100 != 0 {
		if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, script) {
			return false, nil
		}
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".reference-transaction-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(f.Name()) // nothing left to remove once renamed
	if _, err := f.Write(script); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Chmod(0o755); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return false, err
	}
	return true, nil
}

// InstallHooks points every repository's hook at the running binary and the
// data directory. A hook keeps the paths it was written with, so after the
// binary moves (an upgrade installed elsewhere, a data directory restored
// onto another machine) it could not start, and every ref update would be
// refused. serve calls this before anything can update a ref.
func (a *App) InstallHooks(ctx context.Context) error {
	if a.cfg.HookBin == "" {
		return errors.New("no gitsynth binary to point the hooks at")
	}
	repos, err := a.Repos(ctx)
	if err != nil {
		return err
	}
	rewritten := 0
	for _, repo := range repos {
		changed, err := a.installHook(repo.Path)
		if err != nil {
			return fmt.Errorf("repository %s: %w", repo.FullName, err)
		}
		if changed {
			rewritten++
		}
	}
	if rewritten > 0 {
		slog.Info("rewrote repository hooks to call this binary", "count", rewritten, "bin", a.cfg.HookBin)
	}
	return nil
}

// CheckRepos makes sure every repository's directory is in the data
// directory. serve runs it before anything touches a repository, so a
// data directory restored without one of them stops with that
// repository's name instead of failing on it later.
func (a *App) CheckRepos(ctx context.Context) error {
	repos, err := a.Repos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		if st, err := os.Stat(repo.Path); err != nil || !st.IsDir() {
			return fmt.Errorf("repository %s: its directory %s is missing", repo.FullName, repo.Path)
		}
		// git would look for a repository in the directories above one that
		// is not, and the data directory may sit inside a working tree.
		if st, err := os.Stat(filepath.Join(repo.Path, "HEAD")); err != nil || !st.Mode().IsRegular() {
			return fmt.Errorf("repository %s: %s is not a git repository", repo.FullName, repo.Path)
		}
	}
	return nil
}

// repoDir is where a repository lives: in the running server's data
// directory, under the name its row holds. Rows written before the data
// directory could move hold the absolute path it had then; only the last
// element counts, so a moved or restored copy never reaches the original.
func (a *App) repoDir(stored string) string {
	return filepath.Join(a.cfg.DataDir, "repos", filepath.Base(stored))
}

// Repo loads a repository by its full name ("org/name").
func (a *App) Repo(ctx context.Context, q querier, fullName string) (Repo, error) {
	return a.scanRepo(q.QueryRowContext(ctx,
		`SELECT full_name, org, name, path, default_branch, merge_rule FROM repos WHERE full_name = ?`, fullName))
}

// RepoByPath loads the repository at a path on disk, as a hook reports it:
// its working directory, symlinks resolved. Only a directory directly in
// the data directory's repos can be one; a path anywhere else matches
// nothing, even with a repository's name.
func (a *App) RepoByPath(ctx context.Context, path string) (Repo, error) {
	path = filepath.Clean(path)
	if filepath.Dir(path) != filepath.Join(a.cfg.DataDir, "repos") {
		return Repo{}, errf(http.StatusNotFound, "repository not found")
	}
	// The row holds the name, or an older absolute path ending in it.
	return a.scanRepo(a.Store.DB().QueryRowContext(ctx,
		`SELECT full_name, org, name, path, default_branch, merge_rule FROM repos
		 WHERE path = ?1 OR substr(path, -1 - length(?1)) = '/' || ?1`, filepath.Base(path)))
}

type rowScanner interface{ Scan(dest ...any) error }

func (a *App) scanRepo(row rowScanner) (Repo, error) {
	var r Repo
	var rule, stored string
	if err := row.Scan(&r.FullName, &r.Org, &r.Name, &stored, &r.DefaultBranch, &rule); err != nil {
		return Repo{}, notFound("repository", err)
	}
	r.Path = a.repoDir(stored)
	if err := json.Unmarshal([]byte(rule), &r.MergeRule); err != nil {
		return Repo{}, err
	}
	if r.MergeRule.RequiredChecks == nil {
		r.MergeRule.RequiredChecks = []string{}
	}
	return r, nil
}

// Repos lists every repository.
func (a *App) Repos(ctx context.Context) ([]Repo, error) {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT full_name, org, name, path, default_branch, merge_rule FROM repos ORDER BY full_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := a.scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GrantRepo gives an identity a permission set on one repository.
func (a *App) GrantRepo(ctx context.Context, by Actor, fullName, identity, set string) error {
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.Repo(ctx, tx, fullName)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermRepoAdmin); err != nil {
			return err
		}
		if err := a.identityExists(ctx, tx, identity); err != nil {
			return err
		}
		if perms, err := a.setPerms(ctx, tx, repo.FullName, set); err != nil {
			return err
		} else if perms == nil {
			return errf(http.StatusBadRequest, "unknown permission set %q", set)
		}
		if _, err := tx.Exec(`INSERT INTO repo_grants (repo, identity, perm_set) VALUES (?, ?, ?)
			ON CONFLICT (repo, identity) DO UPDATE SET perm_set = excluded.perm_set`, repo.FullName, identity, set); err != nil {
			return err
		}
		_, err = tx.Append("grant.set", repo.scope(), map[string]any{"identity": identity, "set": set})
		return err
	})
	return err
}

// DefinePermSet adds or replaces a repository's own permission set.
func (a *App) DefinePermSet(ctx context.Context, by Actor, fullName, name string, perms []string) error {
	if err := validName("permission set", name); err != nil {
		return err
	}
	if _, ok := builtinSets[name]; ok {
		return errf(http.StatusBadRequest, "%q is a built-in permission set", name)
	}
	for _, p := range perms {
		if !slices.Contains(allPerms, p) || p == PermOrgAdmin {
			return errf(http.StatusBadRequest, "unknown permission %q", p)
		}
	}
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.Repo(ctx, tx, fullName)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermRepoAdmin); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO perm_sets (repo, name, perms) VALUES (?, ?, ?)
			ON CONFLICT (repo, name) DO UPDATE SET perms = excluded.perms`, repo.FullName, name, mustJSON(perms)); err != nil {
			return err
		}
		_, err = tx.Append("perm_set.defined", repo.scope(), map[string]any{"name": name, "perms": perms})
		return err
	})
	return err
}

// SetMergeRule replaces a repository's merge rule.
func (a *App) SetMergeRule(ctx context.Context, by Actor, fullName string, rule MergeRule) error {
	if rule.RequiredApprovals < 0 {
		return errf(http.StatusBadRequest, "required_approvals must not be negative")
	}
	if rule.RequiredChecks == nil {
		rule.RequiredChecks = []string{}
	}
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.Repo(ctx, tx, fullName)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermRepoAdmin); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE repos SET merge_rule = ? WHERE full_name = ?`, mustJSON(rule), repo.FullName); err != nil {
			return err
		}
		_, err = tx.Append("repo.merge_rule_set", repo.scope(), rule)
		return err
	})
	return err
}
