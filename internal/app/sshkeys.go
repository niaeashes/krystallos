package app

import (
	"context"
	"crypto/rsa"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"

	"krystallos/internal/ident"
	"krystallos/internal/store"
)

// An SSH key is a credential of one identity, like a standing token: it
// lasts until revoked and belongs to no session. It lives in the
// credentials table (kind ssh_key), so revoking it, disabling its identity
// and listing credentials work as they do for tokens; its public half is in
// ssh_keys.
//
// The key says which identity acts, nothing more. On one OS account an
// agent can reach the person's keys, so a key cannot tell the person from
// an agent; what keeps them apart is a clone per identity, whose own config
// names the committer and the key (see the clone command in the web UI).
// Ordinary keys and FIDO keys are both accepted; a FIDO key's signature
// must say it was touched, as OpenSSH requires.

const credSSHKey = "ssh_key"

// maxSSHKeyName bounds a key's name, in bytes.
const maxSSHKeyName = 100

// SSHKey is a registered SSH key.
type SSHKey struct {
	ID          string `json:"id"`
	Identity    string `json:"identity"`
	Name        string `json:"name,omitempty"`
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	PublicKey   string `json:"public_key"`
	// CommitterEmail is the canonical address to commit with under the
	// key's identity, for the clone the key pushes from.
	CommitterEmail string `json:"committer_email"`
}

// ParseSSHKey reads one public key in authorized_keys form ("ssh-ed25519
// AAAA... comment") and checks that it is a kind krystallos accepts. The
// comment comes back as the key's default name.
func ParseSSHKey(text string) (ssh.PublicKey, string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, "", errf(http.StatusBadRequest, "no public key given: paste one line from a .pub file")
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		return nil, "", errf(http.StatusBadRequest, "not an SSH public key: paste one line from a .pub file, such as ~/.ssh/id_ed25519.pub")
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return nil, "", errf(http.StatusBadRequest, "one key at a time: the text holds more than one")
	}
	if len(options) > 0 {
		// krystallos honors none of authorized_keys' options; accepting them
		// would suggest that "no-touch-required" or "from=" did something.
		return nil, "", errf(http.StatusBadRequest, "authorized_keys options (%s) are not supported: give the key alone", strings.Join(options, ","))
	}
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoSKED25519,
		ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521, ssh.KeyAlgoSKECDSA256:
	case ssh.KeyAlgoRSA:
		ck, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return nil, "", errf(http.StatusBadRequest, "unreadable RSA key")
		}
		if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); !ok || rk.N.BitLen() < 2048 {
			return nil, "", errf(http.StatusBadRequest, "RSA keys need 2048 bits or more; ed25519 is the better choice")
		}
	default:
		return nil, "", errf(http.StatusBadRequest, "%s keys are not accepted: use ed25519 (or ed25519-sk), ecdsa, or RSA of 2048 bits or more", key.Type())
	}
	return key, strings.TrimSpace(comment), nil
}

func sshKeyName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) > maxSSHKeyName {
		return "", errf(http.StatusBadRequest, "a key's name is at most %d bytes", maxSSHKeyName)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", errf(http.StatusBadRequest, "a key's name must not contain control characters")
	}
	return name, nil
}

