package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestEndToEnd drives a real server with the real git client: a user and
// their agent work on one issue under separate identities, a CI service
// reports checks, and every step is checked against the event log and the
// access log.
func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	h := newHarness(t, dir)

	aliceTok := tokenFrom(t, h.run("admin", "init", "--data", h.data, "--user", "alice"))
	h.start()
	alice := h.client(aliceTok)

	me := ok[map[string]any](t, alice, "GET", "/api/me", nil)
	if me["identity"] != "user:alice" || me["kind"] != "user" || me["admin"] != true {
		t.Fatalf("unexpected /api/me: %v", me)
	}

	ok[map[string]any](t, alice, "POST", "/api/orgs", map[string]string{"name": "acme"})
	ok[map[string]any](t, alice, "POST", "/api/orgs/acme/repos", map[string]string{"name": "app"})

	// Alice seeds main by hand. She administers the repository, so the
	// protected branch lets her through.
	aliceWork := filepath.Join(dir, "alice")
	h.git(dir, "clone", h.cloneURL(aliceTok), aliceWork)
	h.commit(aliceWork, "alice", "README.md", "# app\n", "initial commit")
	h.git(aliceWork, "push", "origin", "HEAD:main")

	// Alice makes her Claude agent an identity of its own, with write but
	// not merge, and hands it a standing token. The agent's run opens a
	// session with it and works through the session's credential.
	created := ok[map[string]string](t, alice, "POST", "/api/identities", map[string]string{"label": "claude"})
	if created["id"] != "user:alice/claude" {
		t.Fatalf("unexpected identity %q", created["id"])
	}
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/grants", map[string]string{"identity": "user:alice/claude", "set": "write"})
	standing := ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude", "ttl_hours": 8})
	agent := h.client(standing["token"].(string))
	issued := ok[openedSession](t, agent, "POST", "/api/sessions", map[string]any{"label": "cc-1"})
	sessID := issued.Session.ID
	claudeTok := issued.Token
	claude := h.client(claudeTok)
	cme := ok[map[string]any](t, claude, "GET", "/api/me", nil)
	if cme["identity"] != "user:alice/claude" || cme["session"] != sessID {
		t.Fatalf("the session's credential should carry its identity and session: %v", cme)
	}
	// An agent makes no identities, below itself or beside it, and issues
	// no standing tokens.
	expectStatus(t, claude, "POST", "/api/identities", map[string]string{"label": "explorer"}, http.StatusForbidden)
	expectStatus(t, claude, "POST", "/api/identities", map[string]string{"parent": "user:alice", "label": "other"}, http.StatusForbidden)
	expectStatus(t, agent, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"}, http.StatusForbidden)

	// Idea -> issue -> plan.
	idea := ok[map[string]int](t, alice, "POST", "/api/orgs/acme/ideas", map[string]any{"title": "greet people", "body": "a hello file", "repos": []string{"app"}})
	conv := ok[map[string]any](t, alice, "POST", "/api/orgs/acme/ideas/1/convert", map[string]string{"repo": "app"})
	if idea["number"] != 1 || conv["issue"] != float64(1) {
		t.Fatalf("unexpected idea/issue numbers: %v %v", idea, conv)
	}
	ideaView := ok[map[string]any](t, alice, "GET", "/api/orgs/acme/ideas/1", nil)
	if issues := ideaView["issues"].([]any); len(issues) != 1 {
		t.Fatalf("idea should link to its issue: %v", ideaView)
	}
	ok[map[string]int](t, claude, "POST", "/api/repos/acme/app/issues/1/plans", map[string]any{
		"title": "add hello", "steps": []map[string]string{{"title": "write hello.txt"}, {"title": "mention it in README"}},
	})

	// Claude pushes a branch; main stays out of its reach. Its commits name
	// it as committer: once by its canonical address, once by an address it
	// registered.
	if standing["committer_email"] != "user:alice/claude@localhost" || issued.CommitterEmail != "user:alice/claude@localhost" {
		t.Fatalf("tokens and sessions should come with the identity's commit address: %v %+v", standing, issued)
	}
	ok[map[string]string](t, claude, "POST", "/api/emails", map[string]string{"email": "claude@example.test"})
	claudeWork := filepath.Join(dir, "claude")
	h.git(dir, "clone", h.cloneURL(claudeTok), claudeWork)
	h.commitAs(claudeWork, "claude", "user:alice/claude@localhost", "hello.txt", "hello\n", "add hello")
	h.commit(claudeWork, "claude", "README.md", "# app\n\nSee hello.txt.\n", "mention hello")
	h.git(claudeWork, "push", "origin", "HEAD:refs/heads/feat")
	if out, err := h.gitErr(claudeWork, "push", "origin", "HEAD:main"); err == nil {
		t.Fatalf("agent push to main should be rejected:\n%s", out)
	}

	// Claude cannot slip in a commit under alice's address: the push says
	// who pushed, and alice is not an identity claude controls.
	ok[map[string]string](t, alice, "POST", "/api/emails", map[string]string{"email": "alice@example.test"})
	expectStatus(t, claude, "POST", "/api/emails", map[string]string{"email": "alice@example.test"}, http.StatusConflict)
	sneaky := filepath.Join(dir, "sneaky")
	h.git(dir, "clone", h.cloneURL(claudeTok), sneaky)
	h.commit(sneaky, "alice", "sneaky.txt", "x\n", "looks like alice")
	if out, err := h.gitErr(sneaky, "push", "origin", "HEAD:refs/heads/sneaky"); err == nil {
		t.Fatalf("a commit under alice's address pushed by claude should be refused:\n%s", out)
	}

	ok[map[string]int](t, claude, "POST", "/api/repos/acme/app/issues/1/changesets", map[string]any{
		"title": "hello", "branch": "feat", "plan": 1, "steps": []string{"s1", "s2"},
	})
	ok[map[string]any](t, alice, "PUT", "/api/repos/acme/app/merge-rule", map[string]any{"required_approvals": 1, "required_checks": []string{"ci"}})

	// Merging needs both the permission and the records the rule asks for.
	expectStatus(t, claude, "POST", "/api/repos/acme/app/changesets/1/merge", nil, http.StatusForbidden)
	expectStatus(t, alice, "POST", "/api/repos/acme/app/changesets/1/merge", nil, http.StatusConflict)

	// CI is a service: registered on the server from a manifest, installed
	// into the org with the permissions the manifest asks for.
	ok[map[string]any](t, alice, "POST", "/api/services", map[string]any{
		"name": "ci", "description": "runs the tests", "permissions": []string{"repo.read", "check.write"}, "events": []string{"git.ref_transaction"},
	})
	ok[map[string]any](t, alice, "POST", "/api/orgs/acme/services", map[string]string{"service": "service:ci"})
	ci := h.client(ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "service:ci"})["token"].(string))
	expectStatus(t, ci, "POST", "/api/repos/acme/app/issues", map[string]string{"title": "not mine to open"}, http.StatusForbidden)
	cs := ok[changeSetView](t, alice, "GET", "/api/repos/acme/app/changesets/1", nil)
	ok[map[string]any](t, ci, "POST", "/api/repos/acme/app/checks", map[string]string{"commit": cs.Status.Head, "name": "ci", "status": "success"})
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/changesets/1/reviews", map[string]any{
		"verdict": "approve", "body": "looks good",
		"comments": []map[string]any{{"path": "hello.txt", "line": 1, "side": "new", "body": "nice greeting"}},
	})
	cs = ok[changeSetView](t, alice, "GET", "/api/repos/acme/app/changesets/1", nil)
	if !cs.Status.Mergeable {
		t.Fatalf("change set should be mergeable now: %+v", cs.Status)
	}
	if len(cs.Reviews) != 1 || len(cs.Reviews[0].Comments) != 1 || cs.Reviews[0].Comments[0].Path != "hello.txt" {
		t.Fatalf("review should carry its line comment: %+v", cs.Reviews)
	}
	merged := ok[map[string]string](t, alice, "POST", "/api/repos/acme/app/changesets/1/merge", nil)["commit"]

	// The merge is a real merge commit with both parents.
	h.git(aliceWork, "fetch", "origin")
	parents := strings.Fields(h.git(aliceWork, "rev-list", "--parents", "-n1", "origin/main"))
	if len(parents) != 3 || parents[0] != merged || parents[2] != cs.Status.Head {
		t.Fatalf("main should be the merge commit %s with parents (base, %s): %v", merged, cs.Status.Head, parents)
	}
	if who := h.git(aliceWork, "log", "-1", "--format=%ce %ae", "origin/main"); who != "service:gitsynth@localhost user:alice@localhost" {
		t.Fatalf("the server commits the merge, on behalf of alice: %q", who)
	}

	// Progress comes from the records: both steps have a merged change set.
	issue := ok[issueView](t, alice, "GET", "/api/repos/acme/app/issues/1", nil)
	if len(issue.Plans) != 1 || !issue.Plans[0].Complete || len(issue.Hints) == 0 {
		t.Fatalf("plan should be complete with a closing hint: %+v", issue)
	}

	// Code can be read through the API, always pinned to a commit.
	repoInfo := ok[struct {
		Refs []struct {
			Name   string `json:"name"`
			Commit string `json:"commit"`
		} `json:"refs"`
		Permissions map[string]bool `json:"permissions"`
	}](t, alice, "GET", "/api/repos/acme/app", nil)
	if len(repoInfo.Refs) != 2 || !repoInfo.Permissions["admin"] {
		t.Fatalf("expected main and feat, with alice as admin: %+v", repoInfo)
	}
	tree := ok[struct {
		Commit  string `json:"commit"`
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}](t, alice, "GET", "/api/repos/acme/app/tree?ref=main", nil)
	if tree.Commit != merged || len(tree.Entries) != 2 {
		t.Fatalf("tree of main should list README.md and hello.txt at %s: %+v", merged, tree)
	}
	blob := ok[struct {
		Blob struct {
			Content string `json:"content"`
		} `json:"blob"`
	}](t, alice, "GET", "/api/repos/acme/app/blob?ref=main&path=hello.txt", nil)
	if blob.Blob.Content != "hello\n" {
		t.Fatalf("unexpected blob: %+v", blob)
	}
	log := ok[struct {
		Commits []struct {
			Subject           string `json:"subject"`
			CommitterIdentity string `json:"committer_identity"`
		} `json:"commits"`
	}](t, alice, "GET", "/api/repos/acme/app/commits?ref=main", nil)
	if len(log.Commits) != 4 || !strings.HasPrefix(log.Commits[0].Subject, "Merge change set #1") {
		t.Fatalf("unexpected log: %+v", log)
	}
	for i, want := range []string{"service:gitsynth", "user:alice/claude", "user:alice/claude", "user:alice"} {
		if log.Commits[i].CommitterIdentity != want {
			t.Fatalf("commit %d (%s) should resolve to %s, got %q", i, log.Commits[i].Subject, want, log.Commits[i].CommitterIdentity)
		}
	}
	diff := ok[struct {
		Files []struct {
			Path      string `json:"path"`
			Status    string `json:"status"`
			Additions int    `json:"additions"`
			Patch     string `json:"patch"`
		} `json:"files"`
	}](t, alice, "GET", "/api/repos/acme/app/changesets/1/diff", nil)
	if len(diff.Files) != 2 || diff.Files[1].Path != "hello.txt" || diff.Files[1].Status != "A" || !strings.Contains(diff.Files[1].Patch, "+hello") {
		t.Fatalf("diff of the merged change set should still show its two files: %+v", diff)
	}
	h.waitAccess("user:alice", func(e accessEntry) bool { return e.Kind == "code.read" })

	// Lists.
	if orgs := ok[map[string][]string](t, alice, "GET", "/api/orgs", nil)["orgs"]; !slices.Equal(orgs, []string{"acme"}) {
		t.Fatalf("unexpected orgs: %v", orgs)
	}
	if css := ok[[]map[string]any](t, claude, "GET", "/api/repos/acme/app/changesets", nil); len(css) != 1 || css[0]["merged"] != true {
		t.Fatalf("unexpected change set list: %v", css)
	}
	ids := ok[[]map[string]any](t, alice, "GET", "/api/identities", nil)
	if len(ids) != 2 || ids[0]["id"] != "user:alice" || ids[1]["id"] != "user:alice/claude" {
		t.Fatalf("unexpected identities: %v", ids)
	}

	// A ref changed by hand on the server, while it runs, is recorded live.
	repoPath := h.repoPath()
	h.git(repoPath, "update-ref", "refs/heads/stray", merged)

	// The event log tells who did what.
	evs := h.events(alice)
	requireEvent(t, evs, "agent push of feat, with its session", func(e event) bool {
		return e.Type == "git.ref_transaction" && e.Identity == "user:alice/claude" && e.Session == sessID && touches(e, "refs/heads/feat")
	})
	requireEvent(t, evs, "alice's first push brought in history the server could not attribute", func(e event) bool {
		_, imported := e.Payload["foreign_commits"]
		return e.Type == "git.ref_transaction" && e.Identity == "user:alice" && imported && touches(e, "refs/heads/main")
	})
	requireEvent(t, evs, "refused commit under alice's address", func(e event) bool {
		reason, _ := e.Payload["reason"].(string)
		return e.Type == "git.ref_transaction_rejected" && e.Identity == "user:alice/claude" && strings.Contains(reason, "GIT_COMMITTER_EMAIL=user:alice/claude@localhost")
	})
	requireEvent(t, evs, "refused agent push to main", func(e event) bool {
		return e.Type == "git.ref_transaction_rejected" && e.Identity == "user:alice/claude" && touches(e, "refs/heads/main")
	})
	requireEvent(t, evs, "merge ref update by the built-in service on behalf of alice", func(e event) bool {
		return e.Type == "git.ref_transaction" && e.Identity == "service:gitsynth" && e.Payload["on_behalf_of"] == "user:alice" && e.Payload["source"] == "merge"
	})
	requireEvent(t, evs, "merge recorded by alice", func(e event) bool {
		return e.Type == "changeset.merged" && e.Identity == "user:alice" && e.Payload["commit"] == merged
	})
	requireEvent(t, evs, "check reported by the ci service", func(e event) bool {
		return e.Type == "check.reported" && e.Identity == "service:ci"
	})
	requireEvent(t, evs, "hand-made ref change marked unattributed", func(e event) bool {
		return e.Type == "git.ref_transaction" && e.Identity == "unattributed" && touches(e, "refs/heads/stray")
	})

	// Reads are recorded too, down to the commits a fetch asked for.
	h.git(claudeWork, "fetch", "origin")
	h.waitAccess("user:alice/claude", func(e accessEntry) bool {
		return e.Kind == "git.fetch" && e.Session == sessID && len(e.Detail.Wants) > 0
	})

	if out := h.run("admin", "verify", "--data", h.data); !strings.HasPrefix(out, "ok:") {
		t.Fatalf("hash chain should verify: %s", out)
	}

	// A different role gets an identity of its own: alice derives one below
	// claude's and hands it a token. It inherits what claude may do, and
	// never more.
	ok[map[string]string](t, alice, "POST", "/api/identities", map[string]string{"parent": "user:alice/claude", "label": "explorer"})
	explorer := h.client(ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude/explorer"})["token"].(string))
	expectStatus(t, explorer, "POST", "/api/identities", map[string]string{"label": "deeper"}, http.StatusForbidden)
	ok[map[string]any](t, explorer, "POST", "/api/repos/acme/app/issues/1/comments", map[string]string{"body": "found the greeting"})
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/grants", map[string]string{"identity": "user:alice/claude/explorer", "set": "admin"})
	expectStatus(t, explorer, "PUT", "/api/repos/acme/app/merge-rule", map[string]any{"required_approvals": 0}, http.StatusForbidden)
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/grants", map[string]string{"identity": "user:alice/claude/explorer", "set": "read"})
	expectStatus(t, explorer, "POST", "/api/repos/acme/app/issues/1/comments", map[string]string{"body": "now read-only"}, http.StatusForbidden)

	// A bot alice owns acts as itself. Its identities and their tokens are
	// made by alice, its owner, never by the bot.
	ok[map[string]string](t, alice, "POST", "/api/bots", map[string]string{"name": "devbox"})
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/grants", map[string]string{"identity": "bot:devbox", "set": "write"})
	devbox := h.client(ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "bot:devbox"})["token"].(string))
	expectStatus(t, devbox, "POST", "/api/identities", map[string]string{"label": "triage"}, http.StatusForbidden)
	ok[map[string]string](t, alice, "POST", "/api/identities", map[string]string{"parent": "bot:devbox", "label": "triage"})
	expectStatus(t, devbox, "POST", "/api/tokens", map[string]any{"identity": "bot:devbox/triage"}, http.StatusForbidden)
	triageStanding := h.client(ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "bot:devbox/triage"})["token"].(string))
	alert := ok[openedSession](t, triageStanding, "POST", "/api/sessions", map[string]any{"label": "alert-42"})
	triage := h.client(alert.Token)
	ok[map[string]int](t, triage, "POST", "/api/orgs/acme/ideas", map[string]any{"title": "the error rate doubled", "repos": []string{"app"}})
	expectStatus(t, devbox, "POST", "/api/bots", map[string]string{"name": "spawned", "owner": "user:alice"}, http.StatusForbidden)

	// The session's credential is listed with its session and label.
	creds := ok[[]map[string]any](t, alice, "GET", "/api/tokens", nil)
	if i := slices.IndexFunc(creds, func(c map[string]any) bool {
		return c["kind"] == "session" && c["session"] == sessID && c["label"] == "cc-1" && c["identity"] == "user:alice/claude"
	}); i < 0 {
		t.Fatalf("the session's credential should be listed: %v", creds)
	}

	// Alice revokes the agent's standing token; the agent, and the session
	// its run opened with it, are locked out at once.
	standingID := standing["id"].(string)
	expectStatus(t, explorer, "POST", "/api/tokens/"+standingID+"/revoke", nil, http.StatusNotFound)
	ok[map[string]any](t, alice, "POST", "/api/tokens/"+standingID+"/revoke", nil)
	expectStatus(t, agent, "GET", "/api/me", nil, http.StatusUnauthorized)
	expectStatus(t, claude, "GET", "/api/me", nil, http.StatusUnauthorized)

	// Disabling an identity stops everything derived from it as well.
	ok[map[string]any](t, explorer, "GET", "/api/me", nil)
	ok[map[string]any](t, alice, "POST", "/api/identities/disable", map[string]string{"id": "user:alice/claude"})
	expectStatus(t, explorer, "GET", "/api/me", nil, http.StatusUnauthorized)

	evs = h.events(alice)
	requireEvent(t, evs, "an idea filed by the bot's derived identity, with its session", func(e event) bool {
		return e.Type == "idea.created" && e.Identity == "bot:devbox/triage" && e.Session == alert.Session.ID
	})
	requireEvent(t, evs, "the bot's session, opened with its label", func(e event) bool {
		return e.Type == "session.opened" && e.Identity == "bot:devbox/triage" && e.Payload["id"] == alert.Session.ID && e.Payload["label"] == "alert-42"
	})

	// A ref changed while the server is down is caught when it starts.
	h.stop()
	h.git(repoPath, "update-ref", "refs/heads/offline", merged)
	h.start()
	evs = h.events(alice)
	requireEvent(t, evs, "offline ref change found at startup", func(e event) bool {
		return e.Type == "git.refs_unattributed" && e.Identity == "unattributed" && touches(e, "refs/heads/offline")
	})
	if out := h.run("admin", "verify", "--data", h.data); !strings.HasPrefix(out, "ok:") {
		t.Fatalf("hash chain should still verify: %s", out)
	}
}

