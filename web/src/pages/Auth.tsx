import { useEffect, useRef, useState, type ReactNode } from 'react'
import { useNavigate, useSearchParams } from 'react-router'
import { useQueryClient } from '@tanstack/react-query'
import {
  browserSupportsWebAuthn,
  startAuthentication,
  startRegistration,
  type PublicKeyCredentialCreationOptionsJSON,
  type PublicKeyCredentialRequestOptionsJSON,
} from '@simplewebauthn/browser'
import { api, post, type LinkInfo } from '../api'
import { ErrorBox } from '../ui'

interface Begin<T> {
  ceremony: string
  options: { publicKey: T }
}

export function AuthCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="auth">
      <div className="auth-card">
        <div className="brand auth-brand">krystallos</div>
        <h1>{title}</h1>
        {children}
      </div>
    </div>
  )
}

function useSignedIn() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  return (next = '/') => {
    queryClient.clear()
    navigate(next.startsWith('/') ? next : '/', { replace: true })
  }
}

export function LoginPage() {
  const [params] = useSearchParams()
  const signedIn = useSignedIn()
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)

  const signIn = async () => {
    setBusy(true)
    setError(null)
    try {
      const begin = await post<Begin<PublicKeyCredentialRequestOptionsJSON>>('/api/auth/login/begin')
      const assertion = await startAuthentication({ optionsJSON: begin.options.publicKey })
      await api('POST', `/api/auth/login/finish?ceremony=${encodeURIComponent(begin.ceremony)}`, assertion)
      signedIn(params.get('next') ?? '/')
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthCard title="Sign in">
      <p className="muted">
        People sign in with a passkey, so that only someone actually present can act as their standard identity.
      </p>
      {!browserSupportsWebAuthn() && <div className="error">This browser does not support passkeys.</div>}
      <button className="button button-primary button-wide" onClick={signIn} disabled={busy}>
        {busy ? 'Waiting for your passkey…' : 'Sign in with a passkey'}
      </button>
      <ErrorBox error={error} />
      <p className="muted small">
        No passkey yet? An admin can give you a registration link with <code>kry admin passkey-link</code>.
      </p>
    </AuthCard>
  )
}

export function RegisterPage() {
  const [params] = useSearchParams()
  const signedIn = useSignedIn()
  const token = params.get('token') ?? ''
  const [error, setError] = useState<unknown>(null)
  const [busy, setBusy] = useState(false)
  const [name, setName] = useState('')
  const [info, setInfo] = useState<LinkInfo | null>(null)

  useEffect(() => {
    if (token) post<LinkInfo>('/api/auth/link/inspect', { token }).then(setInfo).catch(setError)
  }, [token])
  const invited = info?.purpose === 'invite'

  const register = async () => {
    setBusy(true)
    setError(null)
    try {
      const begin = await post<Begin<PublicKeyCredentialCreationOptionsJSON>>('/api/auth/register/begin', { token, name })
      const attestation = await startRegistration({ optionsJSON: begin.options.publicKey })
      await api('POST', `/api/auth/register/finish?ceremony=${encodeURIComponent(begin.ceremony)}`, attestation)
      signedIn('/')
    } catch (err) {
      setError(err)
    } finally {
      setBusy(false)
    }
  }

  return (
    <AuthCard title={invited ? 'Join krystallos' : 'Create your passkey'}>
      {invited ? (
        <>
          <p className="muted">
            You were invited{info?.org ? ` into ${info.org}` : ''}. Pick a user name; you will be{' '}
            <code>user:{name || 'name'}</code>. Then create a passkey to sign in with.
          </p>
          <input
            className="wide-input"
            value={name}
            onChange={(e) => setName(e.target.value.toLowerCase())}
            placeholder="user name"
            autoFocus
          />
        </>
      ) : (
        <p className="muted">
          This one-time link lets {info?.user ? <code>{info.user}</code> : 'you'} add a passkey. You will use it to sign in from now on.
        </p>
      )}
      {!token && <div className="error">The link has no token.</div>}
      <button className="button button-primary button-wide" onClick={register} disabled={busy || !token || !info || (invited && !name)}>
        {busy ? 'Waiting for your authenticator…' : 'Create a passkey'}
      </button>
      <ErrorBox error={error} />
    </AuthCard>
  )
}

// LinkPage redeems a one-time sign-in link from the admin CLI.
export function LinkPage() {
  const [params] = useSearchParams()
  const signedIn = useSignedIn()
  const [error, setError] = useState<unknown>(null)
  const started = useRef(false)

  useEffect(() => {
    if (started.current) return
    started.current = true
    post('/api/auth/link', { token: params.get('token') ?? '' })
      .then(() => signedIn('/'))
      .catch(setError)
  }, [params, signedIn])

  return (
    <AuthCard title="Signing you in">
      {error ? <ErrorBox error={error} /> : <p className="muted">Checking the link…</p>}
    </AuthCard>
  )
}
