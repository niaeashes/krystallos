package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

type openedSession struct {
	Session struct {
		ID       string `json:"id"`
		Identity string `json:"identity"`
		Parent   string `json:"parent"`
		Label    string `json:"label"`
		OpenedAt string `json:"opened_at"`
	} `json:"session"`
	Token          string `json:"token"`
	ExpiresAt      string `json:"expires_at"`
	CommitterEmail string `json:"committer_email"`
}

// TestSessions follows an agent that holds a standing token for
// user:alice/claude: it opens a session for its run and a child session for
// a subagent, and pushes through the child's credential. It can make
// neither identities nor standing tokens. Revoking the standing token stops
// the sessions opened with it; disabling the identity stops every
// credential and session at it, and enabling it brings none of them back,
// though fresh ones work.
func TestSessions(t *testing.T) {
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
	aliceWork := filepath.Join(dir, "alice")
	h.git(dir, "clone", h.cloneURL(aliceTok), aliceWork)
	h.commitAs(aliceWork, "alice", "user:alice@localhost", "README.md", "# app\n", "initial commit")
	h.git(aliceWork, "push", "origin", "HEAD:main")

	ok[map[string]string](t, alice, "POST", "/api/identities", map[string]string{"label": "claude"})
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/grants", map[string]string{"identity": "user:alice/claude", "set": "write"})
	standing := ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude", "ttl_hours": 2})
	agent := h.client(standing["token"].(string))

	// A standing token belongs to no session, and a caller cannot declare one.
	me := meWithHeader(t, h, standing["token"].(string), "Gitsynth-Session", "made-up")
	if me["identity"] != "user:alice/claude" || me["session"] != "" {
		t.Fatalf("a standing token should carry no session, whatever the request says: %v", me)
	}
	expectStatus(t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude", "session": "s"}, http.StatusBadRequest)

	// The agent makes no identities and issues no standing tokens.
	expectStatus(t, agent, "POST", "/api/identities", map[string]string{"label": "explorer"}, http.StatusForbidden)
	expectStatus(t, agent, "POST", "/api/identities", map[string]string{"parent": "user:alice", "label": "other"}, http.StatusForbidden)
	expectStatus(t, agent, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"}, http.StatusForbidden)
	expectStatus(t, agent, "POST", "/api/identities/enable", map[string]string{"id": "user:alice/claude"}, http.StatusForbidden)

	// It opens a session for its run, asking for more time than its token
	// has left, and the run opens a child session for a subagent.
	run := ok[openedSession](t, agent, "POST", "/api/sessions", map[string]any{"label": "cc-run-1", "ttl_hours": 8})
	if run.Session.Identity != "user:alice/claude" || run.Session.Parent != "" || run.Session.Label != "cc-run-1" ||
		run.CommitterEmail != "user:alice/claude@localhost" || run.ExpiresAt > standing["expires_at"].(string) {
		t.Fatalf("unexpected session, or one that outlives its opener (expires %v): %+v", standing["expires_at"], run)
	}
	runC := h.client(run.Token)
	expectStatus(t, runC, "POST", "/api/identities", map[string]string{"label": "explorer"}, http.StatusForbidden)
	expectStatus(t, runC, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"}, http.StatusForbidden)
	sub := ok[openedSession](t, runC, "POST", "/api/sessions", map[string]any{"label": "subagent-explorer"})
	if sub.Session.Parent != run.Session.ID || sub.ExpiresAt > run.ExpiresAt {
		t.Fatalf("the subagent's session should be a child of the run's, and end no later: %+v %+v", sub, run)
	}
	subC := h.client(sub.Token)
	if me := ok[map[string]any](t, subC, "GET", "/api/me", nil); me["identity"] != "user:alice/claude" || me["session"] != sub.Session.ID {
		t.Fatalf("the child session's credential should carry its session: %v", me)
	}

	// The subagent pushes with its session's credential, committing as the
	// identity.
	work := filepath.Join(dir, "sub")
	h.git(dir, "clone", h.cloneURL(sub.Token), work)
	h.commitAs(work, "claude", sub.CommitterEmail, "notes.md", "notes\n", "add notes")
	h.git(work, "push", "origin", "HEAD:refs/heads/notes")

	evs := h.events(alice)
	requireEvent(t, evs, "the run's session, opened with the standing token", func(e event) bool {
		return e.Type == "session.opened" && e.Identity == "user:alice/claude" && e.Session == "" &&
			e.Payload["id"] == run.Session.ID && e.Payload["parent"] == "" && e.Payload["label"] == "cc-run-1"
	})
	requireEvent(t, evs, "the subagent's session, opened by the run's", func(e event) bool {
		return e.Type == "session.opened" && e.Identity == "user:alice/claude" && e.Session == run.Session.ID &&
			e.Payload["id"] == sub.Session.ID && e.Payload["parent"] == run.Session.ID
	})
	requireEvent(t, evs, "the push, under the subagent's session", func(e event) bool {
		return e.Type == "git.ref_transaction" && e.Identity == "user:alice/claude" && e.Session == sub.Session.ID && touches(e, "refs/heads/notes")
	})

	// Revoking a standing token stops the sessions opened with it, and
	// nothing else.
	other := ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"})
	otherRun := ok[openedSession](t, h.client(other["token"].(string)), "POST", "/api/sessions", nil)
	otherSub := ok[openedSession](t, h.client(otherRun.Token), "POST", "/api/sessions", map[string]any{"label": "x"})
	ok[map[string]any](t, h.client(otherSub.Token), "GET", "/api/me", nil)
	ok[map[string]any](t, alice, "POST", "/api/tokens/"+other["id"].(string)+"/revoke", nil)
	expectStatus(t, h.client(otherRun.Token), "GET", "/api/me", nil, http.StatusUnauthorized)
	expectStatus(t, h.client(otherSub.Token), "GET", "/api/me", nil, http.StatusUnauthorized)
	ok[map[string]any](t, subC, "GET", "/api/me", nil)

	// Disabling the identity stops its standing token and every session.
	// Nothing new is issued or made there while it is disabled.
	ok[map[string]any](t, alice, "POST", "/api/identities/disable", map[string]string{"id": "user:alice/claude"})
	for _, c := range []client{agent, runC, subC} {
		expectStatus(t, c, "GET", "/api/me", nil, http.StatusUnauthorized)
	}
	expectStatus(t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"}, http.StatusConflict)
	expectStatus(t, alice, "POST", "/api/identities", map[string]string{"parent": "user:alice/claude", "label": "explorer"}, http.StatusConflict)

	// Enabling it brings none of them back, but a fresh token from alice
	// works and opens sessions.
	ok[map[string]any](t, alice, "POST", "/api/identities/enable", map[string]string{"id": "user:alice/claude"})
	for _, c := range []client{agent, runC, subC} {
		expectStatus(t, c, "GET", "/api/me", nil, http.StatusUnauthorized)
	}
	fresh := h.client(ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"})["token"].(string))
	again := ok[openedSession](t, fresh, "POST", "/api/sessions", map[string]any{"label": "cc-run-2"})
	if me := ok[map[string]any](t, h.client(again.Token), "GET", "/api/me", nil); me["session"] != again.Session.ID {
		t.Fatalf("a session opened after enabling should work: %v", me)
	}

	// The record says which credentials disabling revoked.
	evs = h.events(alice)
	credOf := func(session string) any {
		i := slices.IndexFunc(evs, func(e event) bool { return e.Type == "session.opened" && e.Payload["id"] == session })
		if i < 0 {
			t.Fatalf("no session.opened for %s", session)
		}
		return evs[i].Payload["credential"]
	}
	disabled := evs[slices.IndexFunc(evs, func(e event) bool { return e.Type == "identity.disabled" })]
	revoked, _ := disabled.Payload["revoked"].([]any)
	for _, id := range []any{standing["id"], credOf(run.Session.ID), credOf(sub.Session.ID)} {
		if !slices.Contains(revoked, id) {
			t.Fatalf("disabling should revoke %v, and say so: %v", id, disabled.Payload)
		}
	}
	if slices.Contains(revoked, other["id"]) {
		t.Fatalf("a credential revoked before is not revoked again: %v", disabled.Payload)
	}
	requireEvent(t, evs, "enabled by alice", func(e event) bool {
		return e.Type == "identity.enabled" && e.Identity == "user:alice" && e.Payload["id"] == "user:alice/claude"
	})
}

// meWithHeader calls /api/me with an extra request header.
func meWithHeader(t *testing.T, h *harness, token, name, value string) map[string]any {
	t.Helper()
	req, err := http.NewRequest("GET", h.base+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(name, value)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
		t.Fatalf("GET /api/me: %d %s", resp.StatusCode, raw)
	}
	return out
}
