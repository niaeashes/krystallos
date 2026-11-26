package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitsynth/internal/app"
	"gitsynth/internal/ident"
)

// MCP is the entry point for agents. Each identity has an endpoint of its
// own, /mcp/<kind>/<root>/<label>..., which serves only a credential of
// exactly that identity: an OAuth access token whose audience is the
// endpoint's URL, or a static token or session credential of the identity.
//
// An identity's endpoint keeps state. Each initialize opens a gitsynth
// session of the identity (app.OpenMCPSession), and every later request of
// that MCP session acts as it, so what the client does is recorded under
// one session however often its access token is refreshed. A run sharing
// the connection, such as a subagent, opens a child session with
// open_session and names it in the session argument any tool takes. The
// MCP session ends, and its gitsynth session closes, when the client ends
// it, when it sits idle too long, or when its grant or token stops working.
//
// MCP's revision 2026-07-28 drops sessions (and initialize), so these
// endpoints speak the revisions up to 2025-11-25; a newer client falls back
// to initialize when server/discover fails, as the spec has it.
//
// The old /mcp stays for static tokens: it is stateless, the token decides
// the identity, and it offers no OAuth discovery. It is deprecated.

// mcpIdle is how long an MCP session may sit idle before it ends.
const mcpIdle = 24 * time.Hour

type (
	mcpBindingKey struct{}
	mcpActorKey   struct{}
)

// mcpBinding is one MCP session at an identity's endpoint.
type mcpBinding struct {
	id       string // the Mcp-Session-Id
	identity string
	// anchor is what every request of the MCP session must come with: the
	// grant its access tokens belong to, or the one credential it used.
	anchor string
	// actor acts as the gitsynth session the MCP session opened.
	actor  app.Actor
	server *mcp.Server

	mu    sync.Mutex
	ss    *mcp.ServerSession
	ended sync.Once
}

func (b *mcpBinding) started() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ss != nil
}

// mcpSessions holds the MCP sessions of identities' endpoints.
type mcpSessions struct {
	mu sync.Mutex
	m  map[string]*mcpBinding
}

func (s *Server) binding(id string) *mcpBinding {
	s.mcpSess.mu.Lock()
	defer s.mcpSess.mu.Unlock()
	return s.mcpSess.m[id]
}

func (s *Server) addBinding(b *mcpBinding) {
	s.mcpSess.mu.Lock()
	defer s.mcpSess.mu.Unlock()
	if s.mcpSess.m == nil {
		s.mcpSess.m = map[string]*mcpBinding{}
	}
	s.mcpSess.m[b.id] = b
}

