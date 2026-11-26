package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"gitsynth/internal/gitx"
	"gitsynth/internal/store"
)

func (a *App) resolveBranch(ctx context.Context, repo Repo, branch string) (string, error) {
	oid, err := a.Git.Output(ctx, repo.Path, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", errf(http.StatusConflict, "branch %q not found in %s", branch, repo.FullName)
	}
	return oid, nil
}

// ReviewComment is a comment on one line of a change set's diff. Side "new"
// means a line number in the head, "old" a line number in the merge base.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// SubmitReview records a review of a change set at its current head commit,
// with any comments on lines of its diff. An approval only counts toward
// merging while the head stays the same.
func (a *App) SubmitReview(ctx context.Context, by Actor, fullName string, number int, verdict, body string, comments []ReviewComment) error {
	if !slices.Contains([]string{"approve", "comment", "request_changes"}, verdict) {
		return errf(http.StatusBadRequest, "verdict must be approve, comment or request_changes")
	}
	for i := range comments {
		c := &comments[i]
		if c.Side == "" {
			c.Side = "new"
		}
		if c.Path == "" || c.Line < 1 || (c.Side != "new" && c.Side != "old") || strings.TrimSpace(c.Body) == "" {
			return errf(http.StatusBadRequest, "each comment needs a path, a line of at least 1, a side of new or old, and a body")
		}
	}
	if comments == nil {
		comments = []ReviewComment{}
	}
	repo, err := a.Repo(ctx, a.Store.DB(), fullName)
	if err != nil {
		return err
	}
	cs, err := a.changeSet(ctx, a.Store.DB(), fullName, number)
	if err != nil {
		return err
	}
	head, err := a.resolveBranch(ctx, repo, cs.Branch)
	if err != nil {
		return err
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermReviewWrite); err != nil {
			return err
		}
		if verdict == "approve" {
			if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermReviewApprove); err != nil {
				return err
			}
		}
		e, err := tx.Append("review.submitted", repo.scope(), map[string]any{
			"changeset": number, "verdict": verdict, "commit": head, "body": body, "comments": comments,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO reviews (seq, repo, changeset, author, verdict, commit_oid, body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.Seq, repo.FullName, number, by.Identity, verdict, head, body); err != nil {
			return err
		}
		for _, c := range comments {
			if _, err := tx.Exec(`INSERT INTO review_comments (review_seq, repo, changeset, path, line, side, body) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				e.Seq, repo.FullName, number, c.Path, c.Line, c.Side, c.Body); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

// ReportCheck records the result of a check, such as CI, that an identity ran
// against a commit. It is a statement that someone verified something, signed
// like every other write.
func (a *App) ReportCheck(ctx context.Context, by Actor, fullName, commit, name, status, summary string) error {
	if !slices.Contains([]string{"pending", "success", "failure"}, status) {
		return errf(http.StatusBadRequest, "status must be pending, success or failure")
	}
	if name == "" {
		return errf(http.StatusBadRequest, "name is required")
	}
	repo, err := a.Repo(ctx, a.Store.DB(), fullName)
	if err != nil {
		return err
	}
	if !gitx.IsOID(commit) {
		return errf(http.StatusBadRequest, "commit must be a full object id")
	}
	if _, err := a.Git.Output(ctx, repo.Path, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return errf(http.StatusBadRequest, "commit %s not found in %s", commit, repo.FullName)
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if err := a.require(ctx, tx, by, repo.Org, repo.FullName, PermCheckWrite); err != nil {
			return err
		}
		e, err := tx.Append("check.reported", repo.scope(), map[string]any{
			"commit": commit, "name": name, "status": status, "summary": summary,
		})
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO checks (seq, repo, commit_oid, name, status, author, summary) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			e.Seq, repo.FullName, commit, name, status, by.Identity, summary)
		return err
	})
	return err
}

// MergeStatus says whether the records satisfy the repository's merge rule.
type MergeStatus struct {
	Head      string   `json:"head,omitempty"`
	Mergeable bool     `json:"mergeable"`
	Approvals []string `json:"approvals"`
	Unmet     []string `json:"unmet"`
}

func (a *App) mergeStatus(ctx context.Context, q querier, repo Repo, cs ChangeSet) (MergeStatus, error) {
	st := MergeStatus{Approvals: []string{}, Unmet: []string{}}
	if cs.MergedCommit != "" {
		st.Unmet = append(st.Unmet, "already merged as "+cs.MergedCommit)
		return st, nil
	}
	head, err := a.resolveBranch(ctx, repo, cs.Branch)
	if err != nil {
		st.Unmet = append(st.Unmet, err.Error())
		return st, nil
	}
	st.Head = head
	reviewers, err := queryStrings(ctx, q,
		`SELECT DISTINCT author FROM reviews WHERE repo = ? AND changeset = ? AND verdict = 'approve' AND commit_oid = ? AND author != ? ORDER BY author`,
		repo.FullName, cs.Number, head, cs.Author)
	if err != nil {
		return st, err
	}
	for _, r := range reviewers {
		ok, err := a.Can(ctx, q, Actor{Actor: store.Actor{Identity: r}}, repo.Org, repo.FullName, PermReviewApprove)
		if err != nil {
			return st, err
		}
		if ok {
			st.Approvals = append(st.Approvals, r)
		}
	}
	if len(st.Approvals) < repo.MergeRule.RequiredApprovals {
		st.Unmet = append(st.Unmet, fmt.Sprintf("needs %d approval(s) at %s, has %d", repo.MergeRule.RequiredApprovals, short(head), len(st.Approvals)))
	}
	for _, name := range repo.MergeRule.RequiredChecks {
		var status string
		err := q.QueryRowContext(ctx, `SELECT status FROM checks WHERE repo = ? AND commit_oid = ? AND name = ? ORDER BY seq DESC LIMIT 1`,
			repo.FullName, head, name).Scan(&status)
		if errors.Is(err, sql.ErrNoRows) {
			status = "missing"
		} else if err != nil {
			return st, err
		}
		if status != "success" {
			st.Unmet = append(st.Unmet, fmt.Sprintf("check %q is %s at %s", name, status, short(head)))
		}
	}
	st.Mergeable = len(st.Unmet) == 0
	return st, nil
}

func short(oid string) string {
	if len(oid) > 10 {
		return oid[:10]
	}
	return oid
}

// Merge merges a change set into its base with a merge commit, which keeps
// the authors' commits and their signatures intact. The merge commit is made
// by the server's own identity, on behalf of the identity that asked.
func (a *App) Merge(ctx context.Context, by Actor, fullName string, number int) (string, error) {
	db := a.Store.DB()
	repo, err := a.Repo(ctx, db, fullName)
	if err != nil {
		return "", err
	}
	cs, err := a.changeSet(ctx, db, fullName, number)
	if err != nil {
		return "", err
	}
	if err := a.require(ctx, db, by, repo.Org, repo.FullName, PermChangeSetMerge); err != nil {
		return "", err
	}
	st, err := a.mergeStatus(ctx, db, repo, cs)
	if err != nil {
		return "", err
	}
	if !st.Mergeable {
		return "", errf(http.StatusConflict, "change set %s#%d cannot be merged: %s", fullName, number, strings.Join(st.Unmet, "; "))
	}
	base, err := a.resolveBranch(ctx, repo, cs.Base)
	if err != nil {
		return "", err
	}
	tree, err := a.Git.Output(ctx, repo.Path, nil, "merge-tree", "--write-tree", base, st.Head)
	if err != nil {
		if gitx.ExitCode(err) == 1 {
			return "", errf(http.StatusConflict, "change set %s#%d conflicts with %s", fullName, number, cs.Base)
		}
		return "", err
	}
	tree = strings.SplitN(tree, "\n", 2)[0]
	msg := fmt.Sprintf("Merge change set #%d: %s\n\nIssue: #%d\nMerged-By: %s\n", number, cs.Title, cs.Issue, by.Identity)
	// The server makes the commit, so it is the committer; the identity that
	// asked for the merge is the author. Both use canonical addresses.
	who := []string{
		"GIT_AUTHOR_NAME=" + by.Identity, "GIT_AUTHOR_EMAIL=" + a.CommitEmail(ctx, by.Identity),
		"GIT_COMMITTER_NAME=" + SystemActor.Identity, "GIT_COMMITTER_EMAIL=" + a.CommitEmail(ctx, SystemActor.Identity),
	}
	commit, err := a.Git.Output(ctx, repo.Path, who, "commit-tree", tree, "-p", base, "-p", st.Head, "-m", msg)
	if err != nil {
		return "", err
	}
	token, done := a.NewOp(Op{Actor: SystemActor, OnBehalfOf: by.Identity, Session: by.Session, Repo: repo.FullName, Internal: true, Source: "merge"})
	defer done()
	if _, err := a.Git.Output(ctx, repo.Path, a.HookEnv(token), "update-ref", "-m", "gitsynth merge", "refs/heads/"+cs.Base, commit, base); err != nil {
		return "", err
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE changesets SET merged_commit = ? WHERE repo = ? AND number = ?`, commit, repo.FullName, number); err != nil {
			return err
		}
		_, err := tx.Append("changeset.merged", repo.scope(), map[string]any{
			"changeset": number, "commit": commit, "head": st.Head, "base": cs.Base, "approvals": st.Approvals,
		})
		return err
	})
	return commit, err
}
