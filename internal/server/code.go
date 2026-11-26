package server

import (
	"net/http"
	"strconv"

	"gitsynth/internal/app"
)

// cloneURL is where a repository is cloned from: under the server's public
// URL.
func (s *Server) cloneURL(repo app.Repo) string {
	return s.URL + "/git/" + repo.FullName + ".git"
}

// withCloneURLs adds a repository's clone URLs to m: over HTTPS always,
// and over SSH when the server serves it.
func (s *Server) withCloneURLs(m map[string]any, repo app.Repo) map[string]any {
	m["clone_url"] = s.cloneURL(repo)
	if u := s.sshCloneURL(repo); u != "" {
		m["ssh_clone_url"] = u
	}
	return m
}

func (s *Server) getRepo(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	refs, err := s.App.Refs(r.Context(), repo)
	if err != nil {
		return nil, err
	}
	canPush, err := s.App.Can(r.Context(), s.App.Store.DB(), actor, repo.Org, repo.FullName, app.PermRepoPush)
	if err != nil {
		return nil, err
	}
	canAdmin, err := s.App.Can(r.Context(), s.App.Store.DB(), actor, repo.Org, repo.FullName, app.PermRepoAdmin)
	if err != nil {
		return nil, err
	}
	noteRead(r, "code.read", map[string]any{"refs": refs})
	return s.withCloneURLs(map[string]any{
		"repo": repo, "refs": refs,
		"permissions": map[string]bool{"push": canPush, "admin": canAdmin},
	}, repo), nil
}

func (s *Server) tree(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	commit, err := s.App.ResolveCommit(r.Context(), repo, q.Get("ref"))
	if err != nil {
		return nil, err
	}
	entries, err := s.App.Tree(r.Context(), repo, commit, q.Get("path"))
	if err != nil {
		return nil, err
	}
	noteRead(r, "code.read", map[string]any{"commit": commit, "path": q.Get("path"), "what": "tree"})
	return map[string]any{"commit": commit, "path": q.Get("path"), "entries": entries}, nil
}

func (s *Server) blob(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	commit, err := s.App.ResolveCommit(r.Context(), repo, q.Get("ref"))
	if err != nil {
		return nil, err
	}
	b, err := s.App.Blob(r.Context(), repo, commit, q.Get("path"))
	if err != nil {
		return nil, err
	}
	noteRead(r, "code.read", map[string]any{"commit": commit, "path": b.Path, "what": "blob"})
	return map[string]any{"commit": commit, "blob": b}, nil
}

func (s *Server) commits(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	commit, err := s.App.ResolveCommit(r.Context(), repo, q.Get("ref"))
	if err != nil {
		return nil, err
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	log, err := s.App.Log(r.Context(), repo, commit, q.Get("path"), limit)
	if err != nil {
		return nil, err
	}
	noteRead(r, "code.read", map[string]any{"commit": commit, "path": q.Get("path"), "what": "log"})
	return map[string]any{"commit": commit, "commits": log}, nil
}

func (s *Server) listChangeSets(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	return s.App.ChangeSets(r.Context(), repo.FullName)
}

func (s *Server) changeSetDiff(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	d, err := s.App.ChangeSetDiff(r.Context(), repo.FullName, n)
	if err != nil {
		return nil, err
	}
	noteRead(r, "code.read", map[string]any{"changeset": n, "base": d.MergeBase, "head": d.Head, "what": "diff"})
	return d, nil
}

func (s *Server) listOrgs(r *http.Request, actor app.Actor) (any, error) {
	orgs, err := s.App.Orgs(r.Context(), actor)
	return map[string]any{"orgs": orgs}, err
}

func (s *Server) listRepos(r *http.Request, actor app.Actor) (any, error) {
	if err := s.requireOrg(r, actor); err != nil {
		return nil, err
	}
	return s.App.ReposOf(r.Context(), actor, r.PathValue("org"))
}

func (s *Server) myIdentities(r *http.Request, actor app.Actor) (any, error) {
	return s.App.MyIdentities(r.Context(), actor)
}

func (s *Server) myCredentials(r *http.Request, actor app.Actor) (any, error) {
	return s.App.MyCredentials(r.Context(), actor)
}

func (s *Server) revokeCredential(r *http.Request, actor app.Actor) (any, error) {
	return map[string]bool{"ok": true}, s.App.RevokeCredential(r.Context(), actor, r.PathValue("id"))
}
