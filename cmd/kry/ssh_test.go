package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSSH drives git over SSH with the real ssh and git clients: keys are
// registered by the person for themself and for their agent, each works
// from a clone of its own whose config names the committer and the key,
// and pushes, fetches and refusals land in the same records as over HTTP.
func TestSSH(t *testing.T) {
	for _, bin := range []string{"git", "ssh", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " not installed")
		}
	}
	dir := t.TempDir()
	h := newHarness(t, dir)
	aliceTok := tokenFrom(t, h.run("admin", "init", "--data", h.data, "--user", "alice"))
	h.useSSH()
	h.start()
	alice := h.client(aliceTok)
	_, sshPort, _ := net.SplitHostPort(h.sshAddr)

	ok[map[string]any](t, alice, "POST", "/api/orgs", map[string]string{"name": "acme"})
	repo := ok[map[string]any](t, alice, "POST", "/api/orgs/acme/repos", map[string]string{"name": "app"})
	sshURL := "ssh://git@localhost:" + sshPort + "/acme/app.git"
	if repo["ssh_clone_url"] != sshURL {
		t.Fatalf("a new repository should come with its SSH clone URL %s: %v", sshURL, repo)
	}
	seed := filepath.Join(dir, "seed")
	h.git(dir, "clone", h.cloneURL(aliceTok), seed)
	h.commitAs(seed, "alice", "user:alice@localhost", "README.md", "# app\n", "initial commit")
	h.git(seed, "push", "origin", "HEAD:main")
	if info := ok[map[string]any](t, alice, "GET", "/api/repos/acme/app", nil); info["ssh_clone_url"] != sshURL {
		t.Fatalf("the repository should show its SSH clone URL: %v", info)
	}

	ok[map[string]string](t, alice, "POST", "/api/identities", map[string]string{"label": "claude"})
	ok[map[string]any](t, alice, "POST", "/api/repos/acme/app/grants", map[string]string{"identity": "user:alice/claude", "set": "write"})
	standing := ok[map[string]any](t, alice, "POST", "/api/tokens", map[string]any{"identity": "user:alice/claude"})
	agent := h.client(standing["token"].(string))

	keys := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	aliceKey, alicePub := sshKeygen(t, keys, "alice")
	claudeKey, claudePub := sshKeygen(t, keys, "claude")
	strangerKey, _ := sshKeygen(t, keys, "stranger")
	knownHosts := filepath.Join(dir, "known_hosts")

	// The person hands keys out; the agent adds none, not even for itself.
	expectStatus(t, agent, "POST", "/api/ssh-keys", map[string]string{"public_key": claudePub}, http.StatusForbidden)
	expectStatus(t, alice, "POST", "/api/ssh-keys", map[string]string{"public_key": "not a key"}, http.StatusBadRequest)
	expectStatus(t, alice, "POST", "/api/ssh-keys", map[string]string{"public_key": `from="10.0.0.1" ` + claudePub}, http.StatusBadRequest)
	ok[map[string]any](t, alice, "POST", "/api/ssh-keys", map[string]string{"public_key": alicePub})
	claudeKeyInfo := ok[map[string]any](t, alice, "POST", "/api/ssh-keys", map[string]string{
		"identity": "user:alice/claude", "public_key": claudePub, "name": "claude on the laptop",
	})
	if claudeKeyInfo["identity"] != "user:alice/claude" || claudeKeyInfo["committer_email"] != "user:alice/claude@localhost" ||
		!strings.HasPrefix(claudeKeyInfo["fingerprint"].(string), "SHA256:") {
		t.Fatalf("unexpected key: %v", claudeKeyInfo)
	}
	// A key belongs to one identity on the server.
	expectStatus(t, alice, "POST", "/api/ssh-keys", map[string]string{"public_key": claudePub}, http.StatusConflict)
	listed := false
	for _, c := range ok[[]map[string]any](t, alice, "GET", "/api/tokens", nil) {
		if c["id"] == claudeKeyInfo["id"] {
			listed = c["kind"] == "ssh_key" && c["label"] == "claude on the laptop" && c["fingerprint"] == claudeKeyInfo["fingerprint"]
		}
	}
	if !listed {
		t.Fatalf("the key should be listed among alice's credentials")
	}

	// "ssh -T" says whether a key works and as whom.
	out, err := sshRun(keys, claudeKey, knownHosts, sshPort)
	if err == nil || !strings.Contains(out, "Hi user:alice/claude!") {
		t.Fatalf("ssh without a command should greet the key's identity and refuse a shell: %v\n%s", err, out)
	}
	if out, err := sshRun(keys, strangerKey, knownHosts, sshPort); err == nil || !strings.Contains(out, "Permission denied") {
		t.Fatalf("an unregistered key should be refused: %v\n%s", err, out)
	}

	// The agent's clone holds its identity: the committer and the key are
	// in the clone's config, so nothing has to be remembered per command.
	claudeWork := filepath.Join(dir, "claude")
	h.git(dir, "clone",
		"-c", "user.name=user:alice/claude", "-c", "user.email=user:alice/claude@localhost",
		"-c", "core.sshCommand="+sshCommand(claudeKey, knownHosts),
		sshURL, claudeWork)
	commitHere(h, claudeWork, "hello.txt", "hello\n", "add hello")
	h.git(claudeWork, "push", "origin", "HEAD:refs/heads/feat")
	feat := h.git(claudeWork, "rev-parse", "HEAD")
	requireEvent(t, h.events(alice), "claude's push over SSH, recorded under claude", func(e event) bool {
		return e.Type == "git.ref_transaction" && e.Identity == "user:alice/claude" && touches(e, "refs/heads/feat")
	})
	h.waitAccess("user:alice/claude", func(e accessEntry) bool { return e.Kind == "git.fetch" && len(e.Detail.Wants) > 0 })
	h.waitAccess("user:alice/claude", func(e accessEntry) bool { return e.Kind == "git.advertise" })
	if out, err := h.gitErr(claudeWork, "push", "origin", "HEAD:main"); err == nil {
		t.Fatalf("the agent's push to main should be refused over SSH too:\n%s", out)
	}
	if out, err := h.gitErr(claudeWork, "-c", "protocol.version=0", "fetch", "origin"); err == nil || !strings.Contains(out, "protocol v2") {
		t.Fatalf("a fetch without protocol v2 should be refused with the reason: %v\n%s", err, out)
	}
	if out, err := h.gitErr(dir, "-c", "core.sshCommand="+sshCommand(claudeKey, knownHosts), "ls-remote", "ssh://git@localhost:"+sshPort+"/acme/nope.git"); err == nil || !strings.Contains(out, "not found") {
		t.Fatalf("an unknown repository should not be found: %v\n%s", err, out)
	}

	// A loose clone: the person pushes their own commit and the agent's
	// together. The person controls the agent's identity, so that is let
	// in, and the record says the person pushed.
	aliceWork := filepath.Join(dir, "alice")
	h.git(dir, "clone",
		"-c", "user.name=user:alice", "-c", "user.email=user:alice@localhost",
		"-c", "core.sshCommand="+sshCommand(aliceKey, knownHosts),
		sshURL, aliceWork)
	h.git(aliceWork, "fetch", "origin", "feat")
	h.git(aliceWork, "merge", "-q", "--ff-only", "FETCH_HEAD")
	h.commitAs(aliceWork, "user:alice/claude", "user:alice/claude@localhost", "agent.txt", "by the agent\n", "agent's commit")
	commitHere(h, aliceWork, "person.txt", "by the person\n", "person's commit")
	h.git(aliceWork, "push", "origin", "HEAD:refs/heads/mixed")
	requireEvent(t, h.events(alice), "the mixed push, recorded under alice", func(e event) bool {
		return e.Type == "git.ref_transaction" && e.Identity == "user:alice" && touches(e, "refs/heads/mixed")
	})
	if h.git(aliceWork, "rev-parse", "HEAD~2") != feat {
		t.Fatalf("alice's clone should sit on claude's branch")
	}

	// The host key lives in the data directory: a restarted server is the
	// same host to clients that pinned it.
	if st, err := os.Stat(filepath.Join(h.data, "ssh_host_ed25519_key")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("the host key should be in the data directory, readable by the server alone: %v", err)
	}
	h.stop()
	h.start()
	h.git(claudeWork, "fetch", "origin")

	// A revoked key stops working at once.
	ok[map[string]bool](t, alice, "POST", "/api/tokens/"+claudeKeyInfo["id"].(string)+"/revoke", nil)
	if out, err := h.gitErr(claudeWork, "fetch", "origin"); err == nil {
		t.Fatalf("a revoked key should be refused:\n%s", out)
	}
	// Revoked, it is free to register again.
	ok[map[string]any](t, alice, "POST", "/api/ssh-keys", map[string]string{"identity": "user:alice/claude", "public_key": claudePub})
	h.git(claudeWork, "fetch", "origin")
}

