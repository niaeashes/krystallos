package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"krystallos/internal/store"
)

// TestMoveDataDir moves the whole data directory while the server is down
// and starts it at the new path. Its rows hold absolute paths, as krystallos
// wrote them before a data directory could move. Fetch and push work there,
// and the push is recorded.
func TestMoveDataDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, alice, token, work := relocateSetup(t)
	pushed := h.git(work, "rev-parse", "HEAD")
	h.stop()
	storeAbsolutePaths(t, h.data)
	moved := filepath.Join(filepath.Dir(h.data), "moved")
	if err := os.Rename(h.data, moved); err != nil {
		t.Fatal(err)
	}
	h.data = moved

	h.start()
	h.git(work, "remote", "set-url", "origin", h.cloneURL(token))
	h.git(work, "fetch", "origin")
	if got := h.git(work, "rev-parse", "origin/main"); got != pushed {
		t.Fatalf("fetch after the move: origin/main is %s, want %s", got, pushed)
	}
	h.commitAs(work, "alice", "user:alice@localhost", "README.md", "# app\n\nMoved.\n", "after the move")
	h.git(work, "push", "origin", "HEAD:main")
	head := h.git(work, "rev-parse", "HEAD")
	requireEvent(t, h.events(alice), "the push after the move", pushRecorded("user:alice", "refs/heads/main", head))
	if got := h.git(h.repoPath(), "rev-parse", "refs/heads/main"); got != head {
		t.Fatalf("the moved repository should hold the push: main is %s, want %s", got, head)
	}
	if out := h.run("admin", "verify", "--data", h.data); !strings.HasPrefix(out, "ok:") {
		t.Fatalf("hash chain should verify: %s", out)
	}
}

// TestCopyDataDir restores a copy of the data directory beside the
// original, which stays in place, and runs the server on the copy. The
// copy's rows hold the original's absolute paths; the server works on the
// copy's repositories, and the original is neither read for them nor
// written: its hooks and refs, and every other file, are unchanged.
func TestCopyDataDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, alice, token, work := relocateSetup(t)
	pushed := h.git(work, "rev-parse", "HEAD")
	h.stop()
	storeAbsolutePaths(t, h.data)
	original := h.data
	copied := filepath.Join(filepath.Dir(original), "copy")
	if err := os.CopyFS(copied, os.DirFS(original)); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, original)
	h.data = copied

	h.start()
	h.git(work, "remote", "set-url", "origin", h.cloneURL(token))
	h.git(work, "fetch", "origin")
	if got := h.git(work, "rev-parse", "origin/main"); got != pushed {
		t.Fatalf("fetch from the copy: origin/main is %s, want %s", got, pushed)
	}
	h.commitAs(work, "alice", "user:alice@localhost", "README.md", "# app\n\nCopied.\n", "on the copy")
	h.git(work, "push", "origin", "HEAD:main")
	head := h.git(work, "rev-parse", "HEAD")
	requireEvent(t, h.events(alice), "the push to the copy", pushRecorded("user:alice", "refs/heads/main", head))
	if got := h.git(h.repoPath(), "rev-parse", "refs/heads/main"); got != head {
		t.Fatalf("the copy's repository should hold the push: main is %s, want %s", got, head)
	}
	h.stop()

	after := snapshotTree(t, original)
	var changed []string
	for path, sum := range after {
		if before[path] != sum {
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changed = append(changed, path)
		}
	}
	if len(changed) > 0 {
		slices.Sort(changed)
		t.Fatalf("the original data directory changed under a server running on its copy: %v", changed)
	}
}

// TestMissingRepoDir removes a repository's directory while the server is
// down. serve then refuses to start and names the repository; admin
// commands that touch no repository still work.
func TestMissingRepoDir(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	h, _, _, _ := relocateSetup(t)
	h.stop()
	repo := h.repoPath()
	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"serve", "--data", h.data, "--listen", "127.0.0.1:0", "--web", "off"},
		{"admin", "reconcile", "--data", h.data},
	} {
		cmd := exec.CommandContext(ctx, h.bin, args...)
		cmd.Env = h.env
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "acme/app") || !strings.Contains(string(out), filepath.Base(repo)+" is missing") {
			t.Fatalf("kry %s should refuse, naming acme/app and its missing directory: %v\n%s", args[0], err, out)
		}
	}
	if out := h.run("admin", "verify", "--data", h.data); !strings.HasPrefix(out, "ok:") {
		t.Fatalf("admin verify touches no repository and should still work: %s", out)
	}
}

// relocateSetup starts a server with one repository, acme/app, which
// alice has cloned and pushed one commit to.
func relocateSetup(t *testing.T) (h *harness, alice client, token, work string) {
	t.Helper()
	dir := t.TempDir()
	h = newHarness(t, dir)
	token = tokenFrom(t, h.run("admin", "init", "--data", h.data, "--user", "alice"))
	h.start()
	alice = h.client(token)
	ok[map[string]any](t, alice, "POST", "/api/orgs", map[string]string{"name": "acme"})
	ok[map[string]any](t, alice, "POST", "/api/orgs/acme/repos", map[string]string{"name": "app"})
	work = filepath.Join(dir, "alice")
	h.git(dir, "clone", h.cloneURL(token), work)
	h.commitAs(work, "alice", "user:alice@localhost", "README.md", "# app\n", "initial commit")
	h.git(work, "push", "origin", "HEAD:main")
	return h, alice, token, work
}

// storeAbsolutePaths rewrites the repository rows the way krystallos wrote
// them before a data directory could move: each holds the repository's
// absolute path. The server must be down.
func storeAbsolutePaths(t *testing.T, data string) {
	t.Helper()
	data, err := filepath.EvalSymlinks(data)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(data, "krystallos.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	res, err := st.DB().Exec(`UPDATE repos SET path = ? || path WHERE path NOT LIKE '/%'`, filepath.Join(data, "repos")+"/")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatal("no repository row to rewrite")
	}
}

// pushRecorded matches the record of identity moving ref to oid.
func pushRecorded(identity, ref, oid string) func(event) bool {
	return func(e event) bool {
		if e.Type != "git.ref_transaction" || e.Identity != identity {
			return false
		}
		ups, _ := e.Payload["updates"].([]any)
		for _, u := range ups {
			if m, _ := u.(map[string]any); m["ref"] == ref && m["new"] == oid {
				return true
			}
		}
		return false
	}
}

// snapshotTree maps every path under root to its type, mode and, for a
// file, a hash of its content.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entry := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			entry += " " + hex.EncodeToString(sum[:])
		}
		out[rel] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
