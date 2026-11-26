import { createContext, useContext, type ReactNode } from 'react'
import { Link, NavLink, Navigate, Outlet, Route, Routes, useLocation, useNavigate } from 'react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { get, post, type Me } from './api'
import { ErrorBox, Identity, Loading, isAuthError } from './ui'
import { LinkPage, LoginPage, RegisterPage } from './pages/Auth'
import { ConsentPage } from './pages/Consent'
import { HomePage } from './pages/Home'
import { OrgPage, IdeaPage } from './pages/Org'
import { RepoPage } from './pages/Repo'
import { IssuePage } from './pages/Issue'
import { ChangeSetPage } from './pages/ChangeSet'
import { SettingsPage } from './pages/Settings'
import { ActivityPage } from './pages/Activity'

const MeContext = createContext<Me | null>(null)

export function useMe(): Me {
  const me = useContext(MeContext)
  if (!me) throw new Error('useMe outside a signed-in page')
  return me
}

function RequireAuth({ children }: { children: ReactNode }) {
  const location = useLocation()
  const me = useQuery({ queryKey: ['me'], queryFn: () => get<Me>('/api/me'), retry: false })
  if (me.isPending) return <Loading />
  if (isAuthError(me.error)) {
    const next = encodeURIComponent(location.pathname + location.search)
    return <Navigate to={`/login?next=${next}`} replace />
  }
  if (me.error) return <ErrorBox error={me.error} />
  return <MeContext.Provider value={me.data}>{children}</MeContext.Provider>
}

function Layout() {
  const me = useMe()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const signOut = async () => {
    await post('/api/auth/logout')
    queryClient.clear()
    navigate('/login')
  }
  return (
    <>
      <header className="topbar">
        <div className="topbar-inner">
          <Link to="/" className="brand">
            gitsynth
          </Link>
          <nav className="topnav">
            <NavLink to="/" end>
              Home
            </NavLink>
            <NavLink to="/activity">Activity</NavLink>
            <NavLink to="/settings">Identities</NavLink>
          </nav>
          <div className="topbar-me">
            <Identity handle={me.identity} />
            <button className="button button-quiet" onClick={signOut}>
              Sign out
            </button>
          </div>
        </div>
      </header>
      <main className="page">
        <Outlet />
      </main>
    </>
  )
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route path="/register" element={<RegisterPage />} />
      <Route path="/link" element={<LinkPage />} />
      <Route
        path="/oauth/consent"
        element={
          <RequireAuth>
            <ConsentPage />
          </RequireAuth>
        }
      />
      <Route
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route index element={<HomePage />} />
        <Route path="activity" element={<ActivityPage />} />
        <Route path="settings" element={<SettingsPage />} />
        <Route path=":org" element={<OrgPage />} />
        <Route path=":org/ideas/:n" element={<IdeaPage />} />
        <Route path=":org/:repo" element={<RepoPage tab="issues" />} />
        <Route path=":org/:repo/changesets" element={<RepoPage tab="changesets" />} />
        <Route path=":org/:repo/code" element={<RepoPage tab="code" />} />
        <Route path=":org/:repo/issues/:n" element={<IssuePage />} />
        <Route path=":org/:repo/changesets/:n" element={<ChangeSetPage />} />
      </Route>
    </Routes>
  )
}
