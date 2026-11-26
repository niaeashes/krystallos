import { useState, type ReactNode } from 'react'
import { Link } from 'react-router'
import { ApiError } from './api'

export interface ParsedIdentity {
  kind: 'user' | 'bot' | 'service' | 'unattributed'
  root: string
  labels: string[]
}

// parseIdentity reads "kind:root/label/label[@host]".
export function parseIdentity(id: string): ParsedIdentity {
  if (id === 'unattributed') return { kind: 'unattributed', root: 'unattributed', labels: [] }
  const colon = id.indexOf(':')
  const kind = (colon < 0 ? 'user' : id.slice(0, colon)) as ParsedIdentity['kind']
  const path = (colon < 0 ? id : id.slice(colon + 1)).split('@')[0]
  const [root, ...labels] = path.split('/')
  return { kind, root, labels }
}

// An identity is drawn as its root plus the labels derived below it:
// "alice", "alice / claude", "bot devbox / triage". The derived part is what
// tells a person's own hand from their agent's, so it gets its own color;
// bots and services carry their kind in front.
export function Identity({ handle }: { handle: string }) {
  const id = parseIdentity(handle)
  const style = handle === 'service:krystallos' ? 'server' : id.kind
  return (
    <span className={`identity identity-${style}`} title={handle}>
      <span className="identity-principal">
        {(id.kind === 'bot' || id.kind === 'service') && <span className="identity-kind">{id.kind}</span>}
        {id.root}
      </span>
      {id.labels.length > 0 && <span className="identity-label">{id.labels.join(' / ')}</span>}
    </span>
  )
}

export function Oid({ oid, len = 8 }: { oid?: string; len?: number }) {
  if (!oid) return null
  return (
    <code className="oid" title={oid}>
      {oid.slice(0, len)}
    </code>
  )
}

export function ErrorBox({ error }: { error: unknown }) {
  if (!error) return null
  const msg = error instanceof Error ? error.message : String(error)
  return <div className="error">{msg}</div>
}

export function Loading() {
  return <div className="muted loading">Loading…</div>
}

export function Empty({ children }: { children: ReactNode }) {
  return <div className="empty">{children}</div>
}

export function Section({ title, actions, children }: { title: ReactNode; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="section">
      <header className="section-head">
        <h2>{title}</h2>
        {actions && <div className="section-actions">{actions}</div>}
      </header>
      {children}
    </section>
  )
}

export function Text({ children }: { children: string }) {
  if (!children.trim()) return <p className="muted">No description.</p>
  return <div className="prose">{children}</div>
}

export function State({ ok, children }: { ok: boolean; children: ReactNode }) {
  return <span className={`state ${ok ? 'state-ok' : 'state-open'}`}>{children}</span>
}

// Toggle is a "New …" button that opens a small form in place.
export function Toggle({ label, children }: { label: string; children: (close: () => void) => ReactNode }) {
  const [open, setOpen] = useState(false)
  if (!open)
    return (
      <button className="button" onClick={() => setOpen(true)}>
        {label}
      </button>
    )
  return <div className="toggle-panel">{children(() => setOpen(false))}</div>
}

export function isAuthError(err: unknown) {
  return err instanceof ApiError && err.status === 401
}

export function RepoLink({ fullName }: { fullName: string }) {
  return <Link to={`/${fullName}`}>{fullName}</Link>
}

export function when(at: string) {
  const d = new Date(at)
  const secs = Math.round((Date.now() - d.getTime()) / 1000)
  if (secs < 60) return `${Math.max(secs, 0)}s ago`
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`
  return d.toLocaleDateString()
}
