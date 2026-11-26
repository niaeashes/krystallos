import { useQuery } from '@tanstack/react-query'

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

// Every call sends the CSRF header: the session cookie alone is not enough
// for the server to accept a change.
export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: 'same-origin',
    headers: {
      'X-Gitsynth-CSRF': '1',
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    },
    body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body),
  })
  const text = await res.text()
  let data: unknown = null
  if (text) {
    try {
      data = JSON.parse(text)
    } catch {
      data = text
    }
  }
  if (!res.ok) {
    let msg = res.statusText
    if (data && typeof data === 'object' && 'error' in data) msg = String((data as { error: unknown }).error)
    else if (typeof data === 'string' && data.trim()) msg = data.trim()
    throw new ApiError(res.status, msg)
  }
  return data as T
}

export const get = <T>(path: string) => api<T>('GET', path)
export const post = <T>(path: string, body?: unknown) => api<T>('POST', path, body ?? {})

export function useApi<T>(path: string | null) {
  return useQuery({
    queryKey: [path],
    queryFn: () => get<T>(path!),
    enabled: path !== null,
  })
}

export const repoPath = (org: string, repo: string) => `/api/repos/${org}/${repo}`

// ---- types mirroring the server's JSON ----

export interface Me {
  identity: string
  principal: string
  session: string
  kind: 'user' | 'bot' | 'service'
  admin: boolean
  commit_email: string
}

export interface MergeRule {
  required_approvals: number
  required_checks: string[]
}

export interface Repo {
  full_name: string
  org: string
  name: string
  default_branch: string
  merge_rule: MergeRule
}

export interface Ref {
  name: string
  kind: 'branch' | 'tag'
  commit: string
}

export interface RepoInfo {
  repo: Repo
  refs: Ref[]
  clone_url: string
  // Present when the server serves git over SSH.
  ssh_clone_url?: string
  permissions: { push: boolean; admin: boolean }
}

export interface TreeEntry {
  name: string
  path: string
  type: 'blob' | 'tree' | 'commit'
  mode: string
  oid: string
  size?: number
}

export interface Blob {
  path: string
  size: number
  binary: boolean
  truncated: boolean
  content: string
}

export interface Commit {
  oid: string
  parents: string[]
  author_name: string
  author_email: string
  author_date: string
  committer_name: string
  committer_email: string
  committer_date: string
  subject: string
  body: string
  committer_identity?: string
  author_identity?: string
}

export interface IdentityEmailAddress {
  email: string
  identity: string
}

export interface IdeaRef {
  org: string
  number: number
}

export interface Idea {
  org: string
  number: number
  title: string
  body: string
  author: string
  repos: string[]
  issues: { repo: string; number: number }[]
}

export interface IssueSummary {
  number: number
  title: string
  author: string
  closed: boolean
}

export interface StepView {
  id: string
  title: string
  changesets: number[]
  merged: boolean
}

export interface PlanView {
  number: number
  issue: number
  title: string
  author: string
  version: number
  body: string
  steps: StepView[]
  complete: boolean
}

export interface ChangeSet {
  repo: string
  number: number
  issue: number
  title: string
  author: string
  branch: string
  base: string
  plan?: number
  plan_version?: number
  steps: string[]
  merged_commit?: string
}

export interface ChangeSetSummary {
  number: number
  issue: number
  title: string
  author: string
  branch: string
  base: string
  merged: boolean
}

export interface IssueView {
  repo: string
  number: number
  title: string
  body: string
  author: string
  closed: boolean
  from_ideas: IdeaRef[]
  comments: { author: string; body: string }[]
  plans: PlanView[]
  changesets: ChangeSet[]
  hints: string[]
}

export interface ReviewComment {
  path: string
  line: number
  side: 'new' | 'old'
  body: string
}

export interface Review {
  seq: number
  author: string
  verdict: 'approve' | 'comment' | 'request_changes'
  commit: string
  body: string
  comments: ReviewComment[]
}

export interface MergeStatus {
  head?: string
  mergeable: boolean
  approvals: string[]
  unmet: string[]
}

export interface ChangeSetView extends ChangeSet {
  reviews: Review[]
  status: MergeStatus
}

export interface FileDiff {
  path: string
  old_path?: string
  status: string
  additions: number
  deletions: number
  binary: boolean
  truncated: boolean
  patch: string
}

export interface Diff {
  base: string
  head: string
  merge_base: string
  files: FileDiff[]
}

export interface GitsynthEvent {
  seq: number
  type: string
  version: number
  org?: string
  repo?: string
  identity: string
  principal: string
  session?: string
  payload: Record<string, unknown> | null
  at: string
  prev_hash: string
  hash: string
}

export interface IdentityInfo {
  id: string
  parent?: string
  disabled: boolean
}

export interface BotInfo {
  id: string
  owner: string
  identities: IdentityInfo[]
}

export interface Manifest {
  name: string
  description?: string
  permissions: string[]
  events?: string[]
}

export interface ServiceInfo {
  id: string
  manifest: Manifest
  orgs: string[]
}

export interface LinkInfo {
  purpose: 'register' | 'invite' | 'login'
  user?: string
  org?: string
}

export interface CredentialInfo {
  id: string
  identity: string
  kind: 'token' | 'browser' | 'session' | 'ssh_key'
  session?: string
  // A session's label, or an SSH key's name.
  label?: string
  expires_at?: string
  revoked: boolean
  key_type?: string
  fingerprint?: string
}

export interface SSHKey {
  id: string
  identity: string
  name?: string
  type: string
  fingerprint: string
  public_key: string
  committer_email: string
}

export interface IssuedToken {
  id: string
  identity: string
  expires_at?: string
  token: string
  committer_email: string
}

// AuthRequestView is an MCP client's request to act as an identity, as the
// consent page shows it.
export interface AuthRequestView {
  id: string
  client: { id: string; name: string; via: 'registration' | 'metadata_document'; host?: string }
  // Where the browser takes the answer: a site (web), a program listening on
  // this computer (loopback), or whichever app the operating system hands
  // redirect_scheme links to (app).
  redirect_kind: 'web' | 'loopback' | 'app'
  redirect_host?: string
  redirect_scheme?: string
  identity: string
  resource: string
  create?: string[]
  problem?: string
}

// GrantInfo is an MCP client connected to an identity.
export interface GrantInfo {
  id: string
  client_id: string
  client_name: string
  via: string
  identity: string
  resource: string
  created_at: string
  expires_at: string
}
