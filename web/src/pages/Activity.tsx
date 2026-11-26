import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import type { KrystallosEvent } from '../api'
import { Identity, Oid, when } from '../ui'

// The activity page reads the same event stream any subscriber reads, over
// server-sent events, resuming from the last event it saw.

type Updates = { ref: string; old: string; new: string }[]

function summary(e: KrystallosEvent) {
  const p = (e.payload ?? {}) as Record<string, unknown>
  const updates = (p.updates as Updates | undefined) ?? []
  const refs = updates.map((u) => (
    <span key={u.ref} className="ref-update">
      {u.ref.replace(/^refs\/heads\//, '')} <Oid oid={u.old} len={7} />→<Oid oid={u.new} len={7} />
    </span>
  ))
  switch (e.type) {
    case 'git.ref_transaction':
      return (
        <>
          moved {refs}
          {typeof p.on_behalf_of === 'string' && (
            <>
              {' '}
              on behalf of <Identity handle={p.on_behalf_of} />
            </>
          )}
        </>
      )
    case 'git.ref_transaction_rejected':
      return <>was refused: {String(p.reason)}</>
    case 'git.refs_unattributed':
      return <>found refs changed outside the server: {refs}</>
    case 'changeset.merged':
      return (
        <>
          merged change set {String(p.changeset)} as <Oid oid={String(p.commit)} />
        </>
      )
    case 'review.submitted':
      return (
        <>
          reviewed change set {String(p.changeset)}: {String(p.verdict)}
        </>
      )
    case 'check.reported':
      return (
        <>
          reported check {String(p.name)}: {String(p.status)} at <Oid oid={String(p.commit)} />
        </>
      )
    default: {
      const title = p.title ?? p.name ?? p.id ?? p.service ?? p.number
      return title !== undefined ? <>{String(title)}</> : null
    }
  }
}

function where(e: KrystallosEvent) {
  if (e.repo) return <Link to={`/${e.repo}`}>{e.repo}</Link>
  if (e.org) return <Link to={`/${e.org}`}>{e.org}</Link>
  return null
}

export function ActivityPage() {
  const [events, setEvents] = useState<KrystallosEvent[]>([])
  const [live, setLive] = useState(false)

  useEffect(() => {
    const source = new EventSource('/api/events?after=0')
    source.onopen = () => setLive(true)
    source.onerror = () => setLive(false)
    source.onmessage = (msg: MessageEvent) => {
      const e = JSON.parse(msg.data) as KrystallosEvent
      setEvents((prev) => (prev.some((x) => x.seq === e.seq) ? prev : [e, ...prev].slice(0, 500)))
    }
    return () => source.close()
  }, [])

  return (
    <>
      <div className="page-head">
        <h1>Activity</h1>
        <p className="muted">
          Every write, signed by the identity that made it, in the order of the hash-chained log.{' '}
          <span className={live ? 'live live-on' : 'live'}>{live ? 'live' : 'connecting…'}</span>
        </p>
      </div>
      <table className="table events">
        <thead>
          <tr>
            <th>#</th>
            <th>When</th>
            <th>Who</th>
            <th>What</th>
            <th>Where</th>
          </tr>
        </thead>
        <tbody>
          {events.map((e) => (
            <tr key={e.seq} className={e.identity === 'unattributed' || e.type.endsWith('_rejected') ? 'event-alert' : ''}>
              <td className="muted">{e.seq}</td>
              <td className="muted small" title={e.at}>
                {when(e.at)}
              </td>
              <td>
                <Identity handle={e.identity} />
                {e.session && <div className="muted small">session {e.session}</div>}
              </td>
              <td>
                <code className="event-type">{e.type}</code> {summary(e)}
              </td>
              <td>{where(e)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>
  )
}

