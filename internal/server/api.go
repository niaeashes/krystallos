package server

import (
	"net/http"
	"slices"
	"time"

	"gitsynth/internal/app"
)

func (s *Server) me(r *http.Request, actor app.Actor) (any, error) {
	return map[string]any{
		"identity": actor.Identity, "principal": actor.Principal, "session": actor.Session,
		"kind": actor.Kind, "admin": actor.Admin, "commit_email": s.App.CommitEmail(r.Context(), actor.Identity),
	}, nil
}

func (s *Server) listEmails(r *http.Request, actor app.Actor) (any, error) {
	return s.App.MyEmailAddresses(r.Context(), actor)
}

func (s *Server) addEmail(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[app.IdentityEmailAddress](r)
	if err != nil {
		return nil, err
	}
	return s.App.AddEmailAddress(r.Context(), actor, in.Identity, in.Email)
}

func (s *Server) removeEmail(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Email string `json:"email"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.RemoveEmailAddress(r.Context(), actor, in.Email)
}

// deriveIdentity derives an identity below parent, or below the caller.
// Only the person (a user's root identity) and a bot's managers derive.
func (s *Server) deriveIdentity(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Parent string `json:"parent"`
		Label  string `json:"label"`
	}](r)
	if err != nil {
		return nil, err
	}
	id, err := s.App.Derive(r.Context(), actor, in.Parent, in.Label)
	return map[string]string{"id": id.String()}, err
}

func (s *Server) disableIdentity(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		ID string `json:"id"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.DisableIdentity(r.Context(), actor, in.ID)
}

func (s *Server) enableIdentity(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		ID string `json:"id"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.EnableIdentity(r.Context(), actor, in.ID)
}

// openSession opens a session of the caller's own identity, under the
// caller's session if it came with a session's credential, and returns the
// session's own credential.
func (s *Server) openSession(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Label    string  `json:"label"`
		TTLHours float64 `json:"ttl_hours"`
	}](r)
	if err != nil {
		return nil, err
	}
	return s.App.OpenSession(r.Context(), actor, in.Label, in.TTLHours)
}

func (s *Server) listBots(r *http.Request, actor app.Actor) (any, error) {
	return s.App.ManagedBots(r.Context(), actor)
}

func (s *Server) createBot(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Name  string `json:"name"`
		Owner string `json:"owner"`
	}](r)
	if err != nil {
		return nil, err
	}
	if in.Owner == "" {
		in.Owner = actor.Principal
	}
	id, err := s.App.CreateBot(r.Context(), actor, in.Name, in.Owner)
	return map[string]string{"id": id.String(), "owner": in.Owner}, err
}

func (s *Server) listServices(r *http.Request, actor app.Actor) (any, error) {
	return s.App.Services(r.Context())
}

func (s *Server) registerService(r *http.Request, actor app.Actor) (any, error) {
	m, err := decode[app.Manifest](r)
	if err != nil {
		return nil, err
	}
	id, err := s.App.RegisterService(r.Context(), actor, m)
	return map[string]string{"id": id.String()}, err
}

func (s *Server) installService(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Service string `json:"service"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.InstallService(r.Context(), actor, r.PathValue("org"), in.Service)
}

// invite issues a one-time link for someone to join. The link points at the
// web UI the request came from, when that is one the server knows.
func (s *Server) invite(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Org      string  `json:"org"`
		Set      string  `json:"set"`
		TTLHours float64 `json:"ttl_hours"`
	}](r)
	if err != nil {
		return nil, err
	}
	ttl := 72 * time.Hour
	if in.TTLHours > 0 {
		ttl = time.Duration(in.TTLHours * float64(time.Hour))
	}
	token, err := s.App.IssueInvite(r.Context(), actor, in.Org, in.Set, ttl)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": s.webBase(r) + "/register?token=" + token, "expires_at": time.Now().UTC().Add(ttl).Format(time.RFC3339)}, nil
}

func (s *Server) serverInfo(r *http.Request, actor app.Actor) (any, error) {
	host, err := s.App.Host(r.Context())
	return map[string]string{"host": host}, err
}

