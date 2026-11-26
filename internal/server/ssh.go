package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"gitsynth/internal/app"
	"gitsynth/internal/gitx"
)

// Git over SSH: the same repositories, permission checks, push checks and
// records as git over HTTP, for clients that authenticate with a key
// registered to an identity. Only git-upload-pack and git-receive-pack
// run; there is no shell, no port forwarding and no other command.

const (
	// sshHandshakeTimeout is how long a connection has to authenticate.
	sshHandshakeTimeout = 30 * time.Second
	// sshCommandTimeout is how long an authenticated connection has to ask
	// for a command. A connection runs one, as git's ssh transport does.
	sshCommandTimeout = 30 * time.Second
	// sshShutdownGrace is how long commands still running get to finish
	// when the server stops, as HTTP requests do.
	sshShutdownGrace = 5 * time.Second
	// sshCredential names the extension that carries, from authentication
	// to the command, the ID of the key the client proved it holds.
	sshCredential = "gitsynth-credential"
)

// HostKeyFile is the SSH host key's file in the data directory, so that
// moving or restoring the directory keeps the server's SSH identity.
const HostKeyFile = "ssh_host_ed25519_key"

// HostKey loads the server's SSH host key from the data directory, making
// one on first use.
func HostKey(dataDir string) (ssh.Signer, error) {
	path := filepath.Join(dataDir, HostKeyFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if b, err = newHostKey(path); errors.Is(err, fs.ErrExist) {
			b, err = os.ReadFile(path)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ssh host key %s: %w", path, err)
	}
	key, err := ssh.ParsePrivateKey(b)
	if err != nil {
		return nil, fmt.Errorf("ssh host key %s: %w", path, err)
	}
	return key, nil
}

func newHostKey(path string) ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "gitsynth host key")
	if err != nil {
		return nil, err
	}
	b := pem.EncodeToMemory(block)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	return b, nil
}

// ServeSSH serves git over SSH on ln until ctx ends. Then it takes no more
// connections, gives commands still running sshShutdownGrace to finish,
// and cuts off the rest; it returns once none is left, so nothing runs
// after the caller closes the database.
func (s *Server) ServeSSH(ctx context.Context, ln net.Listener, hostKey ssh.Signer) error {
	connCtx, cancelConns := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelConns()
	cfg := &ssh.ServerConfig{
		ServerVersion: "SSH-2.0-gitsynth",
		// Called for every key a client offers, before it proves it holds
		// one; the library accepts only once the signature checks out, and
		// then hands over the Permissions returned for that very key.
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			act, err := s.App.AuthenticateSSHKey(connCtx, key)
			if err != nil {
				return nil, err
			}
			return &ssh.Permissions{Extensions: map[string]string{sshCredential: act.Credential.ID}}, nil
		},
	}
	cfg.AddHostKey(hostKey)

	var (
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
		wg    sync.WaitGroup
	)
	stopAccepting := context.AfterFunc(ctx, func() { ln.Close() })
	defer stopAccepting()
	var serveErr error
	backoff := 5 * time.Millisecond
	for {
		nc, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			if errors.Is(err, net.ErrClosed) {
				serveErr = err
				break
			}
			// Out of file descriptors and the like: wait, as net/http does.
			slog.Warn("ssh accept failed; retrying", "err", err, "in", backoff)
			time.Sleep(backoff)
			backoff = min(2*backoff, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		mu.Lock()
		conns[nc] = struct{}{}
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				mu.Lock()
				delete(conns, nc)
				mu.Unlock()
			}()
			s.serveSSHConn(connCtx, nc, cfg)
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(sshShutdownGrace):
		cancelConns()
		mu.Lock()
		for nc := range conns {
			nc.Close()
		}
		mu.Unlock()
		<-done
	}
	return serveErr
}

func (s *Server) serveSSHConn(ctx context.Context, nc net.Conn, cfg *ssh.ServerConfig) {
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(sshHandshakeTimeout))
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		slog.Debug("ssh handshake failed", "remote", nc.RemoteAddr().String(), "err", err)
		return
	}
	defer conn.Close()
	nc.SetDeadline(time.Time{})
	// A client that hangs up stops what runs for it, git included, as a
	// request's context does over HTTP.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		conn.Wait()
		cancel()
	}()
	go ssh.DiscardRequests(reqs)
	credential := conn.Permissions.Extensions[sshCredential]
	idle := time.AfterFunc(sshCommandTimeout, func() { conn.Close() })
	defer idle.Stop()
	for nch := range chans {
		if nch.ChannelType() != "session" {
			nch.Reject(ssh.UnknownChannelType, "gitsynth serves git only")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			return
		}
		s.serveSSHSession(ctx, credential, ch, chReqs, idle)
		return
	}
}