// AddSSHKey registers a public key for an identity (the actor's own if
// identity is empty). It follows the rule for standing tokens: the person,
// or a bot's managers, hand keys to identities; an agent adds none, not
// even for itself. A key is held by one identity on the server at a time.
func (a *App) AddSSHKey(ctx context.Context, by Actor, identity, publicKey, name string) (SSHKey, error) {
	if identity == "" {
		identity = by.Identity
	}
	target, err := ident.Parse(identity)
	if err != nil {
		return SSHKey{}, errf(http.StatusBadRequest, "%v", err)
	}
	if target.String() == SystemActor.Identity {
		return SSHKey{}, errf(http.StatusForbidden, "the built-in service has no keys")
	}
	key, comment, err := ParseSSHKey(publicKey)
	if err != nil {
		return SSHKey{}, err
	}
	if name == "" {
		name = comment
	}
	if name, err = sshKeyName(name); err != nil {
		return SSHKey{}, err
	}
	k := SSHKey{
		ID: "cred_" + randomString(10), Identity: target.String(), Name: name, Type: key.Type(),
		Fingerprint: ssh.FingerprintSHA256(key), PublicKey: authorizedKey(key),
		CommitterEmail: a.CommitEmail(ctx, target.String()),
	}
	_, err = a.Store.Write(ctx, by.Actor, func(tx *store.Tx) error {
		ok, err := a.makes(ctx, tx, by, target)
		if err != nil {
			return err
		}
		if !ok {
			return errf(http.StatusForbidden, "%s may not add SSH keys for %s: keys are added by the person, or by a bot's owner", by.Identity, target)
		}
		if err := a.identityActive(ctx, tx, target); err != nil {
			return err
		}
		var held int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ssh_keys k JOIN credentials c ON c.id = k.credential WHERE k.fingerprint = ? AND c.revoked = 0`,
			k.Fingerprint).Scan(&held)
		if err != nil {
			return err
		}
		if held > 0 {
			return errf(http.StatusConflict, "the key %s is already registered on this server; revoke it there first", k.Fingerprint)
		}
		if _, err := tx.Exec(`INSERT INTO credentials (id, identity, hash, kind) VALUES (?, ?, ?, ?)`,
			k.ID, k.Identity, credSSHKey+":"+k.ID, credSSHKey); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO ssh_keys (credential, fingerprint, type, public_key, name) VALUES (?, ?, ?, ?, ?)`,
			k.ID, k.Fingerprint, k.Type, k.PublicKey, k.Name); err != nil {
			return err
		}
		_, err = tx.Append("ssh_key.added", store.Scope{}, map[string]any{
			"id": k.ID, "identity": k.Identity, "fingerprint": k.Fingerprint, "type": k.Type, "name": k.Name,
		})
		return err
	})
	return k, err
}

// authorizedKey is a key in authorized_keys form, without a comment.
func authorizedKey(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// AuthenticateSSHKey resolves a key that a client offers over SSH to the
// identity it belongs to. It works only while the key is unrevoked and its
// identity, and every identity it is derived from, is enabled.
func (a *App) AuthenticateSSHKey(ctx context.Context, key ssh.PublicKey) (Actor, error) {
	return a.sshActor(ctx, `k.fingerprint = ? AND k.public_key = ? AND c.revoked = 0`,
		ssh.FingerprintSHA256(key), authorizedKey(key))
}

// SSHKeyActor resolves an SSH key credential again by its ID, for a
// connection that authenticated with it earlier: a key revoked, or an
// identity disabled, since then stops it.
func (a *App) SSHKeyActor(ctx context.Context, id string) (Actor, error) {
	return a.sshActor(ctx, `c.id = ?`, id)
}

func (a *App) sshActor(ctx context.Context, where string, args ...any) (Actor, error) {
	var act Actor
	var kind string
	var revoked bool
	db := a.Store.DB()
	err := db.QueryRowContext(ctx,
		`SELECT c.id, c.expires_at, c.revoked, c.identity, i.principal, p.kind, p.admin
		 FROM ssh_keys k
		 JOIN credentials c ON c.id = k.credential
		 JOIN identities i ON i.id = c.identity
		 JOIN principals p ON p.id = i.principal
		 WHERE c.kind = 'ssh_key' AND `+where, args...).
		Scan(&act.Credential.ID, &act.Credential.ExpiresAt, &revoked, &act.Identity, &act.Principal, &kind, &act.Admin)
	if errors.Is(err, sql.ErrNoRows) {
		return Actor{}, errf(http.StatusUnauthorized, "this key is not registered to any identity")
	}
	if err != nil {
		return Actor{}, err
	}
	if revoked {
		return Actor{}, errf(http.StatusUnauthorized, "key revoked")
	}
	if expired(act.Credential.ExpiresAt) {
		return Actor{}, errf(http.StatusUnauthorized, "key expired")
	}
	act.Credential.Kind = credSSHKey
	act.Kind = ident.Kind(kind)
	act.Admin = act.Admin && act.Kind == ident.User
	if err := chainEnabled(ctx, db, act.ID()); err != nil {
		return Actor{}, err
	}
	return act, nil
}
