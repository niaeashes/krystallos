package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitsynth/internal/gitx"
	"gitsynth/internal/ident"
	"gitsynth/internal/store"
)

// Op is the context of one git operation the server starts: who it is for,
// and whether it is the server's own internal step (a merge). Hooks find it
// by the token handed to git in the environment.
type Op struct {
	Actor      store.Actor
	OnBehalfOf string
	Session    string
	Repo       string
	Internal   bool
	Source     string
	expires    time.Time
}

// NewOp registers an operation and returns its token and a cleanup func.
func (a *App) NewOp(op Op) (string, func()) {
	token := randomString(20)
	op.expires = time.Now().Add(2 * time.Hour)
	a.opsMu.Lock()
	a.ops[token] = op
	a.opsMu.Unlock()
	return token, func() {
		a.opsMu.Lock()
		delete(a.ops, token)
		a.opsMu.Unlock()
	}
}

func (a *App) op(token string) (Op, bool) {
	if token == "" {
		return Op{}, false
	}
	a.opsMu.Lock()
	defer a.opsMu.Unlock()
	op, ok := a.ops[token]
	if !ok || time.Now().After(op.expires) {
		return Op{}, false
	}
	return op, true
}

// InternalInfo is how hooks reach the running server.
type InternalInfo struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
}

func internalInfoPath(dataDir string) string { return filepath.Join(dataDir, "run", "internal.json") }

// SetInternal records the internal endpoint, in memory for git commands the
// server starts and on disk for git commands run by hand on the server.
func (a *App) SetInternal(url, secret string) error {
	a.internalURL, a.internalSecret = url, secret
	b, _ := json.Marshal(InternalInfo{URL: url, Secret: secret})
	return os.WriteFile(internalInfoPath(a.cfg.DataDir), b, 0o600)
}

// ClearInternal removes the on-disk endpoint at shutdown.
func (a *App) ClearInternal() { os.Remove(internalInfoPath(a.cfg.DataDir)) }

// InternalSecret is the shared secret hooks present.
func (a *App) InternalSecret() string { return a.internalSecret }

// ReadInternalInfo loads the endpoint a running server left on disk.
func ReadInternalInfo(dataDir string) (InternalInfo, error) {
	var info InternalInfo
	b, err := os.ReadFile(internalInfoPath(dataDir))
	if err != nil {
		return info, err
	}
	return info, json.Unmarshal(b, &info)
}

// HookEnv is the environment that lets a hook tie a ref update back to op.
func (a *App) HookEnv(token string) []string {
	return []string{
		"GITSYNTH_OP=" + token,
		"GITSYNTH_INTERNAL_URL=" + a.internalURL,
		"GITSYNTH_INTERNAL_SECRET=" + a.internalSecret,
	}
}

// RefUpdate is one line of a reference transaction.
type RefUpdate struct {
	Old string `json:"old"`
	New string `json:"new"`
	Ref string `json:"ref"`
}

// RefTxnRequest is what the reference-transaction hook sends.
type RefTxnRequest struct {
	State    string      `json:"state"`
	RepoPath string      `json:"repo_path"`
	Op       string      `json:"op"`
	Updates  []RefUpdate `json:"updates"`
}

func updatesKey(updates []RefUpdate) string {
	sorted := append([]RefUpdate(nil), updates...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Ref < sorted[j].Ref })
	return mustJSON(sorted)
}

