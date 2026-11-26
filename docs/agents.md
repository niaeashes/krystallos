# Agents

Give each agent an identity of its own, derived from yours, such as `user:alice/claude`. A different role deserves a different identity; another run of the same one is a session of it. An agent can make neither identities nor tokens, so it never gains more than it was given.

## Connecting over MCP

Each identity has an MCP endpoint (streamable HTTP) with the identity in its path: `/mcp/<kind>/<root>/<label>...`. Add it to Claude Code with the server's public URL:

```bash
claude mcp add --transport http krystallos https://krystallos.example.com/mcp/user/alice/claude
```

The first call opens krystallos's consent page in the browser. Sign in with your passkey and approve; approving creates the identity if it does not exist yet. The client gets an access token for that endpoint alone, which lasts an hour, and a refresh token, which lasts 30 days and is replaced on every use. It keeps them itself: no token of yours is handed to the agent. The Identities page lists the connected clients, each with a Revoke button.

- Only an identity derived from the signed-in person can be bound, never the person's own.
- Clients register with a Client ID Metadata Document or with Dynamic Client Registration. A redirect URI is https; http on localhost, on any port; or a desktop app's own scheme, such as `claude://` for the Claude desktop app, which must match exactly. Schemes the browser handles itself, such as `javascript:`, `data:` and `file:`, are refused. The consent page says where the answer goes: a site, a program on this computer, or whichever app the operating system hands that scheme's links to.
- Claude Code accepts plain http only from a server on localhost.

## Sessions

Each connection is a session of the identity, and everything the client does is recorded under it. A run that shares the connection, such as a Claude Code subagent, calls `open_session` and passes the session it gets as the `session` argument on its later calls.

`git_credential` returns a git credential for the session, good for an hour at most, with the URL and the address to commit with. Set `GIT_COMMITTER_EMAIL` to that address, or the push is refused. `whoami` says which identity and session a call acts as.

## Git over SSH

When the server serves SSH (see [docs/server.md](server.md#git-over-ssh)), a key is a credential of one identity, like a standing token: it lasts until revoked and belongs to no session. The person adds keys on the Identities page, for themself or for an identity derived from them; an agent adds none. Ordinary keys (ed25519, ecdsa, RSA of 2048 bits or more) and FIDO keys (`ed25519-sk`) all work.

On one OS account an agent can reach the person's keys, so the server cannot tell them apart by the key. Give each identity a clone of its own instead. A repository's page gives the command under "Clone as an identity":

```bash
ssh-keygen -t ed25519 -f ~/.ssh/krystallos_claude -C user:alice/claude
git clone \
  -c user.name=user:alice/claude \
  -c user.email=user:alice/claude@krystallos.example.com \
  -c core.sshCommand="ssh -F none -i ~/.ssh/krystallos_claude -o IdentitiesOnly=yes" \
  git@krystallos.example.com:acme/app.git app-claude
```

Add `~/.ssh/krystallos_claude.pub` on the Identities page, and run the agent in `app-claude`. The clone's config names the committer and the key, so whatever is committed and pushed there belongs to `user:alice/claude` without the agent remembering anything. `-F none` keeps a `Host *` IdentityFile in `~/.ssh/config` from offering the person's key after the agent's. A `git worktree` shares its clone's config, so it belongs to the same identity.

Working in one clone together also works: a person's push may carry their agents' commits, and is recorded as the person's.

Pushes over SSH are recorded under the identity, not a session. For a record per run, use `git_credential` over HTTPS.

## Without a browser

Bots and CI use a standing token at the same endpoint. Issue one on the Identities page, or on the server:

```bash
sudo -u krystallos /usr/local/bin/kry admin token --data /var/lib/krystallos --identity user:alice/claude --ttl 168h
claude mcp add --transport http krystallos https://krystallos.example.com/mcp/user/alice/claude --header "Authorization: Bearer kr_..."
```

The old `/mcp` endpoint, which takes the identity from a static token, is deprecated.
