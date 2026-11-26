// Package server exposes gitsynth over HTTP: the public API, Git's smart
// HTTP protocol, the event stream, and the internal endpoint hooks call.
package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"gitsynth/internal/accesslog"
	"gitsynth/internal/app"
)

// Server serves the public endpoints.
type Server struct {
	App    *app.App
	Access *accesslog.Log
	// Web is the built web UI, served on every path the API, git and MCP
	// do not claim. Nil serves no UI.
	Web fs.FS
	// URL is the server's public address, scheme://host[:port]: where
	// people open the web UI and where git and MCP clients connect. Clone
	// URLs and links start from it, and the session cookie is Secure when
	// it is https. Nothing about the address is taken from a request's
	// Host or X-Forwarded-* headers, which any client can set.
	URL string
	// Origins are other origins the web UI may be opened from, such as a
	// dev server. A link made from a page at one of them points back there.
	Origins []string
	// MaxGit is how many git processes may serve clients at once; 0 means
	// no limit. A clone, fetch or push beyond it waits for one to finish.
	// The server's own git work (merges, reads for the API) is not counted.
	MaxGit int

	gitOnce  sync.Once
	gitSlots chan struct{}

	// mcpSess holds the MCP sessions of identities' endpoints.
	mcpSess mcpSessions
}

type apiFunc func(r *http.Request, actor app.Actor) (any, error)

// Handler returns the public HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	mux.HandleFunc("GET /git/{org}/{repo}/info/refs", s.gitInfoRefs)
	mux.HandleFunc("POST /git/{org}/{repo}/git-upload-pack", s.gitUploadPack)
	mux.HandleFunc("POST /git/{org}/{repo}/git-receive-pack", s.gitReceivePack)

	mux.HandleFunc("GET /api/events", s.events)
	// The old endpoint, for static tokens; deprecated.
	mux.Handle("/mcp", s.mcpHandler())
	// Each identity's own endpoint, /mcp/<kind>/<root>/<label>...
	mux.Handle("/mcp/", s.identityMCPHandler())

	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp/", s.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.authServerMetadata)
	mux.HandleFunc("POST /oauth/register", s.registerClient)
	mux.HandleFunc("GET /oauth/authorize", s.authorize)
	mux.HandleFunc("GET /oauth/return/{id}", s.oauthReturn)
	mux.HandleFunc("POST /oauth/token", s.token)

	mux.HandleFunc("POST /api/auth/register/begin", s.registerBegin)
	mux.HandleFunc("POST /api/auth/register/finish", s.registerFinish)
	mux.HandleFunc("POST /api/auth/login/begin", s.loginBegin)
	mux.HandleFunc("POST /api/auth/login/finish", s.loginFinish)
	mux.HandleFunc("POST /api/auth/link", s.loginLink)
	mux.HandleFunc("POST /api/auth/link/inspect", s.inspectLink)
	mux.HandleFunc("POST /api/auth/logout", s.logout)

	api := func(pattern string, fn apiFunc) { mux.Handle(pattern, s.api(fn)) }
	api("GET /api/me", s.me)
	api("GET /api/server", s.serverInfo)
	api("GET /api/identities", s.myIdentities)
	api("POST /api/identities", s.deriveIdentity)
	api("POST /api/identities/disable", s.disableIdentity)
	api("POST /api/identities/enable", s.enableIdentity)
	api("POST /api/sessions", s.openSession)
	api("GET /api/oauth/requests/{id}", s.authRequest)
	api("POST /api/oauth/requests/{id}/approve", s.approveAuth)
	api("POST /api/oauth/requests/{id}/deny", s.denyAuth)
	api("GET /api/oauth/grants", s.listGrants)
	api("POST /api/oauth/grants/{id}/revoke", s.revokeGrant)
	api("GET /api/emails", s.listEmails)
	api("POST /api/emails", s.addEmail)
	api("POST /api/emails/remove", s.removeEmail)
	api("GET /api/bots", s.listBots)
	api("POST /api/bots", s.createBot)
	api("GET /api/services", s.listServices)
	api("POST /api/services", s.registerService)
	api("POST /api/invitations", s.invite)
	api("GET /api/tokens", s.myCredentials)
	api("POST /api/tokens", s.issueToken)
	api("POST /api/tokens/{id}/revoke", s.revokeCredential)
	api("GET /api/orgs", s.listOrgs)
	api("POST /api/orgs", s.createOrg)
	api("POST /api/orgs/{org}/grants", s.grantOrg)
	api("POST /api/orgs/{org}/services", s.installService)
	api("GET /api/orgs/{org}/repos", s.listRepos)
	api("POST /api/orgs/{org}/repos", s.createRepo)
	api("GET /api/repos/{org}/{repo}", s.getRepo)
	api("GET /api/repos/{org}/{repo}/tree", s.tree)
	api("GET /api/repos/{org}/{repo}/blob", s.blob)
	api("GET /api/repos/{org}/{repo}/commits", s.commits)
	api("GET /api/repos/{org}/{repo}/changesets", s.listChangeSets)
	api("GET /api/repos/{org}/{repo}/changesets/{n}/diff", s.changeSetDiff)
	api("POST /api/orgs/{org}/ideas", s.createIdea)
	api("GET /api/orgs/{org}/ideas", s.listIdeas)
	api("GET /api/orgs/{org}/ideas/{n}", s.getIdea)
	api("POST /api/orgs/{org}/ideas/{n}/convert", s.convertIdea)
	api("POST /api/repos/{org}/{repo}/grants", s.grantRepo)
	api("POST /api/repos/{org}/{repo}/perm-sets", s.definePermSet)
	api("PUT /api/repos/{org}/{repo}/merge-rule", s.setMergeRule)
	api("POST /api/repos/{org}/{repo}/issues", s.createIssue)
	api("GET /api/repos/{org}/{repo}/issues", s.listIssues)
	api("GET /api/repos/{org}/{repo}/issues/{n}", s.getIssue)
	api("POST /api/repos/{org}/{repo}/issues/{n}/comments", s.commentIssue)
	api("POST /api/repos/{org}/{repo}/issues/{n}/close", s.closeIssue)
	api("POST /api/repos/{org}/{repo}/issues/{n}/plans", s.createPlan)
	api("GET /api/repos/{org}/{repo}/plans/{n}", s.getPlan)
	api("POST /api/repos/{org}/{repo}/plans/{n}/revisions", s.revisePlan)
	api("POST /api/repos/{org}/{repo}/issues/{n}/changesets", s.createChangeSet)
	api("GET /api/repos/{org}/{repo}/changesets/{n}", s.getChangeSet)
	api("POST /api/repos/{org}/{repo}/changesets/{n}/reviews", s.review)
	api("POST /api/repos/{org}/{repo}/changesets/{n}/merge", s.merge)
	api("POST /api/repos/{org}/{repo}/checks", s.reportCheck)

	if s.Web != nil {
		mux.Handle("/", webUI(s.Web))
	}
	return capBodies(mux)
}

