# Contributing

## Building

It needs Go 1.27, Node 22 or later, and git 2.38 or later.

```bash
scripts/build.sh
```

This builds the web UI, then the server with the UI inside, into `bin/gitsynth`. `GOOS` and `GOARCH` pass through, so `GOOS=linux GOARCH=amd64 scripts/build.sh bin/gitsynth-linux-amd64` cross-compiles.

## Working on the web UI

A plain Go build has no UI inside. Vite serves it instead, reloading as you edit, and passes `/api` and `/git` through to the server. With a data directory made by `admin init` (see the README):

```bash
go build -o bin/gitsynth ./cmd/gitsynth
./bin/gitsynth serve --data ./data
npm --prefix web install
npm --prefix web run dev
```

The UI is then at `http://localhost:5173`, and passkeys work there too. For links that open there, give `admin` `--url http://localhost:5173`. `scripts/demo-seed.sh gs_YOUR_TOKEN` fills a running server with a demo org, repository, issue, plan and change set.

## Tests

```bash
go test ./...
npm --prefix web run typecheck
```

The end-to-end tests run the real git and a real server, without your git config or credential helpers. Passkey sign-in is tested with a virtual authenticator.

## Code layout

| Path | Contents |
|---|---|
| `cmd/gitsynth` | The `serve`, `admin`, `hook` and `version` commands, and the end-to-end tests |
| `internal/ident` | Identity strings: parsing, derivation, ancestry |
| `internal/store` | The hash-chained event log and the schema |
| `internal/app` | Users, bots, services and identities, permissions, ideas, issues, plans and change sets, merging, reading code |
| `internal/server` | The API, sign-in, the OAuth authorization server, Git over smart HTTP and SSH, event delivery and the MCP endpoints |
| `internal/gitx` | Running git and parsing pkt-lines |
| `internal/accesslog` | Read records, in a SQLite file of their own |
| `web` | The web UI, and the Go package that embeds its build (`-tags webui`) |
| `deploy` | The systemd unit and the Caddyfile |
| `scripts` | Building, releasing, and seeding demo data |

## Releasing

Tag the commit with a semantic version, then build the archives for it:

```bash
git tag -a v0.1.0 -m v0.1.0
scripts/release.sh v0.1.0
```

The script runs the tests and builds the tag from a clean clone, so nothing checked out or changed locally gets in. It writes `dist/v0.1.0/`:

- an archive per platform (linux/amd64 and linux/arm64), holding the binary, `deploy/`, the README, the LICENSE and `docs/server.md`.
- `SHA256SUMS`.

Every binary is stamped with the tag, which `gitsynth version` prints. Publish the files somewhere other than a gitsynth server that runs them: when that server is down, the release that fixes it has to come from elsewhere.