// webBase is where links for the browser that sent r start: the origin of
// its page if that is the server's URL or one of its other origins, and
// the URL otherwise. An origin the server does not know never makes it
// into a link.
func (s *Server) webBase(r *http.Request) string {
	if o := r.Header.Get("Origin"); o != "" && (o == s.URL || slices.Contains(s.Origins, o)) {
		return o
	}
	return s.URL
}

// issueToken issues a standing token. It belongs to no session; each run
// that uses it opens one.
func (s *Server) issueToken(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Identity string  `json:"identity"`
		TTLHours float64 `json:"ttl_hours"`
	}](r)
	if err != nil {
		return nil, err
	}
	return s.App.IssueToken(r.Context(), actor, in.Identity, time.Duration(in.TTLHours*float64(time.Hour)))
}

func (s *Server) createOrg(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Name string `json:"name"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]string{"name": in.Name}, s.App.CreateOrg(r.Context(), actor, in.Name)
}

type grantInput struct {
	Identity string `json:"identity"`
	Set      string `json:"set"`
}

func (s *Server) grantOrg(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[grantInput](r)
	if err != nil {
		return nil, err
	}
	return in, s.App.GrantOrg(r.Context(), actor, r.PathValue("org"), in.Identity, in.Set)
}

func (s *Server) createRepo(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Name string `json:"name"`
	}](r)
	if err != nil {
		return nil, err
	}
	repo, err := s.App.CreateRepo(r.Context(), actor, r.PathValue("org"), in.Name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"repo": repo, "clone_url": s.cloneURL(repo)}, nil
}

func (s *Server) requireOrg(r *http.Request, actor app.Actor) error {
	ok, err := s.App.CanSeeOrg(r.Context(), actor, r.PathValue("org"))
	if err != nil {
		return err
	}
	if !ok {
		return &app.Error{Code: http.StatusNotFound, Msg: "org not found"}
	}
	return nil
}

func (s *Server) createIdea(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Title string   `json:"title"`
		Body  string   `json:"body"`
		Repos []string `json:"repos"`
	}](r)
	if err != nil {
		return nil, err
	}
	n, err := s.App.CreateIdea(r.Context(), actor, r.PathValue("org"), in.Title, in.Body, in.Repos)
	return map[string]int{"number": n}, err
}

func (s *Server) listIdeas(r *http.Request, actor app.Actor) (any, error) {
	if err := s.requireOrg(r, actor); err != nil {
		return nil, err
	}
	return s.App.Ideas(r.Context(), r.PathValue("org"))
}

func (s *Server) getIdea(r *http.Request, actor app.Actor) (any, error) {
	if err := s.requireOrg(r, actor); err != nil {
		return nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	return s.App.Idea(r.Context(), r.PathValue("org"), n)
}

func (s *Server) convertIdea(r *http.Request, actor app.Actor) (any, error) {
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	in, err := decode[struct {
		Repo  string `json:"repo"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}](r)
	if err != nil {
		return nil, err
	}
	issue, err := s.App.ConvertIdea(r.Context(), actor, r.PathValue("org"), n, in.Repo, in.Title, in.Body)
	return map[string]any{"repo": r.PathValue("org") + "/" + in.Repo, "issue": issue}, err
}

// readable checks repo.read and answers "not found" otherwise, so a caller
// cannot probe for repositories it may not see.
func (s *Server) readable(r *http.Request, actor app.Actor) (app.Repo, error) {
	repo, ok, err := s.App.Allowed(r.Context(), actor, repoFromPath(r), app.PermRepoRead)
	if err != nil {
		return app.Repo{}, err
	}
	if !ok {
		return app.Repo{}, &app.Error{Code: http.StatusNotFound, Msg: "repository not found"}
	}
	return repo, nil
}

func (s *Server) grantRepo(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[grantInput](r)
	if err != nil {
		return nil, err
	}
	return in, s.App.GrantRepo(r.Context(), actor, repoFromPath(r), in.Identity, in.Set)
}