// maxBody caps every request body but git's: API JSON, passkey responses,
// MCP messages, and what hooks report. Git's own routes stream packs of
// any size; upload-pack has a cap of its own.
const maxBody = 4 << 20

// capBodies holds request bodies outside /git/ to maxBody. A body that says
// it is larger is refused before anything reads it; one that does not say
// stops at the cap, and the handler reading it answers 413.
func capBodies(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/git/") {
			if r.ContentLength > maxBody {
				writeError(w, &http.MaxBytesError{Limit: maxBody})
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// InternalHandler serves the endpoint repository hooks call. It listens on
// loopback only and requires the per-process secret.
func (s *Server) InternalHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/ref-txn", func(w http.ResponseWriter, r *http.Request) {
		got, want := []byte(r.Header.Get("X-Gitsynth-Internal")), []byte(s.App.InternalSecret())
		if len(want) == 0 || subtle.ConstantTimeCompare(got, want) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var req app.RefTxnRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, badBody("invalid JSON body: ", err))
			return
		}
		if err := s.App.HandleRefTxn(r.Context(), req); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	return capBodies(mux)
}

const sessionCookie = "gitsynth_session"

// authenticate resolves the caller. A bearer token is the normal form; git
// sends the token as the basic-auth password; a signed-in browser sends the
// session cookie. The credential decides both the identity and the session:
// a session's credential carries its session, and any other credential
// carries none (or the label it was issued with, before sessions were
// records). A caller never declares a session of its own. A credential
// bound to an audience (an OAuth access token, a git credential) is refused
// here; authenticateFor accepts it where it belongs.
func (s *Server) authenticate(r *http.Request) (app.Actor, error) {
	return s.authenticateFor(r, "")
}