// TestStartRepairsHooks moves the binary out from under a repository's hook
// while the server is down. The hook can no longer start, so no ref can
// change, until the server starts again and points it at itself.
func TestStartRepairsHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	h := newHarness(t, dir)
	aliceTok := tokenFrom(t, h.run("admin", "init", "--data", h.data, "--user", "alice"))
	h.start()
	alice := h.client(aliceTok)
	ok[map[string]any](t, alice, "POST", "/api/orgs", map[string]string{"name": "acme"})
	ok[map[string]any](t, alice, "POST", "/api/orgs/acme/repos", map[string]string{"name": "app"})
	work := filepath.Join(dir, "alice")
	h.git(dir, "clone", h.cloneURL(aliceTok), work)
	h.commitAs(work, "alice", "user:alice@localhost", "README.md", "# app\n", "initial commit")
	h.git(work, "push", "origin", "HEAD:main")

	h.stop()
	repoPath := h.repoPath()
	missing := filepath.Join(dir, "moved-away", "gitsynth")
	hook := "#!/bin/sh\nexec '" + missing + "' hook --data '" + h.data + "' reference-transaction \"$@\"\n"
	if err := os.WriteFile(filepath.Join(repoPath, "hooks", "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := h.gitErr(repoPath, "update-ref", "refs/heads/stray", "refs/heads/main"); err == nil {
		t.Fatalf("a hook that cannot start should stop every ref update:\n%s", out)
	}

	h.start() // on a new port
	h.git(work, "remote", "set-url", "origin", h.cloneURL(aliceTok))
	h.commitAs(work, "alice", "user:alice@localhost", "README.md", "# app\n\nStill here.\n", "after the move")
	h.git(work, "push", "origin", "HEAD:main")
	head := h.git(work, "rev-parse", "HEAD")
	requireEvent(t, h.events(alice), "the push after the restart", func(e event) bool {
		if e.Type != "git.ref_transaction" || e.Identity != "user:alice" {
			return false
		}
		ups, _ := e.Payload["updates"].([]any)
		for _, u := range ups {
			if m, _ := u.(map[string]any); m["ref"] == "refs/heads/main" && m["new"] == head {
				return true
			}
		}
		return false
	})
}

