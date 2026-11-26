package app

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"krystallos/internal/store"
)

// ---- Ideas ----

// Idea is a thought that does not yet say whether or how to act on it.
type Idea struct {
	Org    string     `json:"org"`
	Number int        `json:"number"`
	Title  string     `json:"title"`
	Body   string     `json:"body"`
	Author string     `json:"author"`
	Repos  []string   `json:"repos"`
	Issues []IssueRef `json:"issues"`
}

// IssueRef points at an issue.
type IssueRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

// CreateIdea records an idea in an org, linked to any number of its
// repositories.
func (a *App) CreateIdea(ctx context.Context, by Actor, org, title, body string, repos []string) (int, error) {
	if strings.TrimSpace(title) == "" {
		return 0, errf(http.StatusBadRequest, "title is required")
	}
	var number int
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		allowed, err := a.Can(ctx, tx, by, org, "", PermIdeaWrite)
		if err != nil {
			return err
		}
		full := make([]string, 0, len(repos))
		for _, name := range repos {
			repo, err := a.Repo(ctx, tx, org+"/"+name)
			if err != nil {
				return err
			}
			full = append(full, repo.FullName)
			if !allowed {
				if allowed, err = a.Can(ctx, tx, by, org, repo.FullName, PermIdeaWrite); err != nil {
					return err
				}
			}
		}
		if !allowed {
			return errf(http.StatusForbidden, "%s lacks %s in %s", by.Identity, PermIdeaWrite, org)
		}
		if number, err = nextNumber(ctx, tx, "ideas", "org", org); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO ideas (org, number, title, body, author) VALUES (?, ?, ?, ?, ?)`, org, number, title, body, by.Identity); err != nil {
			return err
		}
		for _, r := range full {
			if _, err := tx.Exec(`INSERT INTO idea_repos (org, idea, repo) VALUES (?, ?, ?)`, org, number, r); err != nil {
				return err
			}
		}
		_, err = tx.Append("idea.created", store.Scope{Org: org}, map[string]any{"number": number, "title": title, "body": body, "repos": full})
		return err
	})
	return number, err
}

// Idea loads an idea with its repositories and the issues made from it.
func (a *App) Idea(ctx context.Context, org string, number int) (Idea, error) {
	db := a.Store.DB()
	it := Idea{Org: org, Number: number, Repos: []string{}, Issues: []IssueRef{}}
	err := db.QueryRowContext(ctx, `SELECT title, body, author FROM ideas WHERE org = ? AND number = ?`, org, number).Scan(&it.Title, &it.Body, &it.Author)
	if err != nil {
		return Idea{}, notFound(fmt.Sprintf("idea %s#%d", org, number), err)
	}
	if it.Repos, err = queryStrings(ctx, db, `SELECT repo FROM idea_repos WHERE org = ? AND idea = ? ORDER BY repo`, org, number); err != nil {
		return Idea{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT repo, issue FROM issue_sources WHERE org = ? AND idea = ? ORDER BY repo, issue`, org, number)
	if err != nil {
		return Idea{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var r IssueRef
		if err := rows.Scan(&r.Repo, &r.Number); err != nil {
			return Idea{}, err
		}
		it.Issues = append(it.Issues, r)
	}
	return it, rows.Err()
}

