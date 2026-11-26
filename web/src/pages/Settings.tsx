import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import {
  post,
  useApi,
  type BotInfo,
  type CredentialInfo,
  type GrantInfo,
  type IdentityEmailAddress,
  type IdentityInfo,
  type IssuedToken,
  type SSHKey,
  type ServiceInfo,
} from '../api'
import { Empty, ErrorBox, Identity, Loading, Section, State, Toggle, parseIdentity, when } from '../ui'
import { useMe } from '../App'

const keys = ['/api/identities', '/api/tokens', '/api/bots', '/api/services', '/api/emails', '/api/oauth/grants']

function depth(id: string) {
  return parseIdentity(id).labels.length
}

// mcpURL is an identity's MCP endpoint: /mcp/<kind>/<root>/<label>...
function mcpURL(identity: string) {
  return `${window.location.origin}/mcp/${identity.replace(':', '/')}`
}

// ConnectedClients lists the MCP clients bound to the identities you control,
// each until it is revoked or goes unused for the life of a refresh token.
function ConnectedClients() {
  const grants = useApi<GrantInfo[]>('/api/oauth/grants')
  const queryClient = useQueryClient()
  const revoke = useMutation({
    mutationFn: (id: string) => post(`/api/oauth/grants/${id}/revoke`),
    onSuccess: () => keys.forEach((k) => queryClient.invalidateQueries({ queryKey: [k] })),
  })
  return (
    <Section title="Connected clients">
      {grants.isPending && <Loading />}
      <ErrorBox error={grants.error} />
      {grants.data?.length === 0 && <Empty>No MCP client is connected to your identities.</Empty>}
      {grants.data && grants.data.length > 0 && (
        <table className="table">
          <thead>
            <tr>
              <th>Identity</th>
              <th>Client</th>
              <th>Connected</th>
              <th>Lapses</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {grants.data.map((g) => (
              <tr key={g.id}>
                <td>
                  <Identity handle={g.identity} />
                </td>
                <td title={g.client_id}>
                  {g.client_name || <span className="muted">unnamed</span>}
                  {g.via === 'metadata_document' && <span className="muted small"> {new URL(g.client_id).host}</span>}
                </td>
                <td>{when(g.created_at)}</td>
                <td title="unless it refreshes its token before then">{new Date(g.expires_at).toLocaleString()}</td>
                <td>
                  <button className="button button-quiet" onClick={() => revoke.mutate(g.id)} disabled={revoke.isPending}>
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <ErrorBox error={revoke.error} />
    </Section>
  )
}

// IdentityTree lists one tree of identities, indented by derivation, with
// the actions that apply to each: derive below it, disable it, enable it
// again. Identities are made by hand, so deriving and enabling are offered
// only to the person (a user's root identity), who also manages their bots.
function IdentityTree({ ids, canDisableRoot }: { ids: IdentityInfo[]; canDisableRoot: boolean }) {
  const me = useMe()
  const byHand = me.kind === 'user' && me.identity === me.principal
  const queryClient = useQueryClient()
  const refresh = () => keys.forEach((k) => queryClient.invalidateQueries({ queryKey: [k] }))
  const [label, setLabel] = useState<Record<string, string>>({})
  const derive = useMutation({
    mutationFn: (parent: string) => post('/api/identities', { parent, label: label[parent] ?? '' }),
    onSuccess: (_, parent) => {
      setLabel({ ...label, [parent]: '' })
      refresh()
    },
  })
  const disable = useMutation({ mutationFn: (id: string) => post('/api/identities/disable', { id }), onSuccess: refresh })
  const enable = useMutation({ mutationFn: (id: string) => post('/api/identities/enable', { id }), onSuccess: refresh })
  return (
    <>
      <ul className="list">
        {ids.map((i) => (
          <li key={i.id} className={`list-row${i.disabled ? ' revoked' : ''}`} style={{ paddingLeft: depth(i.id) * 22 }}>
            <Identity handle={i.id} />
            <code className="muted small">{i.id}</code>
            {i.disabled && <State ok={false}>disabled</State>}
            <span className="spacer" />
            {i.disabled && byHand && (i.parent || canDisableRoot) && (
              <button
                className="button button-quiet"
                onClick={() => enable.mutate(i.id)}
                disabled={enable.isPending}
                title="Its revoked tokens and sessions stay revoked; issue it a fresh token"
              >
                Enable
              </button>
            )}
            {!i.disabled && byHand && (
              <form
                className="form-inline compact"
                onSubmit={(e) => {
                  e.preventDefault()
                  derive.mutate(i.id)
                }}
              >
                <input
                  value={label[i.id] ?? ''}
                  onChange={(e) => setLabel({ ...label, [i.id]: e.target.value })}
                  placeholder="derive: label"
                  className="narrow-wide"
                />
                <button className="button" disabled={!label[i.id] || derive.isPending}>
                  Derive
                </button>
              </form>
            )}
            {!i.disabled && (i.parent || canDisableRoot) && (
              <button className="button button-quiet" onClick={() => disable.mutate(i.id)} disabled={disable.isPending}>
                Disable
              </button>
            )}
          </li>
        ))}
      </ul>
      <ErrorBox error={derive.error || disable.error || enable.error} />
    </>
  )
}

export function SettingsPage() {
  const me = useMe()
  const ids = useApi<IdentityInfo[]>('/api/identities')
  const bots = useApi<BotInfo[]>('/api/bots')
  const creds = useApi<CredentialInfo[]>('/api/tokens')
  const orgs = useApi<{ orgs: string[] }>('/api/orgs')
  const queryClient = useQueryClient()
  const refresh = () => keys.forEach((k) => queryClient.invalidateQueries({ queryKey: [k] }))

  const [botForm, setBotForm] = useState({ name: '', owner: '' })
  const createBot = useMutation({
    mutationFn: () => post('/api/bots', { name: botForm.name, owner: botForm.owner || me.principal }),
    onSuccess: () => {
      setBotForm({ name: '', owner: '' })
      refresh()
    },
  })

  const controlled = [...(ids.data ?? []), ...(bots.data ?? []).flatMap((b) => b.identities)].filter((i) => !i.disabled)
  const [tokForm, setTokForm] = useState({ identity: '', ttl_hours: 8 })
  const [issued, setIssued] = useState<IssuedToken | null>(null)
  const issue = useMutation({
    mutationFn: () => post<IssuedToken>('/api/tokens', { ...tokForm, identity: tokForm.identity || me.identity }),
    onSuccess: (tok) => {
      setIssued(tok)
      refresh()
    },
  })
  const revoke = useMutation({ mutationFn: (id: string) => post(`/api/tokens/${id}/revoke`), onSuccess: refresh })

  return (
    <>
      <div className="page-head">
        <h1>Identities</h1>
        <p className="muted">
          Everything is signed with an identity. Derive one for each agent or role (it can never do more than the one it
          was derived from). An MCP client such as Claude Code connects to an identity at its own endpoint,{' '}
          <code>{mcpURL(me.identity + '/claude')}</code>, and signs in through this site, where you approve it; it can
          create the identity then. Each connection is a session of that identity, so its actions are told apart without
          a new identity. Bots and CI use a standing token instead.
        </p>
      </div>

      <Section title="Your identities">
        {ids.isPending && <Loading />}
        <ErrorBox error={ids.error} />
        {ids.data && <IdentityTree ids={ids.data} canDisableRoot={false} />}
      </Section>

      <ConnectedClients />

      <Emails identities={controlled.map((i) => i.id)} />

      <Section
        title="Bots"
        actions={
          <Toggle label="New bot">
            {(close) => (
              <form
                className="form-inline"
                onSubmit={(e) => {
                  e.preventDefault()
                  createBot.mutate(undefined, { onSuccess: close })
                }}
              >
                <span className="muted">bot:</span>
                <input value={botForm.name} onChange={(e) => setBotForm({ ...botForm, name: e.target.value })} placeholder="devbox" autoFocus />
                <select value={botForm.owner} onChange={(e) => setBotForm({ ...botForm, owner: e.target.value })}>
                  <option value="">owned by me</option>
                  {orgs.data?.orgs.map((o) => (
                    <option key={o} value={`org:${o}`}>
                      owned by org {o}
                    </option>
                  ))}
                  {me.admin && <option value="server">owned by the server</option>}
                </select>
                <button className="button button-primary" disabled={!botForm.name || createBot.isPending}>
                  Create
                </button>
                <button type="button" className="button button-quiet" onClick={close}>
                  Cancel
                </button>
                <ErrorBox error={createBot.error} />
              </form>
            )}
          </Toggle>
        }
      >
        <p className="muted small">Bots act as themselves: a devbox, an alert responder, a scheduled job. Grant them access on a repository like anyone else.</p>
        {bots.data?.length === 0 && <Empty>No bots you manage.</Empty>}
        {bots.data?.map((b) => (
          <div key={b.id} className="bot">
            <div className="muted small">owned by {b.owner === 'server' ? 'the server' : b.owner}</div>
            <IdentityTree ids={b.identities} canDisableRoot />
          </div>
        ))}
      </Section>

      <SSHKeys identities={controlled.map((i) => i.id)} keys={(creds.data ?? []).filter((c) => c.kind === 'ssh_key')} />

      <Section title="Tokens and sessions">
        <form
          className="form-inline"
          onSubmit={(e) => {
            e.preventDefault()
            issue.mutate()
          }}
        >
          <select value={tokForm.identity} onChange={(e) => setTokForm({ ...tokForm, identity: e.target.value })}>
            {controlled.map((i) => (
              <option key={i.id} value={i.id}>
                {i.id}
              </option>
            ))}
          </select>
          <input
            type="number"
            min={0}
            value={tokForm.ttl_hours}
            onChange={(e) => setTokForm({ ...tokForm, ttl_hours: Number(e.target.value) })}
            className="narrow"
            title="hours until it expires (0 = never)"
          />
          <span className="muted small">hours</span>
          <button className="button button-primary" disabled={issue.isPending}>
            Issue token
          </button>
        </form>
        <ErrorBox error={issue.error} />
        {issued && (
          <div className="issued">
            <div>
              Standing token for <Identity handle={issued.identity} />. It is shown only now. Each run that uses it opens
              a session of its own.
            </div>
            <code className="secret">{issued.token}</code>
            <div className="muted small">
              Commit as this identity with <code>GIT_COMMITTER_EMAIL={issued.committer_email}</code>
            </div>
            <button className="button button-quiet" onClick={() => setIssued(null)}>
              Done
            </button>
          </div>
        )}
        <table className="table">
          <thead>
            <tr>
              <th>Identity</th>
              <th>Kind</th>
              <th>Session</th>
              <th>Expires</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {creds.data?.filter((c) => c.kind !== 'ssh_key').map((c) => (
              <tr key={c.id} className={c.revoked ? 'revoked' : ''}>
                <td>
                  <Identity handle={c.identity} />
                </td>
                <td>{c.kind === 'browser' ? 'browser sign-in' : c.kind === 'session' ? 'session' : 'token'}</td>
                <td>
                  {c.session ? <code>{c.session}</code> : <span className="muted">—</span>}
                  {c.label && <span className="muted small"> {c.label}</span>}
                </td>
                <td>{c.expires_at ? new Date(c.expires_at).toLocaleString() : <span className="muted">never</span>}</td>
                <td>
                  {c.revoked ? (
                    <State ok={false}>revoked</State>
                  ) : (
                    <button className="button button-quiet" onClick={() => revoke.mutate(c.id)} disabled={revoke.isPending}>
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        <ErrorBox error={revoke.error} />
      </Section>

      <Invite orgs={orgs.data?.orgs ?? []} />
      {me.admin && <Services />}
    </>
  )
}

// SSHKeys registers keys for git over SSH. A key is a credential of one
// identity, revoked like a token. Each identity works best from a clone of
// its own, whose config names the committer and the key: a repository's
// page gives the command.
function SSHKeys({ identities, keys: list }: { identities: string[]; keys: CredentialInfo[] }) {
  const me = useMe()
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['/api/tokens'] })
  const [form, setForm] = useState({ identity: '', name: '', public_key: '' })
  const add = useMutation({
    mutationFn: () => post<SSHKey>('/api/ssh-keys', { ...form, identity: form.identity || me.identity }),
    onSuccess: () => {
      setForm({ ...form, name: '', public_key: '' })
      refresh()
    },
  })
  const revoke = useMutation({ mutationFn: (id: string) => post(`/api/tokens/${id}/revoke`), onSuccess: refresh })
  return (
    <Section title="SSH keys">
      <p className="muted small">
        A key pushes and fetches over SSH as one identity. Give each agent a key of its own and work for it from a clone of
        its own; a repository's page gives the command, which puts the committer and the key in the clone's config. FIDO keys
        (<code>ed25519-sk</code>) work too, and ask for a touch on every use.
      </p>
      {list.length === 0 && <Empty>No SSH keys yet.</Empty>}
      {list.length > 0 && (
        <table className="table">
          <thead>
            <tr>
              <th>Identity</th>
              <th>Name</th>
              <th>Fingerprint</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {list.map((k) => (
              <tr key={k.id} className={k.revoked ? 'revoked' : ''}>
                <td>
                  <Identity handle={k.identity} />
                </td>
                <td>{k.label || <span className="muted">unnamed</span>}</td>
                <td>
                  <code title={k.key_type}>{k.fingerprint}</code>
                </td>
                <td>
                  {k.revoked ? (
                    <State ok={false}>revoked</State>
                  ) : (
                    <button className="button button-quiet" onClick={() => revoke.mutate(k.id)} disabled={revoke.isPending}>
                      Revoke
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <form
        className="form"
        onSubmit={(e) => {
          e.preventDefault()
          add.mutate()
        }}
      >
        <textarea
          className="mono"
          value={form.public_key}
          onChange={(e) => setForm({ ...form, public_key: e.target.value })}
          placeholder="ssh-ed25519 AAAA... (one line from a .pub file)"
          rows={2}
        />
        <div className="form-actions">
          <select value={form.identity} onChange={(e) => setForm({ ...form, identity: e.target.value })}>
            {identities.map((id) => (
              <option key={id} value={id}>
                {id}
              </option>
            ))}
          </select>
          <input value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} placeholder="name (the key's comment if empty)" />
          <button className="button" disabled={!form.public_key.trim() || add.isPending}>
            Add key
          </button>
        </div>
      </form>
      <ErrorBox error={add.error || revoke.error} />
    </Section>
  )
}

// Emails shows the addresses that commits resolve to an identity by. Every
// identity also has a canonical address that needs no registration.
function Emails({ identities }: { identities: string[] }) {
  const me = useMe()
  const emails = useApi<IdentityEmailAddress[]>('/api/emails')
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['/api/emails'] })
  const [form, setForm] = useState({ identity: '', email: '' })
  const add = useMutation({
    mutationFn: () => post('/api/emails', { identity: form.identity || me.identity, email: form.email }),
    onSuccess: () => {
      setForm({ ...form, email: '' })
      refresh()
    },
  })
  const remove = useMutation({ mutationFn: (email: string) => post('/api/emails/remove', { email }), onSuccess: refresh })
  return (
    <Section title="Commit addresses">
      <p className="muted small">
        A commit belongs to the identity its committer address resolves to. Every identity has a canonical address, such as{' '}
        <code>{me.commit_email}</code>; register the addresses you already commit with here. A push is refused when a new
        commit's committer is not the pusher or an identity it controls.
      </p>
      <ul className="list">
        {emails.data?.map((e) => (
          <li key={e.email} className="list-row">
            <code>{e.email}</code>
            <span className="muted small">→</span>
            <Identity handle={e.identity} />
            <span className="spacer" />
            <button className="button button-quiet" onClick={() => remove.mutate(e.email)} disabled={remove.isPending}>
              Remove
            </button>
          </li>
        ))}
      </ul>
      <form
        className="form-inline"
        onSubmit={(e) => {
          e.preventDefault()
          add.mutate()
        }}
      >
        <input value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} placeholder="you@example.com" />
        <select value={form.identity} onChange={(e) => setForm({ ...form, identity: e.target.value })}>
          {identities.map((id) => (
            <option key={id} value={id}>
              {id}
            </option>
          ))}
        </select>
        <button className="button" disabled={!form.email || add.isPending}>
          Add address
        </button>
      </form>
      <ErrorBox error={add.error || remove.error} />
    </Section>
  )
}

function Invite({ orgs }: { orgs: string[] }) {
  const me = useMe()
  const [form, setForm] = useState({ org: '', set: 'write' })
  const [url, setUrl] = useState('')
  const invite = useMutation({
    mutationFn: () => post<{ url: string }>('/api/invitations', form.org ? form : { org: '' }),
    onSuccess: (res) => setUrl(res.url),
  })
  if (!me.admin && orgs.length === 0) return null
  return (
    <Section title="Invite someone">
      <p className="muted small">A one-time link. The person picks a user name and creates a passkey.</p>
      <form
        className="form-inline"
        onSubmit={(e) => {
          e.preventDefault()
          invite.mutate()
        }}
      >
        <select value={form.org} onChange={(e) => setForm({ ...form, org: e.target.value })}>
          {me.admin && <option value="">to the server, no org</option>}
          {orgs.map((o) => (
            <option key={o} value={o}>
              into org {o}
            </option>
          ))}
        </select>
        {form.org && (
          <select value={form.set} onChange={(e) => setForm({ ...form, set: e.target.value })}>
            {['read', 'write', 'maintain', 'admin'].map((s) => (
              <option key={s} value={s}>
                as {s}
              </option>
            ))}
          </select>
        )}
        <button className="button button-primary" disabled={invite.isPending}>
          Create link
        </button>
      </form>
      <ErrorBox error={invite.error} />
      {url && (
        <div className="issued">
          <div>Send this link to the person. It works once, for 72 hours.</div>
          <code className="secret">{url}</code>
        </div>
      )}
    </Section>
  )
}

const exampleManifest = `{
  "name": "ci",
  "description": "Runs the tests and reports checks",
  "permissions": ["repo.read", "check.write"],
  "events": ["git.ref_transaction"]
}`

function Services() {
  const services = useApi<ServiceInfo[]>('/api/services')
  const queryClient = useQueryClient()
  const [manifest, setManifest] = useState(exampleManifest)
  const register = useMutation({
    mutationFn: () => post('/api/services', JSON.parse(manifest)),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['/api/services'] }),
  })
  return (
    <Section
      title="Services"
      actions={
        <Toggle label="Register a service">
          {(close) => (
            <form
              className="form"
              onSubmit={(e) => {
                e.preventDefault()
                register.mutate(undefined, { onSuccess: close })
              }}
            >
              <textarea value={manifest} onChange={(e) => setManifest(e.target.value)} rows={7} className="mono" />
              <div className="form-actions">
                <button className="button button-primary" disabled={register.isPending}>
                  Register
                </button>
                <button type="button" className="button button-quiet" onClick={close}>
                  Cancel
                </button>
              </div>
              <ErrorBox error={register.error} />
            </form>
          )}
        </Toggle>
      }
    >
      <p className="muted small">External systems, registered from a manifest and installed into orgs. Install them from an org's page.</p>
      <ul className="list">
        {services.data?.map((s) => (
          <li key={s.id} className="list-row">
            <Identity handle={s.id} />
            <span className="list-title">{s.manifest.description}</span>
            {s.id === 'service:krystallos' ? (
              <span className="muted small">built in: the server itself, in every org</span>
            ) : (
              <>
                <span className="muted small">{(s.manifest.permissions ?? []).join(', ') || 'no permissions'}</span>
                <span className="muted small">{s.orgs.length ? `in ${s.orgs.join(', ')}` : 'not installed'}</span>
              </>
            )}
          </li>
        ))}
      </ul>
    </Section>
  )
}
