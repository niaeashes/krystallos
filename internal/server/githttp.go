package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"gitsynth/internal/app"
	"gitsynth/internal/gitx"
)

// gitRepo authenticates a git client and checks perm. Reads of a repository
// the caller may not see answer "not found".
func (s *Server) gitRepo(w http.ResponseWriter, r *http.Request, perm string) (app.Repo, app.Actor, bool) {
	w.Header().Set("Cache-Control", "private, no-store")
	actor, err := s.authenticateFor(r, app.AudienceGit)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="gitsynth"`)
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return app.Repo{}, app.Actor{}, false
	}
	repo, ok, err := s.App.Allowed(r.Context(), actor, repoFromPath(r), perm)
	var ae *app.Error
	switch {
	case errors.As(err, &ae):
		http.Error(w, ae.Msg, ae.Code)
		return app.Repo{}, app.Actor{}, false
	case err != nil:
		slog.Error("git auth failed", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return app.Repo{}, app.Actor{}, false
	case !ok && perm == app.PermRepoRead:
		http.Error(w, "repository not found", http.StatusNotFound)
		return app.Repo{}, app.Actor{}, false
	case !ok:
		http.Error(w, actor.Identity+" lacks "+perm+" on "+repo.FullName, http.StatusForbidden)
		return app.Repo{}, app.Actor{}, false
	}
	return repo, actor, true
}

// gitRetryAfter is what a client turned away for want of a git slot is told
// to wait, in seconds.
const gitRetryAfter = "10"

// acquireGit takes one of MaxGit slots for a git process serving a client,
// waiting while all are taken. If the request ends first it answers 503 and
// returns false. The caller calls release once git has exited. It must run
// before anything is written, so the 503 can still be sent.
func (s *Server) acquireGit(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	release, ok = s.gitSlot(r.Context())
	if !ok {
		w.Header().Set("Retry-After", gitRetryAfter)
		http.Error(w, "the server is busy serving other git clients; try again shortly", http.StatusServiceUnavailable)
	}
	return release, ok
}

// gitSlot takes one of MaxGit slots, waiting while all are taken, for git
// served over HTTP or SSH alike. It returns false if ctx ends first.
func (s *Server) gitSlot(ctx context.Context) (release func(), ok bool) {
	s.gitOnce.Do(func() {
		if s.MaxGit > 0 {
			s.gitSlots = make(chan struct{}, s.MaxGit)
		}
	})
	if s.gitSlots == nil {
		return func() {}, true
	}
	select {
	case s.gitSlots <- struct{}{}:
		return func() { <-s.gitSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func protocolV2(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
}

func protocolEnv(r *http.Request) []string {
	if p := r.Header.Get("Git-Protocol"); p != "" {
		return []string{"GIT_PROTOCOL=" + p}
	}
	return nil
}

// maxUploadRequest caps the fetch request upload-pack's handler reads into
// memory before starting git. A v2 fetch request is wants and haves, sent
// in rounds; 64 MiB is far beyond any real one, and with --max-git it keeps
// what concurrent fetches can hold bounded. It applies after gzip.
const maxUploadRequest = 64 << 20

const v2Required = "gitsynth needs git protocol v2 for fetches (git 2.26 or later), so that every read can be recorded"

func (s *Server) gitInfoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	var perm string
	switch service {
	case "git-upload-pack":
		perm = app.PermRepoRead
	case "git-receive-pack":
		perm = app.PermRepoPush
	default:
		http.Error(w, "only the smart HTTP protocol is supported", http.StatusForbidden)
		return
	}
	repo, actor, ok := s.gitRepo(w, r, perm)
	if !ok {
		return
	}
	v2 := protocolV2(r)
	if service == "git-upload-pack" && !v2 {
		http.Error(w, v2Required, http.StatusBadRequest)
		return
	}
	// Before the v0 preamble below: once it is written, a 503 cannot be.
	release, ok := s.acquireGit(w, r)
	if !ok {
		return
	}
	defer release()
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	if !v2 {
		w.Write(gitx.PktLine("# service=" + service + "\n"))
		io.WriteString(w, "0000")
	}
	if service == "git-receive-pack" {
		// The ref advertisement before a push shows the caller every ref.
		s.recordRead(actor, "git.advertise", repo.FullName, "refs", nil)
	}
	s.runGit(w, r, repo, protocolEnv(r), nil, strings.TrimPrefix(service, "git-"), "--stateless-rpc", "--http-backend-info-refs", ".")
}

func (s *Server) gitUploadPack(w http.ResponseWriter, r *http.Request) {
	repo, actor, ok := s.gitRepo(w, r, app.PermRepoRead)
	if !ok {
		return
	}
	if !protocolV2(r) {
		http.Error(w, v2Required, http.StatusBadRequest)
		return
	}
	// The request is held in memory under the slot, so the slots bound it.
	release, ok := s.acquireGit(w, r)
	if !ok {
		return
	}
	defer release()
	body, err := requestBody(r)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	data, err := io.ReadAll(io.LimitReader(body, maxUploadRequest+1))
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	if len(data) > maxUploadRequest {
		http.Error(w, "fetch request too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Record what was asked for before serving it: the commits a fetch
	// wants are exactly the versions of the code this identity obtains.
	req, perr := gitx.ParseUploadRequest(data)
	var detail any = req
	if perr != nil {
		detail = map[string]any{"request": req, "parse_error": perr.Error()}
	}
	s.recordRead(actor, "git."+req.Command, repo.FullName, req.Command, detail)
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	s.runGit(w, r, repo, protocolEnv(r), bytes.NewReader(data), "upload-pack", "--stateless-rpc", ".")
}

func (s *Server) gitReceivePack(w http.ResponseWriter, r *http.Request) {
	repo, actor, ok := s.gitRepo(w, r, app.PermRepoPush)
	if !ok {
		return
	}
	release, ok := s.acquireGit(w, r)
	if !ok {
		return
	}
	defer release()
	body, err := requestBody(r)
	if err != nil {
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}
	// The hook finds out who is pushing through this token, not through
	// anything the client says.
	token, done := s.App.NewOp(app.Op{Actor: actor.Actor, Repo: repo.FullName, Source: "push"})
	defer done()
	env := append(protocolEnv(r), s.App.HookEnv(token)...)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	s.runGit(w, r, repo, env, body, "receive-pack", "--stateless-rpc", ".")
}

func (s *Server) runGit(w http.ResponseWriter, r *http.Request, repo app.Repo, env []string, stdin io.Reader, args ...string) {
	cmd := s.App.Git.Cmd(r.Context(), repo.Path, env, args...)
	cmd.Stdin = stdin
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		slog.Error("git failed", "repo", repo.FullName, "args", args, "err", err, "stderr", strings.TrimSpace(stderr.String()))
	}
}