// Ideas lists an org's ideas.
func (a *App) Ideas(ctx context.Context, org string) ([]Idea, error) {
	numbers, err := queryInts(ctx, a.Store.DB(), `SELECT number FROM ideas WHERE org = ? ORDER BY number`, org)
	if err != nil {
		return nil, err
	}
	out := make([]Idea, 0, len(numbers))
	for _, n := range numbers {
		it, err := a.Idea(ctx, org, n)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// ConvertIdea makes an issue from an idea in one of its repositories. The
// idea stays; whether it became an issue follows from the link.
func (a *App) ConvertIdea(ctx context.Context, by Actor, org string, number int, repoName, title, body string) (int, error) {
	idea, err := a.Idea(ctx, org, number)
	if err != nil {
		return 0, err
	}
	full := org + "/" + repoName
	if !slices.Contains(idea.Repos, full) {
		return 0, errf(http.StatusBadRequest, "idea %s#%d is not linked to %s", org, number, full)
	}
	if title == "" {
		title = idea.Title
	}
	if body == "" {
		body = idea.Body
	}
	return a.createIssue(ctx, by, full, title, body, &IdeaRef{Org: org, Number: number})
}

// IdeaRef points at an idea.
type IdeaRef struct {
	Org    string `json:"org"`
	Number int    `json:"number"`
}

// ---- Issues ----

// CreateIssue opens an issue.
func (a *App) CreateIssue(ctx context.Context, by Actor, repo, title, body string) (int, error) {
	return a.createIssue(ctx, by, repo, title, body, nil)
}

func (a *App) createIssue(ctx context.Context, by Actor, fullName, title, body string, from *IdeaRef) (int, error) {
	if strings.TrimSpace(title) == "" {
		return 0, errf(http.StatusBadRequest, "title is required")
	}
	var number int
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.Repo(ctx, tx, fullName)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermIssueWrite); err != nil {
			return err
		}
		if number, err = nextNumber(ctx, tx, "issues", "repo", repo.FullName); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO issues (repo, number, title, body, author) VALUES (?, ?, ?, ?, ?)`, repo.FullName, number, title, body, by.Identity); err != nil {
			return err
		}
		payload := map[string]any{"number": number, "title": title, "body": body}
		if from != nil {
			if _, err := tx.Exec(`INSERT INTO issue_sources (repo, issue, org, idea) VALUES (?, ?, ?, ?)`, repo.FullName, number, from.Org, from.Number); err != nil {
				return err
			}
			payload["from_idea"] = from
		}
		_, err = tx.Append("issue.created", repo.scope(), payload)
		return err
	})
	return number, err
}

// CommentIssue adds a comment to an issue's discussion.
func (a *App) CommentIssue(ctx context.Context, by Actor, fullName string, issue int, body string) error {
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.issueRepo(ctx, tx, fullName, issue)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermIssueWrite); err != nil {
			return err
		}
		e, err := tx.Append("issue.commented", repo.scope(), map[string]any{"issue": issue, "body": body})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO comments (seq, repo, issue, author, body) VALUES (?, ?, ?, ?, ?)`, e.Seq, repo.FullName, issue, by.Identity, body)
		return err
	})
	return err
}

// CloseIssue closes an issue. Closing is always an explicit act: with no
// "adopted" plan, the records cannot say on their own that an issue is done.
func (a *App) CloseIssue(ctx context.Context, by Actor, fullName string, issue int) error {
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.issueRepo(ctx, tx, fullName, issue)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermIssueWrite); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE issues SET closed = 1 WHERE repo = ? AND number = ?`, repo.FullName, issue); err != nil {
			return err
		}
		_, err = tx.Append("issue.closed", repo.scope(), map[string]any{"issue": issue})
		return err
	})
	return err
}

func (a *App) issueRepo(ctx context.Context, q querier, fullName string, issue int) (Repo, error) {
	repo, err := a.Repo(ctx, q, fullName)
	if err != nil {
		return Repo{}, err
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues WHERE repo = ? AND number = ?`, repo.FullName, issue).Scan(&n); err != nil {
		return Repo{}, err
	}
	if n == 0 {
		return Repo{}, errf(http.StatusNotFound, "issue %s#%d not found", repo.FullName, issue)
	}
	return repo, nil
}

// ---- Plans ----

// StepInput is one step of a plan as submitted. ID is set only when a
// revision keeps a step from an earlier version.
type StepInput struct {
	ID    string `json:"id,omitempty"`
	Title string `json:"title"`
}

// Step is a plan step. Its ID stays the same across plan versions.
type Step struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// CreatePlan adds a plan to an issue. Plans have no state of their own;
// which plan was followed shows in what got merged.
func (a *App) CreatePlan(ctx context.Context, by Actor, fullName string, issue int, title, body string, steps []StepInput) (int, error) {
	if strings.TrimSpace(title) == "" {
		return 0, errf(http.StatusBadRequest, "title is required")
	}
	var number int
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.issueRepo(ctx, tx, fullName, issue)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermPlanWrite); err != nil {
			return err
		}
		if number, err = nextNumber(ctx, tx, "plans", "repo", repo.FullName); err != nil {
			return err
		}
		out := make([]Step, len(steps))
		for i, s := range steps {
			out[i] = Step{ID: fmt.Sprintf("s%d", i+1), Title: s.Title}
		}
		if _, err := tx.Exec(`INSERT INTO plans (repo, number, issue, title, author, latest) VALUES (?, ?, ?, ?, ?, 1)`, repo.FullName, number, issue, title, by.Identity); err != nil {
			return err
		}
		if err := insertPlanVersion(tx, repo.FullName, number, 1, body, by.Identity, out); err != nil {
			return err
		}
		_, err = tx.Append("plan.created", repo.scope(), map[string]any{
			"number": number, "issue": issue, "title": title, "version": 1, "body": body, "steps": out,
		})
		return err
	})
	return number, err
}