// TestServerIgnoresItsGitConfig runs the server as a user whose git config
// points core.hooksPath at an empty directory, both in the global config file
// and through GIT_CONFIG_PARAMETERS. git would then skip the repository's
// reference-transaction hook; the server's git must not see that config, so
// a push is still recorded through the hook.
func TestServerIgnoresItsGitConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	h := newHarness(t, dir)
	noHooks := filepath.Join(dir, "no-hooks")
	if err := os.MkdirAll(noHooks, 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(dir, "server-gitconfig")
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = "+noHooks+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.serverEnv = []string{
		"GIT_CONFIG_GLOBAL=" + global,
		"GIT_CONFIG_PARAMETERS='core.hooksPath'='" + noHooks + "'",
	}

	aliceTok := tokenFrom(t, h.run("admin", "init", "--data", h.data, "--user", "alice"))
	h.start()
	alice := h.client(aliceTok)
	ok[map[string]any](t, alice, "POST", "/api/orgs", map[string]string{"name": "acme"})
	ok[map[string]any](t, alice, "POST", "/api/orgs/acme/repos", map[string]string{"name": "app"})
	work := filepath.Join(dir, "alice")
	h.git(dir, "clone", h.cloneURL(aliceTok), work)
	h.commitAs(work, "alice", "user:alice@localhost", "README.md", "# app\n", "initial commit")
	h.git(work, "push", "origin", "HEAD:main")
	head := h.git(work, "rev-parse", "HEAD")
	requireEvent(t, h.events(alice), "the push, recorded through the hook", func(e event) bool {
		if e.Type != "git.ref_transaction" || e.Identity != "user:alice" {
			return false
		}
		ups, _ := e.Payload["updates"].([]any)
		for _, u := range ups {
			if m, _ := u.(map[string]any); m["ref"] == "refs/heads/main" && m["new"] == head {
				return true
			}
		}
		return false
	})
}