// authenticateFor is authenticate at a place with an audience: an MCP
// endpoint's URL, or app.AudienceGit.
func (s *Server) authenticateFor(r *http.Request, audience string) (app.Actor, error) {
	var token string
	viaCookie := false
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	} else if _, pw, ok := r.BasicAuth(); ok {
		token = pw
	} else if c, err := r.Cookie(sessionCookie); err == nil {
		token, viaCookie = c.Value, true
	}
	if token == "" {
		return app.Actor{}, &app.Error{Code: http.StatusUnauthorized, Msg: "authentication required"}
	}
	// A cookie rides along on any request the browser makes, so a request
	// that changes something must also carry a header that another site's
	// page cannot set without a CORS preflight we never grant.
	if viaCookie && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("X-Gitsynth-CSRF") != "1" {
		return app.Actor{}, &app.Error{Code: http.StatusForbidden, Msg: "request refused: missing X-Gitsynth-CSRF header"}
	}
	return s.App.AuthenticateFor(r.Context(), token, audience)
}

type readInfoKey struct{}

// readInfo lets a handler say what exactly it read (a commit, a path), so
// the access log holds the version that was seen and not only the URL.
type readInfo struct {
	kind   string
	detail any
}

func noteRead(r *http.Request, kind string, detail any) {
	if ri, ok := r.Context().Value(readInfoKey{}).(*readInfo); ok {
		ri.kind, ri.detail = kind, detail
	}
}

func (s *Server) api(fn apiFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		actor, err := s.authenticate(r)
		if err != nil {
			writeError(w, err)
			return
		}
		ri := &readInfo{kind: "api.read"}
		r = r.WithContext(context.WithValue(r.Context(), readInfoKey{}, ri))
		out, err := fn(r, actor)
		if err != nil {
			writeError(w, err)
			return
		}
		if r.Method == http.MethodGet {
			s.recordRead(actor, ri.kind, repoFromPath(r), r.URL.RequestURI(), ri.detail)
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func (s *Server) recordRead(actor app.Actor, kind, repo, target string, detail any) {
	s.Access.Record(accesslog.Entry{
		Identity: actor.Identity, Principal: actor.Principal, Session: actor.Session,
		Kind: kind, Repo: repo, Target: target, Detail: detail,
	})
}

func repoFromPath(r *http.Request) string {
	org, repo := r.PathValue("org"), r.PathValue("repo")
	if org == "" || repo == "" {
		return ""
	}
	return org + "/" + strings.TrimSuffix(repo, ".git")
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var ae *app.Error
	if errors.As(err, &ae) {
		writeJSON(w, ae.Code, map[string]string{"error": ae.Msg})
		return
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("request body is larger than %d MiB", tooBig.Limit>>20),
		})
		return
	}
	slog.Error("request failed", "err", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}

func decode[T any](r *http.Request) (T, error) {
	var v T
	if r.ContentLength == 0 {
		return v, nil
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, badBody("invalid JSON body: ", err)
	}
	return v, nil
}

// badBody is the answer to a body that could not be read or parsed: 413
// when it ran past maxBody, 400 with the reason otherwise.
func badBody(prefix string, err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return err
	}
	return &app.Error{Code: http.StatusBadRequest, Msg: prefix + err.Error()}
}

// readBody reads a whole body for a parser that would hide why reading it
// failed, so that one past maxBody still answers 413.
func readBody(r *http.Request) (io.Reader, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, badBody("unreadable body: ", err)
	}
	return bytes.NewReader(b), nil
}

func pathInt(r *http.Request, name string) (int, error) {
	n, err := strconv.Atoi(r.PathValue(name))
	if err != nil || n < 1 {
		return 0, &app.Error{Code: http.StatusNotFound, Msg: "not found"}
	}
	return n, nil
}

func requestBody(r *http.Request) (io.Reader, error) {
	if r.Header.Get("Content-Encoding") == "gzip" {
		return gzip.NewReader(r.Body)
	}
	return r.Body, nil
}