// serveSSHSession answers a session's requests until it asks for a
// command, runs that one command and closes the session.
func (s *Server) serveSSHSession(ctx context.Context, credential string, ch ssh.Channel, reqs <-chan *ssh.Request, idle *time.Timer) {
	defer ch.Close()
	protocol := ""
	for req := range reqs {
		switch req.Type {
		case "env":
			// Git passes GIT_PROTOCOL this way (ssh -o SendEnv), which is how
			// a fetch asks for protocol v2. Nothing else is taken.
			var kv struct{ Name, Value string }
			ok := ssh.Unmarshal(req.Payload, &kv) == nil && kv.Name == "GIT_PROTOCOL" && validGitProtocol(kv.Value)
			if ok {
				protocol = kv.Value
			}
			req.Reply(ok, nil)
		case "exec":
			var c struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &c); err != nil {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			idle.Stop()
			go refuseSSHRequests(reqs)
			exitStatus(ch, s.sshCommand(ctx, credential, protocol, c.Command, ch))
			return
		case "shell":
			// What "ssh -T git@host" sees: whether the key works, and as whom.
			req.Reply(true, nil)
			idle.Stop()
			go refuseSSHRequests(reqs)
			if act, err := s.App.SSHKeyActor(ctx, credential); err != nil {
				fmt.Fprintf(ch.Stderr(), "gitsynth: %s\n", sshErrorMessage(err))
			} else {
				fmt.Fprintf(ch.Stderr(), "Hi %s! You have authenticated to gitsynth, which serves git only: there is no shell.\n", act.Identity)
			}
			exitStatus(ch, 1)
			return
		default:
			// pty-req, subsystem, x11-req and the rest.
			req.Reply(false, nil)
		}
	}
}

func refuseSSHRequests(reqs <-chan *ssh.Request) {
	for req := range reqs {
		if req.WantReply {
			req.Reply(false, nil)
		}
	}
}

func exitStatus(ch ssh.Channel, status uint32) {
	ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

// validGitProtocol accepts what git puts in GIT_PROTOCOL ("version=2",
// colon-separated key=value pairs) and nothing that could carry more.
func validGitProtocol(v string) bool {
	if v == "" || len(v) > 256 {
		return false
	}
	for _, c := range v {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("=:._-", c)
		if !ok {
			return false
		}
	}
	return true
}

func sshErrorMessage(err error) string {
	var ae *app.Error
	if errors.As(err, &ae) {
		return ae.Msg
	}
	slog.Error("ssh command failed", "err", err)
	return "internal error"
}

// sshCommand runs one git command for a client and returns its exit
// status. What the client may not do is said on stderr, as git's own
// remote errors are.
func (s *Server) sshCommand(ctx context.Context, credential, protocol, command string, ch ssh.Channel) uint32 {
	fail := func(msg string) uint32 {
		fmt.Fprintf(ch.Stderr(), "gitsynth: %s\n", msg)
		return 1
	}
	service, fullName, err := parseGitCommand(command)
	if err != nil {
		return fail(err.Error())
	}
	// The key is looked up again: one revoked, or an identity disabled,
	// since the handshake stops here.
	actor, err := s.App.SSHKeyActor(ctx, credential)
	if err != nil {
		return fail(sshErrorMessage(err))
	}
	perm := app.PermRepoPush
	if service == "git-upload-pack" {
		perm = app.PermRepoRead
	}
	repo, ok, err := s.App.Allowed(ctx, actor, fullName, perm)
	var ae *app.Error
	switch {
	case errors.As(err, &ae):
		return fail(ae.Msg)
	case err != nil:
		return fail(sshErrorMessage(err))
	case !ok && perm == app.PermRepoRead:
		return fail("repository not found")
	case !ok:
		return fail(actor.Identity + " lacks " + perm + " on " + repo.FullName)
	}
	if service == "git-upload-pack" && !strings.Contains(protocol, "version=2") {
		return fail(v2Required)
	}
	var env []string
	if protocol != "" {
		env = append(env, "GIT_PROTOCOL="+protocol)
	}
	release, ok := s.gitSlot(ctx)
	if !ok {
		return fail("the server is stopping; try again shortly")
	}
	defer release()
	if service == "git-upload-pack" {
		return s.sshGit(ctx, repo, env, ch, s.uploadRelay(actor, repo, ch), "upload-pack", ".")
	}
	// The ref advertisement before a push shows the caller every ref.
	s.recordRead(actor, "git.advertise", repo.FullName, "refs", nil)
	// The hook finds out who is pushing through this token.
	token, done := s.App.NewOp(app.Op{Actor: actor.Actor, Repo: repo.FullName, Source: "push"})
	defer done()
	return s.sshGit(ctx, repo, append(env, s.App.HookEnv(token)...), ch, nil, "receive-pack", ".")
}

// sshGit runs git with its output going to the client. Its input comes
// from relay, or straight from the client when relay is nil.
func (s *Server) sshGit(ctx context.Context, repo app.Repo, env []string, ch ssh.Channel, relay func(io.Reader, io.Writer) error, args ...string) uint32 {
	cmd := s.App.Git.Cmd(ctx, repo.Path, env, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		slog.Error("git failed", "repo", repo.FullName, "args", args, "err", err)
		return 1
	}
	cmd.Stdout = ch
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		slog.Error("git failed", "repo", repo.FullName, "args", args, "err", err)
		return 1
	}
	go func() {
		defer stdin.Close()
		if relay == nil {
			io.Copy(stdin, ch)
			return
		}
		if err := relay(ch, stdin); err != nil {
			fmt.Fprintf(ch.Stderr(), "gitsynth: %s\n", err)
		}
	}()
	if err := cmd.Wait(); err != nil {
		slog.Error("git failed", "repo", repo.FullName, "args", args, "err", err, "stderr", strings.TrimSpace(stderr.String()))
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			return uint32(ee.ExitCode())
		}
		return 1
	}
	return 0
}

