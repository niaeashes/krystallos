# Agents

Give each agent an identity of its own, derived from yours, such as `user:alice/claude`. A different role deserves a different identity; another run of the same one is a session of it. An agent can make neither identities nor tokens, so it never gains more than it was given.

## Connecting over MCP

Each identity has an MCP endpoint (streamable HTTP) with the identity in its path: `/mcp/<kind>/<root>/<label>...`. Add it to Claude Code with the server's public URL:

```bash
claude mcp add --transport http gitsynth https://gitsynth.example.com/mcp/user/alice/claude
```

The first call opens gitsynth's consent page in the browser. Sign in with your passkey and approve; approving creates the identity if it does not exist yet. The client gets an access token for that endpoint alone, which lasts an hour, and a refresh token, which lasts 30 days and is replaced on every use. It keeps them itself: no token of yours is handed to the agent. The Identities page lists the connected clients, each with a Revoke button.

- Only an identity derived from the signed-in person can be bound, never the person's own.
- Clients register with a Client ID Metadata Document or with Dynamic Client Registration. A redirect URI is https; http on localhost, on any port; or a desktop app's own scheme, such as `claude://` for the Claude desktop app, which must match exactly. Schemes the browser handles itself, such as `javascript:`, `data:` and `file:`, are refused. The consent page says where the answer goes: a site, a program on this computer, or whichever app the operating system hands that scheme's links to.
- Claude Code accepts plain http only from a server on localhost.

## Sessions

Each connection is a session of the identity, and everything the client does is recorded under it. A run that shares the connection, such as a Claude Code subagent, calls `open_session` and passes the session it gets as the `session` argument on its later calls.

`git_credential` returns a git credential for the session, good for an hour at most, with the URL and the address to commit with. Set `GIT_COMMITTER_EMAIL` to that address, or the push is refused. `whoami` says which identity and session a call acts as.

## Without a browser

Bots and CI use a standing token at the same endpoint. Issue one on the Identities page, or on the server:

```bash
sudo -u gitsynth /usr/local/bin/gitsynth admin token --data /var/lib/gitsynth --identity user:alice/claude --ttl 168h
claude mcp add --transport http gitsynth https://gitsynth.example.com/mcp/user/alice/claude --header "Authorization: Bearer gs_..."
```

The old `/mcp` endpoint, which takes the identity from a static token, is deprecated.