func (s *Server) definePermSet(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Name  string   `json:"name"`
		Perms []string `json:"perms"`
	}](r)
	if err != nil {
		return nil, err
	}
	return in, s.App.DefinePermSet(r.Context(), actor, repoFromPath(r), in.Name, in.Perms)
}

func (s *Server) setMergeRule(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[app.MergeRule](r)
	if err != nil {
		return nil, err
	}
	return in, s.App.SetMergeRule(r.Context(), actor, repoFromPath(r), in)
}

func (s *Server) createIssue(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}](r)
	if err != nil {
		return nil, err
	}
	n, err := s.App.CreateIssue(r.Context(), actor, repoFromPath(r), in.Title, in.Body)
	return map[string]int{"number": n}, err
}

func (s *Server) listIssues(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	return s.App.Issues(r.Context(), repo.FullName)
}

func (s *Server) getIssue(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	return s.App.Issue(r.Context(), repo.FullName, n)
}

func (s *Server) commentIssue(r *http.Request, actor app.Actor) (any, error) {
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	in, err := decode[struct {
		Body string `json:"body"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.CommentIssue(r.Context(), actor, repoFromPath(r), n, in.Body)
}

func (s *Server) closeIssue(r *http.Request, actor app.Actor) (any, error) {
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.CloseIssue(r.Context(), actor, repoFromPath(r), n)
}

func (s *Server) createPlan(r *http.Request, actor app.Actor) (any, error) {
	issue, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	in, err := decode[struct {
		Title string          `json:"title"`
		Body  string          `json:"body"`
		Steps []app.StepInput `json:"steps"`
	}](r)
	if err != nil {
		return nil, err
	}
	n, err := s.App.CreatePlan(r.Context(), actor, repoFromPath(r), issue, in.Title, in.Body, in.Steps)
	return map[string]int{"number": n, "version": 1}, err
}

func (s *Server) getPlan(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	return s.App.Plan(r.Context(), repo.FullName, n)
}

func (s *Server) revisePlan(r *http.Request, actor app.Actor) (any, error) {
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	in, err := decode[struct {
		Body  string          `json:"body"`
		Steps []app.StepInput `json:"steps"`
	}](r)
	if err != nil {
		return nil, err
	}
	v, err := s.App.RevisePlan(r.Context(), actor, repoFromPath(r), n, in.Body, in.Steps)
	return map[string]int{"number": n, "version": v}, err
}

func (s *Server) createChangeSet(r *http.Request, actor app.Actor) (any, error) {
	issue, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	in, err := decode[app.ChangeSetInput](r)
	if err != nil {
		return nil, err
	}
	n, err := s.App.CreateChangeSet(r.Context(), actor, repoFromPath(r), issue, in)
	return map[string]int{"number": n}, err
}

func (s *Server) getChangeSet(r *http.Request, actor app.Actor) (any, error) {
	repo, err := s.readable(r, actor)
	if err != nil {
		return nil, err
	}
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	return s.App.ChangeSetView(r.Context(), actor, repo.FullName, n)
}

func (s *Server) review(r *http.Request, actor app.Actor) (any, error) {
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	in, err := decode[struct {
		Verdict  string              `json:"verdict"`
		Body     string              `json:"body"`
		Comments []app.ReviewComment `json:"comments"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.SubmitReview(r.Context(), actor, repoFromPath(r), n, in.Verdict, in.Body, in.Comments)
}

func (s *Server) merge(r *http.Request, actor app.Actor) (any, error) {
	n, err := pathInt(r, "n")
	if err != nil {
		return nil, err
	}
	commit, err := s.App.Merge(r.Context(), actor, repoFromPath(r), n)
	return map[string]string{"commit": commit}, err
}

func (s *Server) reportCheck(r *http.Request, actor app.Actor) (any, error) {
	in, err := decode[struct {
		Commit  string `json:"commit"`
		Name    string `json:"name"`
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}](r)
	if err != nil {
		return nil, err
	}
	return map[string]bool{"ok": true}, s.App.ReportCheck(r.Context(), actor, repoFromPath(r), in.Commit, in.Name, in.Status, in.Summary)
}
