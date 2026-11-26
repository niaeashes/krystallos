import { useState } from 'react'
import { Link, NavLink, useNavigate, useParams, useSearchParams } from 'react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import {
  post,
  repoPath,
  useApi,
  type Blob,
  type ChangeSetSummary,
  type Commit,
  type IdentityInfo,
  type IssueSummary,
  type RepoInfo,
  type TreeEntry,
} from '../api'
import { Empty, ErrorBox, Identity, Loading, Oid, Section, State, Toggle, parseIdentity } from '../ui'
import { useMe } from '../App'

type Tab = 'issues' | 'changesets' | 'code'

export function RepoPage({ tab }: { tab: Tab }) {
  const { org = '', repo = '' } = useParams()
  const info = useApi<RepoInfo>(repoPath(org, repo))
  if (info.isPending) return <Loading />
  if (info.error) return <ErrorBox error={info.error} />
  const base = `/${org}/${repo}`
  return (
    <>
      <div className="page-head">
        <div className="crumbs">
          <Link to={`/${org}`}>{org}</Link> / {repo}
        </div>
        <h1>{repo}</h1>
        <Clone info={info.data} />
      </div>
      <nav className="tabs">
        <NavLink to={base} end>
          Issues
        </NavLink>
        <NavLink to={`${base}/changesets`}>Change sets</NavLink>
        <NavLink to={`${base}/code`}>Code</NavLink>
      </nav>
      {tab === 'issues' && <Issues org={org} repo={repo} />}
      {tab === 'changesets' && <ChangeSets org={org} repo={repo} />}
      {tab === 'code' && <Code org={org} repo={repo} info={info.data} />}
    </>
  )
}

// sshKeyFile is where the clone command expects an identity's key: the
// person's usual key, and a key of its own for each identity derived from
// them, so an agent's clone never pushes with the person's.
function sshKeyFile(identity: string) {
  const { labels } = parseIdentity(identity)
  return labels.length === 0 ? '~/.ssh/id_ed25519' : `~/.ssh/gitsynth_${labels.join('_')}`
}

// cloneCommand makes a clone that belongs to one identity: its config names
// the committer and the key, so whoever works in it, a person or an agent,
// has neither to remember. A derived identity's ssh reads no ssh config
// (-F none): a "Host *" IdentityFile there, common on macOS, would offer the
// person's key after the agent's, and a missing or revoked agent key would
// then push as the person.
function cloneCommand(identity: string, email: string, url: string, dir: string) {
  const derived = parseIdentity(identity).labels.length > 0
  const ssh = `ssh${derived ? ' -F none' : ''} -i ${sshKeyFile(identity)} -o IdentitiesOnly=yes`
  return ['git clone', `  -c user.name=${identity}`, `  -c user.email=${email}`, `  -c core.sshCommand="${ssh}"`, `  ${url} ${dir}`].join(
    ' \\\n',
  )
}

function Clone({ info }: { info: RepoInfo }) {
  const me = useMe()
  const ids = useApi<IdentityInfo[]>('/api/identities')
  const [as, setAs] = useState('')
  const identity = as || me.identity
  const host = me.commit_email.slice(me.commit_email.lastIndexOf('@') + 1)
  const { labels } = parseIdentity(identity)
  const dir = [info.repo.name, ...labels].join('-')
  return (
    <div className="clone">
      <span className="muted small">clone</span>
      <code>{info.clone_url}</code>
      {info.ssh_clone_url && <code>{info.ssh_clone_url}</code>}
      {info.ssh_clone_url && (
        <Toggle label="Clone as an identity">
          {(close) => (
            <div className="form">
              <div className="form-actions">
                <select value={identity} onChange={(e) => setAs(e.target.value)}>
                  {(ids.data ?? [{ id: me.identity, disabled: false }])
                    .filter((i) => !i.disabled)
                    .map((i) => (
                      <option key={i.id} value={i.id}>
                        {i.id}
                      </option>
                    ))}
                </select>
                <button type="button" className="button button-quiet" onClick={close}>
                  Close
                </button>
              </div>
              <pre className="command">{cloneCommand(identity, `${identity}@${host}`, info.ssh_clone_url!, dir)}</pre>
              <p className="muted small">
                Everything committed and pushed in that clone belongs to <Identity handle={identity} />.{' '}
                {labels.length > 0 ? (
                  <>
                    Make its key with <code>ssh-keygen -t ed25519 -f {sshKeyFile(identity)} -C {identity}</code> and add
                    the <code>.pub</code> line under SSH keys on the Identities page. Run the agent in that clone.
                  </>
                ) : (
                  <>Add your key's <code>.pub</code> line under SSH keys on the Identities page.</>
                )}{' '}
                Working in one clone together is fine too: your pushes may carry your agents' commits, and are recorded as
                yours.
              </p>
            </div>
          )}
        </Toggle>
      )}
    </div>
  )
}