// endBinding forgets an MCP session and closes its gitsynth session, once.
func (s *Server) endBinding(b *mcpBinding, reason string) {
	b.ended.Do(func() {
		s.mcpSess.mu.Lock()
		delete(s.mcpSess.m, b.id)
		s.mcpSess.mu.Unlock()
		b.mu.Lock()
		ss := b.ss
		b.mu.Unlock()
		if ss != nil {
			ss.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.App.CloseSession(ctx, b.actor, reason); err != nil {
			slog.Error("closing an MCP session's session failed", "session", b.actor.Session, "err", err)
		}
	})
}

// mcpVersions are the protocol revisions an identity's endpoint speaks: the
// ones with MCP sessions.
func mcpVersions() []string {
	var out []string
	for _, v := range mcp.SupportedProtocolVersions() {
		if v < "2026-07-28" {
			out = append(out, v)
		}
	}
	return out
}

// mcpIdentity parses an identity endpoint's path, /mcp/user/alice/claude,
// into the identity it names.
func mcpIdentity(path string) (ident.ID, bool) {
	rest, ok := strings.CutPrefix(path, "/mcp/")
	if !ok {
		return ident.ID{}, false
	}
	kind, name, ok := strings.Cut(rest, "/")
	if !ok {
		return ident.ID{}, false
	}
	switch ident.Kind(kind) {
	case ident.User, ident.Bot, ident.Service:
	default:
		return ident.ID{}, false
	}
	id, err := ident.Parse(kind + ":" + name)
	if err != nil || id.Host != "" || mcpPath(id) != path {
		return ident.ID{}, false
	}
	return id, true
}

// mcpPath is the path of an identity's endpoint.
func mcpPath(id ident.ID) string {
	return "/mcp/" + strings.Replace(id.String(), ":", "/", 1)
}

// mcpResource is the URL of an identity's endpoint: the audience of the
// access tokens issued for it.
func (s *Server) mcpResource(id ident.ID) string {
	return s.URL + mcpPath(id)
}

// mcpUnauthorized answers a request at an identity's endpoint that brought
// no usable credential, pointing the client at the endpoint's protected
// resource metadata so it can start OAuth.
func (s *Server) mcpUnauthorized(w http.ResponseWriter, r *http.Request, id ident.ID, err error) {
	ae, ok := err.(*app.Error)
	if !ok || ae.Code >= 500 {
		writeError(w, err)
		return
	}
	h := fmt.Sprintf(`Bearer resource_metadata=%q`, s.URL+"/.well-known/oauth-protected-resource"+mcpPath(id))
	if r.Header.Get("Authorization") != "" {
		h += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", h)
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": ae.Msg})
}

// identityMCPHandler serves every identity's endpoint.
func (s *Server) identityMCPHandler() http.Handler {
	cache := mcp.NewSchemaCache()
	// A request that is neither initialize nor part of an MCP session gets
	// a server with no tools: server/discover from a newer client fails
	// there, which sends it to initialize.
	unbound := mcp.NewServer(&mcp.Implementation{Name: "gitsynth", Version: "0.1.0"}, &mcp.ServerOptions{
		SupportedProtocolVersions: mcpVersions(),
		GetSessionID:              func() string { return "" },
	})
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		if b, ok := r.Context().Value(mcpBindingKey{}).(*mcpBinding); ok {
			return b.server
		}
		return unbound
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:   true,
		SessionTimeout: mcpIdle,
		// Every request carries a bearer credential, which a page that
		// rebinds DNS to this server cannot have; and behind a reverse
		// proxy on the same machine every request arrives on loopback with
		// the public Host, which this check would refuse.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        maxBody,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		id, ok := mcpIdentity(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		actor, err := s.mcpAuthenticate(r, s.mcpResource(id))
		if err == nil && actor.Identity != id.String() {
			err = &app.Error{Code: http.StatusUnauthorized, Msg: "this credential is " + actor.Identity + "'s, not " + id.String() + "'s"}
		}
		if err != nil {
			s.mcpUnauthorized(w, r, id, err)
			return
		}
		anchor := "credential:" + actor.Credential.ID
		if actor.Credential.Grant != "" {
			anchor = "grant:" + actor.Credential.Grant
		}
		ctx := r.Context()

		if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
			b := s.binding(sid)
			if b == nil || b.identity != actor.Identity || b.anchor != anchor {
				http.Error(w, "session not found", http.StatusNotFound)
				return
			}
			if err := s.App.SessionAlive(ctx, b.actor); err != nil {
				s.endBinding(b, "its session stopped working")
				http.Error(w, "session not found: "+err.Error(), http.StatusNotFound)
				return
			}
			h.ServeHTTP(w, r.WithContext(context.WithValue(ctx, mcpBindingKey{}, b)))
			return
		}

		if r.Method == http.MethodPost {
			label, isInit, err := peekInitialize(r)
			if err != nil {
				writeError(w, badBody("unreadable body: ", err))
				return
			}
			if isInit {
				opened, err := s.App.OpenMCPSession(ctx, actor, label)
				if err != nil {
					writeError(w, err)
					return
				}
				b := &mcpBinding{id: "mcs_" + randomID(), identity: actor.Identity, anchor: anchor, actor: opened.Actor}
				b.server = s.newMCPServer(mcpScope{root: b.actor, stateful: true}, cache, b)
				s.addBinding(b)
				h.ServeHTTP(w, r.WithContext(context.WithValue(ctx, mcpBindingKey{}, b)))
				if !b.started() {
					s.endBinding(b, "the MCP session did not initialize")
				}
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// mcpAuthenticate authenticates an MCP request. It takes the credential
// from the Authorization header only: a signed-in browser's cookie never
// reaches MCP.
func (s *Server) mcpAuthenticate(r *http.Request, audience string) (app.Actor, error) {
	if r.Header.Get("Authorization") == "" {
		return app.Actor{}, &app.Error{Code: http.StatusUnauthorized, Msg: "authentication required"}
	}
	return s.authenticateFor(r, audience)
}

// peekInitialize reads a POST body, leaving it in place for the handler,
// and says whether it is an initialize request, with a label for the
// session from the client's name and version.
func peekInitialize(r *http.Request) (label string, ok bool, err error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", false, err
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	var msg struct {
		Method string `json:"method"`
		Params struct {
			ClientInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"clientInfo"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &msg) != nil || msg.Method != "initialize" {
		return "", false, nil
	}
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(msg.Params.ClientInfo.Name+" "+msg.Params.ClientInfo.Version))
	for len(label) > 200 {
		_, size := utf8.DecodeLastRuneInString(label)
		label = label[:len(label)-size]
	}
	return label, true, nil
}

// mcpHandler serves the old /mcp: stateless, for static tokens, with the
// identity taken from the token.
func (s *Server) mcpHandler() http.Handler {
	cache := mcp.NewSchemaCache()
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		actor, ok := r.Context().Value(mcpActorKey{}).(app.Actor)
		if !ok {
			return nil
		}
		return s.newMCPServer(mcpScope{root: actor}, cache, nil)
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, DisableLocalhostProtection: true, MaxRequestBodyBytes: maxBody})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		actor, err := s.mcpAuthenticate(r, "")
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gitsynth"`)
			writeError(w, err)
			return
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpActorKey{}, actor)))
	})
}

