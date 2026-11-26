package server

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"gitsynth/internal/app"
	"gitsynth/internal/gitx"
)

func TestParseGitCommand(t *testing.T) {
	good := []struct{ command, service, repo string }{
		{"git-upload-pack '/acme/app.git'", "git-upload-pack", "acme/app"},
		{"git-upload-pack 'acme/app.git'", "git-upload-pack", "acme/app"},
		{"git-receive-pack '/acme/app'", "git-receive-pack", "acme/app"},
		{"git-upload-pack '/acme/app.git/'", "git-upload-pack", "acme/app"},
		{`git-upload-pack 'acme/it'\''s.git'`, "git-upload-pack", "acme/it's"},
		{"git-upload-pack acme/app.git", "git-upload-pack", "acme/app"},
	}
	for _, c := range good {
		service, repo, err := parseGitCommand(c.command)
		if err != nil || service != c.service || repo != c.repo {
			t.Errorf("%q: got %q %q %v, want %q %q", c.command, service, repo, err, c.service, c.repo)
		}
	}
	bad := []string{
		"", "ls", "git-upload-archive '/acme/app.git'", "sh -c 'git-upload-pack acme/app'",
		"git-upload-pack", "git-upload-pack ''", "git-upload-pack '/acme'", "git-upload-pack '/git/acme/app.git'",
		"git-upload-pack 'acme/app.git' extra", "git-upload-pack 'acme/app.git", `git-upload-pack acme/app\`,
	}
	for _, c := range bad {
		if service, repo, err := parseGitCommand(c); err == nil {
			t.Errorf("%q: should be refused, got %q %q", c, service, repo)
		}
	}
}

func TestReadUploadRequest(t *testing.T) {
	ls := string(gitx.PktLine("command=ls-refs\n")) + "0001" + string(gitx.PktLine("ref-prefix refs/heads/\n")) + "0000"
	fetch := string(gitx.PktLine("command=fetch\n")) + "0001" + string(gitx.PktLine("want "+strings.Repeat("a", 40)+"\n")) + string(gitx.PktLine("done\n")) + "0000"
	r := strings.NewReader(ls + fetch + "0000")
	for _, want := range []string{ls, fetch, "0000"} {
		got, err := readUploadRequest(r)
		if err != nil || string(got) != want {
			t.Fatalf("got %q %v, want %q", got, err, want)
		}
	}
	if _, err := readUploadRequest(r); !errors.Is(err, io.EOF) {
		t.Fatalf("the end of the stream between requests should be io.EOF, got %v", err)
	}
	req, err := gitx.ParseUploadRequest([]byte(fetch))
	if err != nil || req.Command != "fetch" || len(req.Wants) != 1 || !req.Done {
		t.Fatalf("a request read from the stream should parse: %+v %v", req, err)
	}

	for _, broken := range []string{
		string(gitx.PktLine("command=ls-refs\n")), // no flush
		"00",          // short length
		"zzzz",        // not hex
		"0003",        // below the smallest data packet
		"0010command", // shorter than it says
	} {
		if _, err := readUploadRequest(strings.NewReader(broken)); err == nil || errors.Is(err, io.EOF) {
			t.Errorf("%q: should be an error, got %v", broken, err)
		}
	}

	huge := bytes.Repeat(gitx.PktLine(strings.Repeat("x", 65000)), maxUploadRequest/65000+2)
	if _, err := readUploadRequest(bytes.NewReader(huge)); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("a request past maxUploadRequest should be refused, got %v", err)
	}
}

func TestValidGitProtocol(t *testing.T) {
	for _, v := range []string{"version=2", "version=2:object-format=sha256", "version=1"} {
		if !validGitProtocol(v) {
			t.Errorf("%q should be accepted", v)
		}
	}
	for _, v := range []string{"", "version=2 ", "version=2;rm", "version=2\n", strings.Repeat("a", 257)} {
		if validGitProtocol(v) {
			t.Errorf("%q should be refused", v)
		}
	}
}

func TestSSHCloneURL(t *testing.T) {
	repo := app.Repo{FullName: "acme/app"}
	for _, c := range []struct{ ssh, want string }{
		{"", ""},
		{"git.example.com", "git@git.example.com:acme/app.git"},
		{"git.example.com:22", "git@git.example.com:acme/app.git"},
		{"git.example.com:2222", "ssh://git@git.example.com:2222/acme/app.git"},
		{"[::1]:2222", "ssh://git@[::1]:2222/acme/app.git"},
		{"[::1]:22", "git@[::1]:acme/app.git"},
	} {
		s := &Server{SSH: c.ssh}
		if got := s.sshCloneURL(repo); got != c.want {
			t.Errorf("SSH %q: got %q, want %q", c.ssh, got, c.want)
		}
	}
}