function Issues({ org, repo }: { org: string; repo: string }) {
  const issues = useApi<IssueSummary[]>(`${repoPath(org, repo)}/issues`)
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const [form, setForm] = useState({ title: '', body: '' })
  const create = useMutation({
    mutationFn: () => post<{ number: number }>(`${repoPath(org, repo)}/issues`, form),
    onSuccess: (res) => {
      queryClient.invalidateQueries({ queryKey: [`${repoPath(org, repo)}/issues`] })
      navigate(`/${org}/${repo}/issues/${res.number}`)
    },
  })
  return (
    <Section
      title="Issues"
      actions={
        <Toggle label="New issue">
          {(close) => (
            <form
              className="form"
              onSubmit={(e) => {
                e.preventDefault()
                create.mutate()
              }}
            >
              <input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="Title" autoFocus />
              <textarea
                value={form.body}
                onChange={(e) => setForm({ ...form, body: e.target.value })}
                placeholder="What should be done, and why?"
                rows={4}
              />
              <div className="form-actions">
                <button className="button button-primary" disabled={!form.title || create.isPending}>
                  Open issue
                </button>
                <button type="button" className="button button-quiet" onClick={close}>
                  Cancel
                </button>
              </div>
              <ErrorBox error={create.error} />
            </form>
          )}
        </Toggle>
      }
    >
      {issues.isPending && <Loading />}
      <ErrorBox error={issues.error} />
      {issues.data?.length === 0 && <Empty>No issues yet.</Empty>}
      <ul className="list">
        {issues.data
          ?.slice()
          .reverse()
          .map((i) => (
            <li key={i.number} className="list-row">
              <span className="num">{i.number}</span>
              <Link to={`/${org}/${repo}/issues/${i.number}`} className="list-title">
                {i.title}
              </Link>
              <Identity handle={i.author} />
              <State ok={i.closed}>{i.closed ? 'closed' : 'open'}</State>
            </li>
          ))}
      </ul>
    </Section>
  )
}

function ChangeSets({ org, repo }: { org: string; repo: string }) {
  const sets = useApi<ChangeSetSummary[]>(`${repoPath(org, repo)}/changesets`)
  return (
    <Section title="Change sets">
      <p className="muted small">Every change set belongs to an issue. Open one from its issue.</p>
      {sets.isPending && <Loading />}
      <ErrorBox error={sets.error} />
      {sets.data?.length === 0 && <Empty>No change sets yet.</Empty>}
      <ul className="list">
        {sets.data?.map((c) => (
          <li key={c.number} className="list-row">
            <span className="num">{c.number}</span>
            <Link to={`/${org}/${repo}/changesets/${c.number}`} className="list-title">
              {c.title}
            </Link>
            <span className="muted small">
              {c.branch} → {c.base} · issue {c.issue}
            </span>
            <Identity handle={c.author} />
            <State ok={c.merged}>{c.merged ? 'merged' : 'open'}</State>
          </li>
        ))}
      </ul>
    </Section>
  )
}