// mcpScope is whom an MCP server's tools act for.
type mcpScope struct {
	// root is the actor a tool acts as when no session is named: an MCP
	// session's session at an identity's endpoint, the token's actor at the
	// old /mcp.
	root app.Actor
	// stateful is true at an identity's endpoint.
	stateful bool
}

// actor resolves the session a call names: root's own, or one opened
// somewhere below it.
func (sc mcpScope) actor(ctx context.Context, a *app.App, session string) (app.Actor, error) {
	if session == "" || session == sc.root.Session {
		return sc.root, nil
	}
	return a.DescendantSession(ctx, sc.root, session)
}

// inputSchemas holds each tool input type's schema with the session
// argument added, one pointer per type so the SDK's schema cache keeps it.
var inputSchemas sync.Map

func inputSchema[In any]() *jsonschema.Schema {
	t := reflect.TypeFor[In]()
	if v, ok := inputSchemas.Load(t); ok {
		return v.(*jsonschema.Schema)
	}
	sch, err := jsonschema.For[In](nil)
	if err != nil {
		panic(err)
	}
	if sch.Properties == nil {
		sch.Properties = map[string]*jsonschema.Schema{}
	}
	sch.Properties["session"] = &jsonschema.Schema{
		Type:        "string",
		Description: "act as this session: one you opened with open_session (or one opened below it); your connection's own session if empty",
	}
	v, _ := inputSchemas.LoadOrStore(t, sch)
	return v.(*jsonschema.Schema)
}

// tool registers a tool whose result is returned as structured content. It
// takes the optional session argument every tool has, and runs fn as the
// session it names.
func tool[In any](a *app.App, sc mcpScope, srv *mcp.Server, name, description string, fn func(ctx context.Context, actor app.Actor, in In) (any, error)) {
	mcp.AddTool(srv, &mcp.Tool{Name: name, Description: description, InputSchema: inputSchema[In]()},
		func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
			var pick struct {
				Session string `json:"session"`
			}
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &pick); err != nil {
					return nil, nil, err
				}
			}
			actor, err := sc.actor(ctx, a, pick.Session)
			if err != nil {
				return nil, nil, err
			}
			out, err := fn(ctx, actor, in)
			if err != nil {
				return nil, nil, err
			}
			return nil, out, nil
		})
}

type repoArg struct {
	Repo string `json:"repo" jsonschema:"the repository, as org/name"`
}

type numberArg struct {
	Repo   string `json:"repo" jsonschema:"the repository, as org/name"`
	Number int    `json:"number" jsonschema:"the issue or change set number"`
}

type codeArg struct {
	Repo string `json:"repo" jsonschema:"the repository, as org/name"`
	Ref  string `json:"ref,omitempty" jsonschema:"branch, tag or commit; the default branch if empty"`
	Path string `json:"path,omitempty" jsonschema:"path inside the repository; the root if empty"`
}

