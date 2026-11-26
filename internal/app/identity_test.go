package app

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"gitsynth/internal/store"
)

// TestEnableBringsNothingBack disables an identity the way a server from
// before disabling revoked anything did, leaving its token unrevoked, and
// checks that enabling it does not bring the token back. It also checks
// that a bot's own token makes nothing in the bot's tree while its owner
// does.
func TestEnableBringsNothingBack(t *testing.T) {
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
	root, err := a.IssueToken(ctx, System, "user:alice", 0)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := a.Authenticate(ctx, root.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Derive(ctx, alice, "", "claude"); err != nil {
		t.Fatal(err)
	}
	old, err := a.IssueToken(ctx, alice, "user:alice/claude", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.Write(ctx, SystemActor, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE identities SET disabled = 1 WHERE id = 'user:alice/claude'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = a.Authenticate(ctx, old.Token)
	wantCode(t, err, http.StatusUnauthorized, "a disabled identity's token")
	if err := a.EnableIdentity(ctx, alice, "user:alice/claude"); err != nil {
		t.Fatal(err)
	}
	_, err = a.Authenticate(ctx, old.Token)
	wantCode(t, err, http.StatusUnauthorized, "a token from before the identity was disabled, after enabling it")
	err = a.EnableIdentity(ctx, alice, "user:alice/claude")
	wantCode(t, err, http.StatusConflict, "enabling an identity that is not disabled")

	// A bot's own token makes nothing in its tree; its owner does.
	if _, err := a.CreateBot(ctx, alice, "devbox", "user:alice"); err != nil {
		t.Fatal(err)
	}
	botTok, err := a.IssueToken(ctx, alice, "bot:devbox", 0)
	if err != nil {
		t.Fatal(err)
	}
	bot, err := a.Authenticate(ctx, botTok.Token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Derive(ctx, bot, "", "triage")
	wantCode(t, err, http.StatusForbidden, "a bot deriving below itself")
	if _, err := a.Derive(ctx, alice, "bot:devbox", "triage"); err != nil {
		t.Fatal(err)
	}
	_, err = a.IssueToken(ctx, bot, "bot:devbox/triage", 0)
	wantCode(t, err, http.StatusForbidden, "a bot issuing a token below itself")
	if _, err := a.IssueToken(ctx, alice, "bot:devbox/triage", 0); err != nil {
		t.Fatal(err)
	}
	err = a.DisableIdentity(ctx, bot, "bot:devbox/triage")
	if err != nil {
		t.Fatalf("an identity still controls what is derived from it: %v", err)
	}
	err = a.EnableIdentity(ctx, bot, "bot:devbox/triage")
	wantCode(t, err, http.StatusForbidden, "a bot enabling below itself")
	if err := a.EnableIdentity(ctx, alice, "bot:devbox/triage"); err != nil {
		t.Fatal(err)
	}
}
