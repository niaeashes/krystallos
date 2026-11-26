import { useState } from 'react'
import { Link } from 'react-router'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { post, useApi, type Repo } from '../api'
import { Empty, ErrorBox, Loading, Section, Toggle } from '../ui'
import { useMe } from '../App'

function OrgRepos({ org }: { org: string }) {
  const repos = useApi<Repo[]>(`/api/orgs/${org}/repos`)
  if (repos.isPending) return <Loading />
  if (repos.error) return <ErrorBox error={repos.error} />
  if (repos.data.length === 0) return <Empty>No repositories yet.</Empty>
  return (
    <ul className="list">
      {repos.data.map((r) => (
        <li key={r.full_name} className="list-row">
          <Link to={`/${r.full_name}`} className="list-title">
            {r.name}
          </Link>
          <span className="muted small">default branch {r.default_branch}</span>
        </li>
      ))}
    </ul>
  )
}

export function HomePage() {
  const me = useMe()
  const orgs = useApi<{ orgs: string[] }>('/api/orgs')
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const create = useMutation({
    mutationFn: () => post('/api/orgs', { name }),
    onSuccess: () => {
      setName('')
      queryClient.invalidateQueries({ queryKey: ['/api/orgs'] })
    },
  })

  return (
    <>
      <div className="page-head">
        <h1>Welcome back</h1>
        <p className="muted">
          Signed in as {me.identity}. Everything you do here is signed with this identity and recorded.
        </p>
      </div>
      {orgs.isPending && <Loading />}
      <ErrorBox error={orgs.error} />
      {orgs.data?.orgs.length === 0 && <Empty>You are not in any org yet.</Empty>}
      {orgs.data?.orgs.map((org) => (
        <Section key={org} title={<Link to={`/${org}`}>{org}</Link>}>
          <OrgRepos org={org} />
        </Section>
      ))}
      <Toggle label="New org">
        {(close) => (
          <form
            className="form-inline"
            onSubmit={(e) => {
              e.preventDefault()
              create.mutate(undefined, { onSuccess: close })
            }}
          >
            <input value={name} onChange={(e) => setName(e.target.value)} placeholder="org name" autoFocus />
            <button className="button button-primary" disabled={!name || create.isPending}>
              Create
            </button>
            <button type="button" className="button button-quiet" onClick={close}>
              Cancel
            </button>
            <ErrorBox error={create.error} />
          </form>
        )}
      </Toggle>
    </>
  )
}
