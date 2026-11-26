import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { post, useApi, type Idea, type Repo, type ServiceInfo } from '../api'
import { Empty, ErrorBox, Identity, Loading, Section, State, Text, Toggle } from '../ui'

export function OrgPage() {
  const { org = '' } = useParams()
  const repos = useApi<Repo[]>(`/api/orgs/${org}/repos`)
  const ideas = useApi<Idea[]>(`/api/orgs/${org}/ideas`)
  const queryClient = useQueryClient()

  const [repoName, setRepoName] = useState('')
  const createRepo = useMutation({
    mutationFn: () => post(`/api/orgs/${org}/repos`, { name: repoName }),
    onSuccess: () => {
      setRepoName('')
      queryClient.invalidateQueries({ queryKey: [`/api/orgs/${org}/repos`] })
    },
  })

  const [idea, setIdea] = useState({ title: '', body: '', repos: [] as string[] })
  const createIdea = useMutation({
    mutationFn: () => post(`/api/orgs/${org}/ideas`, idea),
    onSuccess: () => {
      setIdea({ title: '', body: '', repos: [] })
      queryClient.invalidateQueries({ queryKey: [`/api/orgs/${org}/ideas`] })
    },
  })

  return (
    <>
      <div className="page-head">
        <h1>{org}</h1>
      </div>
      <Section
        title="Repositories"
        actions={
          <Toggle label="New repository">
            {(close) => (
              <form
                className="form-inline"
                onSubmit={(e) => {
                  e.preventDefault()
                  createRepo.mutate(undefined, { onSuccess: close })
                }}
              >
                <input value={repoName} onChange={(e) => setRepoName(e.target.value)} placeholder="name" autoFocus />
                <button className="button button-primary" disabled={!repoName || createRepo.isPending}>
                  Create
                </button>
                <button type="button" className="button button-quiet" onClick={close}>
                  Cancel
                </button>
                <ErrorBox error={createRepo.error} />
              </form>
            )}
          </Toggle>
        }
      >
        {repos.isPending && <Loading />}
        <ErrorBox error={repos.error} />
        {repos.data?.length === 0 && <Empty>No repositories yet.</Empty>}
        <ul className="list">
          {repos.data?.map((r) => (
            <li key={r.full_name} className="list-row">
              <Link to={`/${r.full_name}`} className="list-title">
                {r.name}
              </Link>
            </li>
          ))}
        </ul>
      </Section>

      <OrgServices org={org} />

      <Section
        title="Ideas"
        actions={
          <Toggle label="New idea">
            {(close) => (
              <form
                className="form"
                onSubmit={(e) => {
                  e.preventDefault()
                  createIdea.mutate(undefined, { onSuccess: close })
                }}
              >
                <input
                  value={idea.title}
                  onChange={(e) => setIdea({ ...idea, title: e.target.value })}
                  placeholder="What came to mind?"
                  autoFocus
                />
                <textarea
                  value={idea.body}
                  onChange={(e) => setIdea({ ...idea, body: e.target.value })}
                  placeholder="More detail (optional)"
                  rows={3}
                />
                <div className="checks">
                  <span className="muted small">Related repositories</span>
                  {repos.data?.map((r) => (
                    <label key={r.name} className="check">
                      <input
                        type="checkbox"
                        checked={idea.repos.includes(r.name)}
                        onChange={(e) =>
                          setIdea({
                            ...idea,
                            repos: e.target.checked ? [...idea.repos, r.name] : idea.repos.filter((x) => x !== r.name),
                          })
                        }
                      />
                      {r.name}
                    </label>
                  ))}
                </div>
                <div className="form-actions">
                  <button className="button button-primary" disabled={!idea.title || createIdea.isPending}>
                    Add idea
                  </button>
                  <button type="button" className="button button-quiet" onClick={close}>
                    Cancel
                  </button>
                </div>
                <ErrorBox error={createIdea.error} />
              </form>
            )}
          </Toggle>
        }
      >
        <p className="muted small">
          Ideas say neither whether nor how. They stay until someone turns them into an issue.
        </p>
        {ideas.isPending && <Loading />}
        <ErrorBox error={ideas.error} />
        {ideas.data?.length === 0 && <Empty>No ideas yet.</Empty>}
        <ul className="list">
          {ideas.data
            ?.slice()
            .reverse()
            .map((it) => (
              <li key={it.number} className="list-row">
                <span className="num">{it.number}</span>
                <Link to={`/${org}/ideas/${it.number}`} className="list-title">
                  {it.title}
                </Link>
                <Identity handle={it.author} />
                {it.issues.length > 0 && <State ok>became {it.issues.length} issue(s)</State>}
              </li>
            ))}
        </ul>
      </Section>
    </>
  )
}