function Code({ org, repo, info }: { org: string; repo: string; info: RepoInfo }) {
  const [params, setParams] = useSearchParams()
  const ref = params.get('ref') || info.repo.default_branch
  const path = params.get('path') || ''
  const isFile = params.get('file') === '1'
  const go = (next: { ref?: string; path?: string; file?: boolean }) => {
    const p = new URLSearchParams()
    p.set('ref', next.ref ?? ref)
    if (next.path) p.set('path', next.path)
    if (next.file) p.set('file', '1')
    setParams(p)
  }
  if (info.refs.length === 0) {
    return (
      <Section title="Code">
        <Empty>
          This repository is empty. Push to it with <code>git push {info.clone_url} HEAD:{info.repo.default_branch}</code>.
        </Empty>
      </Section>
    )
  }
  const parts = path ? path.split('/') : []
  return (
    <Section
      title={
        <span className="pathbar">
          <select value={ref} onChange={(e) => go({ ref: e.target.value })}>
            {info.refs.map((r) => (
              <option key={`${r.kind}/${r.name}`} value={r.name}>
                {r.kind === 'tag' ? `tag ${r.name}` : r.name}
              </option>
            ))}
          </select>
          <button className="linklike" onClick={() => go({ path: '' })}>
            {repo}
          </button>
          {parts.map((part, i) => (
            <span key={i}>
              {' / '}
              <button className="linklike" onClick={() => go({ path: parts.slice(0, i + 1).join('/') })}>
                {part}
              </button>
            </span>
          ))}
        </span>
      }
    >
      {isFile ? <FileView org={org} repo={repo} gitRef={ref} path={path} /> : <TreeView org={org} repo={repo} gitRef={ref} path={path} onOpen={go} />}
      <History org={org} repo={repo} gitRef={ref} path={path} />
    </Section>
  )
}

function TreeView(props: { org: string; repo: string; gitRef: string; path: string; onOpen: (n: { path: string; file?: boolean }) => void }) {
  const q = new URLSearchParams({ ref: props.gitRef, path: props.path })
  const tree = useApi<{ commit: string; entries: TreeEntry[] }>(`${repoPath(props.org, props.repo)}/tree?${q}`)
  if (tree.isPending) return <Loading />
  if (tree.error) return <ErrorBox error={tree.error} />
  return (
    <>
      <div className="muted small">
        at <Oid oid={tree.data.commit} />
      </div>
      <ul className="tree">
        {tree.data.entries.map((e) => (
          <li key={e.path}>
            <button className="linklike tree-entry" onClick={() => props.onOpen({ path: e.path, file: e.type === 'blob' })}>
              <span className={`tree-icon tree-${e.type}`}>{e.type === 'tree' ? '▸' : '·'}</span>
              {e.name}
            </button>
            {e.size !== undefined && <span className="muted small">{e.size} B</span>}
          </li>
        ))}
      </ul>
    </>
  )
}

function FileView(props: { org: string; repo: string; gitRef: string; path: string }) {
  const q = new URLSearchParams({ ref: props.gitRef, path: props.path })
  const blob = useApi<{ commit: string; blob: Blob }>(`${repoPath(props.org, props.repo)}/blob?${q}`)
  if (blob.isPending) return <Loading />
  if (blob.error) return <ErrorBox error={blob.error} />
  const b = blob.data.blob
  return (
    <div className="file">
      <div className="file-head">
        <span>{b.path}</span>
        <span className="muted small">
          {b.size} bytes at <Oid oid={blob.data.commit} />
        </span>
      </div>
      {b.binary ? (
        <Empty>Binary file.</Empty>
      ) : (
        <pre className="code">
          {b.content.split('\n').map((line, i) => (
            <div key={i} className="code-line">
              <span className="ln">{i + 1}</span>
              <span>{line}</span>
            </div>
          ))}
        </pre>
      )}
      {b.truncated && <div className="muted small">Truncated at 1 MiB.</div>}
    </div>
  )
}

function History(props: { org: string; repo: string; gitRef: string; path: string }) {
  const q = new URLSearchParams({ ref: props.gitRef, path: props.path, limit: '20' })
  const log = useApi<{ commits: Commit[] }>(`${repoPath(props.org, props.repo)}/commits?${q}`)
  if (log.isPending || log.error) return null
  return (
    <div className="history">
      <h3>History</h3>
      <ul className="list">
        {log.data.commits.map((c) => (
          <li key={c.oid} className="list-row">
            <Oid oid={c.oid} />
            <span className="list-title">{c.subject}</span>
            {c.committer_identity ? (
              <Identity handle={c.committer_identity} />
            ) : (
              <span className="muted small" title={c.committer_email}>
                {c.committer_name} (no identity)
              </span>
            )}
            {c.author_identity !== c.committer_identity && (
              <span className="muted small" title={c.author_email}>
                authored by {c.author_identity ?? c.author_name}
              </span>
            )}
            <span className="muted small">{new Date(c.committer_date).toLocaleString()}</span>
          </li>
        ))}
      </ul>
    </div>
  )
}
