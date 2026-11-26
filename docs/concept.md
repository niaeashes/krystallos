# gitsynth concept

> Status: draft (a summary of the discussions up to 2026-11-25)
>
> In each section, "Decided" lists what has been settled as direction, and "Proposed" lists design ideas that came up in discussion and are not settled yet.

## Overview

gitsynth is an open-source Git forge meant to be self-hosted by small teams. It rethinks the GitHub-style forge on the assumption that people and AI agents develop together.

It rests on three pillars.

1. **Identity is a first-class concept.** An identity is the signature attached to every action on the service. Users, bots and services hold identities, and identities can be derived from each other.
2. **Every activity of an identity is recorded.** Reads are recorded as well as writes. Unless the record is comprehensive, identities mean nothing. The record is published as events, and CI and the like live outside the core, as subscribers.
3. **The issue is at the center.** Pull requests (change sets) belong to issues, and the implementation approach (the plan) is a resource of its own. Ahead of issues sit ideas, which do not yet deal with whether to act at all.

## Design principles (proposed)

- **State is derived from the record, not declared.** Whether work competes or is shared out, which plan was carried out, how far things have got: nobody declares these; they are computed from recorded events. Every state shown on screen is an aggregate of the record.
- **Ease of self-hosting comes first.** It starts as a single binary or a single docker compose file, with SQLite as the default database.

## Tech stack

### Decided

- The server is written in Go. Outside contributors are not assumed, so the language was chosen for its fit to the job rather than for how easy it is to find people.
- The Go server handles only the API and Git traffic (HTTP and SSH).
- The web UI is a separate app (a TypeScript SPA) that uses only the public API. People and agents go through the same API, so what each can do always matches, and identity checks and event recording pass through one place. Rendering HTML on the server would split the paths for the UI and the API, and would tie UI releases to server releases.
- The web UI is built with React and Vite.
- People sign in to the web UI with passkeys from the start.

### Proposed

**Overall layout**

```
gitsynth (server)
├─ HTTPS        : API, Git over HTTP
├─ SSH          : built-in SSH server
├─ Jobs         : mergeability checks, restacking, webhooks, notifications
└─ Internal API : loopback HTTP, guarded by a secret made at each start, called by Git hooks

data/
├─ gitsynth.db     write events (the source of truth) and projection tables for the UI
├─ access.db       read records
├─ repos/<ID>.git  bare repositories
└─ blobs/          attachments and the like
```

- One process, one data directory. The only external dependency is the host's `git` command (2.38 or later). No Redis or message queue; the job queue lives in SQLite too.
- The server runs git with its user's global and system git config, gitattributes and `GIT_*` variables ignored (a `core.hooksPath` there would bypass the hook, a `* -diff` would hide changes from review), so it has to run as the user that owns the data directory.
- The internal API was first planned as a Unix socket but became loopback HTTP, because a socket path can exceed the OS length limit (104 bytes on macOS) when the data directory sits deep. It listens on 127.0.0.1 only and cannot be called without the secret made at each start.
- A backup is just the data directory. SQLite can also be replicated continuously to S3-compatible storage with Litestream.
- The data directory can be moved, or restored to another path, as a whole: the server looks for each repository by name under its own `repos/`, wherever the directory was before.

**Recording**

- Appending an event and updating the projection tables happen in one transaction, so the record and the UI never drift apart. SQLite takes one write at a time, which also gives events the single order the hash chain needs.
- Reads are an order of magnitude more numerous, so they live in a separate file (`access.db`). They are written in batches, and aggregated or deleted once past their retention period.
- Ref changes are captured with the reference-transaction hook (Git 2.28 and later). Every ref update passes through it: pushes, and also server-side merges and restacks. The event is written just before the update, and if it cannot be written, the update itself is stopped.
- At startup, the actual refs are compared with the record. This cleans up after a process that died midway, and detects changes made by running git directly on the server as "changes without an identity". Before that, every repository's hook is rewritten to call the running binary, in case it has moved.

**Git**

