package app

import (
	"context"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestLogListsChildrenBeforeParents gives every commit the same author and
// committer date, so time cannot order them: the history must still list
// each commit before every one of its ancestors, with or without a path,
// and a limit must cut that same list short.
func TestLogListsChildrenBeforeParents(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	a, err := Open(ctx, Config{DataDir: filepath.Join(t.TempDir(), "data"), GitBin: "git"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	repo := Repo{FullName: "acme/app", Org: "acme", Name: "app", Path: filepath.Join(t.TempDir(), "app.git")}
	if _, err := a.Git.Output(ctx, t.TempDir(), nil, "init", "-q", "--bare", repo.Path); err != nil {
		t.Fatal(err)
	}

	const when = "1767225600 +0000" // 2026-01-01T00:00:00Z, for every commit
	env := []string{
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com", "GIT_COMMITTER_DATE=" + when,
	}
	git := func(stdin string, args ...string) string {
		t.Helper()
		cmd := a.Git.Cmd(ctx, repo.Path, env, args...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
		return strings.TrimSpace(string(out))
	}
	parents := map[string][]string{}
	subject := map[string]string{}
	commit := func(msg string, files map[string]string, ps ...string) string {
		t.Helper()
		names := make([]string, 0, len(files))
		for name := range files {
			names = append(names, name)
		}
		sort.Strings(names)
		var tree strings.Builder
		for _, name := range names {
			tree.WriteString("100644 blob " + git(files[name], "hash-object", "-w", "--stdin") + "\t" + name + "\n")
		}
		args := []string{"commit-tree", git(tree.String(), "mktree")}
		for _, p := range ps {
			args = append(args, "-p", p)
		}
		oid := git(msg, append(args, "-m", msg)...)
		parents[oid], subject[oid] = ps, msg
		return oid
	}

	// main:    root - base - other ------- merge
	//                    \                 /
	// branch:             one ---------- two
	//
	// other touches only g; merge rewrites f. Walked by date alone, ties
	// go first come, first served: merge queues other and two, other
	// queues base, two queues one, and base comes out ahead of its child
	// one.
	root := commit("root", map[string]string{"f": "root", "g": "root"})
	base := commit("base", map[string]string{"f": "base", "g": "root"}, root)
	other := commit("other", map[string]string{"f": "base", "g": "other"}, base)
	one := commit("one", map[string]string{"f": "one", "g": "root"}, base)
	two := commit("two", map[string]string{"f": "two", "g": "root"}, one)
	merge := commit("merge", map[string]string{"f": "merge", "g": "other"}, other, two)

	var ancestors func(oid string, into map[string]bool) map[string]bool
	ancestors = func(oid string, into map[string]bool) map[string]bool {
		for _, p := range parents[oid] {
			if !into[p] {
				into[p] = true
				ancestors(p, into)
			}
		}
		return into
	}
	names := func(cs []Commit) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = subject[c.OID]
		}
		return out
	}
	check := func(path string, limit int, want ...string) []Commit {
		t.Helper()
		cs, err := a.Log(ctx, repo, merge, path, limit)
		if err != nil {
			t.Fatal(err)
		}
		got := names(cs)
		listed := map[string]bool{}
		for _, c := range cs {
			if strings.Join(c.Parents, " ") != strings.Join(parents[c.OID], " ") {
				t.Errorf("path %q: %s has parents %v, want %v", path, subject[c.OID], c.Parents, parents[c.OID])
			}
			for anc := range ancestors(c.OID, map[string]bool{}) {
				if listed[anc] {
					t.Errorf("path %q: %s is listed after its ancestor %s: %v", path, subject[c.OID], subject[anc], got)
				}
			}
			listed[c.OID] = true
		}
		sortedGot := append([]string(nil), got...)
		sort.Strings(sortedGot)
		sort.Strings(want)
		if strings.Join(sortedGot, " ") != strings.Join(want, " ") {
			t.Errorf("path %q, limit %d: listed %v, want %v in some order", path, limit, got, want)
		}
		return cs
	}

	check("", 0, "root", "base", "other", "one", "two", "merge")
	// other leaves f alone, so it drops out.
	all := check("f", 0, "root", "base", "one", "two", "merge")

	// A limit takes the first commits of that same order.
	first := names(all[:3])
	limited, err := a.Log(ctx, repo, merge, "f", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(limited); strings.Join(got, " ") != strings.Join(first, " ") {
		t.Errorf("path %q, limit 3: listed %v, want the first three of %v", "f", got, names(all))
	}
}
