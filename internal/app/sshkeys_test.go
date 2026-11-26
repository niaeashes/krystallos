package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestParseSSHKey(t *testing.T) {
	edPub, edPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ed, err := ssh.NewPublicKey(edPub)
	if err != nil {
		t.Fatal(err)
	}
	line := func(k ssh.PublicKey) string { return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) }

	// A FIDO key, as ssh-keygen -t ed25519-sk writes it.
	sk, err := ssh.ParsePublicKey(ssh.Marshal(struct {
		Name        string
		Key         []byte
		Application string
	}{ssh.KeyAlgoSKED25519, edPub, "ssh:"}))
	if err != nil {
		t.Fatal(err)
	}

	small, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	smallKey, err := ssh.NewPublicKey(&small.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := ssh.NewSignerFromKey(edPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{Key: ed, CertType: ssh.UserCert, ValidPrincipals: []string{"alice"}, ValidBefore: ssh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, signer); err != nil {
		t.Fatal(err)
	}

	key, comment, err := ParseSSHKey("  " + line(ed) + " alice@laptop \n")
	if err != nil || key.Type() != ssh.KeyAlgoED25519 || comment != "alice@laptop" {
		t.Fatalf("an ed25519 key should be accepted with its comment: %v %q %v", key, comment, err)
	}
	if key, _, err := ParseSSHKey(line(sk)); err != nil || key.Type() != ssh.KeyAlgoSKED25519 {
		t.Fatalf("a FIDO key should be accepted: %v", err)
	}

	for name, text := range map[string]string{
		"empty":          "",
		"garbage":        "ssh-ed25519 not-base64!",
		"two keys":       line(ed) + "\n" + line(sk),
		"options":        "no-touch-required " + line(sk),
		"small RSA":      line(smallKey),
		"certificate":    line(cert),
		"unknown format": "ssh-foo " + base64.StdEncoding.EncodeToString([]byte("x")),
	} {
		if _, _, err := ParseSSHKey(text); err == nil {
			t.Errorf("%s: should be refused", name)
		}
	}
}