- Change sets are received as pushes to `refs/for/...` through the proc-receive hook (Git 2.29 and later). Creating and updating change sets, and assigning change IDs, happen there (the same mechanism as Gitea's AGit flow).
- The SSH server is built in, so that deciding an identity from a key and handling FIDO keys stay under our control.
- Git protocol v2 is required. The commits a client requests are read out of each `git upload-pack` request and recorded, so the parsing is kept simple. Clients from Git 2.26 on use v2 by default.

**Authentication**

- A person's standard identity: passkeys on the web, FIDO keys over SSH.
- Agent identities: tokens and SSH keys issued from a screen the person signed in to with a passkey.
- Commit signing: SSH signatures (Git 2.34 and later), so key management is unified on SSH keys.
- A passkey is created through a one-time link from the admin CLI. The same CLI can issue a one-time sign-in link, as the way back in for someone who lost their passkey; only someone with shell access to the server can do that.
- The browser holds a cookie session bound to the person's standard identity. A request that changes something must also carry the `X-Gitsynth-CSRF` header, which another site's page cannot set without a CORS preflight the server never grants.

**Serving the web UI**

- The SPA's static files (JavaScript, CSS) contain no user data, so a CDN may cache them. Data always comes from the API, and API responses are never cached.
- Whether the SPA build ships inside the release binary or separately is a distribution decision. As long as the sources are separate, either is possible.
- `gitsynth serve --web` chooses: by default the build inside the binary (`scripts/build.sh` embeds it), `off` for none (the SPA is served from elsewhere on the same origin, such as a CDN), or a directory holding a build. Any path that is not the API, git, MCP or a static file gets `index.html`, so deep links reload. Hashed assets are cached as immutable; `index.html` is revalidated every time.

**Deployment**

- The assumption is "one machine with a disk". What it needs: a writable local disk (not network storage such as NFS), a resident process, two TCP ports (443 and 22), the `git` command, and a single instance.
- On AWS and GCP, virtual machines (Lightsail, EC2, Compute Engine) run it as is. Fargate, App Runner, Lambda and Cloud Run do not fit, because they either cannot accept SSH or cannot hold a disk.
- The core cannot run on Cloudflare: Workers have neither the git command nor a disk, and Containers only have an ephemeral disk. Cloudflare serves in front instead: Tunnel, serving the SPA, backups to R2.
- API responses carry `Cache-Control: private, no-store`. If a proxy such as Cloudflare caches by file extension, reads escape the record.
- As a rough size: 2 vCPUs, 2 GB of memory, 30–60 GB of disk. It is git during clones, more than the Go server, that uses memory.
- What a client can make the server hold is bounded: git processes serving clients (`serve --max-git`, twice the CPUs by default; the server's own merges and reads are not counted), passkey sign-ins in progress (1,000), request bodies outside git (4 MiB), a fetch request held before git starts (64 MiB), and idle keep-alive connections (closed after 2 minutes). Pushes and long-lived streams have no time limit. Per-client rate limits are not there yet.
- As a rough cost (as of 2026-11): $12 a month for Lightsail's 2 GB plan, with 3 TB of data transfer included. A machine at hand behind Cloudflare Tunnel costs only electricity.
- When agents clone often from throwaway environments, outbound transfer dominates the cost. Plans with transfer included (such as Lightsail) or a machine at hand suit that.
- Spot instances do not suit a single stateful instance.

### To verify

- Whether Go's SSH library can require the user-presence ("touched") flag of FIDO keys
- In what order the reference-transaction hook runs relative to the push quarantine and the migration of objects out of it

## Identity

### Decided

- An identity is the signature attached to every action on the service.
- Identities belong to three kinds of resource: **User**, **Bot** and **Service**.

| | User | Bot | Service |
|---|---|---|---|
| What it is | A person | Something that acts on gitsynth, any number of them | An external system, or the server's own behavior (CI, error tracking, chat integrations, `gitsynth`) |
| Identities | The user's standard identity, and identities derived from it (`user:alice/claude`) | One per bot, and identities derived from it | One per service |
| Owned by | — | A user, an org, or the server | Registered on the server from a manifest, then installed into orgs |
| Permissions | Granted | Granted | Requested in the manifest, approved by an org admin at installation |

- An identity is written `<kind>:<root>(/<label>)*[@<host>]`, such as `user:alice/claude` or, in full, `user:alice/claude@gitsynth.example.com`. Within one server the host can be left out.
- Identities can be derived from each other with `/`, without limit: `user:alice/claude/explorer`, `bot:devbox/triage`. A derived identity's permissions never exceed its parent's: an identity's effective permissions are its own grants intersected with its parent's effective permissions.
- The server's own behavior (merge commits, reconciliation) is the built-in service `service:gitsynth`, not a bot. The server can still own any number of bots of its own.

Examples: Alice's local Claude Code session acts as `user:alice/claude`; Alice's own manual work acts as `user:alice`; a devbox that starts Claude with different system prompts can be a bot per prompt, or one bot with derived identities; CI is a service.

### Proposed

- **Three layers**: the resource that bears responsibility (a user, a bot's owner, a service), the identity as a signature (`user:alice/claude`), and credentials (tokens and keys). This is close to the relation between subject and actor in OAuth token exchange (RFC 8693).
- **The host is fixed when the server is initialized** and never changes, like a SPIFFE trust domain. Events store the short form (`user:alice/claude`), and the first event of the log declares the host, so the full form can always be rebuilt.
- **A bot's name does not include its owner.** Ownership is a relation, not part of the identity, so a bot can change hands without changing its identity. Root names are unique across users, bots and services.
- **Identities are made on purpose and have a lifecycle**: grants, a name in the record and a kill switch all hang on an identity, so it is created deliberately, never on the fly at runtime. A user's identities are created by the person while present, signed in with a passkey: in the web UI, or on the consent screen that binds an agent to one (see "Entry point for agents"). A bot's identities are created by its owner (the user, the org's admins, or the server's admins), who also issues and revokes its credentials. An agent cannot create identities.
- **An identity is active or disabled.** Disabling stops every credential and session at that identity and below it. It can be undone, so a role's name is never used up. Only a user's root identity can show that the person is present; a derived identity never stands in for it.
- **An identity for a different actor, a session for a different run**: a different role or system prompt deserves its own identity; another run of the same one, a subagent's run included, is a session of that identity.
- **Identities open sessions freely**: any credential of an identity can open a session of that same identity and get a short-lived credential bound to it. Sessions nest: a session can open child sessions (a subagent's run under its parent's run), so the record keeps the tree of runs. A session's credential never outlives the credential that opened it, and stops with it. The server assigns session IDs; an outside ID, such as a Claude Code session ID, is kept on the session as a label, which is what ties events to session logs.
- **The credential decides the identity and the session**: one identity per credential, and one session when a session opened it. Asking for an identity on each action (self-declaration) leads to forgetting to switch, or to reuse.
- **A person's standard identity checks that the person is present**: a local agent can reach the person's `~/.ssh` and Git credentials, so left alone it can act as the person. Requiring a FIDO2 security key that needs a touch (`ed25519-sk`) or a passkey puts them physically out of an agent's reach.
- **Signatures extend to Git commits**: see "Commits and identities" below.

## Commits and identities

### Decided

- In a Git commit, the identity lives in the **committer**. The author is a claim about where the change came from: it survives rebases and cherry-picks, and is not verified. This matches what forges converge on: a signature proves the committer (GitHub, GitLab, Gitea's committer trust model, gitsign), and pushes are checked against the committer (GitLab push rules, Gerrit, GitHub's committer-must-match-pusher rule).
- A push is authenticated, so the server knows which identity pushed. Every new commit's committer must resolve to the pushing identity or to an identity it controls. Alice may push commits committed by `user:alice/claude`; `user:alice/claude` may not push commits committed by `user:alice`.
- A committer's email resolves to an identity in two ways: the canonical form `kind:root/label@host` (with this server's host) always works, and an identity can register any other address it uses (`alice@example.com`). Registered addresses are unique on the server and, for now, not verified.

### Proposed

- **Commits the server makes** (merge commits, restacks) have `service:gitsynth` as committer and are signed with the server's key; the author is the identity that asked for them. Every forge surveyed does the same with its instance identity.
- **The easy path for agents**: a session's credential comes with the committer settings for its identity (`GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL` in the canonical form). A local agent otherwise commits with the person's own git config, which the push check rightly refuses; the refusal says how to fix it.
- **Signatures as a stronger mode**: a repository can require new commits to be signed by a key registered to the committer's identity (SSH signatures). The push check stays; the signature adds proof that survives the repository leaving the server.
- **Imported history** is accepted and marked as imported rather than refused, the way Gerrit's Forge Committer right and GitLab's mirroring exemption handle upstream history.
- **Unknown committers**: a new commit whose committer resolves to no identity is refused.

## Activity record

### Decided

- The activity of identities is recorded comprehensively.
- Reads are recorded as well as writes.

### Proposed

**Writes**

- The activity record is not one feature among others but the write path itself. Every change can only be written as an event carrying an identity (close to event sourcing). If each entry point (web UI, API, Git protocol, CI, webhooks) wrote its own record, one of them would eventually forget.
- For pushes, the server records which identity moved which branch from which commit to which commit.
- The write record is kept forever as the source of truth. Each event includes the hash of the one before it, so tampering can be detected.

**Reads**

- Reads outnumber writes by an order of magnitude, so they are appended to a separate store with its own retention and compaction rules.
- What was seen is recorded with its version. For fetch and clone: the commits the client asked for, and where the branches pointed at the time. For the web and the API: what was read (a file, an issue, a change set) and its version at that moment.
- Repeated reads of the same thing by the same identity within a short time are folded into one entry with a count.
- There is no anonymous access. Everything, browsing included, requires signing in; repositories meant to be public get a separate mirror.
- Authentication and recording happen in front of any cache (reverse proxy or CDN).

**Out of scope**

- Local work before a push (which files an agent read, which commands it ran) is invisible to the forge. Linking commits to session logs is about as far as it can realistically bridge.

**What it is for**

- Joining reads and writes makes it possible to trace "Alice's Claude read issue 12 and file X at commit abc, then pushed commit def". Knowing what a change was built from gives a lead when reviewing changes an AI made.

## Ideas

### Decided

- **Ideas** come before issues. An idea deals with neither how nor whether to act.
- An idea can be converted into an issue.
- An idea can be associated with any number of repositories.

### Proposed

- Ideas themselves live outside repositories, in the org. They can be associated with repositories of the same org.

**What each layer handles**

From idea to change set, each layer adds one more thing to deal with.

```
Idea        A thought. Deals with neither whether nor how
  ↓ convert
Issue       Whether, what, and why
  ↓
Plan        How
  ↓
Change Set  Doing it
```

**Conversion**

- Converting keeps the idea, and records the link "issue 12 was made from idea 5" as an event. Discussion on the idea stays with the idea.
- "Became an issue" is not stored as a state; it follows from whether a link exists (per the design principle).
- One idea can yield several issues, and several ideas can be combined into one issue.
- The resulting issue is created in one of the repositories the idea is associated with.

**No state**

- Ideas have no "rejected" state either. Judging that something should not be done is dealing with "whether", which is outside an idea's role. Ideas that are never converted simply remain.
- Lists are sorted and filtered by recency, by whether they were converted, by number of reactions, and so on.

**A place for agents to put things**

- When an agent notices something out of scope while working ("this module should be rebuilt"), it can leave an idea instead of mixing it into the change set. The change set keeps its scope, and the observation is not thrown away.
- Agents can produce ideas in large numbers, so ideas can be filtered by the identity that wrote them.

## Issue / Plan / Change Set

### Background

Separating issues and pull requests is a design for people, and it becomes a bottleneck once AI joins in. With AI, writing code costs almost nothing, and the bottleneck moves to deciding what to do and checking what came out. When issues and PRs are separate, discussion splits between them, the PR description rehashes the issue, and the two are joined by keyword-driven links such as `Closes #12`.

### Decided

- Pull requests belong to issues. In gitsynth a pull request is called a **change set**.
- A **plan** is a resource of its own.
- An issue can hold any number of plans and any number of change sets.
- A change set is the execution of a plan, and is associated with zero or one plan.
- Several change sets executing the same plan may be competing (one gets adopted) or sharing the work (each executes part of the plan).
- Plans have no "adopted" state. Which plan was followed is shown by the change sets actually merged.

```
Issue
├─ Plan P1 (Alice)
├─ Plan P2 (Alice's Claude)
├─ Change Set A (Alice's Claude) → executes P1
├─ Change Set B (Bob's Codex)    → executes P1
└─ Change Set C (Bob)            → no plan
```

- Issue : Plan = 1 : N
- Issue : Change Set = 1 : N
- Change Set : Plan = N : 0..1

### Proposed

**Issue**

- What to do and why, and the overall discussion, live on the issue. A change set holds only the diff and the review comments on its lines.
- A change set cannot exist without an issue. For small changes such as dependency updates, creating a change set creates a minimal issue automatically.
- Closing an issue is an explicit act by an identity, and the closing is itself recorded as an event. With no designated plan, completion cannot be derived from the record. A notice such as "every step of P1 has been merged" can still prompt someone to close it.

**Plan**

- A plan is a versioned document with no state. Revisions are recorded with their identity.
- A plan can be reviewed before any code exists. Reviewing a plan is far lighter than reviewing code, which pays off when working with AI.
- A plan is a set of steps. Steps get fixed IDs rather than position numbers, so references survive reordering or additions in a revision.
- The author of a plan and the authors of the change sets executing it are recorded separately ("plan by Alice, implementation by Alice's Claude").

**Change Set**

- A change set is associated with a plan down to its version ("P1, version 2"). When the plan is revised, it shows that a newer version exists and lets the author choose whether to follow. Following is recorded as an event too.
- The plan must belong to the same issue.
- A change set holds which steps of the plan it executes. Competing versus sharing is not declared; it is derived from how the steps overlap. The same steps mean competing, different steps mean sharing, and partial overlap means both.

```
Plan P1 (version 2)
├─ step s1
├─ step s2
└─ step s3

Change Set A → P1 v2, s1 and s2
Change Set B → P1 v2, s1 and s2   ← competes with A
Change Set C → P1 v2, s3          ← shares the work with A and B
```

- If a step disappears in a newer version, change sets executing that step get a warning.
- Change sets can be stacked on other change sets (used as a base). In the example above, if C was stacked on A and B is then adopted, C is restacked onto B. Restacking is automatic as far as it can go; on a conflict it goes back to the identity that made C.
- Changes within a change set carry a change ID that stays the same, separate from the commit hash (the same idea as Gerrit's Change-Id or jj's change ID). When a rebase changes the hash, review comments and the record can still follow the change.

**Progress and comparison**

- A plan is shown as a checklist of steps, with which change sets are working on each step and which were merged. Progress is computed per plan from step coverage.
- Several plans may each be partly carried out (s1 of P1 and s2 of P2 merged, for example).
- There are three ways to compare:
  - Plans with plans: compare approaches before any code is written
  - Change sets executing the same plan: compare implementers with the approach held fixed. For comparing agents, this is the fairest setting
  - Change sets of different plans: compare the whole thing, approach included
- A view compares change sets directly even when they started from different base commits.
- Change sets that were not adopted are kept. The record of which identity's proposal was chosen also becomes data on which tasks each agent handles well.

## Organization and permissions

### Decided

- The unit of organization is Org / Repo, as on GitHub.
- Permissions are designed in a general way, as permission sets that can be configured per repository.

### Proposed

- Permissions are defined finely, as pairs of resource and operation (create an idea, create a plan, create a change set, review, merge, change repository settings, and so on).
- A permission set is a bundle of them. Default sets corresponding to GitHub's Read / Write / Maintain / Admin are provided, and each repository can define its own.
- Permission sets are granted to identities, not to principals. "Alice" can be allowed to merge while "Alice's Claude" is allowed no further than creating change sets.

## Merging

### Decided

- Merge rules split in two: how a merge is physically made, and how merging is authorized.
- Merge authorization is handled as part of the permission-set design.
- The default physical merge is a merge commit. The authors' commits, and their signatures, stay intact.

### Proposed

**Physical merge**

- The method (merge commit, fast-forward, squash, rebase) can be chosen per repository.
- The method decides whether the authors' signatures survive.
  - Merge commit and fast-forward: the original commits remain, so the authoring identities' signatures remain.
  - Squash and rebase: commits are recreated, so the authors' signatures do not survive.
- Commits the server makes (merge commits, squashes, rebases, restacks) are signed with the key of the server's own identity. A person's standard identity requires the person to be present, so the server cannot hold people's keys. Whose action produced the commit is recorded in the event.
- If only fast-forward is allowed, updating the base falls to the author's side (an agent, say), and the signature is reapplied by the author's identity.

**Merge authorization**

- Merge conditions are written as a combination of permission sets and records. For example: "there is a review by an identity holding the Reviewer permission set", or "CI passed under the identity of Runner X".
- Whether the conditions are met is never declared; it is judged from the record.

## Publishing events

### Decided

- CI is not part of the core.
- gitsynth publishes events. CI, notifications, bots, agents and the like are subscribers.
- Results from subscribers come back through the ordinary API, as writes under the subscriber's own identity.

### Proposed

**Shape**

- The stream of events is published in order, readable from where one left off, as in `GET /events?after=<number>`.
- Subscribers keep their own position. If they miss something, they read on from there.
- CI results and reviews alike become the same kind of record: someone verified this. Results are tied to the stable change ID and its version, and merge conditions can read "tests passed under the identity of Runner X". An identity that reports results run on a local machine fits the same mechanism. Which identities' results to trust is decided by rules.
- Logs and failure details are readable through the API, so an agent can look at a CI failure and fix it in a loop.

**Why a resumable stream rather than webhooks**

- Nothing is lost. Webhooks are fire-and-forget; if the receiver is down, events go missing.
- Receivers need no open port. Subscribers connect out, so they run on a machine at home or behind NAT.
- One mechanism covers everything. UI updates in the SPA, notifications, CI and agents waiting on something all read the same stream.
- Webhooks can be added later, as an adapter for connecting outside services.

**What this shape makes possible**

- Outside subscribers become witnesses. Events are hash-chained, so if subscribers keep what they received, a later rewrite of the server's record shows up as a mismatch.
- Processing status becomes visible. If the server also keeps each subscriber's position, a change set can show that CI has not processed a push yet.

**To settle up front**

- Events become a public contract. The internal record goes out as is, so events carry a version number from the start and are designed to be published.
- Subscribers receive only the events their identity is allowed to read.
- Subscribing is recorded as a read: which identity read how far.
- Events carry the identity that triggered them, so a subscribing CI can decide from that whether to hand out secrets (no production secrets for jobs triggered by an agent's push, for example).
- Delivery is HTTP first (SSE, or a request that waits until new events arrive). With an SSH command as well, an agent can read events with a single `ssh`.

**If we build a runner**

- A runner of our own, if built, is one more subscriber outside the core: a separate single Go binary that starts a container per job and throws it away afterwards.
- It does not run on the forge's machine. Memory is short there, and it would be poor isolation.
- For code written by the team's own agents, container isolation is enough to start with. Stronger isolation (gVisor, microVMs) can be added later.

## Inbox

### Decided

- The inbox is considered as two separate parts: the way in (what goes in) and the way out (how it is viewed).

### Proposed

- Way in: subscribe to the event stream internally, and turn events that match rules into inbox items. These are things waiting on a person's judgment: review requests, mentions, merges stalled on one's approval, plans waiting for review.
- Way out: the web UI, email digests, MCP, and so on. Kept separate from the way in, new ways out can be added later. If the inbox is readable over MCP, one's own agent can summarize or triage it.

## Entry point for agents (MCP)

### Decided

- An MCP server is provided for agents.
- A convention is defined for passing an agent's session ID over MCP.

### Proposed

- **The URL names the identity**: each identity has its own MCP endpoint, with the identity spelled out in the path, `/mcp/<kind>/<root>/<label>...` (`/mcp/user/alice/claude`), so nothing needs escaping. A request is served only when its credential belongs to exactly the identity in the path.
- **OAuth binds a client to that identity**: gitsynth is the authorization server of MCP's authorization spec (OAuth 2.1 with Protected Resource Metadata and resource indicators). Adding the endpoint to an MCP client opens gitsynth's consent screen, where the person signs in with a passkey and approves the client acting as that identity, creating the identity there if it does not exist yet. The token's audience is the endpoint's URL, so it is no use at another identity's endpoint, nor for git. The client keeps the refresh token, so no person's own token is ever handed to an agent. Clients register with a Client ID Metadata Document, or with Dynamic Client Registration where they have none.
- **An MCP session is a gitsynth session**: the endpoint keeps state, and each `initialize` opens a session of the bound identity. It is opened by the grant, not by an access token, so it lasts through refreshes, and it closes when the MCP session ends. A run inside it that should be told apart, such as a subagent sharing its parent's connection, opens a child session with the tool `open_session` and names it on later calls in the `session` argument every tool takes; it can only name sessions opened below its own. No session ID is ever declared on its own. MCP's 2026-07-28 revision drops protocol sessions and `initialize`, so these endpoints speak the revisions up to 2025-11-25, and a newer client falls back to `initialize` (see "Open questions").
- **Git goes through a credential the session issues**: the tool `git_credential` issues a short-lived git credential (an hour at most) for the same identity and session, together with the address to commit with. It works for git only and ends with its session.
- **Only a derived identity is bound to a client**: the consent screen never binds the person's own identity, so the passkey stays the only way to act as the person.
- Bots, CI and other clients that cannot go through a browser keep using static bearer tokens, each bound to one identity; the path rule applies to them too.
- Events keep the session ID, so they can be tied to links to session logs (the same idea as GitHub attaching session-log links to agents' commits).
- MCP tools mirror the operations of the public API. Subscribing to events is also possible over MCP.

## Open questions

- How a registered email address is shown to belong to its identity (a confirmation mail, an admin's approval, or a domain rule); until then, the first identity to register an address holds it
- How much of the link between principals and identities other members can see
- The rules for the inbox's way in, and which way out comes first
- How a run is told apart once MCP clients stop speaking a revision with sessions. MCP 2026-07-28 removes protocol sessions and `initialize` (each request carries its protocol version and client info in `_meta` instead). gitsynth's endpoints keep to 2025-11-25 for now, and clients fall back to it. Candidates: a session per grant, or a session named in each request's `_meta`.
- Subagents, as checked against Claude Code's documentation: a subagent that names an MCP server by reference shares its parent's connection (https://code.claude.com/docs/en/sub-agents), so it reaches gitsynth as its parent's MCP session and needs a child session opened by tool. A server defined inline in a subagent connects separately, and so gets a session of its own. Whether such a session should hang under its parent's is open.
- Client registration, as checked against Claude Code:
  - It uses a Client ID Metadata Document when the server supports one (client_id `https://claude.ai/oauth/claude-code-client-metadata`), and Dynamic Client Registration otherwise.
  - It redirects to `http://localhost:PORT/callback` on a random port.
  - It sends PKCE S256 and `resource`, refreshes tokens, and checks `iss`.
  - It accepts a plain-http authorization server only on localhost.
  - Sources: https://claude.com/docs/connectors/building/authentication and https://code.claude.com/docs/en/mcp.

  Still open: whether to keep Dynamic Client Registration, which the 2026-07-28 revision deprecates. Anyone can register through it, so gitsynth caps how many unapproved registrations it takes in an hour.
- Who can see whose read records. A person's read records can turn into surveillance within a team. Whether read records go out to subscribers as events belongs here too
- Whether an idea may be associated with zero repositories (a thought for a repository that does not exist yet)
- The form of distribution (a single binary with the SPA bundled, separate delivery, a Docker image, ...)
- What goes into the first version

## Competitors and prior art

Surveyed as of 2026-11-25. No self-hosted forge was found that centers on letting people and bots alike hold several identities that sign every action.

| Name | What is related | How it differs from gitsynth |
|---|---|---|
| [GitLab Composite Identity](https://docs.gitlab.com/user/duo_agent_platform/composite_identity/) | Combines an AI agent's service account and the person who started it into one token. Permissions are the stricter of the two. Shown as "<service account> on behalf of @person" | Only for GitLab's AI features, and only on Premium/Ultimate. No notion of a person switching between identities |
| [GitHub Agent HQ](https://github.blog/news-insights/product-news/welcome-home-agents/) / Copilot coding agent | Copilot commits under its own name and adds a [link to its session logs](https://github.blog/changelog/2026-03-20-trace-any-copilot-coding-agent-commit-to-its-session-logs) to commits. Assigning an issue creates a PR | One account per person is assumed. Issues and PRs are separate objects joined by links |
| Forgejo / Gitea | Have a user type for bots | Creating a user per agent becomes unmanageable; in practice some teams share one bot account and sign at the end of the text |
| [Tangled](https://tangled.org/) | A forge on the AT Protocol. Identities are DIDs, not tied to a server. Git servers (knots) can be self-hosted | About carrying an identity around, not about switching between identities |
| [Radicle](https://radicle.xyz/guides/protocol) | Peer-to-peer. Actions are signed cryptographically with node keys | Sharing one identity across several devices is still a design proposal |
| [SmolForge](https://forge.smol.ai/) | Billed as "Git hosting for coding agents". Alpha, runs on Cloudflare | Its Identity feature is a sign-in service for apps hosted on SmolForge, not an identity model for agents |
| Linear | Links issues to branches and PRs automatically | Only links them; PRs do not belong to issues |
| Gerrit | Streams events as JSON over the SSH command `gerrit stream-events`; Zuul and Jenkins subscribe to it to drive CI | Events carry no identity signature and no hash chain |

Related: there are [reports](https://www.hostzealot.com/blog/news/codeberg-closes-the-gate-on-ai-built-repositories) that Codeberg is moving to shut out repositories built with AI (the original source has not been checked). If which identity did something is always visible, the same mechanism can serve teams that welcome AI and teams that are wary of it.

## Glossary

| Term | Meaning |
|---|---|
| User | A person. Signs in with a passkey |
| Bot | Something that acts on gitsynth, owned by a user, an org or the server |
| Service | An external system registered from a manifest and installed into orgs, or the server's own behavior |
| Identity | The signature on an action, written like `user:alice/claude@host`. Can be derived with `/`. Created on purpose, active or disabled (proposed) |
| Session | One run of an identity. Opened freely by the identity, nests, and holds a short-lived credential (proposed) |
| Credential | A token or a key. Bound to one identity, and to a session when a session opened it (proposed) |
| Idea | A thought. Deals with neither how nor whether. Can be associated with several repositories and converted into an issue |
| Issue | What to do and why. Holds plans and change sets |
| Plan | How to proceed. A versioned resource of its own |
| Step | A unit of a plan. Has a fixed ID (proposed) |
| Change Set | The execution of a plan. The counterpart of a GitHub pull request |
| Change | One unit within a change set. Has a stable change ID (proposed) |
| Event | One entry of the record, carrying an identity. The source of truth for writes. Published outward |
| Subscriber | Something that reads the event stream and writes results back under its own identity (CI, notifications, bots, agents, ...) |
| Org | The unit that groups repositories and ideas |
| Permission set | A bundle of permissions. Configurable per repository |
