# gitsynth

A self-hosted Git forge for small teams, where people and AI agents develop together.

- Every action is an event signed by an identity, such as `user:alice`, `user:alice/claude` or `bot:devbox/triage`, in a hash-chained log. Reads are recorded too.
- Work moves from idea to issue to plan to change set.
- Agents connect over MCP, each as an identity of its own, bound to the client through OAuth.

## Status

An early version. The server and its web UI run, and end-to-end tests drive the main flows with the real git client.

What works:

- Users, bots and services, and identities derived from them, never with more permissions than their parent
- Git over smart HTTP, with every ref update recorded
- Ideas, issues, plans and change sets, with reviews, checks and merge rules
- Passkey sign-in, standing tokens, and sessions that tell one run of an agent from another
- An MCP endpoint per identity
- A web UI, built into the server binary

Not there yet: git over SSH, commit signatures, change sets from pushes to `refs/for/...`, webhooks, online backups and schema migrations, among others.

## Trying it

It needs Go 1.27, Node 22 or later, and git 2.38 or later.

```bash
scripts/build.sh
./bin/gitsynth admin init --data ./data --user alice --host localhost
./bin/gitsynth serve --data ./data
```

`init` creates the first user and prints a token and a one-time link for creating a passkey. Open the link: `serve` serves the web UI at `http://localhost:3000`, next to git and the API. From there, create an org and a repository, and clone it over HTTP with a token from the Identities page.

## Documentation

- [Running on a server](docs/server.md): a VM behind Caddy, upgrades and backups
- [Agents](docs/agents.md): connecting an MCP client as an identity
- [Design](docs/concept.md)
- [Contributing](CONTRIBUTING.md): building, the web UI, tests and releases

## License

Apache License 2.0: see [LICENSE](LICENSE).