// OrgServices shows the services installed in the org, with what each may do
// here, and lets an org admin install more.
function OrgServices({ org }: { org: string }) {
  const services = useApi<ServiceInfo[]>('/api/services')
  const queryClient = useQueryClient()
  const [pick, setPick] = useState('')
  const install = useMutation({
    mutationFn: (service: string) => post(`/api/orgs/${org}/services`, { service }),
    onSuccess: () => {
      setPick('')
      queryClient.invalidateQueries({ queryKey: ['/api/services'] })
    },
  })
  const all = (services.data ?? []).filter((s) => s.id !== 'service:gitsynth')
  const installed = all.filter((s) => s.orgs.includes(org))
  const available = all.filter((s) => !s.orgs.includes(org))
  return (
    <Section title="Services">
      {installed.length === 0 && <Empty>No services installed.</Empty>}
      <ul className="list">
        {installed.map((s) => (
          <li key={s.id} className="list-row">
            <Identity handle={s.id} />
            <span className="list-title">{s.manifest.description}</span>
            <span className="muted small">may {(s.manifest.permissions ?? []).join(', ') || 'nothing'}</span>
          </li>
        ))}
      </ul>
      {available.length > 0 && (
        <form
          className="form-inline"
          onSubmit={(e) => {
            e.preventDefault()
            install.mutate(pick || available[0].id)
          }}
        >
          <select value={pick} onChange={(e) => setPick(e.target.value)}>
            {available.map((s) => (
              <option key={s.id} value={s.id}>
                {s.id}: asks for {(s.manifest.permissions ?? []).join(', ') || 'nothing'}
              </option>
            ))}
          </select>
          <button className="button" disabled={install.isPending}>
            Install and approve
          </button>
        </form>
      )}
      <ErrorBox error={install.error} />
    </Section>
  )
}

export function IdeaPage() {
  const { org = '', n = '' } = useParams()
  const idea = useApi<Idea>(`/api/orgs/${org}/ideas/${n}`)
  const navigate = useNavigate()
  const [repo, setRepo] = useState('')
  const convert = useMutation({
    mutationFn: (target: string) => post<{ repo: string; issue: number }>(`/api/orgs/${org}/ideas/${n}/convert`, { repo: target }),
    onSuccess: (res) => navigate(`/${res.repo}/issues/${res.issue}`),
  })

  if (idea.isPending) return <Loading />
  if (idea.error) return <ErrorBox error={idea.error} />
  const it = idea.data
  const target = repo || it.repos[0]?.split('/')[1] || ''
  return (
    <>
      <div className="page-head">
        <div className="crumbs">
          <Link to={`/${org}`}>{org}</Link> / idea {it.number}
        </div>
        <h1>{it.title}</h1>
        <div className="meta">
          <Identity handle={it.author} />
          {it.repos.map((r) => (
            <Link key={r} to={`/${r}`} className="tag">
              {r}
            </Link>
          ))}
        </div>
      </div>
      <Text>{it.body}</Text>
      <Section title="Issues made from this idea">
        {it.issues.length === 0 && <Empty>Not turned into an issue yet.</Empty>}
        <ul className="list">
          {it.issues.map((i) => (
            <li key={`${i.repo}#${i.number}`} className="list-row">
              <Link to={`/${i.repo}/issues/${i.number}`}>
                {i.repo} issue {i.number}
              </Link>
            </li>
          ))}
        </ul>
        {it.repos.length > 0 && (
          <form
            className="form-inline"
            onSubmit={(e) => {
              e.preventDefault()
              convert.mutate(target)
            }}
          >
            <select value={target} onChange={(e) => setRepo(e.target.value)}>
              {it.repos.map((r) => (
                <option key={r} value={r.split('/')[1]}>
                  {r}
                </option>
              ))}
            </select>
            <button className="button button-primary" disabled={convert.isPending}>
              Turn into an issue
            </button>
            <ErrorBox error={convert.error} />
          </form>
        )}
      </Section>
    </>
  )
}
