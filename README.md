# gitsynth

A self-hosted Git forge for small teams, built on the assumption that people and AI agents develop together.

- Every action is recorded as an event signed by an identity (such as `user:alice`, `user:alice/claude` or `bot:devbox/triage`), in a hash-chained log
- Reads are recorded too: API calls, the code and commits that were looked at, the commits a fetch asked for, event subscriptions
- Work moves from idea to issue to plan to change set

The design is in [docs/concept.md](docs/concept.md).

## Status

An early vertical slice. The server and the web UI run, and end-to-end tests drive the main flows with the real git client.

**What works**

- Users, bots and services. Users join through one-time invitations; bots are owned by a user, an org or the server; services are registered from a manifest and installed into orgs with the permissions it asks for
- Identities written `kind:root/label/...`, derived from each other without limit. Identities are made on purpose: a user derives them in their own tree, and a bot's owner in the bot's; an agent cannot. A derived identity's permissions never exceed its parent's. Disabling an identity revokes every credential and session at it and below it; enabling it again brings none of them back, so it starts over with fresh credentials
- Commits belong to identities through their committer: an identity's canonical address (`user:alice/claude@host`) or an address registered to it. A push is refused when a new commit's committer is not the pusher or an identity it controls; history a repository admin brings in is accepted and recorded as foreign. The server's merge commits are committed by `service:gitsynth`, authored by whoever asked
- Standing tokens, issued by the person (or a bot's owner) for an identity, with the address to commit with
- Sessions as records: any credential of an identity opens a session of that identity, with a short-lived token bound to it, and a session's token opens child sessions (a subagent's run under its parent's). Everything done through a session carries its ID. A session's token never outlives the credential that opened it, and stops when that credential is revoked or expires
- Passkey sign-in for users in the browser; a cookie session guarded against cross-site requests; a one-time admin sign-in link for recovery
- A hash-chained event log, with projection tables updated in the same transaction (`admin verify` detects tampering)
- Orgs and repositories with permission sets: the built-in read / write / maintain / admin, plus each repository's own. Permissions are granted to identities
- Ideas (kept in the org and associated with several repositories), conversion into issues, plans with versions and steps, and change sets tied to a plan version and its steps
- Reviews with comments on diff lines, checks (CI results and the like), merge rules judged from the record, and merging with a merge commit
- Reading code over the API: refs, trees, files, history, and change set diffs, each pinned to a commit and recorded as a read
- Git smart HTTP (fetches require protocol v2). Every ref update passes through the reference-transaction hook and is recorded; if it cannot be recorded, the update stops. Only merges and repository admins can move the default branch
- Reconciliation at startup: refs that changed without going through the server are recorded as `unattributed`
- Event delivery: long polling from `after`, and SSE
- An MCP endpoint per identity for agents, which an MCP client binds to through OAuth (gitsynth is the authorization server; the person approves with a passkey), and where each connection is a session of that identity
- A web UI (React + Vite) for all of the above, with a live activity view, built into the server binary and served at the server's own address

**Not there yet**

SSH and FIDO keys, commit signature verification and server signing, creating change sets by pushing to `refs/for/...`, change set stacking and change IDs, plan revisions in the UI, the inbox, webhooks, aggregation and retention of read records, and capping an identity's permissions.

## Running it

It needs Go 1.27, Node 22 or later, and git 2.38 or later.

```bash
scripts/build.sh
```

This builds the web UI, then the server with the UI inside, into `bin/gitsynth`. Give another path as the argument. `GOOS` and `GOARCH` pass through to the Go build, so `GOOS=linux GOARCH=amd64 scripts/build.sh bin/gitsynth-linux-amd64` cross-compiles.

```bash
./bin/gitsynth admin init --data ./data --user alice --host localhost
```

This fixes the host every identity is qualified with (`user:alice@localhost`), creates the first user, and prints a token (`gs_...`, shown only once) and a one-time link for creating a passkey. Invite more people from the Identities page, or with `gitsynth admin invite`.

```bash
./bin/gitsynth serve --data ./data
```

`gitsynth serve` alone serves the web UI at its own address, `http://localhost:3000`, next to the API and git. Open the passkey link there. `--web DIR` serves a web build from a directory instead of the one inside the binary, and `--web off` serves none. `--max-git N` caps how many git processes serve clones, fetches and pushes at once (default twice the CPUs, `0` for no limit); a client beyond it waits for one to finish, and gets 503 if it gives up first.

That address is the server's public URL: where people open the UI and where git and MCP clients connect. Clone URLs, invitation links and the links `admin` prints start from it, passkeys are bound to its host name, and the session cookie is `Secure` when it is https. It comes from the host given at init: `https://HOST`, or for a server initialized as `localhost`, `http://localhost:` and the port `serve` listens on (`admin` assumes 3000). A localhost server also accepts passkeys from the Vite dev server at `http://localhost:5173`. On a real domain behind a reverse proxy that terminates TLS, initialize with that domain as the host and no address flags are needed. When the address is not `https://HOST` (another port or name, say), give both `serve` and `admin` the same `--url`, such as `--url https://git.example.com:8443`. `serve --origin` lists other origins the UI is opened from, replacing the Vite default. The server never takes its address from a request's `Host` or `X-Forwarded-*` headers.

If the link expired, or you need a way back in without a passkey:

```bash
./bin/gitsynth admin passkey-link --data ./data --user alice
```

```bash
./bin/gitsynth admin login-link --data ./data --user alice
```

### Working on the web UI

A plain Go build has no UI inside; Vite serves it instead, reloading as you edit, and proxies `/api` and `/git` to the server.

```bash
go build -o bin/gitsynth ./cmd/gitsynth
```

```bash
npm --prefix web install
```

```bash
npm --prefix web run dev
```

The UI is then at `http://localhost:5173`. Passkeys work there too: they are bound to `localhost`, whatever the port. For links that open there, give `admin` `--url http://localhost:5173`.

To have something to look at, seed a demo org, repository, agent identity, issue, plan and change set, a bot and a CI service:

```bash
scripts/demo-seed.sh gs_YOUR_TOKEN
```

### From the command line

```bash
export TOKEN=gs_...
curl -H "Authorization: Bearer $TOKEN" -d '{"name":"acme"}' http://127.0.0.1:3000/api/orgs
curl -H "Authorization: Bearer $TOKEN" -d '{"name":"app"}' http://127.0.0.1:3000/api/orgs/acme/repos
git -c "http.extraHeader=Authorization: Bearer $TOKEN" clone http://127.0.0.1:3000/git/acme/app.git
```

Give each agent or role an identity of its own. An MCP client gets one by OAuth at the identity's endpoint (see "For agents (MCP)" below); anything else is handed a standing token. You do this on the Identities page, or with your own token; an agent can do neither, so it never gains more than it was given. A different role or system prompt deserves its own identity; another run of the same one is a session.

```bash
curl -H "Authorization: Bearer $TOKEN" -d '{"label":"claude"}' http://127.0.0.1:3000/api/identities
curl -H "Authorization: Bearer $TOKEN" -d '{"identity":"user:alice/claude","ttl_hours":168}' http://127.0.0.1:3000/api/tokens
```

Each run of the agent opens a session with that token and works through the session's token, so its actions are told apart from other runs. The label is an outside ID, such as a Claude Code session ID. A subagent opens a child session with its parent's session token. A session's token lasts 8 hours unless `ttl_hours` says otherwise (at most 24), never longer than the token that opened it, and stops when that token is revoked. It comes with the address to commit with; set `GIT_COMMITTER_EMAIL` to it (or to an address registered to the identity), or the push is refused.

```bash
curl -H "Authorization: Bearer $AGENT_TOKEN" -d '{"label":"CLAUDE_CODE_SESSION_ID"}' http://127.0.0.1:3000/api/sessions
```

Events can be read on from where you left off.

```bash
curl -H "Authorization: Bearer $TOKEN" "http://127.0.0.1:3000/api/events?after=0&wait=30"
```

### For agents (MCP)

Each identity has its own MCP endpoint, with the identity spelled out in the path: `/mcp/<kind>/<root>/<label>...`, such as `/mcp/user/alice/claude` (streamable HTTP). It serves only a credential of exactly that identity. Add it to Claude Code with the server's public URL:

```bash
claude mcp add --transport http gitsynth https://git.example.com/mcp/user/alice/claude
```

The first call gets a 401 that points at the endpoint's OAuth metadata, and Claude Code opens the browser on gitsynth's consent page. Sign in with your passkey, check which client asks to act as which identity, and approve; if `user:alice/claude` does not exist yet, approving creates it. Claude Code gets an access token for that endpoint alone (an hour) and a refresh token (30 days, replaced on every use), and keeps them itself: no token of yours is handed to the agent. The Identities page lists each identity's connected clients, with a Revoke button; disabling the identity stops them too.

- **What the token is good for.** An access token works only at the endpoint it was issued for. At another identity's endpoint, the API, git and the old `/mcp` it gets 401.
- **Approving.** Only a derived identity of the signed-in person can be bound, never the person's own identity.
- **Registering.** Clients register with a Client ID Metadata Document (Claude Code does) or with Dynamic Client Registration. A loopback redirect URI matches on any port.
- **Plain http.** Claude Code accepts an authorization server over plain http only on `localhost`, `127.0.0.1` or `::1`; a server anywhere else needs https.

The endpoint keeps state: each connection's `initialize` opens a session of the identity, and everything the client does until it disconnects is recorded under that session, however often its access token is refreshed. The endpoints speak MCP revisions up to 2025-11-25, the last with sessions; a client of a newer revision falls back to `initialize` as the spec says.

- **Child sessions.** A run that shares the connection, such as a Claude Code subagent (subagents share their parent's MCP connection), calls `open_session` and passes the session it gets as the `session` argument, which every tool takes, on its later calls. A session can name only sessions opened below its own.
- **Git.** `git_credential` returns a credential for the current or named session: git only, an hour at most, and it ends with the session. It comes with the git URL and the address to commit with (`GIT_COMMITTER_EMAIL`), so pushes are recorded under that session.

Bots, CI and other clients that cannot go through a browser use a static token at the same endpoint. Issue one on the Identities page, or from the server:

```bash
./bin/gitsynth admin token --data ./data --identity user:alice/claude --ttl 168h
claude mcp add --transport http gitsynth https://git.example.com/mcp/user/alice/claude --header "Authorization: Bearer gs_..."
```

The old endpoint `/mcp` still serves static tokens, taking the identity from the token, statelessly and with no OAuth. It is deprecated: move clients to the identity's own endpoint.

The tools mirror the API:
- `whoami`: identity, session, the address to commit with.
- Orgs and repositories: `list_orgs`, `list_repos`, `create_repo`.
- Work: `create_idea`, `list_issues`, `get_issue`, `create_issue`, `comment_issue`, `create_plan`, `revise_plan`, `create_changeset`, `get_changeset`, `get_changeset_diff`, `review_changeset`, `report_check`.
- Code: `list_tree`, `read_file`, `list_commits`.
- Sessions, git and addresses: `open_session`, `git_credential`, `add_commit_email`.
- `wait_for_events`, to follow the event stream.

There are no tools to make identities or standing tokens. Reads through MCP are recorded like any other read.

## Running on a server

One Linux VM runs it, with [Caddy](https://caddyserver.com) in front for TLS. [deploy/](deploy) holds the systemd unit and the Caddyfile used here. The examples use `gitsynth.example.com` for the domain and `alice` for the first user.

It needs:

- A Linux VM with systemd and a writable local disk (not network storage such as NFS). As a rough size: 2 vCPUs, 2 GB of memory, 30–60 GB of disk.
- git 2.38 or later on the VM.
- A domain name, with an A record (and AAAA, if the VM has IPv6) pointing at the VM's public address.
- TCP 80 and 443 open to the VM, in the provider's firewall as well as on the host. Caddy uses 80 to get the certificate and to redirect to 443. Keep 22 for your own SSH.

Build it on your own machine, with Go and Node as above. For an ARM VM, use `GOARCH=arm64`.

```bash
GOOS=linux GOARCH=amd64 scripts/build.sh bin/gitsynth-linux-amd64
```

Or take a release archive for the VM's architecture (see [Releasing](#releasing)). It holds the same binary, as `gitsynth`, and the same `deploy/` files. Check it against the release's `SHA256SUMS` before unpacking it:

```bash
sha256sum --ignore-missing -c SHA256SUMS
```

Copy the binary and the two files in `deploy/` to the VM (`VM` being however you reach it over SSH):

```bash
scp bin/gitsynth-linux-amd64 deploy/gitsynth.service deploy/Caddyfile VM:
```

The rest runs on the VM, in the directory they were copied to. Install the binary:

```bash
sudo install -m 755 gitsynth-linux-amd64 /usr/local/bin/gitsynth
```

Create a system user for the server, and a data directory owned by it. The server has to run as the user that owns the data directory: it runs git with that user's git config ignored, so `safe.directory` cannot excuse repositories owned by someone else.

```bash
sudo useradd --system --user-group --home-dir /var/lib/gitsynth --shell /usr/sbin/nologin gitsynth
```

```bash
sudo install -d -o gitsynth -g gitsynth -m 750 /var/lib/gitsynth
```

Initialize it as that user, with the domain as the host:

```bash
sudo -u gitsynth /usr/local/bin/gitsynth admin init --data /var/lib/gitsynth --user alice --host gitsynth.example.com
```

The host cannot be changed later: every identity is qualified with it (`user:alice@gitsynth.example.com`), and the server's public URL is `https://gitsynth.example.com`. `init` prints a token (shown only once) and a passkey link, `https://gitsynth.example.com/register?token=...`, valid for 24 hours.

Install the unit and start the server. It listens on `127.0.0.1:3000` only; `journalctl -u gitsynth` shows its log.

```bash
sudo install -m 644 gitsynth.service /etc/systemd/system/gitsynth.service
```

```bash
sudo systemctl enable --now gitsynth
```

Install Caddy as its documentation describes for your distribution; the Debian and Ubuntu packages run it as a service that reads `/etc/caddy/Caddyfile`. Put your domain in place of `gitsynth.example.com` in the Caddyfile, install it, and reload Caddy, which then gets the certificate:

```bash
sudo install -m 644 Caddyfile /etc/caddy/Caddyfile
```

```bash
sudo systemctl reload caddy
```

Open the passkey link and create a passkey. From then on you sign in at `https://gitsynth.example.com`. Create an org and a repository there, and issue a token on the Identities page (or use the one `init` printed). Clone and push over HTTPS with the token; SSH is not there yet.

```bash
git -c "http.extraHeader=Authorization: Bearer $TOKEN" clone https://gitsynth.example.com/git/acme/app.git
```

A push is refused unless each new commit's committer is you, so commit with `GIT_COMMITTER_EMAIL=user:alice@gitsynth.example.com`, or register the address you commit with on the Identities page.

Run every `admin` command as the gitsynth user, with the data directory. Their links start from `https://gitsynth.example.com` without `--url`.

```bash
sudo -u gitsynth /usr/local/bin/gitsynth admin login-link --data /var/lib/gitsynth --user alice
```

### Upgrading

Build and copy the new binary as above, then put it in place and restart:

```bash
sudo install -m 755 gitsynth-linux-amd64 /usr/local/bin/gitsynth
```

```bash
sudo systemctl restart gitsynth
```

The repositories' hooks follow on their own: on start, the server points every hook at the binary it runs as.

`gitsynth version` prints the version a binary was built from, and the server logs it when it starts (`journalctl -u gitsynth`).

### Backups

There is no online backup yet. Copying `/var/lib/gitsynth` while the server runs is not a backup: the databases and repositories can be caught halfway through a write. Stop the server, copy the directory, and start it again:

```bash
sudo systemctl stop gitsynth
```

```bash
sudo tar -C /var/lib -czf gitsynth-backup.tar.gz gitsynth
```

```bash
sudo systemctl start gitsynth
```

A restore can go to a different path or another machine: start the server with `--data` pointing at it, and it uses the repositories there.

### Bringing in an existing repository

Create the repository in the web UI; it starts empty, with `main` as its default branch. Then push the project's history from a clone of it, with its main branch checked out, as a repository admin (whoever created the org is one):

```bash
git -c "http.extraHeader=Authorization: Bearer $TOKEN" push --tags https://gitsynth.example.com/git/acme/app.git HEAD:main
```

Push other branches the same way. Commits whose committer the server does not know are accepted, because a repository admin pushed them, and are recorded as foreign.

### On Sakura Cloud

Notes for a server on Sakura Cloud (さくらのクラウド):

- Open the ports in the packet filter on the server's NIC, not only on the host: TCP 80 and 443, and 22 for administration. Git over SSH will need a port of its own when it comes.
- The Ubuntu Server 26.04 LTS image is an option. Per Sakura's release notes, it ships with fail2ban enabled and its firewall off; ufw is the host-side tool.
- The server's disk appears to the OS as an ordinary block device with a local filesystem, not a network file system such as NFS, which is what the data directory needs.
- Sakura's object storage speaks the S3 protocol, so it is a candidate destination for continuous backups once they exist (not yet tried).

## API

Requests authenticate with `Authorization: Bearer TOKEN`, with a token as the basic-auth password (git), or with the browser session cookie. Requests that change something with the cookie must also send `X-Gitsynth-CSRF: 1`.

| Method and path | What it does |
|---|---|
| `POST /api/auth/register/begin`, `/finish` | Create a passkey through a one-time link |
| `POST /api/auth/login/begin`, `/finish` | Sign in with a passkey |
| `POST /api/auth/link` | Sign in once with an admin recovery link |
| `POST /api/auth/link/inspect` | Say what a one-time link is for (an invitation asks for a user name) |
| `POST /api/auth/logout` | End the browser session |
| `GET /api/me` | Your identity and session |
| `GET /api/server` | The host identities are qualified with |
| `GET`, `POST /api/identities` | List your identities, or derive one (`{"parent", "label"}`): in your own tree as a user's root identity, or in a bot's tree as its owner |
| `POST /api/identities/disable` | Disable an identity you control and everything derived from it, revoking every credential and session there (`{"id"}`) |
| `POST /api/identities/enable` | Enable a disabled identity again (`{"id"}`); its revoked credentials stay revoked. For the person, or a bot's owner |
| `POST /api/sessions` | Open a session of your own identity (`{"label", "ttl_hours"}`, both optional), as a child of your session if you call with a session's token; returns the session, its `token`, `expires_at` and `committer_email` |
| `GET`, `POST /api/emails` | List, or register, the addresses commits resolve to your identities by |
| `POST /api/emails/remove` | Remove a registered address |
| `GET`, `POST /api/bots` | List the bots you manage, or create one owned by you, an org or the server |
| `GET`, `POST /api/services` | List services, or register one from a manifest (instance admins) |
| `POST /api/invitations` | A one-time link for someone to join, optionally into an org |
| `GET`, `POST /api/tokens` | List the credentials you control, or issue a standing token (`{"identity", "ttl_hours"}`) as the person, or as a bot's owner |
| `POST /api/tokens/{id}/revoke` | Revoke a credential |
| `GET`, `POST /api/orgs` | List your orgs, or create one |
| `POST /api/orgs/{org}/grants` | Grant a permission set across the org |
| `POST /api/orgs/{org}/services` | Install a service into the org, approving what its manifest asks for |
| `GET`, `POST /api/orgs/{org}/repos` | List or create repositories |
| `GET`, `POST /api/orgs/{org}/ideas` | List or create ideas |
| `GET /api/orgs/{org}/ideas/{n}` | An idea and the issues made from it |
| `POST /api/orgs/{org}/ideas/{n}/convert` | Make an issue from an idea |
| `GET /api/repos/{org}/{repo}` | Repository, branches and tags, your permissions |
| `GET /api/repos/{org}/{repo}/tree?ref=&path=` | A directory at a commit |
| `GET /api/repos/{org}/{repo}/blob?ref=&path=` | A file at a commit |
| `GET /api/repos/{org}/{repo}/commits?ref=&path=` | History |
| `POST /api/repos/{org}/{repo}/grants` | Grant a permission set on the repository |
| `POST /api/repos/{org}/{repo}/perm-sets` | Define the repository's own permission set |
| `PUT /api/repos/{org}/{repo}/merge-rule` | Set the merge rule (required approvals and checks) |
| `GET`, `POST /api/repos/{org}/{repo}/issues` | List or open issues |
| `GET /api/repos/{org}/{repo}/issues/{n}` | An issue with its plans' progress and its change sets |
| `POST /api/repos/{org}/{repo}/issues/{n}/comments` | Comment |
| `POST /api/repos/{org}/{repo}/issues/{n}/close` | Close an issue |
| `POST /api/repos/{org}/{repo}/issues/{n}/plans` | Create a plan |
| `GET /api/repos/{org}/{repo}/plans/{n}` | A plan's latest version and the state of each step |
| `POST /api/repos/{org}/{repo}/plans/{n}/revisions` | Revise a plan (step IDs can carry over) |
| `POST /api/repos/{org}/{repo}/issues/{n}/changesets` | Open a change set |
| `GET /api/repos/{org}/{repo}/changesets` | List change sets |
| `GET /api/repos/{org}/{repo}/changesets/{n}` | A change set, its reviews, and whether it can be merged |
| `GET /api/repos/{org}/{repo}/changesets/{n}/diff` | What the change set changes |
| `POST /api/repos/{org}/{repo}/changesets/{n}/reviews` | Review (approve / comment / request_changes), with line comments |
| `POST /api/repos/{org}/{repo}/changesets/{n}/merge` | Merge |
| `POST /api/repos/{org}/{repo}/checks` | Report a check result |
| `GET /api/events` | The event stream (`after`, `limit`, `wait`, SSE) |
| `GET /api/oauth/requests/{id}` | A client's request to act as an identity, for the consent page (a signed-in person) |
| `POST /api/oauth/requests/{id}/approve`, `/deny` | Answer it (`{"create": true}` creates a missing identity); returns the `redirect` back to the client |
| `GET /api/oauth/grants` | The MCP clients connected to the identities you control |
| `POST /api/oauth/grants/{id}/revoke` | Disconnect one: its tokens, and the sessions it opened, stop |
| `/mcp/{kind}/{root}/{label...}` | An identity's MCP endpoint (streamable HTTP, with sessions); an OAuth access token for it, or a static token of that identity |
| `/mcp` | The old MCP endpoint, for static tokens only (stateless; deprecated) |

The OAuth endpoints, for MCP clients:

| Method and path | What it does |
|---|---|
| `GET /.well-known/oauth-protected-resource/mcp/...` | Protected Resource Metadata (RFC 9728) of an identity's MCP endpoint |
| `GET /.well-known/oauth-authorization-server` | Authorization Server Metadata (RFC 8414) |
| `POST /oauth/register` | Dynamic Client Registration (RFC 7591), for public clients |
| `GET /oauth/authorize` | The authorization code flow: PKCE (S256) and `resource` (the endpoint's URL) are required; it leads to the consent page |
| `POST /oauth/token` | Trade a code, or a refresh token, for an access token bound to the endpoint and a new refresh token |

Git is served at `/git/{org}/{repo}.git`.

## Code layout

| Path | Contents |
|---|---|
| `cmd/gitsynth` | The `serve`, `admin`, `hook` and `version` commands, and the end-to-end tests |
| `internal/ident` | Identity strings: parsing, derivation, ancestry |
| `internal/store` | The hash-chained event log and the schema |
| `internal/app` | Users, bots, services and identities, passkeys and invitations, permissions, ideas / issues / plans / change sets, merging, reading code, ref recording |
| `internal/server` | The API, sign-in, the OAuth authorization server, Git smart HTTP, event delivery, the MCP endpoints, and the internal endpoint for hooks |
| `internal/gitx` | Running git and parsing pkt-lines |
| `internal/accesslog` | Read records, batched into a separate SQLite file |
| `web` | The web UI, and the Go package that embeds its build (`gitsynth/web`, with `-tags webui`) |
| `deploy` | The systemd unit and the Caddyfile for running on a server |
| `scripts/build.sh` | Builds the web UI, then the server with it inside |
| `scripts/release.sh` | Builds the release archives for a tag |
| `scripts/demo-seed.sh` | Seeds a running server with demo data |

## Tests

```bash
go test ./...
```

```bash
npm --prefix web run typecheck
```

The end-to-end tests run the real git and a real server. They do not use your git config or credential helpers. Passkey sign-in is tested against the server with a virtual authenticator; the browser side of it needs a real authenticator to try.

## Releasing

Tag the commit to release with a semantic version, then build the archives for it:

```bash
git tag -a v0.1.0 -m v0.1.0
```

```bash
scripts/release.sh v0.1.0
```

The script clones the tag into a temporary directory, runs the tests there, and builds with that commit's own `scripts/build.sh`, so what is checked out or changed here does not matter. It writes `dist/v0.1.0/`:

- `gitsynth-v0.1.0-linux-amd64.tar.gz` and `gitsynth-v0.1.0-linux-arm64.tar.gz`, each holding the binary with the web UI inside, `deploy/` and this README. `PLATFORMS="linux/amd64"` builds only the ones listed.
- `SHA256SUMS`, which `sha256sum -c` checks.

Every binary is stamped with the tag, which `gitsynth version` prints, and the script stops if one is not.

Publishing the files is a separate step. Put them somewhere other than a gitsynth server that runs them: when that server is down, the release that fixes it has to come from elsewhere.
