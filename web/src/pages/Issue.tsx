import { useState } from 'react'
import { Link, useParams } from 'react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { post, repoPath, useApi, type IssueView, type PlanView, type RepoInfo } from '../api'
import { Empty, ErrorBox, Identity, Loading, Section, State, Text, Toggle } from '../ui'

export function IssuePage() {
  const { org = '', repo = '', n = '' } = useParams()
  const path = `${repoPath(org, repo)}/issues/${n}`
  const issue = useApi<IssueView>(path)
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: [path] })

  const [comment, setComment] = useState('')
  const addComment = useMutation({
    mutationFn: () => post(`${path}/comments`, { body: comment }),
    onSuccess: () => {
      setComment('')
      refresh()
    },
  })
  const close = useMutation({ mutationFn: () => post(`${path}/close`), onSuccess: refresh })

  if (issue.isPending) return <Loading />
  if (issue.error) return <ErrorBox error={issue.error} />
  const it = issue.data
  return (
    <>
      <div className="page-head">
        <div className="crumbs">
          <Link to={`/${org}`}>{org}</Link> / <Link to={`/${org}/${repo}`}>{repo}</Link> / issue {it.number}
        </div>
        <h1>{it.title}</h1>
        <div className="meta">
          <State ok={it.closed}>{it.closed ? 'closed' : 'open'}</State>
          <Identity handle={it.author} />
          {it.from_ideas.map((i) => (
            <Link key={`${i.org}/${i.number}`} to={`/${i.org}/ideas/${i.number}`} className="tag">
              from idea {i.number}
            </Link>
          ))}
        </div>
      </div>
      <Text>{it.body}</Text>

      {it.hints.map((h) => (
        <div key={h} className="hint">
          <span>{h}. Close the issue if it is done.</span>
          <button className="button" onClick={() => close.mutate()} disabled={close.isPending}>
            Close issue
          </button>
        </div>
      ))}

      <Section title="Plans" actions={<NewPlan path={path} onDone={refresh} />}>
        <p className="muted small">
          A plan says how. Change sets execute plans; which plan was followed shows in what gets merged.
        </p>
        {it.plans.length === 0 && <Empty>No plan yet. Change sets can also go ahead without one.</Empty>}
        {it.plans.map((p) => (
          <Plan key={p.number} org={org} repo={repo} plan={p} />
        ))}
      </Section>

      <Section title="Change sets" actions={<NewChangeSet org={org} repo={repo} issue={it} onDone={refresh} />}>
        {it.changesets.length === 0 && <Empty>No change sets yet. Push a branch, then open one here.</Empty>}
        <ul className="list">
          {it.changesets.map((c) => (
            <li key={c.number} className="list-row">
              <span className="num">{c.number}</span>
              <Link to={`/${org}/${repo}/changesets/${c.number}`} className="list-title">
                {c.title}
              </Link>
              <span className="muted small">
                {c.branch} → {c.base}
                {c.plan ? ` · plan ${c.plan} v${c.plan_version} ${c.steps.join(', ')}` : ''}
              </span>
              <Identity handle={c.author} />
              <State ok={!!c.merged_commit}>{c.merged_commit ? 'merged' : 'open'}</State>
            </li>
          ))}
        </ul>
      </Section>

      <Section title="Discussion">
        {it.comments.length === 0 && <Empty>No comments yet.</Empty>}
        {it.comments.map((c, i) => (
          <div key={i} className="comment">
            <Identity handle={c.author} />
            <div className="prose">{c.body}</div>
          </div>
        ))}
        <form
          className="form"
          onSubmit={(e) => {
            e.preventDefault()
            addComment.mutate()
          }}
        >
          <textarea value={comment} onChange={(e) => setComment(e.target.value)} placeholder="Add to the discussion" rows={3} />
          <div className="form-actions">
            <button className="button button-primary" disabled={!comment.trim() || addComment.isPending}>
              Comment
            </button>
            {!it.closed && (
              <button type="button" className="button" onClick={() => close.mutate()} disabled={close.isPending}>
                Close issue
              </button>
            )}
          </div>
          <ErrorBox error={addComment.error || close.error} />
        </form>
      </Section>
    </>
  )
}

