import { useState } from 'react'
import { Link, useParams } from 'react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { post, repoPath, useApi, type ChangeSetView, type Diff, type ReviewComment } from '../api'
import { DiffView, type PlacedComment } from '../diff'
import { ErrorBox, Identity, Loading, Oid, Section, State } from '../ui'

const verdictLabel = { approve: 'approved', comment: 'commented', request_changes: 'asked for changes' }

export function ChangeSetPage() {
  const { org = '', repo = '', n = '' } = useParams()
  const path = `${repoPath(org, repo)}/changesets/${n}`
  const cs = useApi<ChangeSetView>(path)
  const diff = useApi<Diff>(`${path}/diff`)
  const queryClient = useQueryClient()
  const refresh = () => {
    queryClient.invalidateQueries({ queryKey: [path] })
    queryClient.invalidateQueries({ queryKey: [`${path}/diff`] })
  }

  const [pending, setPending] = useState<ReviewComment[]>([])
  const [review, setReview] = useState({ verdict: 'comment', body: '' })
  const submit = useMutation({
    mutationFn: () => post(`${path}/reviews`, { ...review, comments: pending }),
    onSuccess: () => {
      setPending([])
      setReview({ verdict: 'comment', body: '' })
      refresh()
    },
  })
  const merge = useMutation({ mutationFn: () => post(`${path}/merge`), onSuccess: refresh })

  if (cs.isPending) return <Loading />
  if (cs.error) return <ErrorBox error={cs.error} />
  const c = cs.data
  const placed: PlacedComment[] = [
    ...c.reviews.flatMap((r) => r.comments.map((x) => ({ ...x, author: r.author }))),
    ...pending.map((x) => ({ ...x, pending: true })),
  ]
  return (
    <>
      <div className="page-head">
        <div className="crumbs">
          <Link to={`/${org}`}>{org}</Link> / <Link to={`/${org}/${repo}`}>{repo}</Link> /{' '}
          <Link to={`/${org}/${repo}/issues/${c.issue}`}>issue {c.issue}</Link> / change set {c.number}
        </div>
        <h1>{c.title}</h1>
        <div className="meta">
          <State ok={!!c.merged_commit}>{c.merged_commit ? 'merged' : 'open'}</State>
          <Identity handle={c.author} />
          <span className="muted small">
            {c.branch} → {c.base}
          </span>
          {c.plan ? (
            <span className="tag">
              executes plan {c.plan} v{c.plan_version}: {c.steps.join(', ') || 'no steps named'}
            </span>
          ) : (
            <span className="tag">no plan</span>
          )}
        </div>
      </div>

      <div className={`merge-box ${c.merged_commit ? 'merge-done' : c.status.mergeable ? 'merge-ready' : 'merge-blocked'}`}>
        {c.merged_commit ? (
          <span>
            Merged as <Oid oid={c.merged_commit} />.
          </span>
        ) : (
          <>
            <div>
              {c.status.mergeable ? <strong>Ready to merge.</strong> : <strong>Not mergeable yet.</strong>}
              {c.status.head && (
                <span className="muted small">
                  {' '}
                  head <Oid oid={c.status.head} />
                </span>
              )}
              {c.status.approvals.length > 0 && (
                <span className="muted small">
                  {' '}
                  approved by{' '}
                  {c.status.approvals.map((a) => (
                    <Identity key={a} handle={a} />
                  ))}
                </span>
              )}
              <ul className="unmet">
                {c.status.unmet.map((u) => (
                  <li key={u}>{u}</li>
                ))}
              </ul>
            </div>
            <button className="button button-primary" onClick={() => merge.mutate()} disabled={!c.status.mergeable || merge.isPending}>
              Merge
            </button>
          </>
        )}
        <ErrorBox error={merge.error} />
      </div>

      <Section title="Reviews">
        {c.reviews.length === 0 && <div className="muted small">No reviews yet.</div>}
        {c.reviews.map((r) => (
          <div key={r.seq} className={`review review-${r.verdict}`}>
            <Identity handle={r.author} /> <strong>{verdictLabel[r.verdict]}</strong>{' '}
            <span className="muted small">
              at <Oid oid={r.commit} />
              {r.comments.length > 0 && ` · ${r.comments.length} line comment(s)`}
            </span>
            {r.body && <div className="prose">{r.body}</div>}
          </div>
        ))}
        {!c.merged_commit && (
          <form
            className="form review-form"
            onSubmit={(e) => {
              e.preventDefault()
              submit.mutate()
            }}
          >
            <div className="radio-row">
              {(['comment', 'approve', 'request_changes'] as const).map((v) => (
                <label key={v} className="check">
                  <input type="radio" name="verdict" checked={review.verdict === v} onChange={() => setReview({ ...review, verdict: v })} />
                  {v === 'comment' ? 'Comment' : v === 'approve' ? 'Approve' : 'Request changes'}
                </label>
              ))}
            </div>
            <textarea value={review.body} onChange={(e) => setReview({ ...review, body: e.target.value })} placeholder="Overall review" rows={3} />
            <div className="form-actions">
              <button className="button button-primary" disabled={submit.isPending}>
                Submit review{pending.length > 0 ? ` with ${pending.length} line comment(s)` : ''}
              </button>
              <span className="muted small">Click a line number in the diff to comment on that line.</span>
            </div>
            <ErrorBox error={submit.error} />
          </form>
        )}
      </Section>

      <Section title="Changes">
        {diff.isPending && <Loading />}
        <ErrorBox error={diff.error} />
        {diff.data && (
          <>
            <div className="muted small">
              <Oid oid={diff.data.merge_base} /> → <Oid oid={diff.data.head} />
            </div>
            <DiffView
              files={diff.data.files}
              comments={placed}
              onComment={c.merged_commit ? undefined : (x) => setPending((p) => [...p, x])}
            />
          </>
        )}
      </Section>
    </>
  )
}
