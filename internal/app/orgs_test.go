package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestInstallHooks restarts with the binary somewhere else: the hook is
// rewritten to call it, a hook that is already right is left alone, and
// without a binary there is nothing to point the hooks at.
func TestInstallHooks(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	data := filepath.Join(t.TempDir(), "data")
	open := func(hookBin string) *App {
		t.Helper()
		a, err := Open(ctx, Config{DataDir: data, GitBin: "git", HookBin: hookBin})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		return a
	}

	a := open("/old/place/gitsynth")
	if err := a.Initialize(ctx, "localhost"); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateOrg(ctx, System, "acme"); err != nil {
		t.Fatal(err)
	}
	repo, err := a.CreateRepo(ctx, System, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	hooks := filepath.Join(repo.Path, "hooks")
	hook := filepath.Join(hooks, "reference-transaction")

	a = open("/new/place/gitsynth")
	if err := a.InstallHooks(ctx); err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\nexec '/new/place/gitsynth' hook --data '" + a.DataDir() + "' reference-transaction \"$@\"\n"
	if got, err := os.ReadFile(hook); err != nil || string(got) != want {
		t.Fatalf("hook should call the new binary:\n got %q (%v)\nwant %q", got, err, want)
	}
	if st, err := os.Stat(hook); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("hook should be executable: %v %v", st.Mode(), err)
	}
	if left, _ := filepath.Glob(filepath.Join(hooks, ".reference-transaction-*")); len(left) > 0 {
		t.Fatalf("no temporary file should be left behind: %v", left)
	}

	// Already right: not written again.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(hook, past, past); err != nil {
		t.Fatal(err)
	}
	if err := a.InstallHooks(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(hook); err != nil || !st.ModTime().Equal(past) {
		t.Fatalf("an identical hook should be left alone, but it was modified at %v (%v)", st.ModTime(), err)
	}

	// Right words, but git would not run it.
	if err := os.Chmod(hook, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.InstallHooks(ctx); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(hook); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("a hook that cannot run should be made executable again: %v %v", st.Mode(), err)
	}

	// A failure says which repository.
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	if err := a.InstallHooks(ctx); err == nil || !strings.Contains(err.Error(), "acme/app") {
		t.Fatalf("an error should name the repository: %v", err)
	}
	a.Close()

	if err := open("").InstallHooks(ctx); err == nil {
		t.Fatal("without a binary to call, installing hooks should fail")
	}
}
