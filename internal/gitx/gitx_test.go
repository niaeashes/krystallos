package gitx

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCmdIgnoresServerGitConfig gives the process every way the server
// user's environment can configure or redirect git, and checks that none of
// them reaches a git that Cmd runs, while what the caller passes does.
func TestCmdIgnoresServerGitConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	hooks := filepath.Join(dir, "no-hooks")
	t.Setenv("GIT_CONFIG_GLOBAL", write("global", "[core]\n\thooksPath = "+hooks+"\n[gitsynth]\n\tglobal = leak\n"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	t.Setenv("GIT_CONFIG_SYSTEM", write("system", "[gitsynth]\n\tsystem = leak\n"))
	t.Setenv("GIT_CONFIG_PARAMETERS", "'gitsynth.parameters'='leak'")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "gitsynth.count")
	t.Setenv("GIT_CONFIG_VALUE_0", "leak")
	bogus := filepath.Join(dir, "bogus.git")
	t.Setenv("GIT_DIR", bogus)

	// Make sure the environment above would reach a plain git.
	leaked, err := exec.Command("git", "config", "--get", "core.hooksPath").Output()
	if err != nil || strings.TrimSpace(string(leaked)) != hooks {
		t.Fatalf("the test's environment should configure a plain git: %q, %v", leaked, err)
	}

	ctx := context.Background()
	g := Git{Bin: "git"}
	repo := filepath.Join(dir, "repo.git")
	if _, err := g.Output(ctx, "", nil, "init", "--bare", repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bogus); !os.IsNotExist(err) {
		t.Fatalf("GIT_DIR from the environment redirected git: %v", err)
	}
	gitDir, err := g.Output(ctx, repo, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := filepath.EvalSymlinks(repo); gitDir != want && gitDir != repo {
		t.Fatalf("git ran in %s, want %s", gitDir, repo)
	}
	for _, key := range []string{"core.hooksPath", "gitsynth.global", "gitsynth.system", "gitsynth.parameters", "gitsynth.count"} {
		out, err := g.Output(ctx, repo, nil, "config", "--get", key)
		if ExitCode(err) != 1 || out != "" {
			t.Errorf("%s reached git from the server's environment: %q, %v", key, out, err)
		}
	}

	// What the caller passes still reaches git, GIT_* included.
	ident, err := g.Output(ctx, repo, []string{"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.test"}, "var", "GIT_AUTHOR_IDENT")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ident, "Ada <ada@example.test> ") {
		t.Fatalf("GIT_AUTHOR_IDENT = %q, want the caller's author", ident)
	}
}

// TestCmdIgnoresServerGitAttributes gives the server user an attributes file
// that marks every file as binary for diffs. A git that Cmd runs must still
// show the change, so a reviewer sees it.
func TestCmdIgnoresServerGitAttributes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	xdg, home := filepath.Join(dir, "xdg"), filepath.Join(dir, "home")
	write(filepath.Join(xdg, "git", "attributes"), "* -diff\n")
	write(filepath.Join(home, ".config", "git", "attributes"), "* -diff\n")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", home)
	// Keep whoever runs the test from setting core.attributesFile elsewhere.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	ctx := context.Background()
	g := Git{Bin: "git"}
	work := filepath.Join(dir, "work")
	if _, err := g.Output(ctx, "", nil, "init", "-q", work); err != nil {
		t.Fatal(err)
	}
	who := []string{"GIT_AUTHOR_NAME=Ada", "GIT_AUTHOR_EMAIL=ada@example.test", "GIT_COMMITTER_NAME=Ada", "GIT_COMMITTER_EMAIL=ada@example.test"}
	for _, content := range []string{"before\n", "after\n"} {
		write(filepath.Join(work, "a.txt"), content)
		if _, err := g.Output(ctx, work, nil, "add", "a.txt"); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Output(ctx, work, who, "commit", "-q", "-m", content); err != nil {
			t.Fatal(err)
		}
	}

	// Make sure the attributes would hide the change from a plain git.
	plain := exec.Command("git", "diff", "HEAD~1", "HEAD")
	plain.Dir = work
	if out, err := plain.Output(); err != nil || !strings.Contains(string(out), "Binary files") {
		t.Fatalf("the test's attributes should make a plain git diff binary: %q, %v", out, err)
	}

	diff, err := g.Output(ctx, work, nil, "diff", "HEAD~1", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(diff, "Binary files") || !strings.Contains(diff, "-before\n+after") {
		t.Fatalf("the server user's attributes reached git; diff:\n%s", diff)
	}
}

func TestParseUploadRequestFetch(t *testing.T) {
	var b bytes.Buffer
	b.Write(PktLine("command=fetch\n"))
	b.Write(PktLine("agent=git/2.54.0\n"))
	b.WriteString("0001")
	b.Write(PktLine("thin-pack\n"))
	b.Write(PktLine("want 1111111111111111111111111111111111111111\n"))
	b.Write(PktLine("want 2222222222222222222222222222222222222222\n"))
	b.Write(PktLine("have 3333333333333333333333333333333333333333\n"))
	b.Write(PktLine("done\n"))
	b.WriteString("0000")

	req, err := ParseUploadRequest(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if req.Command != "fetch" || !req.Done || req.Haves != 1 {
		t.Fatalf("unexpected request: %+v", req)
	}
	want := []string{"1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"}
	if !slices.Equal(req.Wants, want) {
		t.Fatalf("wants = %v, want %v", req.Wants, want)
	}
}

func TestParseUploadRequestLsRefs(t *testing.T) {
	var b bytes.Buffer
	b.Write(PktLine("command=ls-refs\n"))
	b.WriteString("0001")
	b.Write(PktLine("peel\n"))
	b.Write(PktLine("ref-prefix refs/heads/\n"))
	b.WriteString("0000")
	req, err := ParseUploadRequest(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if req.Command != "ls-refs" || !slices.Equal(req.RefPrefixes, []string{"refs/heads/"}) {
		t.Fatalf("unexpected request: %+v", req)
	}
}

func TestReadPktsRejectsTruncatedInput(t *testing.T) {
	if _, err := ReadPkts([]byte("00ffshort")); err == nil {
		t.Fatal("expected an error for a truncated pkt-line")
	}
}