// uploadRelay passes a fetch's requests to upload-pack one at a time,
// recording each before git sees it: over SSH a fetch is one stream, and
// the commits a request wants are the versions of the code this identity
// obtains, as over HTTP.
func (s *Server) uploadRelay(actor app.Actor, repo app.Repo, ch ssh.Channel) func(io.Reader, io.Writer) error {
	return func(from io.Reader, to io.Writer) error {
		r := bufio.NewReader(from)
		for {
			req, err := readUploadRequest(r)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			// A flush packet alone ends the session; it asks for nothing.
			if !bytes.Equal(req, flushPkt) {
				parsed, perr := gitx.ParseUploadRequest(req)
				var detail any = parsed
				if perr != nil {
					detail = map[string]any{"request": parsed, "parse_error": perr.Error()}
				}
				s.recordRead(actor, "git."+parsed.Command, repo.FullName, parsed.Command, detail)
			}
			if _, err := to.Write(req); err != nil {
				// git has gone; its exit says why.
				return nil
			}
		}
	}
}

var flushPkt = []byte("0000")

// readUploadRequest reads one protocol v2 request from a stream: pkt-lines
// up to and including the flush packet that ends it, at most
// maxUploadRequest bytes. It returns io.EOF when the stream ends between
// requests.
func readUploadRequest(r io.Reader) ([]byte, error) {
	var buf []byte
	head := make([]byte, 4)
	for {
		if _, err := io.ReadFull(r, head); err != nil {
			if errors.Is(err, io.EOF) && len(buf) == 0 {
				return nil, io.EOF
			}
			return nil, errors.New("the fetch request ended part way")
		}
		n, err := strconv.ParseUint(string(head), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("bad pkt-line length %q", head)
		}
		buf = append(buf, head...)
		switch {
		case n == 0:
			return buf, nil
		case n == 1 || n == 2:
			continue
		case n < 4:
			return nil, fmt.Errorf("bad pkt-line length %d", n)
		}
		if len(buf)+int(n)-4 > maxUploadRequest {
			return nil, errors.New("fetch request too large")
		}
		start := len(buf)
		buf = append(buf, make([]byte, n-4)...)
		if _, err := io.ReadFull(r, buf[start:]); err != nil {
			return nil, errors.New("the fetch request ended part way")
		}
	}
}

// parseGitCommand reads the command git's ssh transport sends: the service
// and the repository's path as one shell word ("git-upload-pack
// '/org/repo.git'"). The path may start with "/" (ssh://host/org/repo.git)
// or not (host:org/repo.git), and ".git" is optional.
func parseGitCommand(command string) (service, fullName string, err error) {
	service, arg, _ := strings.Cut(strings.TrimSpace(command), " ")
	switch service {
	case "git-upload-pack", "git-receive-pack":
	default:
		return "", "", errors.New("gitsynth serves git-upload-pack and git-receive-pack only: there is no shell")
	}
	path, err := shellWord(strings.TrimSpace(arg))
	if err != nil {
		return "", "", fmt.Errorf("unreadable repository path in %q: %v", command, err)
	}
	fullName = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	org, name, ok := strings.Cut(fullName, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("no repository at %q: give it as org/name, such as git@host:acme/app.git", path)
	}
	return service, fullName, nil
}

// shellWord reads one word quoted for a POSIX shell, as git quotes the
// path: single quotes, with ' and ! written outside them after a backslash.
func shellWord(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return "", errors.New("unterminated quote")
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case '\\':
			if i+1 == len(s) {
				return "", errors.New("trailing backslash")
			}
			b.WriteByte(s[i+1])
			i += 2
		case ' ', '\t', '\n':
			return "", errors.New("more than one argument")
		default:
			b.WriteByte(c)
			i++
		}
	}
	if b.Len() == 0 {
		return "", errors.New("no path")
	}
	return b.String(), nil
}

// sshCloneURL is a repository's SSH address, or "" when the server serves
// no SSH: the short scp form on port 22, the ssh:// form on any other.
func (s *Server) sshCloneURL(repo app.Repo) string {
	if s.SSH == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(s.SSH)
	if err != nil {
		host, port = s.SSH, ""
	}
	if port == "" || port == "22" {
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		return "git@" + host + ":" + repo.FullName + ".git"
	}
	return "ssh://git@" + net.JoinHostPort(host, port) + "/" + repo.FullName + ".git"
}
