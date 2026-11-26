package app

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepoPaths checks that a repository is found under the running
// server's data directory, whether its row holds the directory's name or,
// as rows written before data directories could move do, an absolute path
// somewhere else.
func TestRepoPaths(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	a, err := Open(ctx, Config{DataDir: filepath.Join(t.TempDir(), "data"), GitBin: "git", HookBin: "/usr/local/bin/gitsynth"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	if err := a.Initialize(ctx, "localhost"); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateOrg(ctx, System, "acme"); err != nil {
		t.Fatal(err)
	}
	created, err := a.CreateRepo(ctx, System, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	repos := filepath.Join(a.DataDir(), "repos")
	name := filepath.Base(created.Path)
	want := filepath.Join(repos, name)
	if created.Path != want {
		t.Fatalf("a new repository should live in the data directory: %s", created.Path)
	}
	db := a.Store.DB()
	var stored string
	if err := db.QueryRowContext(ctx, `SELECT path FROM repos WHERE full_name = 'acme/app'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != name {
		t.Fatalf("the row should hold only the directory's name, got %q", stored)
	}

	elsewhere := filepath.Join(t.TempDir(), "old", "data", "repos", name)
	for _, row := range []string{name, elsewhere} {
		if _, err := db.ExecContext(ctx, `UPDATE repos SET path = ? WHERE full_name = 'acme/app'`, row); err != nil {
			t.Fatal(err)
		}
		repo, err := a.Repo(ctx, db, "acme/app")
		if err != nil || repo.Path != want {
			t.Fatalf("row %q: want %s, got %q (%v)", row, want, repo.Path, err)
		}
		all, err := a.Repos(ctx)
		if err != nil || len(all) != 1 || all[0].Path != want {
			t.Fatalf("row %q: listing should resolve the same way: %+v (%v)", row, all, err)
		}
		for _, path := range []string{want, want + "/", repos + "/./" + name} {
			if repo, err := a.RepoByPath(ctx, path); err != nil || repo.FullName != "acme/app" || repo.Path != want {
				t.Fatalf("row %q: %s should be acme/app at %s, got %+v (%v)", row, path, want, repo, err)
			}
		}
		for _, path := range []string{
			elsewhere, // the original of a moved or copied data directory
			filepath.Join(a.DataDir(), name),
			filepath.Join(repos, "nested", name),
			filepath.Join(repos, name[1:]), // a suffix of the name, not the name
			repos,
			"relative/repos/" + name,
		} {
			var e *Error
			if repo, err := a.RepoByPath(ctx, path); !errors.As(err, &e) || e.Code != http.StatusNotFound {
				t.Fatalf("row %q: %s should match no repository, got %+v (%v)", row, path, repo, err)
			}
		}
	}

	if err := a.CheckRepos(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(want); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRepos(ctx); err == nil || !strings.Contains(err.Error(), "acme/app") || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("a missing repository should be named: %v", err)
	}
	// An empty directory is not the repository either; git would go looking
	// in the directories above it.
	if err := os.Mkdir(want, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := a.CheckRepos(ctx); err == nil || !strings.Contains(err.Error(), "acme/app") || !strings.Contains(err.Error(), "not a git repository") {
		t.Fatalf("an empty directory should not pass for the repository: %v", err)
	}
}