function Plan({ org, repo, plan }: { org: string; repo: string; plan: PlanView }) {
  return (
    <div className="plan">
      <div className="plan-head">
        <strong>
          Plan {plan.number}: {plan.title}
        </strong>
        <span className="muted small">version {plan.version}</span>
        <Identity handle={plan.author} />
        {plan.complete && <State ok>every step merged</State>}
      </div>
      {plan.body && <div className="prose">{plan.body}</div>}
      <ol className="steps">
        {plan.steps.map((s) => (
          <li key={s.id} className={s.merged ? 'step step-done' : 'step'}>
            <span className="step-mark">{s.merged ? '✓' : ''}</span>
            <code className="step-id">{s.id}</code>
            <span className="step-title">{s.title}</span>
            <span className="step-sets">
              {s.changesets.map((c) => (
                <Link key={c} to={`/${org}/${repo}/changesets/${c}`} className="tag">
                  change set {c}
                </Link>
              ))}
            </span>
          </li>
        ))}
      </ol>
    </div>
  )
}

function NewPlan({ path, onDone }: { path: string; onDone: () => void }) {
  const [form, setForm] = useState({ title: '', body: '', steps: '' })
  const create = useMutation({
    mutationFn: () =>
      post(`${path}/plans`, {
        title: form.title,
        body: form.body,
        steps: form.steps
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean)
          .map((title) => ({ title })),
      }),
    onSuccess: () => {
      setForm({ title: '', body: '', steps: '' })
      onDone()
    },
  })
  return (
    <Toggle label="New plan">
      {(close) => (
        <form
          className="form"
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate(undefined, { onSuccess: close })
          }}
        >
          <input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="Approach" autoFocus />
          <textarea value={form.body} onChange={(e) => setForm({ ...form, body: e.target.value })} placeholder="Notes (optional)" rows={2} />
          <textarea
            value={form.steps}
            onChange={(e) => setForm({ ...form, steps: e.target.value })}
            placeholder={'Steps, one per line'}
            rows={4}
          />
          <div className="form-actions">
            <button className="button button-primary" disabled={!form.title || create.isPending}>
              Add plan
            </button>
            <button type="button" className="button button-quiet" onClick={close}>
              Cancel
            </button>
          </div>
          <ErrorBox error={create.error} />
        </form>
      )}
    </Toggle>
  )
}

function NewChangeSet({ org, repo, issue, onDone }: { org: string; repo: string; issue: IssueView; onDone: () => void }) {
  const info = useApi<RepoInfo>(repoPath(org, repo))
  const [form, setForm] = useState({ title: '', branch: '', plan: 0, steps: [] as string[] })
  const plan = issue.plans.find((p) => p.number === form.plan)
  const create = useMutation({
    mutationFn: () =>
      post(`${repoPath(org, repo)}/issues/${issue.number}/changesets`, {
        title: form.title,
        branch: form.branch,
        ...(form.plan ? { plan: form.plan, steps: form.steps } : {}),
      }),
    onSuccess: () => {
      setForm({ title: '', branch: '', plan: 0, steps: [] })
      onDone()
    },
  })
  const branches = info.data?.refs.filter((r) => r.kind === 'branch' && r.name !== info.data.repo.default_branch) ?? []
  return (
    <Toggle label="New change set">
      {(close) => (
        <form
          className="form"
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate(undefined, { onSuccess: close })
          }}
        >
          <input value={form.title} onChange={(e) => setForm({ ...form, title: e.target.value })} placeholder="Title" autoFocus />
          <select value={form.branch} onChange={(e) => setForm({ ...form, branch: e.target.value })}>
            <option value="">Choose a pushed branch…</option>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
          <select value={form.plan} onChange={(e) => setForm({ ...form, plan: Number(e.target.value), steps: [] })}>
            <option value={0}>No plan</option>
            {issue.plans.map((p) => (
              <option key={p.number} value={p.number}>
                Executes plan {p.number} (v{p.version}): {p.title}
              </option>
            ))}
          </select>
          {plan && (
            <div className="checks">
              <span className="muted small">Steps this change set carries out</span>
              {plan.steps.map((s) => (
                <label key={s.id} className="check">
                  <input
                    type="checkbox"
                    checked={form.steps.includes(s.id)}
                    onChange={(e) =>
                      setForm({ ...form, steps: e.target.checked ? [...form.steps, s.id] : form.steps.filter((x) => x !== s.id) })
                    }
                  />
                  <code>{s.id}</code> {s.title}
                </label>
              ))}
            </div>
          )}
          <div className="form-actions">
            <button className="button button-primary" disabled={!form.title || !form.branch || create.isPending}>
              Open change set
            </button>
            <button type="button" className="button button-quiet" onClick={close}>
              Cancel
            </button>
          </div>
          <ErrorBox error={create.error} />
        </form>
      )}
    </Toggle>
  )
}