func insertPlanVersion(tx *store.Tx, repo string, plan, version int, body, author string, steps []Step) error {
	if _, err := tx.Exec(`INSERT INTO plan_versions (repo, plan, version, body, author) VALUES (?, ?, ?, ?, ?)`, repo, plan, version, body, author); err != nil {
		return err
	}
	for i, s := range steps {
		if _, err := tx.Exec(`INSERT INTO plan_steps (repo, plan, version, step, position, title) VALUES (?, ?, ?, ?, ?, ?)`, repo, plan, version, s.ID, i, s.Title); err != nil {
			return err
		}
	}
	return nil
}

// RevisePlan adds a new version of a plan. Steps that keep their ID keep
// their identity across versions; new steps get fresh IDs.
func (a *App) RevisePlan(ctx context.Context, by Actor, fullName string, plan int, body string, steps []StepInput) (int, error) {
	var version int
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.Repo(ctx, tx, fullName)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermPlanWrite); err != nil {
			return err
		}
		var latest int
		if err := tx.QueryRowContext(ctx, `SELECT latest FROM plans WHERE repo = ? AND number = ?`, repo.FullName, plan).Scan(&latest); err != nil {
			return notFound(fmt.Sprintf("plan %s/%d", repo.FullName, plan), err)
		}
		known, err := queryStrings(ctx, tx, `SELECT DISTINCT step FROM plan_steps WHERE repo = ? AND plan = ?`, repo.FullName, plan)
		if err != nil {
			return err
		}
		next := 0
		for _, id := range known {
			var n int
			if _, err := fmt.Sscanf(id, "s%d", &n); err == nil && n > next {
				next = n
			}
		}
		out := make([]Step, len(steps))
		for i, s := range steps {
			if s.ID != "" {
				if !slices.Contains(known, s.ID) {
					return errf(http.StatusBadRequest, "step %q is not part of plan %d", s.ID, plan)
				}
				out[i] = Step{ID: s.ID, Title: s.Title}
				continue
			}
			next++
			out[i] = Step{ID: fmt.Sprintf("s%d", next), Title: s.Title}
		}
		version = latest + 1
		if err := insertPlanVersion(tx, repo.FullName, plan, version, body, by.Identity, out); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE plans SET latest = ? WHERE repo = ? AND number = ?`, version, repo.FullName, plan); err != nil {
			return err
		}
		_, err = tx.Append("plan.revised", repo.scope(), map[string]any{"number": plan, "version": version, "body": body, "steps": out})
		return err
	})
	return version, err
}

// PlanView is a plan at its latest version, with how far its steps got.
type PlanView struct {
	Number   int        `json:"number"`
	Issue    int        `json:"issue"`
	Title    string     `json:"title"`
	Author   string     `json:"author"`
	Version  int        `json:"version"`
	Body     string     `json:"body"`
	Steps    []StepView `json:"steps"`
	Complete bool       `json:"complete"`
}

// StepView shows which change sets execute a step and whether one of them
// was merged.
type StepView struct {
	Step
	ChangeSets []int `json:"changesets"`
	Merged     bool  `json:"merged"`
}

// Plan loads a plan's latest version and computes its progress from the
// change sets that execute it.
func (a *App) Plan(ctx context.Context, fullName string, number int) (PlanView, error) {
	db := a.Store.DB()
	p := PlanView{Number: number, Steps: []StepView{}}
	err := db.QueryRowContext(ctx,
		`SELECT p.issue, p.title, p.author, p.latest, v.body FROM plans p
		 JOIN plan_versions v ON v.repo = p.repo AND v.plan = p.number AND v.version = p.latest
		 WHERE p.repo = ? AND p.number = ?`, fullName, number).Scan(&p.Issue, &p.Title, &p.Author, &p.Version, &p.Body)
	if err != nil {
		return PlanView{}, notFound(fmt.Sprintf("plan %s/%d", fullName, number), err)
	}
	rows, err := db.QueryContext(ctx, `SELECT step, title FROM plan_steps WHERE repo = ? AND plan = ? AND version = ? ORDER BY position`, fullName, number, p.Version)
	if err != nil {
		return PlanView{}, err
	}
	for rows.Next() {
		var s StepView
		if err := rows.Scan(&s.ID, &s.Title); err != nil {
			rows.Close()
			return PlanView{}, err
		}
		s.ChangeSets = []int{}
		p.Steps = append(p.Steps, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return PlanView{}, err
	}
	p.Complete = len(p.Steps) > 0
	for i := range p.Steps {
		s := &p.Steps[i]
		rows, err := db.QueryContext(ctx,
			`SELECT c.number, c.merged_commit FROM changeset_steps cs
			 JOIN changesets c ON c.repo = cs.repo AND c.number = cs.changeset
			 WHERE cs.repo = ? AND c.plan = ? AND cs.step = ? ORDER BY c.number`, fullName, number, s.ID)
		if err != nil {
			return PlanView{}, err
		}
		for rows.Next() {
			var n int
			var merged string
			if err := rows.Scan(&n, &merged); err != nil {
				rows.Close()
				return PlanView{}, err
			}
			s.ChangeSets = append(s.ChangeSets, n)
			s.Merged = s.Merged || merged != ""
		}
		rows.Close()
		p.Complete = p.Complete && s.Merged
	}
	return p, nil
}

// ---- Change sets ----

// ChangeSetInput describes a new change set.
type ChangeSetInput struct {
	Title       string   `json:"title"`
	Branch      string   `json:"branch"`
	Base        string   `json:"base"`
	Plan        int      `json:"plan"`
	PlanVersion int      `json:"plan_version"`
	Steps       []string `json:"steps"`
}

// CreateChangeSet adds a change set to an issue: the execution of at most
// one plan, pinned to a plan version and to the steps it covers.
func (a *App) CreateChangeSet(ctx context.Context, by Actor, fullName string, issue int, in ChangeSetInput) (int, error) {
	if strings.TrimSpace(in.Title) == "" {
		return 0, errf(http.StatusBadRequest, "title is required")
	}
	var number int
	_, err := a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		repo, err := a.issueRepo(ctx, tx, fullName, issue)
		if err != nil {
			return err
		}
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermChangeSetWrite); err != nil {
			return err
		}
		if in.Base == "" {
			in.Base = repo.DefaultBranch
		}
		for _, b := range []string{in.Branch, in.Base} {
			if _, err := a.Git.Output(ctx, repo.Path, nil, "check-ref-format", "refs/heads/"+b); err != nil || b == "" {
				return errf(http.StatusBadRequest, "invalid branch name %q", b)
			}
		}
		if in.Branch == in.Base {
			return errf(http.StatusBadRequest, "branch and base must differ")
		}
		if in.Plan == 0 && (in.PlanVersion != 0 || len(in.Steps) > 0) {
			return errf(http.StatusBadRequest, "plan_version and steps need a plan")
		}
		if in.Plan != 0 {
			var planIssue, latest int
			err := tx.QueryRowContext(ctx, `SELECT issue, latest FROM plans WHERE repo = ? AND number = ?`, repo.FullName, in.Plan).Scan(&planIssue, &latest)
			if err != nil {
				return notFound(fmt.Sprintf("plan %s/%d", repo.FullName, in.Plan), err)
			}
			if planIssue != issue {
				return errf(http.StatusBadRequest, "plan %d belongs to issue %d, not %d", in.Plan, planIssue, issue)
			}
			if in.PlanVersion == 0 {
				in.PlanVersion = latest
			}
			if in.PlanVersion < 1 || in.PlanVersion > latest {
				return errf(http.StatusBadRequest, "plan %d has no version %d", in.Plan, in.PlanVersion)
			}
			steps, err := queryStrings(ctx, tx, `SELECT step FROM plan_steps WHERE repo = ? AND plan = ? AND version = ?`, repo.FullName, in.Plan, in.PlanVersion)
			if err != nil {
				return err
			}
			for _, s := range in.Steps {
				if !slices.Contains(steps, s) {
					return errf(http.StatusBadRequest, "step %q is not in plan %d version %d", s, in.Plan, in.PlanVersion)
				}
			}
		}
		if in.Steps == nil {
			in.Steps = []string{}
		}
		if number, err = nextNumber(ctx, tx, "changesets", "repo", repo.FullName); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO changesets (repo, number, issue, title, author, branch, base, plan, plan_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			repo.FullName, number, issue, in.Title, by.Identity, in.Branch, in.Base, in.Plan, in.PlanVersion); err != nil {
			return err
		}
		for _, s := range in.Steps {
			if _, err := tx.Exec(`INSERT INTO changeset_steps (repo, changeset, step) VALUES (?, ?, ?)`, repo.FullName, number, s); err != nil {
				return err
			}
		}
		_, err = tx.Append("changeset.created", repo.scope(), map[string]any{
			"number": number, "issue": issue, "title": in.Title, "branch": in.Branch, "base": in.Base,
			"plan": in.Plan, "plan_version": in.PlanVersion, "steps": in.Steps,
		})
		return err
	})
	return number, err
}

// ChangeSet is a change set's stored fields.
type ChangeSet struct {
	Repo         string   `json:"repo"`
	Number       int      `json:"number"`
	Issue        int      `json:"issue"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	Branch       string   `json:"branch"`
	Base         string   `json:"base"`
	Plan         int      `json:"plan,omitempty"`
	PlanVersion  int      `json:"plan_version,omitempty"`
	Steps        []string `json:"steps"`
	MergedCommit string   `json:"merged_commit,omitempty"`
}

