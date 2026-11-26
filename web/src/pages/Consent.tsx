import { useState } from 'react'
import { useSearchParams } from 'react-router'
import { post, useApi, type AuthRequestView } from '../api'
import { ErrorBox, Identity, Loading } from '../ui'
import { useMe } from '../App'
import { AuthCard } from './Auth'

// ConsentPage is where an MCP client, such as Claude Code, is bound to one of
// the person's identities. The client sent the browser here through
// /oauth/authorize; the person is signed in (with a passkey) to get here,
// sees which client asks to act as which identity, and answers. The answer
// sends the browser back to the client.
export function ConsentPage() {
  const me = useMe()
  const [params] = useSearchParams()
  const id = params.get('request') ?? ''
  const req = useApi<AuthRequestView>(id ? `/api/oauth/requests/${encodeURIComponent(id)}` : null)
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)

  const answer = async (approve: boolean) => {
    setBusy(true)
    setError(null)
    try {
      const path = `/api/oauth/requests/${encodeURIComponent(id)}/${approve ? 'approve' : 'deny'}`
      const out = await post<{ redirect: string }>(path, approve ? { create: (req.data?.create?.length ?? 0) > 0 } : undefined)
      window.location.assign(out.redirect)
    } catch (err) {
      setError(err)
      setBusy(false)
    }
  }

  if (!id) {
    return (
      <AuthCard title="Connect an agent">
        <div className="error">This page is reached from an MCP client signing in. Start again from the client.</div>
      </AuthCard>
    )
  }
  if (req.isPending) return <Loading />
  if (req.error || !req.data) {
    return (
      <AuthCard title="Connect an agent">
        <ErrorBox error={req.error} />
      </AuthCard>
    )
  }
  const r = req.data
  const name = r.client.name || 'An unnamed client'
  const creates = r.create ?? []
  return (
    <AuthCard title="Connect an agent">
      <p>
        <strong>{name}</strong> asks to act as <Identity handle={r.identity} /> through gitsynth's MCP endpoint.
      </p>
      <p className="muted small">
        {r.client.via === 'metadata_document' ? (
          <>
            Its name is published by <strong>{r.client.host}</strong> (<code>{r.client.id}</code>).
          </>
        ) : (
          <>It registered itself under this name; nothing vouches for it.</>
        )}{' '}
        {r.local ? (
          <>After you answer, your browser goes back to a program on this computer ({r.redirect_host}).</>
        ) : (
          <>
            After you answer, your browser goes to <strong>{r.redirect_host}</strong>.
          </>
        )}
      </p>
      <p className="muted small">
        If you approve, it can do whatever <code>{r.identity}</code> can, each connection as a session of its own, until you
        revoke it on the Identities page. It never acts as you, <code>{me.identity}</code>.
      </p>
      {creates.length > 0 && !r.problem && (
        <p className="small">
          {creates.length === 1 ? 'This identity does not exist yet. ' : 'These identities do not exist yet: '}
          {creates.length > 1 && creates.map((c) => <code key={c}>{c} </code>)}
          Approving creates {creates.length === 1 ? 'it' : 'them'}.
        </p>
      )}
      {r.problem && <div className="error">{r.problem}</div>}
      <div className="form-inline">
        <button className="button button-primary" onClick={() => answer(true)} disabled={busy || !!r.problem}>
          {creates.length > 0 ? 'Create and approve' : 'Approve'}
        </button>
        <button className="button" onClick={() => answer(false)} disabled={busy}>
          Deny
        </button>
      </div>
      <ErrorBox error={error} />
    </AuthCard>
  )
}
