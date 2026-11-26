package store

// schema creates the event log and the projection tables. Every projection
// row is written in the same transaction as the event that caused it, so the
// projections never drift from the log.
const schema = `
CREATE TABLE IF NOT EXISTS events (
	seq       INTEGER PRIMARY KEY,
	type      TEXT NOT NULL,
	version   INTEGER NOT NULL,
	org       TEXT NOT NULL DEFAULT '',
	repo      TEXT NOT NULL DEFAULT '',
	identity  TEXT NOT NULL,
	principal TEXT NOT NULL,
	session   TEXT NOT NULL DEFAULT '',
	payload   TEXT NOT NULL,
	at        TEXT NOT NULL,
	prev_hash TEXT NOT NULL,
	hash      TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

-- A principal is a user, a bot or a service: the resource at the root of a
-- tree of identities. Its id is its root identity, such as "user:alice".
-- Root names are unique across kinds.
CREATE TABLE IF NOT EXISTS principals (
	id       TEXT PRIMARY KEY,
	kind     TEXT NOT NULL,
	name     TEXT NOT NULL UNIQUE,
	admin    INTEGER NOT NULL DEFAULT 0,
	owner    TEXT NOT NULL DEFAULT '',
	manifest TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS identities (
	id        TEXT PRIMARY KEY,
	principal TEXT NOT NULL,
	parent    TEXT NOT NULL DEFAULT '',
	disabled  INTEGER NOT NULL DEFAULT 0
);

-- Addresses an identity commits with, besides its canonical form
-- ("user:alice/claude@host"), which always resolves without a row here.
-- An address belongs to one identity on the server; it is not verified yet.
CREATE TABLE IF NOT EXISTS identity_email_addresses (
	email    TEXT PRIMARY KEY,
	identity TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS service_installs (
	service TEXT NOT NULL,
	org     TEXT NOT NULL,
	perms   TEXT NOT NULL,
	PRIMARY KEY (service, org)
);

-- kind is 'token' (a standing token), 'browser' (a signed-in browser),
-- 'session' (a credential bound to a session, whose session column holds
-- the session's id), 'ssh_key' (an SSH key, whose public half is in
-- ssh_keys), or one of OAuth's: 'oauth_grant' (a grant's own),
-- 'oauth_access' and 'oauth_refresh'. On token and browser, session is a
-- free-form label from before sessions were records. An SSH key has no
-- secret here: its hash is "ssh_key:" and its own id, which no token
-- hashes to.
CREATE TABLE IF NOT EXISTS credentials (
	id         TEXT PRIMARY KEY,
	identity   TEXT NOT NULL,
	hash       TEXT NOT NULL UNIQUE,
	session    TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL DEFAULT '',
	revoked    INTEGER NOT NULL DEFAULT 0,
	kind       TEXT NOT NULL DEFAULT 'token'
);

-- The public half of each SSH key credential. A fingerprint (SHA256, as
-- ssh-keygen prints it) is held by at most one unrevoked key on the server.
CREATE TABLE IF NOT EXISTS ssh_keys (
	credential  TEXT PRIMARY KEY,
	fingerprint TEXT NOT NULL,
	type        TEXT NOT NULL,
	public_key  TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS ssh_keys_fingerprint ON ssh_keys (fingerprint);

-- A session is one run of an identity. parent is the session it was opened
-- from ('' when a standing token or a browser opened it), opened_by the
-- credential that opened it, credential the session's own, and label an
-- outside ID such as a Claude Code session ID.
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	identity   TEXT NOT NULL,
	parent     TEXT NOT NULL DEFAULT '',
	opened_by  TEXT NOT NULL,
	credential TEXT NOT NULL,
	label      TEXT NOT NULL DEFAULT '',
	opened_at  TEXT NOT NULL
);

-- The audience a credential is bound to. A credential with no row here
-- works wherever its identity is accepted; one with a row works only where
-- the audience matches: an OAuth access token at the MCP endpoint whose URL
-- is its audience, a git credential ('git') at git's endpoints. For OAuth
-- credentials, grant_id and client name the grant and client they belong to.
CREATE TABLE IF NOT EXISTS credential_audiences (
	credential TEXT PRIMARY KEY,
	audience   TEXT NOT NULL,
	grant_id   TEXT NOT NULL DEFAULT '',
	client     TEXT NOT NULL DEFAULT ''
);

-- OAuth clients of the MCP endpoints. id is the client_id: one issued by
-- Dynamic Client Registration (via 'registration'), or the https URL of a
-- Client ID Metadata Document (via 'metadata_document'), recorded when a
-- grant for it is first approved. redirect_uris is a JSON array.
CREATE TABLE IF NOT EXISTS oauth_clients (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL DEFAULT '',
	redirect_uris TEXT NOT NULL DEFAULT '[]',
	via           TEXT NOT NULL,
	registered_at TEXT NOT NULL
);

-- A grant binds a client to an identity at one MCP endpoint (resource),
-- approved by the person in a signed-in browser (approved_by is that
-- browser's credential). credential is the grant's own credential (kind
-- 'oauth_grant', with no usable token): revoking it, or its expiry, ends
-- the grant, and the sessions the grant's MCP sessions opened hang on it.
CREATE TABLE IF NOT EXISTS oauth_grants (
	id          TEXT PRIMARY KEY,
	client      TEXT NOT NULL,
	identity    TEXT NOT NULL,
	resource    TEXT NOT NULL,
	credential  TEXT NOT NULL,
	approved_by TEXT NOT NULL,
	created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS passkeys (
	id        TEXT PRIMARY KEY,
	principal TEXT NOT NULL,
	data      TEXT NOT NULL
);

-- One-time links: "register" adds a passkey to an existing user, "login"
-- signs a user in once, "invite" lets someone join as a new user (with an
-- optional grant in an org).
CREATE TABLE IF NOT EXISTS enrollment_links (
	hash       TEXT PRIMARY KEY,
	principal  TEXT NOT NULL DEFAULT '',
	purpose    TEXT NOT NULL,
	org        TEXT NOT NULL DEFAULT '',
	perm_set   TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL,
	used       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS review_comments (
	review_seq INTEGER NOT NULL,
	repo       TEXT NOT NULL,
	changeset  INTEGER NOT NULL,
	path       TEXT NOT NULL,
	line       INTEGER NOT NULL,
	side       TEXT NOT NULL,
	body       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS orgs (
	name TEXT PRIMARY KEY
);

CREATE TABLE IF NOT EXISTS org_grants (
	org      TEXT NOT NULL,
	identity TEXT NOT NULL,
	perm_set TEXT NOT NULL,
	PRIMARY KEY (org, identity)
);

CREATE TABLE IF NOT EXISTS repos (
	full_name      TEXT PRIMARY KEY,
	org            TEXT NOT NULL,
	name           TEXT NOT NULL,
	path           TEXT NOT NULL UNIQUE,
	default_branch TEXT NOT NULL DEFAULT 'main',
	merge_rule     TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS repo_grants (
	repo     TEXT NOT NULL,
	identity TEXT NOT NULL,
	perm_set TEXT NOT NULL,
	PRIMARY KEY (repo, identity)
);

CREATE TABLE IF NOT EXISTS perm_sets (
	repo  TEXT NOT NULL,
	name  TEXT NOT NULL,
	perms TEXT NOT NULL,
	PRIMARY KEY (repo, name)
);

CREATE TABLE IF NOT EXISTS refs (
	repo TEXT NOT NULL,
	ref  TEXT NOT NULL,
	oid  TEXT NOT NULL,
	PRIMARY KEY (repo, ref)
);

CREATE TABLE IF NOT EXISTS ref_txns (
	seq     INTEGER PRIMARY KEY,
	repo    TEXT NOT NULL,
	state   TEXT NOT NULL,
	updates TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS ideas (
	org    TEXT NOT NULL,
	number INTEGER NOT NULL,
	title  TEXT NOT NULL,
	body   TEXT NOT NULL,
	author TEXT NOT NULL,
	PRIMARY KEY (org, number)
);

CREATE TABLE IF NOT EXISTS idea_repos (
	org  TEXT NOT NULL,
	idea INTEGER NOT NULL,
	repo TEXT NOT NULL,
	PRIMARY KEY (org, idea, repo)
);

CREATE TABLE IF NOT EXISTS issues (
	repo   TEXT NOT NULL,
	number INTEGER NOT NULL,
	title  TEXT NOT NULL,
	body   TEXT NOT NULL,
	author TEXT NOT NULL,
	closed INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS issue_sources (
	repo  TEXT NOT NULL,
	issue INTEGER NOT NULL,
	org   TEXT NOT NULL,
	idea  INTEGER NOT NULL,
	PRIMARY KEY (repo, issue, org, idea)
);

CREATE TABLE IF NOT EXISTS comments (
	seq    INTEGER PRIMARY KEY,
	repo   TEXT NOT NULL,
	issue  INTEGER NOT NULL,
	author TEXT NOT NULL,
	body   TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS plans (
	repo   TEXT NOT NULL,
	number INTEGER NOT NULL,
	issue  INTEGER NOT NULL,
	title  TEXT NOT NULL,
	author TEXT NOT NULL,
	latest INTEGER NOT NULL,
	PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS plan_versions (
	repo    TEXT NOT NULL,
	plan    INTEGER NOT NULL,
	version INTEGER NOT NULL,
	body    TEXT NOT NULL,
	author  TEXT NOT NULL,
	PRIMARY KEY (repo, plan, version)
);

CREATE TABLE IF NOT EXISTS plan_steps (
	repo     TEXT NOT NULL,
	plan     INTEGER NOT NULL,
	version  INTEGER NOT NULL,
	step     TEXT NOT NULL,
	position INTEGER NOT NULL,
	title    TEXT NOT NULL,
	PRIMARY KEY (repo, plan, version, step)
);

CREATE TABLE IF NOT EXISTS changesets (
	repo          TEXT NOT NULL,
	number        INTEGER NOT NULL,
	issue         INTEGER NOT NULL,
	title         TEXT NOT NULL,
	author        TEXT NOT NULL,
	branch        TEXT NOT NULL,
	base          TEXT NOT NULL,
	plan          INTEGER NOT NULL DEFAULT 0,
	plan_version  INTEGER NOT NULL DEFAULT 0,
	merged_commit TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repo, number)
);

CREATE TABLE IF NOT EXISTS changeset_steps (
	repo      TEXT NOT NULL,
	changeset INTEGER NOT NULL,
	step      TEXT NOT NULL,
	PRIMARY KEY (repo, changeset, step)
);

CREATE TABLE IF NOT EXISTS reviews (
	seq        INTEGER PRIMARY KEY,
	repo       TEXT NOT NULL,
	changeset  INTEGER NOT NULL,
	author     TEXT NOT NULL,
	verdict    TEXT NOT NULL,
	commit_oid TEXT NOT NULL,
	body       TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS checks (
	seq        INTEGER PRIMARY KEY,
	repo       TEXT NOT NULL,
	commit_oid TEXT NOT NULL,
	name       TEXT NOT NULL,
	status     TEXT NOT NULL,
	author     TEXT NOT NULL,
	summary    TEXT NOT NULL
);
`