func (a *App) changeSet(ctx context.Context, q querier, fullName string, number int) (ChangeSet, error) {
	c := ChangeSet{Repo: fullName, Number: number}
	err := q.QueryRowContext(ctx,
		`SELECT issue, title, author, branch, base, plan, plan_version, merged_commit FROM changesets WHERE repo = ? AND number = ?`,
		fullName, number).Scan(&c.Issue, &c.Title, &c.Author, &c.Branch, &c.Base, &c.Plan, &c.PlanVersion, &c.MergedCommit)
	if err != nil {
		return ChangeSet{}, notFound(fmt.Sprintf("change set %s#%d", fullName, number), err)
	}
	c.Steps, err = queryStrings(ctx, q, `SELECT step FROM changeset_steps WHERE repo = ? AND changeset = ? ORDER BY step`, fullName, number)
	return c, err
}

// Review is a review of a change set at one commit.
type Review struct {
	Seq      int64           `json:"seq"`
	Author   string          `json:"author"`
	Verdict  string          `json:"verdict"`
	Commit   string          `json:"commit"`
	Body     string          `json:"body"`
	Comments []ReviewComment `json:"comments"`
}

// ChangeSetView is a change set with its reviews and merge status.
type ChangeSetView struct {
	ChangeSet
	Reviews []Review    `json:"reviews"`
	Status  MergeStatus `json:"status"`
}