// ---- harness ----

type harness struct {
	t         *testing.T
	bin       string
	data      string
	env       []string
	serverEnv []string // added to env for the server only
	base      string   // where the tests reach the server
	url       string   // the server's public URL: localhost, on the port it listens on
	srv       *exec.Cmd
	logs      *bytes.Buffer
	// sshAddr is where serve takes git over SSH, when a test asks for it
	// with useSSH before start; it stays the same across restarts.
	sshAddr string
}

// useSSH has the server serve git over SSH on a free loopback port too.
func (h *harness) useSSH() {
	h.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	h.sshAddr = ln.Addr().String()
	ln.Close()
}

func newHarness(t *testing.T, dir string) *harness {
	h := &harness{t: t, bin: filepath.Join(dir, "gitsynth"), data: filepath.Join(dir, "data")}
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Keep the user's git config, credential helpers and proxies out of it.
	// An ssh command from the environment would override the one a clone's
	// own config names, which is what the SSH tests are about.
	h.env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return name == "GIT_SSH_COMMAND" || name == "GIT_SSH" || name == "GIT_SSH_VARIANT"
	}),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(home, ".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=credential.helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=init.defaultBranch", "GIT_CONFIG_VALUE_1=main",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost",
	)
	build := exec.Command("go", "build", "-o", h.bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		h.stop()
		if t.Failed() && h.logs != nil {
			t.Logf("server log:\n%s", h.logs.String())
		}
	})
	return h
}

