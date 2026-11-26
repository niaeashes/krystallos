// Command gitsynth is the gitsynth server, its admin tool, and the hook that
// repositories call back into.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"gitsynth/internal/accesslog"
	"gitsynth/internal/app"
	"gitsynth/internal/server"
	"gitsynth/web"
)

const usage = `usage:
  gitsynth serve [--data DIR] [--listen ADDR] [--url URL] [--origin URLS] [--git BIN] [--web off|DIR] [--max-git N]
  gitsynth admin init       --data DIR --user NAME [--host HOST] [--url URL]   set the host and create the first (admin) user
  gitsynth admin add-user   --data DIR --user NAME [--url URL]
  gitsynth admin invite     --data DIR [--org ORG --set SET] [--url URL]  one-time link for someone to join
  gitsynth admin passkey-link --data DIR --user NAME [--url URL]   one-time link to add a passkey
  gitsynth admin login-link   --data DIR --user NAME [--url URL]   one-time sign-in link, for recovery
  gitsynth admin token      --data DIR --identity ID [--ttl DURATION]   a standing token; each run opens a session with it
  gitsynth admin verify     --data DIR                      check the event log's hash chain
  gitsynth admin reconcile  --data DIR                      compare records with the repositories on disk
  gitsynth admin access-log --data DIR [--identity ID] [--limit N]
  gitsynth version                                          the version this binary was built from

--url is the server's public address, where people open the web UI and git
and MCP clients connect: scheme://host[:port], no path. It defaults to
https://HOST for the host fixed at init, or for a server initialized as
localhost to http://localhost:PORT, with the port serve listens on (3000 for
admin). Clone URLs and the links admin prints start from it, and passkeys
are bound to its host name. --origin lists other origins the web UI is
opened from; without it, a localhost server also accepts the Vite dev
server at http://localhost:5173. serve's web UI is the one built in, unless
--web says otherwise. --max-git caps the git processes serving clones,
fetches and pushes at once (default twice the CPUs, 0 for no limit); a
client beyond it waits for one to finish.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "admin":
		err = cmdAdmin(os.Args[2:])
	case "hook":
		os.Exit(cmdHook(os.Args[2:]))
	case "version":
		fmt.Println(version())
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gitsynth:", err)
		os.Exit(1)
	}
}

// version is the version this binary was built from, as go build stamped it
// from the repository: the tag, for a clean checkout of a tagged commit (what
// scripts/release.sh builds), or else a pseudo-version naming the commit,
// ending in +dirty when there were uncommitted changes. A build with no
// repository to read, such as go run, has (devel).
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	data := fs.String("data", "./data", "data directory")
	listen := fs.String("listen", "127.0.0.1:3000", "address to listen on")
	gitBin := fs.String("git", "git", "git binary")
	urlFlag := fs.String("url", "", "public URL, scheme://host[:port]: where people open the web UI and git and MCP clients connect (default https://HOST, or http://localhost:PORT for a localhost server)")
	origin := fs.String("origin", "", "comma-separated other origins the web UI is opened from (default "+devOrigin+" for a localhost server)")
	webSource := fs.String("web", "", "web UI to serve: empty for the one built into the binary, off for none, or a directory holding a web build")
	maxGit := fs.Int("max-git", 2*runtime.NumCPU(), "git processes that may serve clones, fetches and pushes at once; 0 for no limit")
	fs.Parse(args)
	if *maxGit < 0 {
		return fmt.Errorf("--max-git %d: give 0 for no limit, or a positive number", *maxGit)
	}
	originSet := false
	fs.Visit(func(f *flag.Flag) { originSet = originSet || f.Name == "origin" })
	_, port, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen %s: %w", *listen, err)
	}

	ui, err := webFiles(*webSource)
	if err != nil {
		return err
	}

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := app.Open(ctx, app.Config{DataDir: *data, GitBin: *gitBin, HookBin: exe})
	if err != nil {
		return err
	}
	defer a.Close()
	if err := a.CheckRepos(ctx); err != nil {
		return err
	}
	// Hooks hold the binary's path as it was when they were written; point
	// them at this one before anything updates a ref.
	if err := a.InstallHooks(ctx); err != nil {
		return fmt.Errorf("install hooks: %w", err)
	}
	host, err := a.Host(ctx)
	if err != nil {
		return err
	}
	addr, err := resolveAddress(*urlFlag, *origin, originSet, host, port)
	if err != nil {
		return err
	}
	// Passkeys are bound to the URL's host name. An IP address cannot be a
	// relying party ID, so such a server runs without passkey sign-in.
	if problem := passkeyProblem(addr.URL); problem != "" {
		slog.Warn("passkey sign-in will not work at this URL: "+problem+"; pass --url", "url", addr.URL)
	}
	if rpID := hostName(addr.URL); net.ParseIP(rpID) == nil {
		if err := a.EnablePasskeys(app.PasskeyConfig{RPID: rpID, Origins: append([]string{addr.URL}, addr.Origins...)}); err != nil {
			return fmt.Errorf("passkeys: %w", err)
		}
	}
	// A test seam: tests serve Client ID Metadata Documents from a local TLS
	// server, which the guarded fetch would otherwise refuse.
	if caFile := os.Getenv("GITSYNTH_TEST_CLIENT_METADATA_CA"); caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return fmt.Errorf("GITSYNTH_TEST_CLIENT_METADATA_CA: no certificate in %s", caFile)
		}
		slog.Warn("TEST ONLY: client metadata documents are fetched from loopback addresses, trusting " + caFile)
		a.ConfigureMetadataFetch(app.MetadataFetchConfig{RootCAs: pool, AllowLoopback: true})
	}
	access, err := accesslog.Open(filepath.Join(a.DataDir(), "access.db"))
	if err != nil {
		return err
	}
	defer access.Close()
	srv := &server.Server{App: a, Access: access, Web: ui, URL: addr.URL, Origins: addr.Origins, MaxGit: *maxGit}

	// Hooks report ref updates here. Loopback only, guarded by a secret
	// that lives as long as this process.
	iln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	isrv := &http.Server{Handler: srv.InternalHandler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: idleTimeout}
	go isrv.Serve(iln)
	if err := a.SetInternal("http://"+iln.Addr().String(), randomHex(32)); err != nil {
		return err
	}
	defer a.ClearInternal()

	if err := a.Reconcile(ctx); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 30 * time.Second, IdleTimeout: idleTimeout}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		// Let requests finish, then cut off long polls and streams, so no
		// handler is still running when the logs and database close.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hs.Shutdown(sctx); err != nil {
			hs.Close()
		}
		if err := isrv.Shutdown(sctx); err != nil {
			isrv.Close()
		}
	}()
	slog.Info("gitsynth listening", "version", version(), "addr", ln.Addr().String(), "url", addr.URL, "origins", addr.Origins, "data", a.DataDir(), "max_git", *maxGit)
	if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		stop()
		<-stopped
		return err
	}
	<-stopped
	return nil
}

// idleTimeout closes a keep-alive connection that has sat between requests
// this long. There is no read or write timeout: long polls, the event
// stream and large pushes run as long as they need.
const idleTimeout = 2 * time.Minute

// webFiles picks the web UI serve answers browsers with: the build inside
// the binary (scripts/build.sh puts it there), none ("off", for a UI served
// from elsewhere on the same origin), or a build in a directory.
func webFiles(source string) (fs.FS, error) {
	switch source {
	case "":
		if files := web.Files(); files != nil {
			slog.Info("serving the web UI built into the binary")
			return files, nil
		}
		slog.Info("this binary has no web UI: build it with scripts/build.sh, or pass --web DIR")
		return nil, nil
	case "off":
		slog.Info("serving no web UI (--web off)")
		return nil, nil
	}
	dir, err := filepath.Abs(source)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("--web %s: no index.html there (build the UI with npm --prefix web run build)", source)
	}
	slog.Info("serving the web UI from a directory", "dir", dir)
	return os.DirFS(dir), nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func cmdAdmin(args []string) error {
	if len(args) == 0 {
		return errors.New("admin needs a subcommand\n" + usage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("admin "+sub, flag.ExitOnError)
	data := fs.String("data", "./data", "data directory")
	user := fs.String("user", "", "user name")
	host := fs.String("host", "localhost", "host every identity is qualified with; fixed at init")
	org := fs.String("org", "", "org to invite into")
	set := fs.String("set", "", "permission set an invited user gets in --org")
	identity := fs.String("identity", "", "identity, such as user:alice/claude")
	ttl := fs.Duration("ttl", 0, "token lifetime (0 = no expiry)")
	limit := fs.Int("limit", 50, "number of entries")
	urlFlag := fs.String("url", "", "the server's public URL, where the links point (default https://HOST, or http://localhost:3000 for a localhost server)")
	fs.Parse(args[1:])

	ctx := context.Background()
	if sub == "access-log" {
		entries, err := accesslog.Read(ctx, filepath.Join(*data, "access.db"), *identity, *limit)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		for _, e := range entries {
			enc.Encode(e)
		}
		return nil
	}

	a, err := app.Open(ctx, app.Config{DataDir: *data})
	if err != nil {
		return err
	}
	defer a.Close()

	// The links admin prints start from the server's public URL, worked out
	// as serve does, for a server on the default port.
	var base string
	switch sub {
	case "init", "add-user", "invite", "passkey-link", "login-link":
		h := *host
		if sub != "init" {
			if h, err = a.Host(ctx); err != nil {
				return err
			}
		}
		if base, err = publicURL(*urlFlag, h, "3000"); err != nil {
			return err
		}
	}

	switch sub {
	case "init", "add-user":
		if sub == "init" {
			if err := a.Initialize(ctx, *host); err != nil {
				return err
			}
		}
		id, err := a.CreateUser(ctx, app.System, *user, sub == "init")
		if err != nil {
			return err
		}
		tok, err := a.IssueToken(ctx, app.System, id.String(), 0)
		if err != nil {
			return err
		}
		link, err := a.IssueEnrollmentLink(ctx, app.System, id.String(), "register", 24*time.Hour)
		if err != nil {
			return err
		}
		fmt.Printf("identity: %s\ntoken: %s\npasskey: %s/register?token=%s\n", id, tok.Token, base, link)
	case "invite":
		link, err := a.IssueInvite(ctx, app.System, *org, *set, 72*time.Hour)
		if err != nil {
			return err
		}
		fmt.Printf("%s/register?token=%s\n(valid for 72h, once)\n", base, link)
	case "passkey-link", "login-link":
		purpose, path, life := "register", "register", 24*time.Hour
		if sub == "login-link" {
			purpose, path, life = "login", "link", 15*time.Minute
		}
		link, err := a.IssueEnrollmentLink(ctx, app.System, *user, purpose, life)
		if err != nil {
			return err
		}
		fmt.Printf("%s/%s?token=%s\n(valid for %s, once)\n", base, path, link, life)
	case "token":
		tok, err := a.IssueToken(ctx, app.System, *identity, *ttl)
		if err != nil {
			return err
		}
		fmt.Printf("identity: %s\ntoken: %s\n", tok.Identity, tok.Token)
	case "verify":
		n, err := a.Store.Verify(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("ok: %d events, hash chain intact\n", n)
	case "reconcile":
		if err := a.CheckRepos(ctx); err != nil {
			return err
		}
		return a.Reconcile(ctx)
	default:
		return fmt.Errorf("unknown admin subcommand %q\n%s", sub, usage)
	}
	return nil
}

// cmdHook is the reference-transaction hook. git calls it for every ref
// update in a repository; it reports the update to the server, and in the
// prepared state a refusal aborts the update.
func cmdHook(args []string) int {
	fs := flag.NewFlagSet("hook", flag.ContinueOnError)
	data := fs.String("data", "", "data directory")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	stdin, _ := io.ReadAll(os.Stdin)
	rest := fs.Args()
	if len(rest) < 2 || rest[0] != "reference-transaction" {
		return 0
	}
	state := rest[1]
	if state != "prepared" && state != "committed" && state != "aborted" {
		return 0
	}
	var updates []app.RefUpdate
	for _, line := range strings.Split(strings.TrimSpace(string(stdin)), "\n") {
		if f := strings.Fields(line); len(f) == 3 {
			updates = append(updates, app.RefUpdate{Old: f[0], New: f[1], Ref: f[2]})
		}
	}
	if len(updates) == 0 {
		return 0
	}

	url, secret := os.Getenv("GITSYNTH_INTERNAL_URL"), os.Getenv("GITSYNTH_INTERNAL_SECRET")
	startedByServer := url != ""
	if !startedByServer {
		// Someone ran git directly on the server. Report it if the server is
		// up; otherwise it is caught when the server next starts.
		info, err := app.ReadInternalInfo(*data)
		if err != nil {
			if state == "prepared" {
				fmt.Fprintln(os.Stderr, "gitsynth: server is not running; this ref change will be recorded as unattributed when it starts")
			}
			return 0
		}
		url, secret = info.URL, info.Secret
	}
	cwd, _ := os.Getwd()
	repoPath, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		repoPath = cwd
	}
	body, _ := json.Marshal(app.RefTxnRequest{State: state, RepoPath: repoPath, Op: os.Getenv("GITSYNTH_OP"), Updates: updates})
	req, err := http.NewRequest(http.MethodPost, url+"/internal/ref-txn", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gitsynth:", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Gitsynth-Internal", secret)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		if !startedByServer {
			fmt.Fprintln(os.Stderr, "gitsynth: server unreachable; this ref change will be recorded when it starts")
			return 0
		}
		fmt.Fprintln(os.Stderr, "gitsynth: cannot record ref update:", err)
		if state == "prepared" {
			return 1
		}
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var msg struct {
			Error string `json:"error"`
		}
		raw, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(raw, &msg) != nil || msg.Error == "" {
			msg.Error = strings.TrimSpace(string(raw))
		}
		fmt.Fprintln(os.Stderr, "gitsynth:", msg.Error)
		if state == "prepared" {
			return 1
		}
	}
	return 0
}
