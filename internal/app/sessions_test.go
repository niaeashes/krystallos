package app

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"krystallos/internal/store"
)

func wantCode(t *testing.T, err error, code int, what string) {
	t.Helper()
	var ae *Error
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("%s: want %d, got %v", what, code, err)
	}
}

// TestSessionsFollowTheirOpeners opens sessions with a standing token and
// checks the rules a session's credential lives by: it never outlives the
// credential that opened it, it stops when any opener up the chain expires,
// and sessions nest only so deep. It also checks who may make identities and
// standing tokens.
func TestSessionsFollowTheirOpeners(t *testing.T) {
	ctx := context.Background()
	a, err := Open(ctx, Config{DataDir: filepath.Join(t.TempDir(), "data")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if err := a.Initialize(ctx, "localhost"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CreateUser(ctx, System, "alice", false); err != nil {
		t.Fatal(err)
	}
	login := func(tok string) Actor {
		t.Helper()
		act, err := a.Authenticate(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		return act
	}
	root, err := a.IssueToken(ctx, System, "user:alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	alice := login(root.Token)
	if _, err := a.Derive(ctx, alice, "", "claude"); err != nil {
		t.Fatal(err)
	}
	standing, err := a.IssueToken(ctx, alice, "user:alice/claude", 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claude := login(standing.Token)

	// The agent makes no identities and no standing tokens, not even below
	// or for itself.
	_, err = a.Derive(ctx, claude, "", "explorer")
	wantCode(t, err, http.StatusForbidden, "derive below itself")
	_, err = a.IssueToken(ctx, claude, "user:alice/claude", 0)
	wantCode(t, err, http.StatusForbidden, "standing token for itself")
	err = a.EnableIdentity(ctx, claude, "user:alice/claude")
	wantCode(t, err, http.StatusForbidden, "enable itself")

	// A session asked for 8 hours stops when its 2-hour opener does.
	run, err := a.OpenSession(ctx, claude, "cc-run", 8)
	if err != nil {
		t.Fatal(err)
	}
	if run.Session.Parent != "" || run.Session.Identity != "user:alice/claude" || run.ExpiresAt > standing.ExpiresAt {
		t.Fatalf("a session opened with a standing token is a root session that expires with it: %+v (opener expires %s)", run, standing.ExpiresAt)
	}
	sess := login(run.Token)
	if sess.Session != run.Session.ID || sess.Credential.Kind != credSession {
		t.Fatalf("the session's credential should carry the session: %+v", sess)
	}
	sub, err := a.OpenSession(ctx, sess, "subagent", 1)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Session.Parent != run.Session.ID {
		t.Fatalf("a session opened with a session's credential is its child: %+v", sub)
	}
	login(sub.Token)

	// Labels and lifetimes are bounded.
	_, err = a.OpenSession(ctx, claude, strings.Repeat("x", maxSessionLabel+1), 0)
	wantCode(t, err, http.StatusBadRequest, "long label")
	_, err = a.OpenSession(ctx, claude, "two\nlines", 0)
	wantCode(t, err, http.StatusBadRequest, "label with a newline")
	_, err = a.OpenSession(ctx, claude, "", 25)
	wantCode(t, err, http.StatusBadRequest, "ttl over 24 hours")
	_, err = a.OpenSession(ctx, alice.withoutCredential(), "", 0)
	wantCode(t, err, http.StatusBadRequest, "no credential")

	// Sessions nest only so deep.
	deep := sub
	for i := 2; i < maxSessionDepth; i++ {
		if deep, err = a.OpenSession(ctx, login(deep.Token), "", 0); err != nil {
			t.Fatalf("session at depth %d: %v", i+1, err)
		}
	}
	_, err = a.OpenSession(ctx, login(deep.Token), "", 0)
	wantCode(t, err, http.StatusBadRequest, "one session too deep")

	// When the standing token expires, everything opened below it stops.
	if _, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE credentials SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), standing.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for what, tok := range map[string]string{"the session": run.Token, "its child": sub.Token, "the deepest": deep.Token} {
		_, err := a.Authenticate(ctx, tok)
		wantCode(t, err, http.StatusUnauthorized, what+" after the standing token expired")
	}
}

func (a Actor) withoutCredential() Actor {
	a.Credential = CredentialRef{}
	return a
}