func (h *harness) run(args ...string) string {
	h.t.Helper()
	cmd := exec.Command(h.bin, args...)
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		h.t.Fatalf("gitsynth %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func (h *harness) start() {
	h.t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		h.t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	h.base = "http://" + addr
	_, port, _ := net.SplitHostPort(addr)
	h.url = "http://localhost:" + port
	if h.logs == nil {
		h.logs = &bytes.Buffer{}
	}
	args := []string{"serve", "--data", h.data, "--listen", addr}
	if h.sshAddr != "" {
		args = append(args, "--ssh", h.sshAddr)
	}
	cmd := exec.Command(h.bin, args...)
	cmd.Env = append(slices.Clip(h.env), h.serverEnv...)
	cmd.Stdout, cmd.Stderr = h.logs, h.logs
	if err := cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.srv = cmd
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(h.base + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("server did not come up: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *harness) stop() {
	if h.srv == nil {
		return
	}
	h.srv.Process.Signal(os.Interrupt)
	h.srv.Wait()
	h.srv = nil
}

func (h *harness) cloneURL(token string) string { return h.repoURL(token, "acme/app") }

func (h *harness) repoURL(token, repo string) string {
	return strings.Replace(h.base, "http://", "http://x:"+token+"@", 1) + "/git/" + repo + ".git"
}

func (h *harness) gitErr(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = h.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String() + stderr.String(), err
	}
	return stdout.String(), nil
}

func (h *harness) git(dir string, args ...string) string {
	h.t.Helper()
	out, err := h.gitErr(dir, args...)
	if err != nil {
		h.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(out)
}

func (h *harness) commit(dir, who, file, content, msg string) {
	h.t.Helper()
	h.commitAs(dir, who, who+"@example.test", file, content, msg)
}

func (h *harness) commitAs(dir, name, email, file, content, msg string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(dir, "add", file)
	h.git(dir, "-c", "user.name="+name, "-c", "user.email="+email, "commit", "-q", "-m", msg)
}

func (h *harness) repoPath() string {
	h.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(h.data, "repos", "*.git"))
	if len(matches) != 1 {
		h.t.Fatalf("expected one repository on disk, found %v", matches)
	}
	return matches[0]
}

type event struct {
	Seq      int64          `json:"seq"`
	Type     string         `json:"type"`
	Identity string         `json:"identity"`
	Session  string         `json:"session"`
	Payload  map[string]any `json:"payload"`
}

func (h *harness) events(c client) []event {
	h.t.Helper()
	out := ok[struct {
		Events []event `json:"events"`
	}](h.t, c, "GET", "/api/events?after=0&limit=1000", nil)
	return out.Events
}

func touches(e event, ref string) bool {
	ups, _ := e.Payload["updates"].([]any)
	for _, u := range ups {
		if m, _ := u.(map[string]any); m["ref"] == ref {
			return true
		}
	}
	return false
}

func requireEvent(t *testing.T, evs []event, what string, match func(event) bool) {
	t.Helper()
	if slices.IndexFunc(evs, match) < 0 {
		var types []string
		for _, e := range evs {
			types = append(types, e.Type+"/"+e.Identity)
		}
		t.Fatalf("missing event: %s\nevents: %v", what, types)
	}
}

type accessEntry struct {
	Kind    string `json:"kind"`
	Session string `json:"session"`
	Detail  struct {
		Wants []string `json:"wants"`
	} `json:"detail"`
}

func (h *harness) waitAccess(identity string, match func(accessEntry) bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := h.run("admin", "access-log", "--data", h.data, "--identity", identity, "--limit", "100")
		sc := bufio.NewScanner(strings.NewReader(out))
		for sc.Scan() {
			var e accessEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil && match(e) {
				return
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("no matching access log entry for %s:\n%s", identity, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- API client ----

type client struct {
	h     *harness
	token string
}

func (h *harness) client(token string) client { return client{h: h, token: token} }

func (c client) call(method, path string, body any) (int, []byte) {
	c.h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.h.base+path, rd)
	if err != nil {
		c.h.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func ok[T any](t *testing.T, c client, method, path string, body any) T {
	t.Helper()
	code, raw := c.call(method, path, body)
	if code != http.StatusOK {
		t.Fatalf("%s %s: %d %s", method, path, code, raw)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s %s: decode: %v\n%s", method, path, err, raw)
	}
	return v
}

func expectStatus(t *testing.T, c client, method, path string, body any, want int) {
	t.Helper()
	code, raw := c.call(method, path, body)
	if code != want {
		t.Fatalf("%s %s: got %d, want %d: %s", method, path, code, want, raw)
	}
}

func tokenFrom(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if tok, found := strings.CutPrefix(line, "token: "); found {
			return strings.TrimSpace(tok)
		}
	}
	t.Fatalf("no token in output:\n%s", out)
	return ""
}

type changeSetView struct {
	Status struct {
		Head      string   `json:"head"`
		Mergeable bool     `json:"mergeable"`
		Unmet     []string `json:"unmet"`
	} `json:"status"`
	Reviews []struct {
		Comments []struct {
			Path string `json:"path"`
		} `json:"comments"`
	} `json:"reviews"`
}

type issueView struct {
	Plans []struct {
		Complete bool `json:"complete"`
	} `json:"plans"`
	Hints []string `json:"hints"`
}