// sshKeygen makes an ed25519 key pair without a passphrase and returns the
// private key's path and the public key's line.
func sshKeygen(t *testing.T, dir, name string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", path).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(path + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return path, strings.TrimSpace(string(pub))
}

// sshOptions keep the user's ssh config, agent and known hosts out of it.
// The server is reached at 127.0.0.1 whatever host a URL names, so a
// sandbox that keeps ssh from looking names up does not get in the way.
func sshOptions(key, knownHosts string) []string {
	return []string{
		"-F", "/dev/null", "-i", key, "-o", "HostName=127.0.0.1",
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "UserKnownHostsFile=" + knownHosts, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "StrictHostKeyChecking=accept-new",
	}
}

func sshCommand(key, knownHosts string) string {
	var b strings.Builder
	b.WriteString("ssh")
	for _, o := range sshOptions(key, knownHosts) {
		fmt.Fprintf(&b, " '%s'", o)
	}
	return b.String()
}

// sshRun connects with no command, as "ssh -T git@host" does.
func sshRun(dir, key, knownHosts, port string) (string, error) {
	cmd := exec.Command("ssh", append(sshOptions(key, knownHosts), "-T", "-p", port, "git@localhost")...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// commitHere commits with the clone's own config, as anyone working in it
// would.
func commitHere(h *harness, dir, file, content, msg string) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	h.git(dir, "add", file)
	h.git(dir, "commit", "-q", "-m", msg)
}