// HandleRefTxn records a reference transaction. In the prepared state the
// event is written before git commits the update; if it cannot be written,
// or policy forbids the update, the error makes the hook abort the update.
// So no ref changes without a record.
func (a *App) HandleRefTxn(ctx context.Context, req RefTxnRequest) error {
	repo, err := a.RepoByPath(ctx, req.RepoPath)
	if err != nil {
		return err
	}
	var updates []RefUpdate
	for _, u := range req.Updates {
		if strings.HasPrefix(u.Ref, "refs/") && gitx.IsOID(u.Old) && gitx.IsOID(u.New) {
			updates = append(updates, u)
		}
	}
	if len(updates) == 0 {
		return nil
	}
	op, known := a.op(req.Op)
	actor, source := UnattributedActor, "direct"
	if known {
		actor, source = op.Actor, op.Source
		if actor.Session == "" {
			actor.Session = op.Session
		}
	}
	key := updatesKey(updates)
	switch req.State {
	case "prepared":
		var foreign []string
		if known {
			// A refused attempt is activity too, so it is recorded before
			// the refusal goes back to git.
			perr := a.checkRefPolicy(ctx, op, repo, updates)
			if perr == nil && op.Source == "push" {
				foreign, perr = a.checkCommitters(ctx, op, repo, updates)
			}
			if perr != nil {
				_, err := a.Store.Write(ctx, actor, func(tx *store.Tx) error {
					_, err := tx.Append("git.ref_transaction_rejected", repo.scope(), map[string]any{
						"updates": updates, "source": source, "reason": perr.Error(),
					})
					return err
				})
				if err != nil {
					return err
				}
				return perr
			}
		}
		_, err := a.Store.Write(ctx, actor, func(tx *store.Tx) error {
			payload := map[string]any{"updates": updates, "source": source}
			if op.OnBehalfOf != "" {
				payload["on_behalf_of"] = op.OnBehalfOf
			}
			if len(foreign) > 0 {
				// Commits whose committer is not the pusher or an identity
				// it controls, let in by a repository admin: imported history.
				payload["foreign_commits"] = foreign
			}
			e, err := tx.Append("git.ref_transaction", repo.scope(), payload)
			if err != nil {
				return err
			}
			_, err = tx.Exec(`INSERT INTO ref_txns (seq, repo, state, updates) VALUES (?, ?, 'prepared', ?)`, e.Seq, repo.FullName, key)
			return err
		})
		return err
	case "committed", "aborted":
		_, err := a.Store.Write(ctx, actor, func(tx *store.Tx) error {
			var seq int64
			err := tx.QueryRowContext(ctx, `SELECT seq FROM ref_txns WHERE repo = ? AND state = 'prepared' AND updates = ? ORDER BY seq DESC LIMIT 1`,
				repo.FullName, key).Scan(&seq)
			if errors.Is(err, sql.ErrNoRows) {
				// An update refused in the prepared state comes back as
				// aborted with nothing to settle; anything else is odd.
				if req.State == "committed" {
					slog.Warn("committed ref transaction without a prepared record", "repo", repo.FullName)
				}
				return nil
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE ref_txns SET state = ? WHERE seq = ?`, req.State, seq); err != nil {
				return err
			}
			if req.State == "aborted" {
				_, err := tx.Append("git.ref_transaction_aborted", repo.scope(), map[string]any{"txn": seq})
				return err
			}
			return applyRefs(tx, repo.FullName, updates)
		})
		return err
	}
	return nil
}

func applyRefs(tx *store.Tx, repo string, updates []RefUpdate) error {
	for _, u := range updates {
		var err error
		if gitx.IsZeroOID(u.New) {
			_, err = tx.Exec(`DELETE FROM refs WHERE repo = ? AND ref = ?`, repo, u.Ref)
		} else {
			_, err = tx.Exec(`INSERT INTO refs (repo, ref, oid) VALUES (?, ?, ?) ON CONFLICT (repo, ref) DO UPDATE SET oid = excluded.oid`, repo, u.Ref, u.New)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkCommitters holds every commit a push brings in to the rule that its
// committer is the pushing identity or one it controls. The push is
// authenticated, so the server knows who pushed; the committer is what the
// commit itself says. A repository admin may bring in other commits (an
// existing project's history, an upstream mirror); those come back as
// foreign so the record says so.
func (a *App) checkCommitters(ctx context.Context, op Op, repo Repo, updates []RefUpdate) ([]string, error) {
	args := []string{"log", "--format=%H%x00%ce%x00%cn"}
	n := 0
	for _, u := range updates {
		if !gitx.IsZeroOID(u.New) {
			args = append(args, u.New)
			n++
		}
	}
	if n == 0 {
		return nil, nil
	}
	// At the prepared state the pushed objects are in the repository but no
	// ref points at them yet, so "--not --all" leaves exactly what is new.
	out, err := a.Git.Output(ctx, repo.Path, nil, append(args, "--not", "--all")...)
	if err != nil {
		return nil, err
	}
	pusher := Actor{Actor: op.Actor}
	db := a.Store.DB()
	admin, err := a.Can(ctx, db, pusher, repo.Org, repo.FullName, PermRepoAdmin)
	if err != nil {
		return nil, err
	}
	var foreign []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\x00")
		if len(f) < 3 {
			continue
		}
		sha, email := f[0], f[1]
		identity, found, err := a.ResolveEmail(ctx, db, email)
		if err != nil {
			return nil, err
		}
		ok := false
		if found {
			if identity == pusher.Identity {
				ok = true
			} else if ok, err = a.controls(ctx, db, pusher, ident.MustParse(identity)); err != nil {
				return nil, err
			}
		}
		if ok {
			continue
		}
		if admin {
			foreign = append(foreign, sha)
			continue
		}
		who := email
		if found {
			who = identity
		}
		return nil, errf(http.StatusForbidden,
			"commit %s is committed as %s, which is not %s or an identity it controls; commit with GIT_COMMITTER_EMAIL=%s or an address registered to it",
			short(sha), who, pusher.Identity, a.CommitEmail(ctx, pusher.Identity))
	}
	return foreign, nil
}

// checkRefPolicy guards the default branch: it moves only through merges or
// by a repository admin's hand.
func (a *App) checkRefPolicy(ctx context.Context, op Op, repo Repo, updates []RefUpdate) error {
	if op.Internal {
		return nil
	}
	protected := "refs/heads/" + repo.DefaultBranch
	for _, u := range updates {
		if u.Ref != protected {
			continue
		}
		actor := Actor{Actor: op.Actor}
		ok, err := a.Can(ctx, a.Store.DB(), actor, repo.Org, repo.FullName, PermRepoAdmin)
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusForbidden, "%s is protected: it moves through merged change sets, or by a repository admin", protected)
		}
	}
	return nil
}

// Reconcile compares the records with the repositories on disk. It settles
// transactions left half-way by a crash, and records any ref that changed
// without passing through the server as an unattributed change.
func (a *App) Reconcile(ctx context.Context) error {
	repos, err := a.Repos(ctx)
	if err != nil {
		return err
	}
	for _, repo := range repos {
		actual, err := a.listRefs(ctx, repo)
		if err != nil {
			return err
		}
		if err := a.settlePending(ctx, repo, actual); err != nil {
			return err
		}
		if err := a.recordDrift(ctx, repo, actual); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) listRefs(ctx context.Context, repo Repo) (map[string]string, error) {
	out, err := a.Git.Output(ctx, repo.Path, nil, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if oid, ref, ok := strings.Cut(line, " "); ok {
			refs[ref] = oid
		}
	}
	return refs, nil
}

func (a *App) settlePending(ctx context.Context, repo Repo, actual map[string]string) error {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT seq, updates FROM ref_txns WHERE repo = ? AND state = 'prepared' ORDER BY seq`, repo.FullName)
	if err != nil {
		return err
	}
	type pending struct {
		seq     int64
		updates []RefUpdate
	}
	var list []pending
	for rows.Next() {
		var p pending
		var raw string
		if err := rows.Scan(&p.seq, &raw); err != nil {
			rows.Close()
			return err
		}
		if err := json.Unmarshal([]byte(raw), &p.updates); err != nil {
			rows.Close()
			return err
		}
		list = append(list, p)
	}
	rows.Close()
	for _, p := range list {
		applied := true
		for _, u := range p.updates {
			want := u.New
			if gitx.IsZeroOID(want) {
				want = ""
			}
			if actual[u.Ref] != want {
				applied = false
			}
		}
		_, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
			state := "aborted"
			if applied {
				state = "committed"
			}
			if _, err := tx.Exec(`UPDATE ref_txns SET state = ? WHERE seq = ?`, state, p.seq); err != nil {
				return err
			}
			if applied {
				return applyRefs(tx, repo.FullName, p.updates)
			}
			_, err := tx.Append("git.ref_transaction_aborted", repo.scope(), map[string]any{"txn": p.seq, "reason": "reconciled at startup"})
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (a *App) recordDrift(ctx context.Context, repo Repo, actual map[string]string) error {
	rows, err := a.Store.DB().QueryContext(ctx, `SELECT ref, oid FROM refs WHERE repo = ?`, repo.FullName)
	if err != nil {
		return err
	}
	recorded := map[string]string{}
	for rows.Next() {
		var ref, oid string
		if err := rows.Scan(&ref, &oid); err != nil {
			rows.Close()
			return err
		}
		recorded[ref] = oid
	}
	rows.Close()
	zero := strings.Repeat("0", 40)
	var drift []RefUpdate
	for ref, oid := range actual {
		if recorded[ref] != oid {
			old := recorded[ref]
			if old == "" {
				old = zero
			}
			drift = append(drift, RefUpdate{Old: old, New: oid, Ref: ref})
		}
	}
	for ref, oid := range recorded {
		if _, ok := actual[ref]; !ok {
			drift = append(drift, RefUpdate{Old: oid, New: zero, Ref: ref})
		}
	}
	if len(drift) == 0 {
		return nil
	}
	sort.Slice(drift, func(i, j int) bool { return drift[i].Ref < drift[j].Ref })
	_, err = a.Store.Write(ctx, UnattributedActor, func(tx *store.Tx) error {
		if _, err := tx.Append("git.refs_unattributed", repo.scope(), map[string]any{"updates": drift, "found_by": "reconcile"}); err != nil {
			return err
		}
		return applyRefs(tx, repo.FullName, drift)
	})
	if err == nil {
		slog.Warn("recorded unattributed ref changes", "repo", repo.FullName, "count", len(drift))
	}
	return err
}