// ChangeSetView loads a change set and evaluates its merge rule.
func (a *App) ChangeSetView(ctx context.Context, by Actor, fullName string, number int) (ChangeSetView, error) {
	db := a.Store.DB()
	repo, err := a.Repo(ctx, db, fullName)
	if err != nil {
		return ChangeSetView{}, err
	}
	cs, err := a.changeSet(ctx, db, fullName, number)
	if err != nil {
		return ChangeSetView{}, err
	}
	v := ChangeSetView{ChangeSet: cs, Reviews: []Review{}}
	rows, err := db.QueryContext(ctx, `SELECT seq, author, verdict, commit_oid, body FROM reviews WHERE repo = ? AND changeset = ? ORDER BY seq`, fullName, number)
	if err != nil {
		return ChangeSetView{}, err
	}
	for rows.Next() {
		r := Review{Comments: []ReviewComment{}}
		if err := rows.Scan(&r.Seq, &r.Author, &r.Verdict, &r.Commit, &r.Body); err != nil {
			rows.Close()
			return ChangeSetView{}, err
		}
		v.Reviews = append(v.Reviews, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ChangeSetView{}, err
	}
	for i := range v.Reviews {
		r := &v.Reviews[i]
		crows, err := db.QueryContext(ctx, `SELECT path, line, side, body FROM review_comments WHERE review_seq = ? ORDER BY rowid`, r.Seq)
		if err != nil {
			return ChangeSetView{}, err
		}
		for crows.Next() {
			var c ReviewComment
			if err := crows.Scan(&c.Path, &c.Line, &c.Side, &c.Body); err != nil {
				crows.Close()
				return ChangeSetView{}, err
			}
			r.Comments = append(r.Comments, c)
		}
		crows.Close()
	}
	v.Status, err = a.mergeStatus(ctx, db, repo, cs)
	return v, err
}

// ---- Issue view ----

// IssueView is an issue with everything hanging off it.
type IssueView struct {
	Repo       string      `json:"repo"`
	Number     int         `json:"number"`
	Title      string      `json:"title"`
	Body       string      `json:"body"`
	Author     string      `json:"author"`
	Closed     bool        `json:"closed"`
	FromIdeas  []IdeaRef   `json:"from_ideas"`
	Comments   []Comment   `json:"comments"`
	Plans      []PlanView  `json:"plans"`
	ChangeSets []ChangeSet `json:"changesets"`
	// Hints suggest an explicit act, such as closing, that the records
	// support but do not decide.
	Hints []string `json:"hints"`
}

// Comment is one comment in an issue's discussion.
type Comment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

// Issue loads an issue view.
func (a *App) Issue(ctx context.Context, fullName string, number int) (IssueView, error) {
	db := a.Store.DB()
	v := IssueView{Repo: fullName, Number: number, FromIdeas: []IdeaRef{}, Comments: []Comment{}, Plans: []PlanView{}, ChangeSets: []ChangeSet{}, Hints: []string{}}
	err := db.QueryRowContext(ctx, `SELECT title, body, author, closed FROM issues WHERE repo = ? AND number = ?`, fullName, number).
		Scan(&v.Title, &v.Body, &v.Author, &v.Closed)
	if err != nil {
		return IssueView{}, notFound(fmt.Sprintf("issue %s#%d", fullName, number), err)
	}
	rows, err := db.QueryContext(ctx, `SELECT org, idea FROM issue_sources WHERE repo = ? AND issue = ?`, fullName, number)
	if err != nil {
		return IssueView{}, err
	}
	for rows.Next() {
		var r IdeaRef
		if err := rows.Scan(&r.Org, &r.Number); err != nil {
			rows.Close()
			return IssueView{}, err
		}
		v.FromIdeas = append(v.FromIdeas, r)
	}
	rows.Close()
	rows, err = db.QueryContext(ctx, `SELECT author, body FROM comments WHERE repo = ? AND issue = ? ORDER BY seq`, fullName, number)
	if err != nil {
		return IssueView{}, err
	}
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.Author, &c.Body); err != nil {
			rows.Close()
			return IssueView{}, err
		}
		v.Comments = append(v.Comments, c)
	}
	rows.Close()
	plans, err := queryInts(ctx, db, `SELECT number FROM plans WHERE repo = ? AND issue = ? ORDER BY number`, fullName, number)
	if err != nil {
		return IssueView{}, err
	}
	for _, p := range plans {
		pv, err := a.Plan(ctx, fullName, p)
		if err != nil {
			return IssueView{}, err
		}
		v.Plans = append(v.Plans, pv)
		if pv.Complete && !v.Closed {
			v.Hints = append(v.Hints, fmt.Sprintf("every step of plan %d (version %d) has a merged change set", pv.Number, pv.Version))
		}
	}
	css, err := queryInts(ctx, db, `SELECT number FROM changesets WHERE repo = ? AND issue = ? ORDER BY number`, fullName, number)
	if err != nil {
		return IssueView{}, err
	}
	for _, n := range css {
		cs, err := a.changeSet(ctx, db, fullName, n)
		if err != nil {
			return IssueView{}, err
		}
		v.ChangeSets = append(v.ChangeSets, cs)
	}
	return v, nil
}

// IssueSummary is one row of an issue list.
type IssueSummary struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Author string `json:"author"`
	Closed bool   `json:"closed"`
}

// Issues lists a repository's issues.
func (a *App) Issues(ctx context.Context, fullName string) ([]IssueSummary, error) {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT number, title, author, closed FROM issues WHERE repo = ? ORDER BY number`, fullName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IssueSummary{}
	for rows.Next() {
		var s IssueSummary
		if err := rows.Scan(&s.Number, &s.Title, &s.Author, &s.Closed); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- helpers ----

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func queryInts(ctx context.Context, q querier, query string, args ...any) ([]int, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int{}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