// newMCPServer builds the MCP server for a scope. b is the MCP session it
// serves at an identity's endpoint, nil at the old /mcp.
func (s *Server) newMCPServer(sc mcpScope, cache *mcp.SchemaCache, b *mcpBinding) *mcp.Server {
	opts := &mcp.ServerOptions{SchemaCache: cache}
	root := sc.root
	if sc.stateful {
		opts.SupportedProtocolVersions = mcpVersions()
		opts.GetSessionID = func() string { return b.id }
		opts.Instructions = "gitsynth is a Git forge where every action is signed by an identity. You act as " + root.Identity +
			", in session " + root.Session + ", which this connection opened. Work moves from idea to issue to plan to change set." +
			" For git, get a short-lived credential with git_credential and commit with GIT_COMMITTER_EMAIL set to the committer_email it gives, or pushes are refused." +
			" A separate run sharing this connection, such as a subagent, opens its own session with open_session and passes its id as the session argument on every call, git_credential included."
	} else {
		opts.Instructions = "gitsynth is a Git forge where every action is signed by an identity. You act as " + root.Identity +
			". Work moves from idea to issue to plan to change set. Commit with GIT_COMMITTER_EMAIL set to your commit_email (see whoami), or pushes are refused. Give a separate run, such as a subagent, its own session with open_session." +
			" This endpoint is deprecated: connect to " + s.URL + mcpPath(root.ID()) + " instead."
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "gitsynth", Version: "0.1.0"}, opts)
	if b != nil {
		// Once initialize succeeds the MCP session exists; when it closes
		// (the client ends it, or it sits idle), its session closes too.
		srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				res, err := next(ctx, method, req)
				if method == "initialize" && err == nil {
					if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
						b.mu.Lock()
						first := b.ss == nil
						if first {
							b.ss = ss
						}
						b.mu.Unlock()
						if first {
							go func() {
								ss.Wait()
								s.endBinding(b, "the MCP session ended")
							}()
						}
					}
				}
				return res, err
			}
		})
	}
	a := s.App

	readable := func(ctx context.Context, actor app.Actor, repo string) (app.Repo, error) {
		rp, ok, err := a.Allowed(ctx, actor, repo, app.PermRepoRead)
		if err != nil {
			return app.Repo{}, err
		}
		if !ok {
			return app.Repo{}, &app.Error{Code: http.StatusNotFound, Msg: "repository not found"}
		}
		return rp, nil
	}
	read := func(actor app.Actor, tool, repo string, detail any) {
		s.recordRead(actor, "mcp.read", repo, tool, detail)
	}

	tool(a, sc, srv, "whoami", "Your identity, session, and the address to commit with.", func(ctx context.Context, actor app.Actor, _ struct{}) (any, error) {
		host, err := a.Host(ctx)
		out := map[string]any{
			"identity": actor.Identity, "principal": actor.Principal, "kind": actor.Kind, "session": actor.Session,
			"commit_email": a.CommitEmail(ctx, actor.Identity), "host": host,
		}
		if sc.stateful {
			out["connection_session"] = root.Session
		}
		return out, err
	})

	tool(a, sc, srv, "list_orgs", "Orgs you belong to.", func(ctx context.Context, actor app.Actor, _ struct{}) (any, error) {
		orgs, err := a.Orgs(ctx, actor)
		read(actor, "list_orgs", "", nil)
		return map[string]any{"orgs": orgs}, err
	})

	type orgArg struct {
		Org string `json:"org"`
	}
	tool(a, sc, srv, "list_repos", "Repositories in an org that you can read, with their clone URLs.", func(ctx context.Context, actor app.Actor, in orgArg) (any, error) {
		repos, err := a.ReposOf(ctx, actor, in.Org)
		if err != nil {
			return nil, err
		}
		out := []map[string]any{}
		for _, rp := range repos {
			out = append(out, map[string]any{"repo": rp, "clone_url": s.cloneURL(rp)})
		}
		read(actor, "list_repos", "", map[string]string{"org": in.Org})
		return map[string]any{"repos": out}, nil
	})

	type createRepoArg struct {
		Org  string `json:"org"`
		Name string `json:"name" jsonschema:"repository name: lowercase letters, digits, '.', '_' and '-'"`
	}
	tool(a, sc, srv, "create_repo", "Create a repository in an org (needs org admin).", func(ctx context.Context, actor app.Actor, in createRepoArg) (any, error) {
		rp, err := a.CreateRepo(ctx, actor, in.Org, in.Name)
		if err != nil {
			return nil, err
		}
		return map[string]any{"repo": rp, "clone_url": s.cloneURL(rp)}, nil
	})

	tool(a, sc, srv, "list_issues", "Issues of a repository.", func(ctx context.Context, actor app.Actor, in repoArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		issues, err := a.Issues(ctx, rp.FullName)
		read(actor, "list_issues", rp.FullName, nil)
		return map[string]any{"issues": issues}, err
	})

	tool(a, sc, srv, "get_issue", "An issue with its discussion, plans (and how far each step got) and change sets.", func(ctx context.Context, actor app.Actor, in numberArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		read(actor, "get_issue", rp.FullName, map[string]int{"issue": in.Number})
		return a.Issue(ctx, rp.FullName, in.Number)
	})

	type createIssueArg struct {
		Repo  string `json:"repo" jsonschema:"the repository, as org/name"`
		Title string `json:"title"`
		Body  string `json:"body,omitempty" jsonschema:"what should be done, and why"`
	}
	tool(a, sc, srv, "create_issue", "Open an issue.", func(ctx context.Context, actor app.Actor, in createIssueArg) (any, error) {
		n, err := a.CreateIssue(ctx, actor, in.Repo, in.Title, in.Body)
		return map[string]int{"number": n}, err
	})

	type commentArg struct {
		Repo   string `json:"repo" jsonschema:"the repository, as org/name"`
		Number int    `json:"number" jsonschema:"the issue number"`
		Body   string `json:"body"`
	}
	tool(a, sc, srv, "comment_issue", "Add to an issue's discussion.", func(ctx context.Context, actor app.Actor, in commentArg) (any, error) {
		return map[string]bool{"ok": true}, a.CommentIssue(ctx, actor, in.Repo, in.Number, in.Body)
	})

	type ideaArg struct {
		Org   string   `json:"org"`
		Title string   `json:"title"`
		Body  string   `json:"body,omitempty"`
		Repos []string `json:"repos,omitempty" jsonschema:"names of related repositories in the org"`
	}
	tool(a, sc, srv, "create_idea", "Leave an idea: something noticed that is out of scope now. It says neither whether nor how.", func(ctx context.Context, actor app.Actor, in ideaArg) (any, error) {
		n, err := a.CreateIdea(ctx, actor, in.Org, in.Title, in.Body, in.Repos)
		return map[string]int{"number": n}, err
	})

	type planArg struct {
		Repo  string   `json:"repo" jsonschema:"the repository, as org/name"`
		Issue int      `json:"issue"`
		Title string   `json:"title" jsonschema:"the approach"`
		Body  string   `json:"body,omitempty"`
		Steps []string `json:"steps" jsonschema:"step titles, in order; they get ids s1, s2, ..."`
	}
	tool(a, sc, srv, "create_plan", "Write a plan for an issue: how to do it, as steps. Plans can be reviewed before any code exists.", func(ctx context.Context, actor app.Actor, in planArg) (any, error) {
		steps := make([]app.StepInput, len(in.Steps))
		for i, t := range in.Steps {
			steps[i] = app.StepInput{Title: t}
		}
		n, err := a.CreatePlan(ctx, actor, in.Repo, in.Issue, in.Title, in.Body, steps)
		return map[string]int{"number": n, "version": 1}, err
	})

	type reviseArg struct {
		Repo  string          `json:"repo" jsonschema:"the repository, as org/name"`
		Plan  int             `json:"plan"`
		Body  string          `json:"body,omitempty"`
		Steps []app.StepInput `json:"steps" jsonschema:"steps in order; keep an id to keep a step across versions"`
	}
	tool(a, sc, srv, "revise_plan", "Write a new version of a plan.", func(ctx context.Context, actor app.Actor, in reviseArg) (any, error) {
		v, err := a.RevisePlan(ctx, actor, in.Repo, in.Plan, in.Body, in.Steps)
		return map[string]int{"version": v}, err
	})

	type changeSetArg struct {
		Repo        string   `json:"repo" jsonschema:"the repository, as org/name"`
		Issue       int      `json:"issue"`
		Title       string   `json:"title"`
		Branch      string   `json:"branch" jsonschema:"a branch you pushed"`
		Base        string   `json:"base,omitempty" jsonschema:"the branch to merge into; the default branch if empty"`
		Plan        int      `json:"plan,omitempty" jsonschema:"the plan this change set executes, if any"`
		PlanVersion int      `json:"plan_version,omitempty"`
		Steps       []string `json:"steps,omitempty" jsonschema:"ids of the plan steps this change set carries out"`
	}
	tool(a, sc, srv, "create_changeset", "Open a change set on an issue from a pushed branch, optionally executing steps of a plan.", func(ctx context.Context, actor app.Actor, in changeSetArg) (any, error) {
		n, err := a.CreateChangeSet(ctx, actor, in.Repo, in.Issue, app.ChangeSetInput{
			Title: in.Title, Branch: in.Branch, Base: in.Base, Plan: in.Plan, PlanVersion: in.PlanVersion, Steps: in.Steps,
		})
		return map[string]int{"number": n}, err
	})

	tool(a, sc, srv, "get_changeset", "A change set with its reviews and whether it can be merged (and what is missing).", func(ctx context.Context, actor app.Actor, in numberArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		read(actor, "get_changeset", rp.FullName, map[string]int{"changeset": in.Number})
		return a.ChangeSetView(ctx, actor, rp.FullName, in.Number)
	})

	tool(a, sc, srv, "get_changeset_diff", "What a change set changes, per file, as unified diffs.", func(ctx context.Context, actor app.Actor, in numberArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		d, err := a.ChangeSetDiff(ctx, rp.FullName, in.Number)
		if err == nil {
			read(actor, "get_changeset_diff", rp.FullName, map[string]any{"changeset": in.Number, "base": d.MergeBase, "head": d.Head})
		}
		return d, err
	})

	type reviewArg struct {
		Repo     string              `json:"repo" jsonschema:"the repository, as org/name"`
		Number   int                 `json:"number" jsonschema:"the change set number"`
		Verdict  string              `json:"verdict" jsonschema:"approve, comment or request_changes"`
		Body     string              `json:"body,omitempty"`
		Comments []app.ReviewComment `json:"comments,omitempty" jsonschema:"comments on lines of the diff"`
	}
	tool(a, sc, srv, "review_changeset", "Review a change set at its current head.", func(ctx context.Context, actor app.Actor, in reviewArg) (any, error) {
		return map[string]bool{"ok": true}, a.SubmitReview(ctx, actor, in.Repo, in.Number, in.Verdict, in.Body, in.Comments)
	})

	type checkArg struct {
		Repo    string `json:"repo" jsonschema:"the repository, as org/name"`
		Commit  string `json:"commit" jsonschema:"full commit id"`
		Name    string `json:"name" jsonschema:"check name, such as ci"`
		Status  string `json:"status" jsonschema:"pending, success or failure"`
		Summary string `json:"summary,omitempty"`
	}
	tool(a, sc, srv, "report_check", "Report the result of a check you ran against a commit.", func(ctx context.Context, actor app.Actor, in checkArg) (any, error) {
		return map[string]bool{"ok": true}, a.ReportCheck(ctx, actor, in.Repo, in.Commit, in.Name, in.Status, in.Summary)
	})

	tool(a, sc, srv, "list_tree", "List a directory at a ref.", func(ctx context.Context, actor app.Actor, in codeArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		commit, err := a.ResolveCommit(ctx, rp, in.Ref)
		if err != nil {
			return nil, err
		}
		entries, err := a.Tree(ctx, rp, commit, in.Path)
		if err != nil {
			return nil, err
		}
		read(actor, "list_tree", rp.FullName, map[string]string{"commit": commit, "path": in.Path})
		return map[string]any{"commit": commit, "entries": entries}, nil
	})

	tool(a, sc, srv, "read_file", "Read a file at a ref.", func(ctx context.Context, actor app.Actor, in codeArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		commit, err := a.ResolveCommit(ctx, rp, in.Ref)
		if err != nil {
			return nil, err
		}
		blob, err := a.Blob(ctx, rp, commit, in.Path)
		if err != nil {
			return nil, err
		}
		read(actor, "read_file", rp.FullName, map[string]string{"commit": commit, "path": blob.Path})
		return map[string]any{"commit": commit, "file": blob}, nil
	})

	type commitsArg struct {
		Repo  string `json:"repo" jsonschema:"the repository, as org/name"`
		Ref   string `json:"ref,omitempty"`
		Path  string `json:"path,omitempty"`
		Limit int    `json:"limit,omitempty"`
	}
	tool(a, sc, srv, "list_commits", "History at a ref, newest first, with the identity each commit belongs to.", func(ctx context.Context, actor app.Actor, in commitsArg) (any, error) {
		rp, err := readable(ctx, actor, in.Repo)
		if err != nil {
			return nil, err
		}
		commit, err := a.ResolveCommit(ctx, rp, in.Ref)
		if err != nil {
			return nil, err
		}
		log, err := a.Log(ctx, rp, commit, in.Path, in.Limit)
		if err != nil {
			return nil, err
		}
		read(actor, "list_commits", rp.FullName, map[string]string{"commit": commit, "path": in.Path})
		return map[string]any{"commit": commit, "commits": log}, nil
	})

	type sessionArg struct {
		Label    string  `json:"label,omitempty" jsonschema:"an outside ID for the run, such as a Claude Code session ID"`
		TTLHours float64 `json:"ttl_hours,omitempty" jsonschema:"hours the session lasts: 8 if empty, at most 24, and never past the session it is opened from"`
	}
	if sc.stateful {
		tool(a, sc, srv, "open_session", "Open a child session for a separate run that shares this connection, such as a subagent: it is opened below your connection's session (or below the one you name), and you act as it by passing its id as the session argument on later calls.", func(ctx context.Context, actor app.Actor, in sessionArg) (any, error) {
			opened, err := a.OpenSession(ctx, actor, in.Label, in.TTLHours)
			opened.Token = ""
			return opened, err
		})
	} else {
		tool(a, sc, srv, "open_session", "Open a session of your own identity for a separate run, such as a subagent; returns a token bound to it and the address to commit with.", func(ctx context.Context, actor app.Actor, in sessionArg) (any, error) {
			return a.OpenSession(ctx, actor, in.Label, in.TTLHours)
		})
	}

	tool(a, sc, srv, "git_credential", "A short-lived credential for git (an hour at most) bound to your session, or to the session you name: what you push with it is recorded under that session. It works only for git, and stops when the session ends. Use it as the password with any user name, and commit with GIT_COMMITTER_EMAIL set to committer_email.", func(ctx context.Context, actor app.Actor, _ struct{}) (any, error) {
		c, err := a.IssueGitCredential(ctx, actor)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"token": c.Token, "expires_at": c.ExpiresAt, "identity": c.Identity, "session": c.Session,
			"committer_email": c.CommitterEmail, "git_url": s.URL + "/git/", "username": "x",
		}, nil
	})

	type emailArg struct {
		Email    string `json:"email"`
		Identity string `json:"identity,omitempty" jsonschema:"yours if empty"`
	}
	tool(a, sc, srv, "add_commit_email", "Register an address you commit with, so your commits resolve to your identity.", func(ctx context.Context, actor app.Actor, in emailArg) (any, error) {
		return a.AddEmailAddress(ctx, actor, in.Identity, in.Email)
	})

	type waitArg struct {
		After       int64 `json:"after" jsonschema:"read events after this sequence number; 0 for all"`
		WaitSeconds int   `json:"wait_seconds,omitempty" jsonschema:"how long to wait for a new event, up to 60"`
	}
	tool(a, sc, srv, "wait_for_events", "Read the event stream from a cursor, waiting for new events. Keep the returned next cursor.", func(ctx context.Context, actor app.Actor, in waitArg) (any, error) {
		vis := &visibility{app: a, actor: actor, cache: map[string]bool{}}
		wait := time.Duration(min(max(in.WaitSeconds, 0), 60)) * time.Second
		evs, next, err := s.pollEvents(ctx, actor, vis, in.After, 200, wait)
		return map[string]any{"events": evs, "next": next}, err
	})

	return srv
}
